package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestTTYTransientAPIFailureKeepsChatUsable(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			var calls atomic.Int32
			requests := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var c *websocket.Conn
				var data []byte
				var err error
				if transport == "websocket" {
					c, err = websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer c.CloseNow()
					_, data, err = c.Read(r.Context())
				} else {
					data, err = io.ReadAll(r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
				}
				if err != nil {
					return
				}
				requests <- string(data)
				var frames []string
				if calls.Add(1) == 1 {
					frames = []string{`{"type":"response.created","response":{"id":"partial"}}`, `{"type":"error","error":{"code":"server_error","message":"request ID fixture-123"}}`}
				} else {
					frame, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "continued", "status": "completed", "output": []any{messageOutput("CONTINUED AFTER SERVER ERROR")}}})
					frames = []string{string(frame)}
				}
				for _, frame := range frames {
					if c != nil {
						if err := c.Write(r.Context(), websocket.MessageText, []byte(frame)); err != nil {
							return
						}
					} else {
						_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
					}
				}
				if c != nil {
					_, _, _ = c.Read(r.Context())
				}
			}))
			defer server.Close()
			cli := startTTY(t, []string{"TERM=xterm"}, "-provider", "openai", "-base-url", server.URL, "-transport", transport, t.TempDir())
			cli.wait("you> ")
			cli.send("original request\r")
			cli.wait("Provider temporarily unavailable; work stopped.")
			<-requests
			select {
			case <-requests:
				t.Fatal("partial request was resubmitted")
			case <-time.After(100 * time.Millisecond):
			}
			cli.send("continue\r")
			select {
			case request := <-requests:
				if !strings.Contains(request, "original request") || !strings.Contains(request, "continue") || strings.Contains(request, "previous_response_id") {
					t.Fatal("history lost or stale continuation retained")
				}
			case <-time.After(8 * time.Second):
				t.Fatal("chat did not accept continuation")
			}
			cli.wait("CONTINUED AFTER SERVER ERROR")
			cli.send("/exit\r")
			cli.finish(0)
			if calls.Load() != 2 {
				t.Fatalf("unexpected request count: %d", calls.Load())
			}
		})
	}
}
