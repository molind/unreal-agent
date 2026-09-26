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
	"time"

	"github.com/coder/websocket"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestWebsocketServiceRestartResynchronizesOnce(t *testing.T) {
	var connections atomic.Int32
	requests := make(chan wsRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		connection := connections.Add(1)
		for call := 0; ; call++ {
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
			if connection == 1 && call == 1 {
				_ = c.Close(websocket.StatusServiceRestart, "")
				return
			}
			if err := wsComplete(r.Context(), c, fmt.Sprintf("response-%d-%d", connection, call), `[]`); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	a := wsAdapter(t, server, TransportAuto)
	defer a.Close()
	req := baseWSRequest()
	first, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "same-session"})
	if err != nil {
		t.Fatal(err)
	}
	original := <-requests
	req = extendWS(req, first, "tool result already saved")
	recovered, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "same-session"})
	if err != nil {
		t.Fatal(err)
	}
	stale, full := <-requests, <-requests
	if stale.Previous != first.ID || len(stale.Input) != 1 || full.Previous != "" || len(full.Input) != len(req.Input) || recovered.Transport.Incremental || recovered.Transport.Reused || connections.Load() != 2 {
		t.Fatalf("restart did not fully resync: stale=%+v full=%+v transport=%+v connections=%d", stale, full, recovered.Transport, connections.Load())
	}
	expected := append(original.Input, stale.Input...)
	for i := range full.Input {
		if string(full.Input[i]) != string(expected[i]) {
			t.Fatal("canonical input changed on recovery")
		}
	}
	req = extendWS(req, recovered, "continue normally")
	next, err := a.Respond(t.Context(), req, llm.RequestOptions{CacheKey: "same-session"})
	if err != nil {
		t.Fatal(err)
	}
	wire := <-requests
	if wire.Previous != recovered.ID || len(wire.Input) != 1 || !next.Transport.Incremental || connections.Load() != 2 {
		t.Fatal("recovered connection did not resume incremental requests")
	}
}

func TestWebsocketRestartRecoveryBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name      string
		code      websocket.StatusCode
		frames    []string
		wantCalls int32
	}{
		{"restart before generation", websocket.StatusServiceRestart, nil, 2},
		{"restart after creation", websocket.StatusServiceRestart, []string{`{"type":"response.created","response":{"id":"active"}}`}, 1},
		{"restart after partial tool", websocket.StatusServiceRestart, []string{`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"partial","name":"Bash","arguments":"{}"}}`}, 1},
		{"normal close", websocket.StatusNormalClosure, nil, 1},
		{"going away is ambiguous", websocket.StatusGoingAway, nil, 1},
		{"policy close", websocket.StatusPolicyViolation, nil, 1},
		{"internal error is ambiguous", websocket.StatusInternalError, nil, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls, posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.WriteHeader(http.StatusServiceUnavailable)
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
				calls.Add(1)
				for _, frame := range tt.frames {
					if err := c.Write(r.Context(), websocket.MessageText, []byte(frame)); err != nil {
						return
					}
				}
				_ = c.Close(tt.code, "")
			}))
			defer server.Close()
			a := wsAdapter(t, server, TransportAuto)
			defer a.Close()
			a.maxAttempts = 5 // Transport recovery remains bounded independently.
			response, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{CacheKey: "s"})
			if websocket.CloseStatus(err) != tt.code || calls.Load() != tt.wantCalls || posts.Load() != 0 || len(response.Output) != 0 {
				t.Fatalf("unsafe recovery: calls=%d posts=%d output=%v error=%v", calls.Load(), posts.Load(), response.Output, err)
			}
			if a.websocket.conn != nil || a.websocket.previousID != "" {
				t.Fatal("failed restart retained stale continuation")
			}
		})
	}
}

func TestWebsocketRestartCancellationAndClassification(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitWebsocketRestart(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during restart backoff: %v", err)
	}
	restart := fmt.Errorf("failed to get reader: %w", websocket.CloseError{Code: websocket.StatusServiceRestart})
	if !recoverableWebsocketFailure(&websocketFailure{error: restart, beforeGeneration: true}) || recoverableWebsocketFailure(&websocketFailure{error: restart}) || recoverableWebsocketFailure(restart) {
		t.Fatal("lost before-generation recovery boundary")
	}
	if recoverableWebsocketFailure(&websocketFailure{error: errors.New("StatusServiceRestart"), beforeGeneration: true}) {
		t.Fatal("error text must not grant retry permission")
	}
}

func TestWebsocketCloseDuringRestartBackoff(t *testing.T) {
	closed := make(chan struct{})
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
		if calls.Add(1) == 1 {
			_ = c.Close(websocket.StatusServiceRestart, "")
			close(closed)
		}
	}))
	defer server.Close()
	a := wsAdapter(t, server, TransportWebSocket)
	result := make(chan error, 1)
	go func() {
		_, err := a.Respond(t.Context(), baseWSRequest(), llm.RequestOptions{})
		result <- err
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not close")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("close retried work: calls=%d err=%v", calls.Load(), err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close did not join retrying request")
	}
}
