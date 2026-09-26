package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"golang.org/x/sys/unix"
)

func TestTTYStructuredEditOverIncrementalWebsocket(t *testing.T) {
	for _, color := range []bool{false, true} {
		t.Run(fmt.Sprintf("color=%t", color), func(t *testing.T) {
			testTTYStructuredEdit(t, color)
		})
	}
}

func testTTYStructuredEdit(t *testing.T, color bool) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var requests, connections atomic.Int32
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
			var body struct {
				Previous string           `json:"previous_response_id"`
				Input    []jsontext.Value `json:"input"`
				Tools    []struct {
					Name string `json:"name"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(data, &body); err != nil {
				t.Error(err)
				return
			}
			n := requests.Add(1)
			var output []any
			switch n {
			case 1:
				if body.Previous != "" || len(body.Input) != 2 {
					t.Error("initial request not full")
				}
				names := map[string]bool{}
				for _, v := range body.Tools {
					names[v.Name] = true
				}
				for _, name := range []string{"Read", "Edit", "Write"} {
					if !names[name] {
						t.Error("missing tool", name)
					}
				}
				args, _ := json.Marshal(map[string]any{"path": "source.txt"})
				output = []any{map[string]any{"id": "item-read", "type": "function_call", "call_id": "read", "name": "Read", "arguments": string(args), "status": "completed"}}
			case 2:
				if body.Previous != "response-1" || len(body.Input) != 1 {
					t.Error("Read result resent old history")
				}
				var result struct {
					Output []struct {
						Text string `json:"text"`
					} `json:"output"`
				}
				if err := json.Unmarshal(body.Input[0], &result); err != nil || len(result.Output) != 1 {
					t.Error("bad Read result", err)
					return
				}
				var file operation.FileResult
				if err := json.Unmarshal([]byte(result.Output[0].Text), &file); err != nil || len(file.Revision) != 16 {
					t.Error("missing short revision", err)
					return
				}
				args, _ := json.Marshal(map[string]any{"path": "source.txt", "revision": file.Revision, "old_text": "hello", "new_text": "world"})
				output = []any{map[string]any{"id": "item-edit", "type": "function_call", "call_id": "edit", "name": "Edit", "arguments": string(args), "status": "completed"}}
			case 3:
				if body.Previous != "response-2" || len(body.Input) != 1 {
					t.Error("Edit result resent old history")
				}
				if !strings.Contains(string(body.Input[0]), `\"applied\":true`) {
					t.Error("Edit was not applied")
				}
				output = []any{messageOutput("STRUCTURED EDIT DONE")}
			default:
				t.Errorf("unexpected model request %d", n)
				return
			}
			response, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("response-%d", n), "status": "completed", "output": output}})
			if err := c.Write(r.Context(), websocket.MessageText, response); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	env := []string{"TERM=xterm"}
	if !color {
		env = append(env, "NO_COLOR=1")
	}
	cli := startTTY(t, env, "-provider", "openai", "-base-url", server.URL, "-transport", "websocket", workspace)
	cli.wait("you> ")
	cli.send("change hello to world\r")
	cli.wait("STRUCTURED EDIT DONE")
	if color {
		cli.wait(" \x1b[32m✓\x1b[0m Edit: source.txt")
		// Pale row backgrounds, stronger changed spans, and a pale common 'o'.
		cli.wait("\x1b[38;5;16;48;5;224m-\x1b[38;5;16;48;5;210mhell\x1b[38;5;16;48;5;224mo\x1b[0m\r\n" +
			"\x1b[38;5;16;48;5;194m+\x1b[38;5;16;48;5;120mw\x1b[38;5;16;48;5;194mo\x1b[38;5;16;48;5;120mrld\x1b[0m")
	} else {
		cli.wait(" ✓ Edit: source.txt")
		cli.wait("-hello\r\n+world")
		if strings.Contains(cli.snapshot(), "\x1b[38;5;16;48;5;") {
			t.Fatal("diff background ignored NO_COLOR")
		}
	}
	if err := cli.resize(30, 120); err != nil {
		t.Fatal(err)
	}
	if err := cli.cmd.Process.Signal(unix.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	cli.send("/status\r")
	cli.wait("Last response transport: websocket / incremental")
	cli.closeViewer()
	cli.send("/exit\r")
	cli.finish(0)
	content, _ := os.ReadFile(filepath.Join(workspace, "source.txt"))
	if string(content) != "world\n" || requests.Load() != 3 || connections.Load() != 1 {
		t.Fatalf("content=%q requests=%d connections=%d", content, requests.Load(), connections.Load())
	}
}
