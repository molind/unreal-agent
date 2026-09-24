package agentrunner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func TestRunnerSQLiteAutomaticallyMigratesWorkspaceSessions(t *testing.T) {
	workspace, stateHome := t.TempDir(), t.TempDir()
	t.Chdir(workspace)
	source := filepath.Join(workspace, ".harness", "sessions")
	legacy, err := localfile.New(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Create(t.Context(), "old"); err != nil {
		t.Fatal(err)
	}
	getenv := func(name string) string {
		return map[string]string{llmAPIKeyEnvironment: "secret", "XDG_STATE_HOME": stateHome}[name]
	}
	client := &fakeClient{respond: func(context.Context, llm.Request) (llm.Response, error) { return llm.Response{}, nil }}
	var out, stderr bytes.Buffer
	code := RunMain(t.Context(), []string{"-storage-format", "sqlite", "-workspace", workspace, "-p", "hello"}, getenv, func() []string { return nil }, strings.NewReader(""), &out, &stderr, testConfig(client))
	if code != 0 {
		t.Fatal(code, stderr.String())
	}
	if _, err = os.Stat(filepath.Dir(source)); !os.IsNotExist(err) {
		t.Fatal("old .harness remains", err)
	}
	directory, err := storage.Directory(workspace, getenv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := localfile.NewSQLite(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.Resume(t.Context(), "old"); err != nil {
		t.Fatal("old session missing", err)
	}
}
