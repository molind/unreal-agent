package responsesapi

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/primitives"
)

type wsRequest struct {
	Type     string           `json:"type"`
	Input    []jsontext.Value `json:"input"`
	Previous string           `json:"previous_response_id"`
	Model    string           `json:"model"`
	Store    bool             `json:"store"`
}

func wsAdapter(t *testing.T, server *httptest.Server, mode Transport) *adapter {
	t.Helper()
	remote := primitives.NewRemoteClient()
	t.Cleanup(func() { _ = remote.Close() })
	a, err := NewAdapter(remote, Config{Endpoint: server.URL + "/responses", Transport: mode, Headers: map[string][]string{"Authorization": {"Bearer synthetic"}}, CacheKeyPlacement: CacheKeyPlacement{Header: "session-id", UsePromptCacheKeyField: true}, MaxAttempts: new(1)})
	if err != nil {
		t.Fatal(err)
	}
	current := a.(*adapter)
	t.Cleanup(func() { _ = current.Close() })
	return current
}
func wsComplete(ctx context.Context, c *websocket.Conn, id string, output string) error {
	data := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":%s}}`, id, output))
	return c.Write(ctx, websocket.MessageText, data)
}
func baseWSRequest() llm.Request {
	return llm.Request{Model: llm.Model{ID: "test", ReasoningEffort: llm.ReasoningEffortHigh}, Input: []llm.Item{
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleSystem, Text: "workspace instructions"}},
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: strings.Repeat("old-context ", 1000)}},
	}}
}
func extendWS(r llm.Request, response llm.Response, text string) llm.Request {
	r.Input = append(append(append([]llm.Item(nil), r.Input...), response.Output...), llm.Item{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: text}})
	return r
}
func TestWebsocketSendsOnlyNewSuffixAndResetsChangedContext(t *testing.T) {
	requests := make(chan wsRequest, 16)
	var connections atomic.Int32
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic" || r.Header.Get("OpenAI-Beta") != "responses_websockets=2026-02-06" || r.Header.Get("session-id") == "" {
			t.Error("missing websocket protocol headers")
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		connections.Add(1)
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var req wsRequest
			if err := json.Unmarshal(data, &req); err != nil {
				t.Error(err)
				return
			}
			requests <- req
			n := calls.Add(1)
			// Match Codex: output items arrive separately; final response omits output.
			item := fmt.Sprintf(`{"id":"message-%d","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done"}]}`, n)
			if err := c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`)); err != nil {
				return
			}
			if err := wsComplete(r.Context(), c, fmt.Sprintf("response-%d", n), `[]`); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	a := wsAdapter(t, server, TransportWebSocket)
	defer a.Close()
	req := baseWSRequest()
	first, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	one := <-requests
	if len(one.Input) != 2 || one.Previous != "" || one.Type != "response.create" || one.Store {
		t.Fatalf("first wire %+v", one)
	}
	req = extendWS(req, first, "next")
	second, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	two := <-requests
	if two.Previous != first.ID || len(two.Input) != 1 || !second.Transport.Incremental || second.Transport.RequestBytes >= first.Transport.RequestBytes || connections.Load() != 1 {
		t.Fatalf("not incremental: %+v, %+v, connections %d", two, second.Transport, connections.Load())
	}
	req = extendWS(req, second, "after compaction")
	req.Input = req.Input[len(req.Input)-1:]
	third, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	if wire := <-requests; wire.Previous != "" || third.Transport.Incremental || connections.Load() != 2 {
		t.Fatal("changed prefix reused stale continuation")
	}
	req = extendWS(req, third, "other session")
	fourth, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "session-2"})
	if err != nil {
		t.Fatal(err)
	}
	if wire := <-requests; wire.Previous != "" || fourth.Transport.Incremental || connections.Load() != 3 {
		t.Fatal("cross-session continuation")
	}
	req = extendWS(req, fourth, "new model")
	req.Model.ID = "other-model"
	if _, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "session-2"}); err != nil {
		t.Fatal(err)
	}
	if wire := <-requests; wire.Previous != "" || connections.Load() != 4 {
		t.Fatal("model change reused continuation")
	}
}
func TestWebsocketMissingReferenceResynchronizesOnce(t *testing.T) {
	var calls, connections atomic.Int32
	requests := make(chan wsRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		connections.Add(1)
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var req wsRequest
			_ = json.Unmarshal(data, &req)
			requests <- req
			if calls.Add(1) == 2 {
				_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"error","error":{"code":"previous_response_not_found","message":"expired"}}`))
				continue
			}
			if err := wsComplete(r.Context(), c, "ok", `[]`); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	a := wsAdapter(t, server, TransportWebSocket)
	defer a.Close()
	req := baseWSRequest()
	r, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "one"})
	if err != nil {
		t.Fatal(err)
	}
	<-requests
	req = extendWS(req, r, "new")
	r, err = a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "one"})
	if err != nil {
		t.Fatal(err)
	}
	stale, full := <-requests, <-requests
	if stale.Previous == "" || full.Previous != "" || len(full.Input) != len(req.Input) || r.Transport.Incremental || connections.Load() != 2 {
		t.Fatal("missing reference did not retry full on a new connection")
	}
}
func TestWebsocketCancellationAndCloseResetAndJoin(t *testing.T) {
	started := make(chan wsRequest, 8)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var req wsRequest
		_ = json.Unmarshal(data, &req)
		started <- req
		if calls.Add(1) == 1 {
			_, _, _ = c.Read(r.Context())
			return
		}
		_ = wsComplete(r.Context(), c, "done", `[]`)
		_, _, _ = c.Read(r.Context())
	}))
	defer server.Close()
	a := wsAdapter(t, server, TransportWebSocket)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := a.Respond(ctx, baseWSRequest(), llm.RequestOptions{CacheKey: "session"})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled websocket hung")
	}
	if _, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{CacheKey: "session"}); err != nil {
		t.Fatal(err)
	}
	if wire := <-started; wire.Previous != "" {
		t.Fatal("canceled generation retained continuation")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed client accepted work: %v", err)
	}
}
func TestWebsocketAutoFallbackOnlyForUnsupportedUpgrade(t *testing.T) {
	for _, status := range []int{405, 401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var gets, posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					gets.Add(1)
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"code":"upgrade_failed","message":"not available"}}`)
					return
				}
				posts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"ok","status":"completed","output":[]}}`+"\n\n")
			}))
			defer server.Close()
			a := wsAdapter(t, server, TransportAuto)
			defer a.Close()
			r, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{CacheKey: "session"})
			if status == 405 {
				if err != nil || r.Transport.Mode != "http" || r.Transport.Fallback == "" {
					t.Fatal("missing observable fallback", err)
				}
				if _, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{CacheKey: "session"}); err != nil {
					t.Fatal(err)
				}
				if gets.Load() != 1 || posts.Load() != 2 {
					t.Fatal("fallback repeatedly probed websocket")
				}
			} else {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != status || posts.Load() != 0 {
					t.Fatalf("authentication/quota silently fell back: %v", err)
				}
			}
		})
	}
}
func TestWebsocketContextErrorDoesNotRetryOrFallback(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		calls.Add(1)
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"error","error":{"code":"context_length_exceeded","message":"too big"}}`))
		_, _, _ = c.Read(r.Context())
	}))
	defer server.Close()
	a := wsAdapter(t, server, TransportAuto)
	defer a.Close()
	_, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{CacheKey: "s"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.ContextLimitExceeded() || calls.Load() != 1 {
		t.Fatalf("overflow swallowed: %v", err)
	}
}

func TestWebsocketKeepsReadingPingsWhileToolsRun(t *testing.T) {
	ping := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		if err := wsComplete(r.Context(), c, "first", `[]`); err != nil {
			return
		}
		// Ping needs a concurrent server reader to receive the client's pong.
		go func() {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			ping <- c.Ping(ctx)
		}()
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		_ = wsComplete(r.Context(), c, "second", `[]`)
		_, _, _ = c.Read(r.Context())
	}))
	defer server.Close()
	a := wsAdapter(t, server, TransportWebSocket)
	defer a.Close()
	req := baseWSRequest()
	first, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ping:
		if err != nil {
			t.Fatal("idle reader did not answer ping", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle ping hung")
	}
	req = extendWS(req, first, "tool result")
	next, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "s"})
	if err != nil || !next.Transport.Incremental {
		t.Fatal("lost idle continuation", err)
	}
}

func TestWebsocketRecoveryDoesNotReplayPartialGeneration(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		calls.Add(1)
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"active"}}`))
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"error","code":"previous_response_not_found","message":"not safe to resubmit partial work"}`))
		_, _, _ = c.Read(r.Context())
	}))
	defer server.Close()
	a := wsAdapter(t, server, TransportAuto)
	defer a.Close()
	_, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{CacheKey: "s"})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("partial generation replayed: calls %d error %v", calls.Load(), err)
	}
}

func TestWebsocketNeverForwardsCredentialsThroughRedirect(t *testing.T) {
	var hit atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer server.Close()
	a := wsAdapter(t, server, TransportAuto)
	defer a.Close()
	_, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 307 || hit.Load() != 0 {
		t.Fatalf("redirect followed or hidden: %v", err)
	}
}

func TestWebsocketCloseInterruptsActiveAndQueuedRequests(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		started <- struct{}{}
		_, _, _ = c.Read(r.Context())
	}))
	defer server.Close()
	a := wsAdapter(t, server, TransportWebSocket)
	results := make(chan error, 2)
	go func() {
		_, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{CacheKey: "s"})
		results <- err
	}()
	<-started
	go func() {
		_, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{CacheKey: "s"})
		results <- err
	}()
	closed := make(chan struct{})
	go func() { _ = a.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not join the active connection")
	}
	for range 2 {
		select {
		case err := <-results:
			if err == nil {
				t.Fatal("closed request succeeded")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Close left a blocked request")
		}
	}
}
