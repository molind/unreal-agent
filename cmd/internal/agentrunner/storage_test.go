package agentrunner

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func TestRunnerSQLiteStorageAndCaptureReferences(t *testing.T) {
	workspace, stateHome := t.TempDir(), t.TempDir()
	t.Chdir(workspace)
	getenv := func(name string) string {
		return map[string]string{llmAPIKeyEnvironment: "secret", "XDG_STATE_HOME": stateHome, "SHELL": "/bin/sh"}[name]
	}
	calls := 0
	var reference string
	client := &fakeClient{respond: func(ctx context.Context, request llm.Request) (llm.Response, error) {
		calls++
		if calls == 1 {
			return llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "c", Name: "Bash", Arguments: `{"command":"printf 'a captured output'","max_output_length":2}`}}}}, nil
		}
		for _, item := range request.Input {
			if result, ok := item.Data.(llm.ToolResult); ok && result.CallID == "c" {
				text := result.Output[0].Value
				start := strings.Index(text, "capture:")
				if start >= 0 && len(text) >= start+72 {
					reference = text[start : start+72]
				}
			}
		}
		return llm.Response{}, nil
	}}
	var out, stderr bytes.Buffer
	code := RunMain(t.Context(), []string{"-storage-format", "sqlite", "-workspace", workspace, "-p", "hello"}, getenv, func() []string { return nil }, strings.NewReader(""), &out, &stderr, testConfig(client))
	if code != 0 {
		t.Fatal(code, stderr.String())
	}
	if reference == "" {
		t.Fatal("no capture reference")
	}
	dir, err := storage.Directory(workspace, getenv)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var captured bytes.Buffer
	if err = db.Copy(t.Context(), reference, &captured); err != nil || captured.String() != "a captured output" {
		t.Fatal(captured.String(), err)
	}
	loose, err := filepath.Glob(filepath.Join(dir, "operations", "*", "*", "out"))
	if err != nil || len(loose) != 0 {
		t.Fatal(loose, err)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	if err = db.Copy(t.Context(), reference, io.Discard); err != nil {
		t.Fatal(err)
	}
}
