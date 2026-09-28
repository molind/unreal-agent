// Package webchat hosts local agent sessions and a mobile web interface.
package webchat

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/cmd/internal/chat"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/storage"
	"golang.org/x/sys/unix"
)

//go:embed static/*
var assets embed.FS

type Config struct {
	StateDirectory string
	Origins        []string
	TailscaleUser  string
	ChatArgs       []string
	Getenv         func(string) string
	Providers      []agentrunner.Provider
}

type Project struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
}

type workspace struct {
	Project
	mu        sync.Mutex
	store     *localfile.Store
	release   func() error
	owners    map[string]*chat.Headless
	inFlight  map[string]int // Owner references held by HTTP actions; protected by mu.
	summaries map[string]*sessionSummary
}

type Server struct {
	config          Config
	ctx             context.Context
	cancel          context.CancelFunc
	mu              sync.Mutex
	projects        map[string]*workspace
	pins            []sessionRef
	navigationMu    sync.Mutex // Serialize catalog scans across connected browsers.
	changed         chan struct{}
	closed          bool
	lock            *os.File
	token           string
	safe            func(string) string
	handler         http.Handler
	maintenanceStop chan struct{}
	maintenanceDone chan struct{}
}

func New(c Config) (_ *Server, result error) {
	if c.Getenv == nil {
		c.Getenv = os.Getenv
	}
	if c.Providers == nil {
		c.Providers = agentrunner.DefaultProviders()
	}
	if len(c.Origins) == 0 {
		return nil, errors.New("at least one allowed origin is required")
	}
	for _, origin := range c.Origins {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("invalid origin %q; use scheme://host[:port]", origin)
		}
	}
	if err := os.MkdirAll(c.StateDirectory, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(c.StateDirectory, "server.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another web server owns this state directory")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{config: c, ctx: ctx, cancel: cancel, projects: make(map[string]*workspace), changed: make(chan struct{}), lock: lock, safe: chat.WebRedactor(c.Getenv)}
	defer func() {
		if result != nil {
			result = errors.Join(result, s.Close())
		}
	}()
	tokenPath := filepath.Join(c.StateDirectory, "access-token")
	data, err := os.ReadFile(tokenPath)
	if errors.Is(err, os.ErrNotExist) {
		b := make([]byte, 32)
		if _, err = rand.Read(b); err != nil {
			return nil, err
		}
		data = []byte(hex.EncodeToString(b))
		err = os.WriteFile(tokenPath, data, 0600)
	}
	if err != nil {
		return nil, err
	}
	s.token = strings.TrimSpace(string(data))
	if len(s.token) != 64 {
		return nil, errors.New("invalid access-token file")
	}
	if err = os.Chmod(tokenPath, 0600); err != nil {
		return nil, err
	}
	data, err = os.ReadFile(filepath.Join(c.StateDirectory, "projects.json"))
	if err == nil {
		var projects []Project
		if err = json.Unmarshal(data, &projects); err != nil {
			return nil, err
		}
		for _, p := range projects {
			s.projects[p.ID] = &workspace{Project: p, owners: make(map[string]*chat.Headless)}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data, err = os.ReadFile(filepath.Join(c.StateDirectory, "pins.json"))
	if err == nil {
		if err = json.Unmarshal(data, &s.pins); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("GET /api/navigation", s.navigation)
	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.addProject)
	mux.HandleFunc("GET /api/projects/{project}/sessions", s.listSessions)
	mux.HandleFunc("POST /api/projects/{project}/sessions", s.createSession)
	mux.HandleFunc("POST /api/projects/{project}/sessions/{session}/pin", s.pinSession)
	mux.HandleFunc("POST /api/projects/{project}/sessions/{session}/rename", s.renameSession)
	mux.HandleFunc("GET /api/projects/{project}/sessions/{session}", s.history)
	mux.HandleFunc("DELETE /api/projects/{project}/sessions/{session}", s.deleteSession)
	mux.HandleFunc("POST /api/projects/{project}/sessions/{session}/{action}", s.action)
	mux.HandleFunc("GET /api/events", s.events)
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	s.handler = s.guard(mux)
	s.maintenanceStop, s.maintenanceDone = make(chan struct{}), make(chan struct{})
	go s.maintain()
	return s, nil
}

func (s *Server) TokenPath() string                                { return filepath.Join(s.config.StateDirectory, "access-token") }
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) signal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		close(s.changed)
		s.changed = make(chan struct{})
	}
}

