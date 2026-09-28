package webchat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/chat"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func waitPermission(t *testing.T, server *Server, base string) chat.WebOperation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		w := request(t, server, "GET", base, nil)
		ok(t, w)
		var page struct {
			Status chat.HeadlessStatus `json:"status"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, op := range page.Status.Operations {
			if op.ApprovalID != "" {
				if page.Status.State != "waiting" || op.State != "awaiting permission" {
					t.Fatalf("incorrect status: %+v", page.Status)
				}
				return op
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pending shell permission")
	return chat.WebOperation{}
}

func TestWebShellPermission(t *testing.T) {
	for _, action := range []string{"permit", "deny", "stop", "cancel", "release"} {
		t.Run(action, func(t *testing.T) {
			config, model := testConfig(t)
			server := newTestServer(t, config)
			workspace, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			// Local fake function, never the real ssh executable.
			command := "ssh() { printf invoked > proof; }; printf prefix > prefix; ssh prod"
			base := projectSession(t, server, workspace, "11111111-1111-4111-8111-111111111111")
			other := projectSession(t, server, t.TempDir(), "22222222-2222-4222-8222-222222222222")
			ok(t, request(t, server, "POST", base+"/send", map[string]string{"id": "first", "text": "do the task"}))
			call := nextCall(t, model)
			args, _ := json.Marshal(map[string]string{"command": command})
			call.reply <- llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "protected", Name: "Bash", Arguments: string(args)}}}}
			op := waitPermission(t, server, base)
			if op.Description != command || op.Directory != workspace {
				t.Fatalf("incomplete permission preview: %+v", op)
			}
			checkAbsent := func() {
				t.Helper()
				for _, name := range []string{"prefix", "proof"} {
					if _, err := os.Stat(filepath.Join(workspace, name)); !os.IsNotExist(err) {
						t.Fatalf("unapproved side effect: %s, %v", name, err)
					}
				}
			}
			checkAbsent()
			for _, attempt := range []struct{ base, id, token string }{{base, op.ID, "stale"}, {base, "other-op", op.ApprovalID}, {other, op.ID, op.ApprovalID}} {
				if w := request(t, server, "POST", attempt.base+"/permit", map[string]string{"id": attempt.id, "request_id": attempt.token}); w.Code == 200 {
					t.Fatal("stale/wrong approval accepted")
				}
			}
			ok(t, request(t, server, "POST", base+"/send", map[string]string{"id": "ordinary-yes", "text": "yes"}))
			checkAbsent()
			ok(t, request(t, server, "POST", base+"/"+action, map[string]string{"id": op.ID, "request_id": op.ApprovalID}))
			if w := request(t, server, "POST", base+"/permit", map[string]string{"id": op.ID, "request_id": op.ApprovalID}); w.Code == 200 {
				t.Fatal("permission reused")
			}
			if action == "permit" {
				deadline := time.Now().Add(5 * time.Second)
				for {
					data, err := os.ReadFile(filepath.Join(workspace, "proof"))
					if err == nil && string(data) == "invoked" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("approved command did not run", err)
					}
					time.Sleep(10 * time.Millisecond)
				}
			} else {
				checkAbsent()
			}
		})
	}
}
