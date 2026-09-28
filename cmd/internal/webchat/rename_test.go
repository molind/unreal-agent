package webchat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

func TestRenamePersistsWithoutChangingHistoryOrOrder(t *testing.T) {
	c, model := testConfig(t)
	a, storeA := seedWorkspace(t, c, "alpha")
	b, storeB := seedWorkspace(t, c, "beta")
	seedInput(t, storeA, "same-id", "Original question")
	seedInput(t, storeB, "same-id", "Another project")
	s := newTestServer(t, c)
	path := "/api/projects/" + a.ID + "/sessions/same-id"
	before := catalog(t, s)
	history := request(t, s, "GET", path, nil).Body.String()
	ok(t, request(t, s, "POST", path+"/pin", map[string]bool{"pinned": true}))
	s.mu.Lock()
	changed := s.changed
	s.mu.Unlock()
	for range 2 {
		ok(t, request(t, s, "POST", path+"/rename", map[string]string{"title": "  Назва\n размовы 🚀  "}))
	}
	select {
	case <-changed:
	default:
		t.Fatal("rename did not notify other browsers")
	}
	after := catalog(t, s)
	if len(after.Pinned) != 1 || after.Pinned[0].Project != a.ID {
		t.Fatal("rename lost pin")
	}
	if after.Projects[0].ID != before.Projects[0].ID || after.Projects[1].ID != before.Projects[1].ID {
		t.Fatal("rename reordered projects")
	}
	for i, project := range after.Projects {
		if !project.Updated.Equal(before.Projects[i].Updated) {
			t.Fatal("rename changed activity time")
		}
		want := "Назва размовы 🚀"
		if project.ID == b.ID {
			want = "Another project"
		}
		if project.Sessions[0].Title != want {
			t.Fatalf("wrong title: %+v", project)
		}
	}
	if got := request(t, s, "GET", path, nil).Body.String(); got != history {
		t.Fatal("rename changed passive history/status")
	}
	list := request(t, s, "GET", "/api/projects/"+a.ID+"/sessions", nil)
	ok(t, list)
	var sessions []sessionInfo
	if err := json.Unmarshal(list.Body.Bytes(), &sessions); err != nil || sessions[0].Title != "Назва размовы 🚀" {
		t.Fatal("session list did not reflect override", list.Body, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = newTestServer(t, c)
	if !reflect.DeepEqual(after, catalog(t, s)) {
		t.Fatal("title or ordering lost after restart")
	}
	ok(t, request(t, s, "POST", path+"/rename", map[string]string{"title": ""}))
	reset := catalog(t, s)
	for _, p := range reset.Projects {
		if p.ID == a.ID && p.Sessions[0].Title != "Original question" {
			t.Fatal("auto title did not return")
		}
	}
	select {
	case <-model.calls:
		t.Fatal("rename started model work")
	default:
	}
}

func TestRenameWhileModelWorks(t *testing.T) {
	c, model := testConfig(t)
	s := newTestServer(t, c)
	base := projectSession(t, s, t.TempDir(), "11111111-1111-4111-8111-111111111111")
	ok(t, request(t, s, "POST", base+"/send", map[string]string{"id": "first", "text": "Original question"}))
	call := nextCall(t, model)
	ok(t, request(t, s, "POST", base+"/rename", map[string]string{"title": "My precise topic"}))
	select {
	case <-call.ctx.Done():
		t.Fatal("rename stopped model")
	default:
	}
	call.reply <- answer("Answer after rename")
	waitHistory(t, s, base, `"state":"idle"`)
	if catalog(t, s).Projects[0].Sessions[0].Title != "My precise topic" {
		t.Fatal("reply overwrote manual name")
	}
	ok(t, request(t, s, "POST", base+"/send", map[string]string{"id": "second", "text": "Continue"}))
	call = nextCall(t, model)
	encoded, err := json.Marshal(call.request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "My precise topic") {
		t.Fatal("UI title leaked into model context")
	}
	call.reply <- answer("Done")
	waitHistory(t, s, base, `"state":"idle"`)
}

func TestRenameValidationDeletionAndSecurity(t *testing.T) {
	c, _ := testConfig(t)
	s := newTestServer(t, c)
	base := projectSession(t, s, t.TempDir(), "11111111-1111-4111-8111-111111111111")
	ok(t, request(t, s, "POST", base+"/rename", map[string]string{"title": "Empty but named"}))
	if catalog(t, s).Projects[0].Sessions[0].Title != "Empty but named" {
		t.Fatal("empty conversation not named")
	}
	for _, body := range []any{map[string]any{}, map[string]any{"title": nil}, map[string]any{"title": 3}, map[string]any{"title": "ok", "extra": true}, map[string]string{"title": strings.Repeat("а", 201)}, map[string]string{"title": "bad\x00title"}} {
		if w := request(t, s, "POST", base+"/rename", body); w.Code != http.StatusBadRequest {
			t.Fatalf("bad title accepted: %d %s", w.Code, w.Body)
		}
	}
	for _, tc := range []struct {
		auth            bool
		origin, content string
		code            int
	}{
		{false, "http://unreal.test", "application/json", 401},
		{true, "https://evil.test", "application/json", 403},
		{true, "http://unreal.test", "text/plain", 415},
	} {
		r := httptest.NewRequest("POST", "http://unreal.test"+base+"/rename", strings.NewReader(`{"title":"forbidden"}`))
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Content-Type", tc.content)
		if tc.auth {
			r.AddCookie(&http.Cookie{Name: "unreal_session", Value: s.token})
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("unsafe rename accepted: %d", w.Code)
		}
	}
	if catalog(t, s).Projects[0].Sessions[0].Title != "Empty but named" {
		t.Fatal("invalid request changed title")
	}
	ok(t, request(t, s, "POST", base+"/rename", map[string]string{"title": "private-example-key"}))
	if strings.Contains(request(t, s, "GET", "/api/navigation", nil).Body.String(), "private-example-key") {
		t.Fatal("title not redacted")
	}
	ok(t, request(t, s, "DELETE", base, nil))
	if w := request(t, s, "POST", base+"/rename", map[string]string{"title": "late"}); w.Code != 404 {
		t.Fatal("renamed deleted conversation", w.Code)
	}
	p := s.projects[strings.Split(base, "/")[3]]
	titles, err := p.store.SessionTitles(t.Context())
	if err != nil || titles[session.ID("11111111-1111-4111-8111-111111111111")] != "" {
		t.Fatal("deleted title retained", err)
	}
}
