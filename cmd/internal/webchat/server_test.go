package webchat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/cmd/internal/chat"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

type modelCall struct {
	ctx     context.Context
	request llm.Request
	reply   chan llm.Response
}
type fakeModel struct{ calls chan modelCall }

func (m *fakeModel) Respond(ctx context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
	c := modelCall{ctx: ctx, request: r, reply: make(chan llm.Response, 1)}
	select {
	case m.calls <- c:
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
	select {
	case reply := <-c.reply:
		return reply, nil
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
}
func (*fakeModel) Close() error { return nil }

func testConfig(t *testing.T) (Config, *fakeModel) {
	t.Helper()
	root := t.TempDir()
	m := &fakeModel{calls: make(chan modelCall, 32)}
	getenv := func(key string) string {
		if key == "HOME" {
			return root
		}
		if key == "OPENAI_API_KEY" {
			return "private-example-key"
		}
		return ""
	}
	c := Config{StateDirectory: filepath.Join(root, "web"), Origins: []string{"http://unreal.test"}, Getenv: getenv,
		Providers: []agentrunner.Provider{{Name: "openai-codex", NewClient: func(string, string, int, func(string) string) (agentrunner.Client, error) { return m, nil }}}}
	return c, m
}
func newTestServer(t *testing.T, c Config) *Server {
	t.Helper()
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func request(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, "http://unreal.test"+path, bytes.NewReader(data))
	r.Header.Set("Origin", "http://unreal.test")
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: "unreal_session", Value: s.token})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func ok(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body)
	}
}
func projectSession(t *testing.T, s *Server, workspace, id string) string {
	t.Helper()
	p, err := s.AddProject(workspace)
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/projects/" + p.ID + "/sessions"
	ok(t, request(t, s, "POST", base, map[string]string{"id": id}))
	return base + "/" + id
}
func nextCall(t *testing.T, m *fakeModel) modelCall {
	t.Helper()
	select {
	case c := <-m.calls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("model not called")
		return modelCall{}
	}
}
func answer(text string) llm.Response {
	return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}}}}
}
func waitHistory(t *testing.T, s *Server, path, contains string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w := request(t, s, "GET", path, nil)
		ok(t, w)
		if strings.Contains(w.Body.String(), contains) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("history never contained", contains)
}

func TestAuthenticationOriginAndMarkdown(t *testing.T) {
	c, _ := testConfig(t)
	s := newTestServer(t, c)
	for _, path := range []string{"/api/projects", "/api/events"} {
		r := httptest.NewRequest("GET", "http://unreal.test"+path, nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
	}
	for _, test := range []struct{ host, origin, content string }{{"evil.test", "http://evil.test", "application/json"}, {"unreal.test", "https://evil.test", "application/json"}, {"unreal.test", "", "application/json"}, {"unreal.test", "http://unreal.test", "text/plain"}} {
		r := httptest.NewRequest("POST", "http://"+test.host+"/api/projects", strings.NewReader(`{"path":"/"}`))
		r.Header.Set("Origin", test.origin)
		r.Header.Set("Content-Type", test.content)
		r.AddCookie(&http.Cookie{Name: "unreal_session", Value: s.token})
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 && w.Code != 415 {
			t.Fatalf("unsafe request accepted: %+v: %d", test, w.Code)
		}
	}
	login := request(t, s, "POST", "/api/login", map[string]string{"token": s.token})
	ok(t, login)
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe login cookie", cookies)
	}
	bad := request(t, s, "POST", "/api/login", map[string]string{"token": "wrong"})
	if bad.Code != 401 {
		t.Fatal(bad.Code)
	}
	static := request(t, s, "GET", "/", nil)
	ok(t, static)
	if !strings.Contains(static.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("CSP missing")
	}
	info, err := os.Stat(s.TokenPath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("token permissions", err)
	}
}

func TestDirectHTTPTokenSignIn(t *testing.T) {
	c, _ := testConfig(t)
	c.Origins = []string{"http://100.124.20.20:8097"}
	s := newTestServer(t, c)
	r := httptest.NewRequest("GET", c.Origins[0]+"/api/info", nil)
	r.Header.Set("Tailscale-User-Login", "owner@example.com")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("direct access accepted identity header without token: %d", w.Code)
	}
	r = httptest.NewRequest("POST", c.Origins[0]+"/api/login", strings.NewReader(`{"token":"`+s.token+`"}`))
	r.Header.Set("Origin", c.Origins[0])
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	ok(t, w)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("expected a cookie usable on direct HTTP: %+v", cookies)
	}
	r = httptest.NewRequest("GET", c.Origins[0]+"/api/info", nil)
	r.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	ok(t, w)
}

