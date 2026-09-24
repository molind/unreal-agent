package chat

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

func records(t *testing.T, path string) []logRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []logRecord
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r logRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.Time.IsZero() || r.Stage == "" || r.Event == "" {
			t.Fatalf("incomplete record: %+v", r)
		}
		records = append(records, r)
	}
	return records
}
func TestCommandLogsLifecycleAndReplay(t *testing.T) {
	workspace := t.TempDir()
	c := launch(t, workspace, false)
	c.wait("Diagnostic log:")
	c.send("execute three commands")
	call := c.call()
	id := sessionID(t, c.output.snapshot())
	call.reply <- shellResponse("printf hello", "exit 7", "echo $$ > log.pid; exec sleep 60")
	c.wait("completed")
	c.wait("failed (exit 7)")
	op := c.toolID("echo $$ > log.pid; exec sleep 60")
	c.waitFile("log.pid")
	c.send("/stop")
	c.wait("tool " + op + " canceled")
	c.wait("Stopped.")
	c.finish("/exit")
	assertNoProcesses(t, workspace, id)
	paths, err := filepath.Glob(filepath.Join(workspace, ".harness/sessions/logs/commands", id, "*.jsonl"))
	if err != nil || len(paths) != 3 {
		t.Fatalf("command logs: %v %v", paths, err)
	}
	before := map[string]string{}
	outcomes := map[string]bool{}
	for _, path := range paths {
		data, _ := os.ReadFile(path)
		before[path] = string(data)
		list := records(t, path)
		if len(list) < 2 || len(list) > 3 {
			t.Fatalf("duplicate/missing lifecycle: %+v", list)
		}
		first, last := list[0], list[len(list)-1]
		if first.Event != "running" || first.Started.IsZero() || last.Started != first.Started || last.Duration <= 0 || last.Session != session.ID(id) || last.Operation == "" || last.Directory != workspace || last.Stdout == "" || last.Stderr == "" {
			t.Fatalf("incomplete command identity: %+v", list)
		}
		if last.Event == "completed" && (last.ExitCode == nil || *last.ExitCode != 0) {
			t.Fatal(last)
		}
		if last.Event == "failed (exit 7)" && (last.ExitCode == nil || *last.ExitCode != 7) {
			t.Fatal(last)
		}
		outcomes[last.Event] = true
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0600 {
			t.Fatal(info.Mode())
		}
	}
	for _, want := range []string{"completed", "failed (exit 7)", "canceled"} {
		if !outcomes[want] {
			t.Fatal(outcomes)
		}
	}
	c = launch(t, workspace, false, "-session", id)
	c.finish("/exit")
	for path, want := range before {
		data, _ := os.ReadFile(path)
		if string(data) != want {
			t.Fatalf("replay appended a command: %s", path)
		}
	}
}

func TestCommandLogResumeKeepsIdentity(t *testing.T) {
	dir := t.TempDir()
	d := newDisplay(io.Discard, func(string) string { return "" })
	log, err := openLogs(dir, d.safe)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := operation.NewShellSpec(operation.ShellInput{Command: "printf hi", Shell: "/bin/sh", Directory: dir}, dir, 1000)
	if err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{MaxOutputLength: 1000, ID: "same-operation", Type: spec.Type, Version: spec.Version, State: spec.State, Status: operation.StatusAwaiting}
	if err := log.command("session", op); err != nil {
		t.Fatal(err)
	}
	_ = log.file.Close()
	log, err = openLogs(dir, d.safe)
	if err != nil {
		t.Fatal(err)
	}
	defer log.file.Close()
	if err := log.command("session", op); err != nil {
		t.Fatal(err)
	} // recovered same running operation
	op.Status = operation.StatusCanceled
	if err := log.command("session", op); err != nil {
		t.Fatal(err)
	}
	list := records(t, filepath.Join(dir, "logs/commands/session/same-operation.jsonl"))
	if len(list) != 2 || list[0].Started != list[1].Started || list[1].Operation != list[0].Operation {
		t.Fatal(list)
	}
}

func TestDiagnosticCausesStacksRedactionAndPermissions(t *testing.T) {
	workspace := t.TempDir()
	cause := errors.New(`dial refused Bearer bearer-value {"access_token":"json-value","account_id":"account-value"} https://user:pass@example.invalid/?api_key=query-value sk-testsecret`)
	provider := agentrunner.Provider{Name: "openai-codex", NewClient: func(string, string, int, func(string) string) (agentrunner.Client, error) {
		return nil, fmt.Errorf("provider init: %w", cause)
	}}
	err := Run(t.Context(), []string{"-storage-format", "jsonl", workspace}, func(string) string { return "" }, io.NopCloser(strings.NewReader("")), io.Discard, io.Discard, nil, []agentrunner.Provider{provider})
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "dial refused") {
		t.Fatalf("lost cause: %v", err)
	}
	paths, _ := filepath.Glob(filepath.Join(workspace, ".harness/sessions/logs/diagnostic-*.jsonl"))
	if len(paths) != 1 {
		t.Fatal(paths)
	}
	list := records(t, paths[0])
	last := list[len(list)-1]
	// Provider setup precedes the first message: there is no durable session ID.
	durableCount(t, workspace, 0)
	if last.Stage != "provider setup" || last.Session != "" || !strings.Contains(last.Error, "dial refused") || !strings.Contains(last.Stack, "chat.Run") {
		t.Fatalf("not actionable: %+v", last)
	}
	data, _ := os.ReadFile(paths[0])
	both := string(data) + err.Error()
	for _, secret := range []string{"bearer-value", "json-value", "account-value", "user:pass", "query-value", "sk-testsecret"} {
		if strings.Contains(both, secret) {
			t.Fatalf("secret %q in diagnostics", secret)
		}
	}
	info, _ := os.Stat(paths[0])
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	info, _ = os.Stat(filepath.Dir(paths[0]))
	if info.Mode().Perm() != 0700 {
		t.Fatal(info.Mode())
	}
}

type panicModel struct{}

func (panicModel) Respond(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
	panic("specific panic API_KEY=private")
}
func TestProviderPanicBoundary(t *testing.T) {
	d := newDisplay(io.Discard, func(string) string { return "" })
	log, err := openLogs(t.TempDir(), d.safe)
	if err != nil {
		t.Fatal(err)
	}
	defer log.file.Close()
	adapter := loggedAdapter{panicModel{}, log, "panic-session"}
	_, err = adapter.Respond(t.Context(), llm.Request{}, llm.RequestOptions{})
	if err == nil || !strings.Contains(err.Error(), "specific panic") {
		t.Fatal(err)
	}
	list := records(t, log.diagnostic)
	last := list[len(list)-1]
	if !strings.Contains(last.Stack, "panicModel.Respond") || strings.Contains(last.Error, "private") || last.Session != "panic-session" || last.Event != "request_failed" {
		t.Fatalf("panic not captured at source: %+v", last)
	}
}
