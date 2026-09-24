package chat

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func fileCall(id, name string, args any) llm.Response {
	data, _ := json.Marshal(args)
	return llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: id, Name: name, Arguments: string(data)}}}}
}
func fileResult(t *testing.T, request llm.Request, id string) operation.FileResult {
	t.Helper()
	for _, item := range request.Input {
		if result, ok := item.Data.(llm.ToolResult); ok && result.CallID == id {
			var value operation.FileResult
			if err := json.Unmarshal([]byte(result.Output[0].Value), &value); err != nil {
				t.Fatal(err)
			}
			return value
		}
	}
	t.Fatal("missing file result", id)
	return operation.FileResult{}
}
func TestChatStructuredFilesAndDiff(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("hello\n"), 0640); err != nil {
		t.Fatal(err)
	}
	c := launch(t, workspace, false)
	c.send("edit the source and create a new file")
	call := c.call()
	var tools []string
	for _, tool := range call.request.Tools {
		tools = append(tools, tool.Name)
	}
	for _, name := range []string{"Read", "Edit", "Write"} {
		if !slices.Contains(tools, name) {
			t.Fatal("tool not exposed", name)
		}
	}
	call.reply <- fileCall("read", "Read", map[string]any{"path": "source.txt"})
	call = c.call()
	read := fileResult(t, call.request, "read")
	if read.Text != "1: hello\n" || len(read.Revision) != 16 {
		t.Fatalf("read %+v", read)
	}
	call.reply <- fileCall("edit", "Edit", map[string]any{"path": "source.txt", "revision": read.Revision, "old_text": "hello", "new_text": "world"})
	call = c.call()
	edited := fileResult(t, call.request, "edit")
	if !edited.Applied || edited.Error != "" || len(edited.Revision) != 16 {
		t.Fatalf("edit %+v", edited)
	}
	c.wait("-hello\n+world")
	call.reply <- fileCall("write", "Write", map[string]any{"path": "new.txt", "revision": "missing", "content": "created\n"})
	call = c.call()
	created := fileResult(t, call.request, "write")
	if !created.Created || !created.Applied {
		t.Fatalf("create %+v", created)
	}
	call.reply <- reply("files done")
	c.wait("files done")
	c.finish("/exit")
	for name, want := range map[string]string{"source.txt": "world\n", "new.txt": "created\n"} {
		data, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil || string(data) != want {
			t.Fatal(name, string(data), err)
		}
	}
	// Lifecycle logs do not copy file contents or replacement text.
	paths, _ := filepath.Glob(filepath.Join(workspace, ".harness/sessions/logs/commands/*/*.jsonl"))
	if len(paths) != 3 {
		t.Fatalf("file lifecycle logs=%d", len(paths))
	}
	for _, path := range paths {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), `"OldText"`) || strings.Contains(string(data), `"Content"`) {
			t.Fatal("source text leaked into command log")
		}
	}
}