func TestParallelSessionsDurableRetryAndRestart(t *testing.T) {
	c, m := testConfig(t)
	s := newTestServer(t, c)
	a := projectSession(t, s, t.TempDir(), "019a0000-0000-7000-8000-000000000001")
	b := projectSession(t, s, t.TempDir(), "019a0000-0000-7000-8000-000000000002")
	// Merely creating/reading a conversation cannot recover old effects.
	ok(t, request(t, s, "GET", a, nil))
	select {
	case <-m.calls:
		t.Fatal("read started model")
	default:
	}
	body := map[string]string{"id": "phone-message", "text": "hello\nfrom the phone"}
	ok(t, request(t, s, "POST", a+"/send", body))
	callA := nextCall(t, m)
	ok(t, request(t, s, "POST", a+"/send", body))
	if request(t, s, "POST", a+"/send", map[string]string{"id": "phone-message", "text": "different"}).Code != 409 {
		t.Fatal("reused ID accepted new text")
	}
	ok(t, request(t, s, "POST", b+"/send", map[string]string{"id": "other", "text": "independent"}))
	callB := nextCall(t, m)
	if callA.request.Input[0].Type == "" {
		t.Fatal("missing model context")
	}
	ok(t, request(t, s, "POST", b+"/stop", map[string]string{}))
	select {
	case <-callB.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not join model")
	}
	select {
	case <-callA.ctx.Done():
		t.Fatal("stopped other session")
	default:
	}
	callA.reply <- answer("**Answer** with private-example-key and <script>alert(1)</script>")
	waitHistory(t, s, a, "Answer")
	w := request(t, s, "GET", a, nil)
	ok(t, w)
	if strings.Count(w.Body.String(), `"kind":"user"`) != 1 || strings.Contains(w.Body.String(), "private-example-key") {
		t.Fatal("duplicate or unredacted history", w.Body.String())
	}
	var page struct {
		After uint64         `json:"after"`
		Items []chat.WebItem `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if strings.Contains(item.HTML, "<script>") {
			t.Fatal("unsafe markdown", item.HTML)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := newTestServer(t, c)
	ok(t, request(t, s2, "GET", a, nil))
	select {
	case <-m.calls:
		t.Fatal("restart/read automatically resumed or duplicate request")
	default:
	}
	ok(t, request(t, s2, "POST", a+"/send", body))
	select {
	case <-m.calls:
		t.Fatal("durable retry started work after restart")
	default:
	}
	ok(t, request(t, s2, "POST", a+"/send", map[string]string{"id": "next", "text": "continue"}))
	next := nextCall(t, m)
	var users []string
	for _, item := range next.request.Input {
		if message, yes := item.Data.(llm.Message); yes && message.Role == llm.RoleUser {
			users = append(users, message.Text)
		}
	}
	if strings.Join(users, "|") != "hello\nfrom the phone|continue" {
		t.Fatal("incorrect restored context", users)
	}
	next.reply <- answer("continued")
	waitHistory(t, s2, a, "continued")
}

func TestSessionLeaseAndCanonicalProject(t *testing.T) {
	c, _ := testConfig(t)
	s := newTestServer(t, c)
	dir := t.TempDir()
	id := "019a0000-0000-7000-8000-000000000003"
	path := projectSession(t, s, dir, id)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	p, err := s.AddProject(alias)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != storage.Hash([]byte(canonical)) {
		t.Fatal("alias created another workspace")
	}
	directory, err := storage.Directory(dir, c.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := localfile.NewSQLite(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	release, err := store.LockSession(session.ID(id))
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, s, "POST", path+"/resume", map[string]string{})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "already in use") {
		t.Fatal("peer lease ignored", w.Body)
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	ok(t, request(t, s, "POST", path+"/stop", map[string]string{}))
	if unlock, err := store.LockSession(session.ID(id)); err == nil {
		unlock()
		t.Fatal("stop released selection")
	}
	ok(t, request(t, s, "POST", path+"/release", map[string]string{}))
	unlock, err := store.LockSession(session.ID(id))
	if err != nil {
		t.Fatal("release leaked lease", err)
	}
	unlock()
}

func TestToolRunsInSelectedWorkspaceAndReplays(t *testing.T) {
	c, m := testConfig(t)
	s := newTestServer(t, c)
	dir := t.TempDir()
	path := projectSession(t, s, dir, "019a0000-0000-7000-8000-000000000005")
	ok(t, request(t, s, "POST", path+"/send", map[string]string{"id": "tool-message", "text": "show the current directory"}))
	call := nextCall(t, m)
	call.reply <- llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "pwd-call", Name: "Bash", Arguments: `{"command":"pwd"}`}}}}
	next := nextCall(t, m)
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for _, item := range next.request.Input {
		if result, ok := item.Data.(llm.ToolResult); ok && result.CallID == "pwd-call" {
			for _, part := range result.Output {
				if part.Kind == llm.ToolResultText {
					output.WriteString(part.Value)
				}
			}
		}
	}
	if !strings.Contains(output.String(), canonical) {
		t.Fatal("tool did not use selected workspace", output.String())
	}
	next.reply <- answer("directory checked")
	waitHistory(t, s, path, "directory checked")
	w := request(t, s, "GET", path, nil)
	ok(t, w)
	if !strings.Contains(w.Body.String(), `"state":"completed"`) || !strings.Contains(w.Body.String(), `"description":"pwd"`) {
		t.Fatal("missing completed tool", w.Body)
	}
	ok(t, request(t, s, "POST", path+"/release", map[string]string{}))
	w = request(t, s, "GET", path, nil)
	ok(t, w)
	if !strings.Contains(w.Body.String(), `"state":"completed"`) {
		t.Fatal("tool outcome lost after release", w.Body)
	}
}

type slowWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
}

func (w *slowWriter) Write(p []byte) (int, error) {
	select {
	case <-w.entered:
	default:
		close(w.entered)
	}
	<-w.release
	return w.ResponseRecorder.Write(p)
}

func TestSlowHistoryReaderDoesNotLockWorkspace(t *testing.T) {
	c, _ := testConfig(t)
	s := newTestServer(t, c)
	dir := t.TempDir()
	path := projectSession(t, s, dir, "019a0000-0000-7000-8000-000000000006")
	w := &slowWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	defer func() { close(w.release); <-done }()
	r := httptest.NewRequest("GET", "http://unreal.test"+path, nil)
	r.AddCookie(&http.Cookie{Name: "unreal_session", Value: s.token})
	go func() { defer close(done); s.ServeHTTP(w, r) }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("history did not reach response writer")
	}
	created := make(chan *httptest.ResponseRecorder, 1)
	base := path[:strings.LastIndex(path, "/")]
	go func() {
		created <- request(t, s, "POST", base, map[string]string{"id": "019a0000-0000-7000-8000-000000000007"})
	}()
	select {
	case response := <-created:
		ok(t, response)
	case <-time.After(2 * time.Second):
		t.Fatal("slow browser blocked workspace")
	}
}

func TestSSEDisconnectDoesNotStopAgentAndShutdownClosesStream(t *testing.T) {
	c, m := testConfig(t)
	s := newTestServer(t, c)
	path := projectSession(t, s, t.TempDir(), "019a0000-0000-7000-8000-000000000004")
	// A real loopback stream exercises Flush and client disconnect behavior.
	httpServer := httptest.NewServer(s)
	defer httpServer.Close()
	r, err := http.NewRequest("GET", httpServer.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = "unreal.test"
	r.AddCookie(&http.Cookie{Name: "unreal_session", Value: s.token})
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "event: change\n" {
		t.Fatal(line, err)
	}
	ok(t, request(t, s, "POST", path+"/send", map[string]string{"id": "stream-message", "text": "work while I leave"}))
	call := nextCall(t, m)
	response.Body.Close()
	select {
	case <-call.ctx.Done():
		t.Fatal("browser disconnect canceled model")
	default:
	}
	call.reply <- answer("finished without a browser")
	waitHistory(t, s, path, "finished without")
	response, err = http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, response.Body); done <- err }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SSE survived shutdown")
	}
}
