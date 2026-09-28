package webchat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/chat"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

type sessionRef struct {
	Project string `json:"project"`
	Session string `json:"session"`
}

type projectNavigation struct {
	Project
	Updated  time.Time     `json:"updated"`
	Sessions []sessionInfo `json:"sessions"`
	Error    string        `json:"error,omitempty"`
}

type navigation struct {
	Projects []projectNavigation `json:"projects"`
	Pinned   []sessionRef        `json:"pinned"`
	Warnings []string            `json:"warnings"`
}

// Discover only the standard catalog, not arbitrary folders on the machine.
// Read-only SQLite connections include live WAL data without creating databases
// or triggering a workspace migration merely to find its identity.
func (s *Server) discoverProjects(ctx context.Context) []string {
	root, err := storage.WorkspacesDirectory(s.config.Getenv)
	if err != nil {
		return []string{s.safe(err.Error())}
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []string{s.safe(err.Error())}
	}
	var warnings []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), storage.Filename)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil && !info.Mode().IsRegular() {
			err = errors.New("not a regular SQLite file")
		}
		var workspacePath string
		if err == nil {
			workspacePath, err = storedWorkspace(ctx, path)
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue // Unbound databases and empty test/initialization databases.
		}
		if err == nil && (!filepath.IsAbs(workspacePath) || storage.Hash([]byte(workspacePath)) != entry.Name()) {
			err = errors.New("workspace identity does not match its storage directory")
		}
		if err != nil {
			warnings = append(warnings, s.safe(fmt.Sprintf("%s: %v", path, err)))
			continue
		}
		p := Project{ID: entry.Name(), Path: workspacePath, Name: filepath.Base(workspacePath)}
		s.mu.Lock()
		if !s.closed && s.projects[p.ID] == nil {
			s.projects[p.ID] = &workspace{Project: p, owners: make(map[string]*chat.Headless)}
		}
		s.mu.Unlock()
	}
	return warnings
}

func storedWorkspace(ctx context.Context, path string) (string, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(1000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return "", err
	}
	defer db.Close()
	var pathBytes []byte
	err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE namespace='workspace' AND key='path' AND EXISTS (SELECT 1 FROM sessions)").Scan(&pathBytes)
	return string(pathBytes), err
}

func (s *Server) navigation(w http.ResponseWriter, r *http.Request) {
	s.navigationMu.Lock()
	result := navigation{Projects: []projectNavigation{}, Warnings: s.discoverProjects(r.Context())}
	s.mu.Lock()
	projects := make([]*workspace, 0, len(s.projects))
	for _, p := range s.projects {
		projects = append(projects, p)
	}
	result.Pinned = append([]sessionRef{}, s.pins...)
	s.mu.Unlock()
	for _, p := range projects {
		item := projectNavigation{Project: p.Project, Sessions: []sessionInfo{}}
		list, err := s.sessions(r.Context(), p)
		if err != nil {
			item.Error = s.safe(err.Error())
		} else {
			item.Sessions = list
			if len(list) > 0 {
				item.Updated = list[0].Updated
			}
		}
		result.Projects = append(result.Projects, item)
	}
	// Ignore stale pins after a deletion even if the separate pins file could
	// not be written. Keep pins for unavailable projects so they can recover.
	result.Pinned = slices.DeleteFunc(result.Pinned, func(ref sessionRef) bool {
		for _, p := range result.Projects {
			if p.ID == ref.Project && p.Error == "" {
				return !slices.ContainsFunc(p.Sessions, func(info sessionInfo) bool { return info.ID == ref.Session })
			}
		}
		return false
	})
	slices.SortFunc(result.Projects, func(a, b projectNavigation) int {
		if c := b.Updated.Compare(a.Updated); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	s.navigationMu.Unlock() // Never hold catalog or workspace locks over a network write.
	respond(w, result)
}

func (s *Server) pinSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Pinned bool `json:"pinned"`
	}
	if !decode(w, r, &body) {
		return
	}
	p, err := s.project(r)
	if err == nil {
		err = func() error {
			p.mu.Lock()
			defer p.mu.Unlock()
			if body.Pinned {
				if err := s.open(p); err != nil {
					return err
				}
				if _, err := p.store.Inspect(r.Context(), session.ID(r.PathValue("session"))); err != nil {
					return err
				}
			}
			return s.setPinned(sessionRef{Project: p.ID, Session: r.PathValue("session")}, body.Pinned)
		}()
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	respond(w, map[string]bool{"pinned": body.Pinned})
}

// Callers hold p.mu to serialize the existence check with deletion.
func (s *Server) setPinned(ref sessionRef, pinned bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("server is shutting down")
	}
	if slices.Contains(s.pins, ref) == pinned {
		return nil
	}
	pins := make([]sessionRef, 0, len(s.pins)+1)
	for _, old := range s.pins {
		if old != ref {
			pins = append(pins, old)
		}
	}
	if pinned {
		pins = append([]sessionRef{ref}, pins...)
	}
	data, err := json.MarshalIndent(pins, "", "  ")
	if err == nil {
		err = writePrivate(filepath.Join(s.config.StateDirectory, "pins.json"), data)
	}
	if err != nil {
		return err
	}
	s.pins = pins
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

// Cache only the append-only history projection. SQL's updated timestamp also
// changes for tool checkpoints, so it is an invalidation key, never sort order.
type sessionSummary struct {
	info        sessionInfo
	changed     time.Time
	after       sessionstore.Sequence
	compactions map[session.TurnID]bool
}

func compareSessions(a, b sessionInfo) int {
	if c := b.Updated.Compare(a.Updated); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

func (s *Server) sessionSummary(ctx context.Context, p *workspace, row sessionstore.SessionInfo) (sessionInfo, error) {
	id := string(row.ID)
	cached := p.summaries[id]
	if cached == nil {
		snapshot, err := p.store.Inspect(ctx, row.ID)
		if err != nil {
			return sessionInfo{}, err
		}
		cached = &sessionSummary{info: sessionInfo{ID: id, Updated: snapshot.Session.CreatedAt, State: "saved"}, compactions: make(map[session.TurnID]bool)}
		p.summaries[id] = cached
	}
	if cached.changed.Equal(row.LastUpdatedAt) {
		return cached.info, nil
	}
	for {
		page, err := p.store.Items(ctx, row.ID, cached.after, 64)
		if err != nil {
			return sessionInfo{}, err
		}
		for _, item := range page.Items {
			switch value := item.Data.(type) {
			case inbox.Input:
				if value.Kind == inbox.InputExternal {
					if cached.info.Title == "" {
						var raw string
						if err := json.Unmarshal(value.Payload, &raw); err != nil {
							return sessionInfo{}, err
						}
						text := []rune(strings.Join(strings.Fields(s.safe(raw)), " "))
						if len(text) > 0 {
							cached.info.Title = string(text[:min(90, len(text))])
						}
					}
					cached.info.Updated = item.RecordedAt
				}
			case session.Turn:
				if value.Type == session.TurnCompaction {
					cached.compactions[value.ID] = true
				}
			case sessionstore.ModelResponse:
				if cached.compactions[value.TurnID] {
					continue
				}
				for _, output := range value.Response.Output {
					if msg, ok := output.Data.(llm.Message); ok && output.Type == llm.ItemMessage && msg.Role == llm.RoleAssistant && strings.TrimSpace(msg.Text) != "" {
						cached.info.Updated = item.RecordedAt
					}
				}
			}
		}
		cached.after = page.NextAfter
		if !page.More {
			break
		}
	}
	cached.changed = row.LastUpdatedAt
	return cached.info, nil
}
