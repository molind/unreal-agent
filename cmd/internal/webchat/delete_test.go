package webchat

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

func TestDeleteRunningConversationAndRestart(t *testing.T) {
	c, model := testConfig(t)
	s := newTestServer(t, c)
	workspace := t.TempDir()
	a := projectSession(t, s, workspace, "019a0000-0000-7000-8000-000000000001")
	b := projectSession(t, s, workspace, "019a0000-0000-7000-8000-000000000002")
	file := filepath.Join(workspace, "keep.txt")
	if err := os.WriteFile(file, []byte("project file"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{a, b} {
		ok(t, request(t, s, "POST", path+"/pin", map[string]bool{"pinned": true}))
		ok(t, request(t, s, "POST", path+"/send", map[string]string{"id": "input", "text": "hello"}))
	}
	callA, callB := nextCall(t, model), nextCall(t, model)
	catalog(t, s) // Populate the summary cache before deletion.
	ok(t, request(t, s, "DELETE", a, nil))
	select {
	case <-callA.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("deleted without joining model")
	}
	select {
	case <-callB.ctx.Done():
		t.Fatal("stopped another conversation")
	default:
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "project file" {
		t.Fatal("project file changed", err)
	}
	for _, action := range []string{"send", "resume"} {
		if request(t, s, "POST", a+"/"+action, map[string]string{"id": "late", "text": "stale browser"}).Code != 409 {
			t.Fatal("deleted conversation resurrected", action)
		}
	}
	if request(t, s, "POST", a+"/pin", map[string]bool{"pinned": true}).Code != 409 {
		t.Fatal("pinned deleted session")
	}
	ok(t, request(t, s, "DELETE", a, nil)) // Lost acknowledgement is retryable.
	nav := catalog(t, s)
	if len(nav.Projects) != 1 || len(nav.Projects[0].Sessions) != 1 || len(nav.Pinned) != 1 || nav.Pinned[0].Session != "019a0000-0000-7000-8000-000000000002" {
		t.Fatalf("stale navigation: %+v", nav)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = newTestServer(t, c)
	if request(t, s, "GET", a, nil).Code != 409 {
		t.Fatal("deleted history returned after restart")
	}
	ok(t, request(t, s, "GET", b, nil))
	if len(catalog(t, s).Pinned) != 1 {
		t.Fatal("pin deletion did not persist")
	}
}

func TestDeleteRejectsExternalOwnerAndUnsafeRequests(t *testing.T) {
	c, _ := testConfig(t)
	p, store := seedWorkspace(t, c, "project")
	seedInput(t, store, "owned", "do not delete while leased")
	s := newTestServer(t, c)
	catalog(t, s)
	path := "/api/projects/" + p.ID + "/sessions/owned"
	release, err := store.LockSession(session.ID("owned"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ok(t, request(t, s, "POST", path+"/pin", map[string]bool{"pinned": true}))
	if request(t, s, "DELETE", path, nil).Code != 409 {
		t.Fatal("deleted leased session")
	}
	if len(catalog(t, s).Pinned) != 1 {
		t.Fatal("failed deletion changed pin")
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		auth   bool
		origin string
		code   int
	}{{false, "http://unreal.test", 401}, {true, "https://evil.test", 403}} {
		r := httptest.NewRequest("DELETE", "http://unreal.test"+path, nil)
		r.Header.Set("Origin", test.origin)
		r.Header.Set("Content-Type", "application/json")
		if test.auth {
			r.AddCookie(&http.Cookie{Name: "unreal_session", Value: s.token})
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != test.code {
			t.Fatal("unsafe request", w.Code)
		}
	}
	ok(t, request(t, s, "GET", path, nil))

	// A separate pins-file failure cannot turn a committed deletion into an
	// apparent failure or leave an unavailable row in the sidebar after restart.
	if err = os.Remove(filepath.Join(c.StateDirectory, "pins.json")); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(c.StateDirectory, "pins.json"), 0700); err != nil {
		t.Fatal(err)
	}
	w := request(t, s, "DELETE", path, nil)
	ok(t, w)
	if !strings.Contains(w.Body.String(), "не ўдалося захаваць спіс замацаваных") {
		t.Fatal("missing partial-cleanup warning", w.Body.String())
	}
	if len(catalog(t, s).Pinned) != 0 {
		t.Fatal("stale pin shown")
	}
}