func (s *Server) authenticated(r *http.Request) bool {
	if s.config.TailscaleUser != "" && r.Header.Get("Tailscale-User-Login") == s.config.TailscaleUser {
		return true
	}
	cookie, err := r.Cookie("unreal_session")
	return err == nil && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(s.token)) == 1
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(35 * time.Second))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'none'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		allowed := ""
		for _, origin := range s.config.Origins {
			u, _ := url.Parse(origin)
			if r.Host == u.Host {
				allowed = origin
				break
			}
		}
		if allowed == "" {
			http.Error(w, "unrecognized host", http.StatusForbidden)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("Origin") != allowed {
				http.Error(w, "invalid origin", http.StatusForbidden)
				return
			}
			if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
				http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
				return
			}
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != allowed {
			http.Error(w, "invalid origin", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/login" && !s.authenticated(r) {
			http.Error(w, "sign in required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, (1<<20)+4096)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		http.Error(w, "invalid or oversized JSON body", 400)
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		http.Error(w, "expected one JSON object", 400)
		return false
	}
	return true
}

func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) fail(w http.ResponseWriter, err error) {
	http.Error(w, s.safe(err.Error()), http.StatusConflict)
}

func (s *Server) cookie(r *http.Request, value string, maxAge int) *http.Cookie {
	secure := true
	for _, origin := range s.config.Origins {
		u, _ := url.Parse(origin)
		if u.Host == r.Host {
			secure = u.Scheme == "https"
			break
		}
	}
	return &http.Cookie{Name: "unreal_session", Value: value, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: maxAge}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &body) {
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.Token)), []byte(s.token)) != 1 {
		http.Error(w, "invalid access token", 401)
		return
	}
	http.SetCookie(w, s.cookie(r, s.token, 30*24*3600))
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.cookie(r, "", -1))
	respond(w, map[string]bool{"ok": true})
}
func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]any{"tailscale_identity": s.config.TailscaleUser != ""})
}

func (s *Server) projectList() []Project {
	projects := make([]Project, 0, len(s.projects))
	for _, p := range s.projects {
		projects = append(projects, p.Project)
	}
	slices.SortFunc(projects, func(a, b Project) int { return strings.Compare(a.Path, b.Path) })
	return projects
}
func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	list := s.projectList()
	s.mu.Unlock()
	respond(w, list)
}

// AddProject registers an existing canonical folder. It does not run an agent.
func (s *Server) AddProject(path string) (Project, error) {
	if !filepath.IsAbs(path) {
		return Project{}, errors.New("use an absolute path on the server machine")
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Project{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Project{}, err
	}
	if !info.IsDir() {
		return Project{}, errors.New("workspace must be a directory")
	}
	p := Project{ID: storage.Hash([]byte(path)), Path: path, Name: filepath.Base(path)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Project{}, errors.New("server is shutting down")
	}
	if old := s.projects[p.ID]; old != nil {
		return old.Project, nil
	}
	s.projects[p.ID] = &workspace{Project: p, owners: make(map[string]*chat.Headless)}
	data, err := json.MarshalIndent(s.projectList(), "", "  ")
	if err == nil {
		err = writePrivate(filepath.Join(s.config.StateDirectory, "projects.json"), data)
	}
	if err != nil {
		delete(s.projects, p.ID)
		return Project{}, err
	}
	close(s.changed)
	s.changed = make(chan struct{})
	return p, nil
}

func writePrivate(path string, data []byte) (result error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".web-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func (s *Server) addProject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &body) {
		return
	}
	p, err := s.AddProject(body.Path)
	if err != nil {
		s.fail(w, err)
		return
	}
	respond(w, p)
}

