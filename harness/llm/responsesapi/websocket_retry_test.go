package responsesapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestWebsocketTransientRejectionResynchronizesFullContext(t *testing.T) {
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
			var wire wsRequest
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Error(err)
				return
			}
			requests <- wire
			n := calls.Add(1)
			if n == 2 {
				_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"error","error":{"code":"server_error","message":"temporary failure; request ID fixture-123"}}`))
				continue
			}
			if err := wsComplete(r.Context(), c, fmt.Sprintf("response-%d", n), `[]`); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	a := wsAdapter(t, server, TransportWebSocket)
	defer a.Close()
	a.maxAttempts = 3
	req := baseWSRequest()
	first, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "session"})
	if err != nil {
		t.Fatal(err)
	}
	original := <-requests
	req = extendWS(req, first, "new user input")
	recovered, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "session"})
	if err != nil {
		t.Fatal(err)
	}
	delta, full := <-requests, <-requests
	if delta.Previous != first.ID || full.Previous != "" || len(full.Input) != len(req.Input) || recovered.Transport.Incremental || connections.Load() != 2 || calls.Load() != 3 {
		t.Fatalf("failed to resync: delta=%+v full=%+v transport=%+v", delta, full, recovered.Transport)
	}
	want := append(original.Input, delta.Input...)
	for i := range want {
		if string(want[i]) != string(full.Input[i]) {
			t.Fatal("retry changed canonical input")
		}
	}
}

func TestWebsocketRetryPolicyTimingAndLimits(t *testing.T) {
	for _, tt := range []struct {
		name             string
		err              error
		begun, dial      bool
		attempt, limit   int
		headers          http.Header
		want             bool
		minimum, maximum time.Duration
	}{
		{"server", &APIError{Code: "server_error"}, false, false, 1, 3, nil, true, 1600 * time.Millisecond, 2 * time.Second},
		{"exponential", &APIError{Code: "server_error"}, false, false, 2, 3, nil, true, 3200 * time.Millisecond, 4 * time.Second},
		{"rate hint", &APIError{Code: "rate_limit_exceeded", Message: "try again in 15ms"}, false, false, 1, 3, nil, true, 15 * time.Millisecond, 15 * time.Millisecond},
		{"handshake hint", &APIError{StatusCode: 503}, false, true, 1, 3, http.Header{"Retry-After": {"1"}}, true, time.Second, time.Second},
		{"overload", &APIError{Code: "server_is_overloaded"}, false, false, 1, 3, nil, true, 8 * time.Second, 10 * time.Second},
		{"exhausted", &APIError{Code: "server_error"}, false, false, 3, 3, nil, false, 0, 0},
		{"disabled", &APIError{Code: "server_error"}, false, false, 1, 1, nil, false, 0, 0},
		{"partial", &APIError{Code: "server_error"}, true, false, 1, 3, nil, false, 0, 0},
		{"quota", &APIError{StatusCode: 429, Code: "insufficient_quota"}, false, true, 1, 3, nil, false, 0, 0},
		{"auth", &APIError{StatusCode: 401, Code: "server_error"}, false, true, 1, 3, nil, false, 0, 0},
		{"unknown", &APIError{StatusCode: 200, Code: "unknown"}, false, false, 1, 3, nil, false, 0, 0},
		{"auth overrides resync", &APIError{StatusCode: 400, Code: "previous_response_not_found", Type: "authentication_error"}, false, false, 1, 3, nil, false, 0, 0},
		{"policy code", &APIError{StatusCode: 503, Code: "policy_violation"}, false, false, 1, 3, nil, false, 0, 0},
		{"ambiguous read", websocket.CloseError{Code: websocket.StatusInternalError}, false, false, 1, 3, nil, false, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := &adapter{maxAttempts: tt.limit}
				used := false
				start := time.Now()
				got, err := a.retryWebsocketFailure(t.Context(), &websocketFailure{error: tt.err, beforeGeneration: !tt.begun, beforeSend: tt.dial}, tt.headers, tt.attempt, &used)
				elapsed := time.Since(start)
				if err != nil || got != tt.want || elapsed < tt.minimum || elapsed > tt.maximum || used {
					t.Fatalf("retry=%v err=%v delay=%v resync=%v", got, err, elapsed, used)
				}
			})
		})
	}
}

func TestWebsocketMixedFailuresShareBudget(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(503)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		if n == 2 {
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"error","code":"rate_limit_exceeded","message":"try again in 1ms"}`))
			_, _, _ = c.Read(r.Context())
			return
		}
		_ = c.Close(websocket.StatusServiceRestart, "")
	}))
	defer server.Close()
	a := wsAdapter(t, server, TransportAuto)
	defer a.Close()
	a.maxAttempts = 3
	_, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{})
	if calls.Load() != 3 || websocket.CloseStatus(err) != websocket.StatusServiceRestart {
		t.Fatalf("mixed failures exceeded attempt limit: calls=%d error=%v", calls.Load(), err)
	}
}

func TestWebsocketBackoffCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		a := &adapter{maxAttempts: 3}
		done := make(chan error, 1)
		go func() {
			used := false
			_, err := a.retryWebsocketFailure(ctx, &websocketFailure{error: &APIError{Code: "server_error"}, beforeGeneration: true}, nil, 1, &used)
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestTransientFailuresAfterGenerationNeverRetry(t *testing.T) {
	for _, transport := range []Transport{TransportHTTP, TransportWebSocket} {
		for _, kind := range []string{"error", "response.failed"} {
			t.Run(string(transport)+"/"+kind, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					created := `{"type":"response.created","response":{"id":"partial"}}`
					failed := `{"type":"error","code":"server_error","message":"request ID fixture-123"}`
					if kind == "response.failed" {
						failed = `{"type":"response.failed","response":{"id":"partial","status":"failed","error":{"code":"server_error","message":"request ID fixture-123"},"output":[]}}`
					}
					if transport == TransportHTTP {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", created, failed)
						return
					}
					c, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer c.CloseNow()
					if _, _, err := c.Read(r.Context()); err != nil {
						return
					}
					_ = c.Write(r.Context(), websocket.MessageText, []byte(created))
					_ = c.Write(r.Context(), websocket.MessageText, []byte(failed))
					_, _, _ = c.Read(r.Context())
				}))
				defer server.Close()
				a := wsAdapter(t, server, transport)
				defer a.Close()
				a.maxAttempts = 3
				response, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{})
				if err == nil && response.Failure != nil {
					err = response.Failure
				}
				if !IsTransientError(err) || calls.Load() != 1 || len(response.Output) != 0 {
					t.Fatalf("unsafe replay/partial output: calls=%d response=%+v err=%v", calls.Load(), response, err)
				}
			})
		}
	}
}
