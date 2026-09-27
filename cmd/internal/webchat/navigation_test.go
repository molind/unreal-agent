package webchat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/storage"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func catalog(t *testing.T, s *Server) navigation {
	t.Helper()
	w := request(t, s, "GET", "/api/navigation", nil)
	ok(t, w)
	var result navigation
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func seedWorkspace(t *testing.T, c Config, name string) (Project, *localfile.Store) {
	t.Helper()
	path := filepath.Join(c.Getenv("HOME"), name)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := storage.Directory(path, c.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	store, release, err := localfile.OpenWorkspace(t.Context(), dir, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(); _ = release() })
	return Project{ID: storage.Hash([]byte(path)), Path: path, Name: name}, store
}

func seedInput(t *testing.T, store *localfile.Store, id, text string) {
	t.Helper()
	if _, err := store.Create(t.Context(), session.ID(id)); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(text)
	if err := store.AppendInput(t.Context(), session.ID(id), inbox.Input{ID: inbox.ID(id + "-input"), Kind: inbox.InputExternal, Payload: raw}); err != nil {
		t.Fatal(err)
	}
}

func TestNavigationDiscoversCLIProjectsAndSortsMessages(t *testing.T) {
	c, model := testConfig(t)
	a, storeA := seedWorkspace(t, c, "alpha")
	seedInput(t, storeA, "a", "First question")
	s := newTestServer(t, c)
	first := catalog(t, s)
	if len(first.Projects) != 1 || first.Projects[0].ID != a.ID {
		t.Fatalf("CLI workspace not discovered: %+v", first)
	}
	aTime := first.Projects[0].Sessions[0].Updated

	// Discover a database created by another host after web startup.
	b, storeB := seedWorkspace(t, c, "beta")
	seedInput(t, storeB, "b", "Second question")
	second := catalog(t, s)
	if len(second.Projects) != 2 || second.Projects[0].ID != b.ID {
		t.Fatalf("latest input not first: %+v", second)
	}

	if err := storeA.AppendTurn(t.Context(), "a", session.Turn{ID: "turn", Type: session.TurnRegular}); err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: "op", Type: "test", Version: 1, Status: operation.StatusReady, State: []byte(`{}`)}
	status := sessionstore.ToolCallStatus{TurnID: "turn", CallID: "call", Status: tool.CallStatus{WaitingFor: []operation.ID{"op"}}, Operations: []operation.Operation{op}}
	if err := storeA.AppendToolCallStatus(t.Context(), "a", status); err != nil {
		t.Fatal(err)
	}
	op.Status = operation.StatusCompleted
	if err := storeA.SaveOperation(t.Context(), "a", op); err != nil {
		t.Fatal(err)
	}
	third := catalog(t, s)
	if third.Projects[0].ID != b.ID || !third.Projects[1].Sessions[0].Updated.Equal(aTime) {
		t.Fatal("tool checkpoint changed conversation order")
	}

	if err := storeA.AppendModelResponse(t.Context(), "a", sessionstore.ModelResponse{TurnID: "turn", Response: answer("Latest assistant answer")}); err != nil {
		t.Fatal(err)
	}
	fourth := catalog(t, s)
	if fourth.Projects[0].ID != a.ID || !fourth.Projects[0].Updated.After(second.Projects[0].Updated) {
		t.Fatal("assistant reply did not change order")
	}
	if fourth.Projects[0].Sessions[0].Title != "First question" {
		t.Fatal("cached title changed")
	}
	if err := storeB.AppendTurn(t.Context(), "b", session.Turn{ID: "compact", Type: session.TurnCompaction}); err != nil {
		t.Fatal(err)
	}
	if err := storeB.AppendModelResponse(t.Context(), "b", sessionstore.ModelResponse{TurnID: "compact", Response: answer("Internal compaction summary")}); err != nil {
		t.Fatal(err)
	}
	if compacted := catalog(t, s); compacted.Projects[0].ID != a.ID || !compacted.Projects[1].Updated.Equal(second.Projects[0].Updated) {
		t.Fatal("compaction summary changed conversation order")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = newTestServer(t, c)
	restarted := catalog(t, s)
	if restarted.Projects[0].ID != a.ID || !restarted.Projects[0].Updated.Equal(fourth.Projects[0].Updated) {
		t.Fatal("order did not survive restart")
	}
	select {
	case <-model.calls:
		t.Fatal("catalog started an agent")
	default:
	}
}

func TestPinnedConversationsPersistAcrossProjects(t *testing.T) {
	c, model := testConfig(t)
	a, storeA := seedWorkspace(t, c, "alpha")
	b, storeB := seedWorkspace(t, c, "beta")
	seedInput(t, storeA, "same-id", "Alpha question")
	seedInput(t, storeB, "same-id", "Beta question")
	s := newTestServer(t, c)
	catalog(t, s)
	pathA := "/api/projects/" + a.ID + "/sessions/same-id/pin"
	pathB := "/api/projects/" + b.ID + "/sessions/same-id/pin"
	for _, path := range []string{pathA, pathB, pathA} {
		ok(t, request(t, s, "POST", path, map[string]bool{"pinned": true}))
	}
	result := catalog(t, s)
	if len(result.Pinned) != 2 || result.Pinned[0].Project != b.ID {
		t.Fatalf("pin identity/order: %+v", result.Pinned)
	}
	if request(t, s, "POST", "/api/projects/"+a.ID+"/sessions/missing/pin", map[string]bool{"pinned": true}).Code == 200 {
		t.Fatal("pinned missing conversation")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = newTestServer(t, c)
	if len(catalog(t, s).Pinned) != 2 {
		t.Fatal("pins not persisted")
	}
	ok(t, request(t, s, "POST", pathA, map[string]bool{"pinned": false}))
	result = catalog(t, s)
	if len(result.Pinned) != 1 || result.Pinned[0].Project != b.ID {
		t.Fatal("unpin changed another workspace")
	}
	select {
	case <-model.calls:
		t.Fatal("pin started an agent")
	default:
	}
}

func TestBrokenDatabaseDoesNotHideOtherProjects(t *testing.T) {
	c, _ := testConfig(t)
	a, store := seedWorkspace(t, c, "alpha")
	seedInput(t, store, "a", "Question")
	root, err := storage.WorkspacesDirectory(c.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, storage.Filename), []byte("invalid sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, c)
	result := catalog(t, s)
	if len(result.Projects) != 1 || result.Projects[0].ID != a.ID || len(result.Warnings) != 1 {
		t.Fatalf("partial catalog: %+v", result)
	}
}