// Callers hold p.mu while using the reader or changing its owner map.
func (s *Server) project(r *http.Request) (*workspace, error) {
	s.mu.Lock()
	p, closed := s.projects[r.PathValue("project")], s.closed
	s.mu.Unlock()
	if closed {
		return nil, errors.New("server is shutting down")
	}
	if p == nil {
		return nil, errors.New("unknown workspace")
	}
	return p, nil
}
func (s *Server) open(p *workspace) error {
	if err := s.checkOpen(); err != nil {
		return err
	}
	if p.store != nil {
		return nil
	}
	dir, err := storage.Directory(p.Path, s.config.Getenv)
	if err != nil {
		return err
	}
	p.store, p.release, err = localfile.OpenWorkspace(s.ctx, dir, p.Path)
	return err
}

func (s *Server) checkOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("server is shutting down")
	}
	return nil
}

type sessionInfo struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Updated time.Time `json:"updated"`
	State   string    `json:"state"`
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	p, err := s.project(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	list, err := s.sessions(r.Context(), p)
	if err != nil {
		s.fail(w, err)
		return
	}
	respond(w, list)
}

func (s *Server) sessions(ctx context.Context, p *workspace) ([]sessionInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := s.open(p); err != nil {
		return nil, err
	}
	rows, err := p.store.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	titles, err := p.store.SessionTitles(ctx)
	if err != nil {
		return nil, err
	}
	if p.summaries == nil {
		p.summaries = make(map[string]*sessionSummary)
	}
	list := make([]sessionInfo, 0, len(rows))
	for _, row := range rows {
		x, err := s.sessionSummary(ctx, p, row)
		if err != nil {
			return nil, err
		}
		if title := titles[row.ID]; title != "" {
			x.Title = s.safe(title)
		}
		if x.Title == "" {
			x.Title = "Новая размова"
		}
		if owner := p.owners[x.ID]; owner != nil {
			x.State = owner.State()
		}
		list = append(list, x)
	}
	slices.SortFunc(list, compareSessions)
	return list, nil
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &body) {
		return
	}
	if _, err := uuid.Parse(body.ID); err != nil {
		http.Error(w, "session ID must be a UUID", 400)
		return
	}
	p, err := s.project(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	id := session.ID(body.ID)
	err = func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		if err := s.open(p); err != nil {
			return err
		}
		_, err := p.store.Inspect(r.Context(), id)
		if errors.Is(err, os.ErrNotExist) {
			release, lockErr := p.store.LockSession(id)
			if lockErr != nil {
				return lockErr
			}
			defer release()
			// Recheck under the lease: a cooperating host may have just created it.
			if _, err = p.store.Inspect(r.Context(), id); errors.Is(err, os.ErrNotExist) {
				_, err = p.store.Create(r.Context(), id)
			}
		}
		return err
	}()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.signal()
	respond(w, map[string]string{"id": body.ID})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if r.URL.Query().Get("after") == "" {
		after, err = 0, nil
	}
	if err != nil {
		http.Error(w, "invalid history cursor", 400)
		return
	}
	p, err := s.project(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("session")
	page, status, err := s.readPage(r.Context(), p, id, sessionstore.Sequence(after))
	if err != nil {
		s.fail(w, err)
		return
	}
	items := []chat.WebItem{}
	for _, item := range page.Items {
		views, err := chat.ProjectItem(item, s.safe)
		if err != nil {
			s.fail(w, err)
			return
		}
		items = append(items, views...)
	}
	respond(w, map[string]any{"items": items, "after": page.NextAfter, "more": page.More, "status": status})
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	p, err := s.project(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("session")
	var warning string
	err = func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		if err := s.open(p); err != nil {
			return err
		}
		if owner := p.owners[id]; owner != nil {
			err := owner.Close() // Join all model/tool work before dropping history.
			delete(p.owners, id)
			if err != nil {
				return err
			}
		}
		if err := p.store.DeleteSession(r.Context(), session.ID(id)); err != nil {
			return err
		}
		delete(p.summaries, id)
		if err := s.setPinned(sessionRef{Project: p.ID, Session: id}, false); err != nil {
			// The database commit has succeeded. Never report a failed deletion
			// that invites the user to keep editing an already deleted session.
			warning = "Размова выдаленая, але не ўдалося захаваць спіс замацаваных: " + s.safe(err.Error())
		}
		return nil
	}()
	if err != nil {
		s.fail(w, err)
		return
	}
	s.signal()
	respond(w, map[string]any{"deleted": true, "warning": warning})
}

