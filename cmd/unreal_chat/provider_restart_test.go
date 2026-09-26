package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestTTYProviderRestartKeepsDraftAndHistory(t *testing.T) {
	var connections atomic.Int32
	started, drop := make(chan struct{}), make(chan struct{})
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		connection := connections.Add(1)
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		requests <- string(data)
		if connection == 1 {
			if err := c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","response":{"id":"interrupted"}}`)); err != nil {
				return
			}
			// A partial function call must not be executed when the socket closes.
			partial := map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{
				"type": "function_call", "call_id": "partial", "name": "Bash", "arguments": `{"command":"touch should-not-exist"}`,
			}}
			frame, _ := json.Marshal(partial)
			if err := c.Write(r.Context(), websocket.MessageText, frame); err != nil {
				return
			}
			close(started)
			select {
			case <-drop:
			case <-r.Context().Done():
				return
			}
			_ = c.Close(websocket.StatusServiceRestart, "")
			return
		}
		frame, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "continued", "status": "completed", "output": []any{messageOutput("CONTINUED AFTER RESTART")},
		}})
		_ = c.Write(r.Context(), websocket.MessageText, frame)
		_, _, _ = c.Read(r.Context())
	}))
	defer server.Close()
	workspace := t.TempDir()
	cli := startTTY(t, []string{"TERM=xterm"}, "-provider", "openai", "-base-url", server.URL, "-transport", "websocket", workspace)
	cli.wait("you> ")
	for _, noise := range []string{"Command logs:", "Diagnostic log:", "Context: recover"} {
		if strings.Contains(cli.snapshot(), noise) {
			t.Fatalf("startup contains %q", noise)
		}
	}
	s := &pickerScreen{t, cli, newLiveScreen(40, 24)}
	s.resizeTo(100, 32)
	cli.send("original request\r")
	select {
	case <-started:
	case <-time.After(8 * time.Second):
		t.Fatal("no model request")
	}
	<-requests
	cli.send("continue") // Unsubmitted draft must survive the provider failure.
	s.wait("you> continue")
	close(drop)
	cli.wait("Provider restarted; work stopped.")
	s.wait("you> continue")
	select {
	case <-requests:
		t.Fatal("partial generation retried automatically")
	case <-time.After(100 * time.Millisecond):
	}
	cli.send("\r")
	select {
	case request := <-requests:
		if !strings.Contains(request, "original request") || !strings.Contains(request, "continue") || strings.Contains(request, "previous_response_id") || strings.Contains(request, "should-not-exist") {
			t.Fatal("reconnect lost history, kept partial output, or reused stale continuation")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("explicit continuation did not reconnect")
	}
	cli.wait("CONTINUED AFTER RESTART")
	cli.send("/status\r")
	cli.wait("Diagnostic log:")
	cli.closeViewer()
	cli.send("/exit\r")
	cli.finish(0)
	if connections.Load() != 2 {
		t.Fatalf("unexpected retries: %d connections", connections.Load())
	}
	if _, err := os.Stat(filepath.Join(workspace, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("partial tool call executed: %v", err)
	}
}