func (s *Server) readPage(ctx context.Context, p *workspace, id string, after sessionstore.Sequence) (sessionstore.Page, chat.HeadlessStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	status := chat.HeadlessStatus{State: "saved", Operations: []chat.WebOperation{}}
	if err := s.open(p); err != nil {
		return sessionstore.Page{}, status, err
	}
	page, err := p.store.Items(ctx, session.ID(id), after, 64)
	if owner := p.owners[id]; owner != nil {
		status = owner.Status()
	}
	return page, status, err
}

func (s *Server) acquireOwner(p *workspace, id string) (*chat.Headless, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return nil, nil, err
	}
	owner := p.owners[id]
	if owner == nil {
		args := append([]string{}, s.config.ChatArgs...)
		args = append(args, "-session", id, p.Path)
		var err error
		owner, err = chat.OpenHeadless(s.ctx, args, s.config.Getenv, s.config.Providers, s.signal)
		if err != nil {
			return nil, nil, err
		}
		p.owners[id] = owner
	}
	if p.inFlight == nil {
		p.inFlight = make(map[string]int)
	}
	p.inFlight[id]++
	return owner, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.inFlight[id]--
		if p.inFlight[id] == 0 {
			delete(p.inFlight, id)
		}
	}, nil
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID        string `json:"id"`
		Text      string `json:"text"`
		RequestID string `json:"request_id"`
	}
	if !decode(w, r, &body) {
		return
	}
	action := r.PathValue("action")
	if !slices.Contains([]string{"send", "resume", "stop", "compact", "cancel", "release", "permit", "deny"}, action) {
		http.NotFound(w, r)
		return
	}
	p, err := s.project(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	id := r.PathValue("session")
	if action == "release" {
		p.mu.Lock()
		if owner := p.owners[id]; owner != nil {
			err = owner.Close()
			delete(p.owners, id)
		}
		p.mu.Unlock()
	} else {
		owner, done, e := s.acquireOwner(p, id)
		err = e
		if err == nil {
			defer done()
			switch action {
			case "send":
				ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
				defer cancel()
				err = owner.Send(ctx, body.ID, body.Text)
			case "resume":
				err = owner.Resume()
			case "stop":
				err = owner.Stop()
			case "compact":
				err = owner.Approve(body.RequestID)
			case "permit", "deny":
				err = owner.Permit(body.ID, body.RequestID, action == "permit")
			case "cancel":
				err = owner.Cancel(body.ID)
			}
		}
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.signal()
	respond(w, map[string]bool{"ok": true})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		changed := s.changed
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(w, "event: change\ndata: {}\n\n"); err != nil {
			return
		}
		if err := controller.Flush(); err != nil {
			return
		}
		select {
		case <-changed:
		case <-ticker.C:
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		}
	}
}

// Close joins agents before closing databases and releasing ownership.
// Call after the HTTP server has stopped accepting/drained ordinary requests.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	projects := make([]*workspace, 0, len(s.projects))
	for _, p := range s.projects {
		projects = append(projects, p)
	}
	close(s.changed)
	if s.maintenanceStop != nil {
		close(s.maintenanceStop)
	}
	s.mu.Unlock()
	if s.maintenanceDone != nil {
		<-s.maintenanceDone
	}
	// SSE handlers also observe this context; runtime shutdown below uses stop
	// first, then cancellation as the final resource cleanup.
	var result error
	for _, p := range projects {
		p.mu.Lock()
		for _, owner := range p.owners {
			result = errors.Join(result, owner.Close())
		}
		if p.store != nil {
			result = errors.Join(result, p.store.Close(), p.release())
		}
		p.mu.Unlock()
	}
	s.cancel()
	if s.lock != nil {
		result = errors.Join(result, s.lock.Close())
	}
	return result
}
