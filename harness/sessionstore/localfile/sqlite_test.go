package localfile

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/storage"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func sqliteStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := NewSQLite(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func sqlInput(id, text string) inbox.Input {
	raw, _ := json.Marshal(text)
	return inbox.Input{ID: inbox.ID(id), Kind: inbox.InputExternal, Payload: raw}
}
func TestSQLiteHistoryPagingProjectionForkAndExport(t *testing.T) {
	s := sqliteStore(t, t.TempDir())
	ctx := t.Context()
	if _, err := s.Create(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "parent"); !errors.Is(err, os.ErrExist) {
		t.Fatal("duplicate", err)
	}
	if err := s.AppendInput(ctx, "parent", sqlInput("input", "hello")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTurn(ctx, "parent", session.Turn{ID: "turn", Type: session.TurnRegular}); err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: "op", Type: "test", Version: 1, Status: operation.StatusReady, State: []byte(`{"large":"initial"}`)}
	status := sessionstore.ToolCallStatus{TurnID: "turn", CallID: "call", Status: tool.CallStatus{WaitingFor: []operation.ID{"op"}}, Operations: []operation.Operation{op}}
	if err := s.AppendToolCallStatus(ctx, "parent", status); err != nil {
		t.Fatal(err)
	}
	op.Status = operation.StatusCompleted
	op.State = []byte(`{"large":"terminal"}`)
	if err := s.SaveOperation(ctx, "parent", op); err != nil {
		t.Fatal(err)
	}
	// Resume must use the latest operation projection, including terminal states
	// whose completion has not yet been added to tool-call history.
	resume, err := s.Resume(ctx, "parent")
	if err != nil || len(resume.Operations) != 1 || resume.Operations[0].Status != operation.StatusCompleted {
		t.Fatal(resume, err)
	}
	page, err := s.Items(ctx, "parent", 0, 2)
	if err != nil || len(page.Items) != 2 || !page.More || page.NextAfter != 2 {
		t.Fatal(page, err)
	}
	page, err = s.Items(ctx, "parent", page.NextAfter, 2)
	if err != nil || len(page.Items) != 1 || page.More || len(page.Items[0].Data.(sessionstore.ToolCallStatus).Operations) != 1 {
		t.Fatal(page, err)
	}
	if _, err = s.Fork(ctx, "child", "parent", "turn"); err != nil {
		t.Fatal(err)
	}
	child, err := s.Resume(ctx, "child")
	if err != nil || len(child.Operations) != 0 {
		t.Fatal(child, err)
	}
	var exported bytes.Buffer
	if err = s.ExportJSONL(ctx, "parent", &exported); err != nil {
		t.Fatal(err)
	}
	decoded, _, err := decodeLog(exported.Bytes())
	if err != nil || len(decoded.Items) != 3 || decoded.Operations[0].Status != operation.StatusCompleted {
		t.Fatal(err)
	}
	reopen := sqliteStore(t, s.directory)
	again, err := reopen.Resume(ctx, "parent")
	if err != nil || len(again.Operations) != 1 {
		t.Fatal(again, err)
	}
	if err = s.database.Check(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestSQLiteFailedTransactionAndStaleWriterDoNotLoseHistory(t *testing.T) {
	dir := t.TempDir()
	s := sqliteStore(t, dir)
	ctx := t.Context()
	if _, err := s.Create(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	other := sqliteStore(t, dir)
	if _, _, err := other.loadWriteState(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendInput(ctx, "s", sqlInput("one", "one")); err != nil {
		t.Fatal(err)
	}
	if err := other.AppendInput(ctx, "s", sqlInput("two", "two")); err == nil {
		t.Fatal("stale writer accepted")
	}
	if err := other.AppendInput(ctx, "s", sqlInput("two", "two")); err != nil {
		t.Fatal("cache not evicted", err)
	}
	if _, err := s.database.Exec("CREATE TRIGGER reject_event BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'injected disk failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := other.AppendInput(ctx, "s", sqlInput("bad", "bad")); err == nil {
		t.Fatal("failure hidden")
	}
	if _, err := s.database.Exec("DROP TRIGGER reject_event"); err != nil {
		t.Fatal(err)
	}
	if err := other.AppendInput(ctx, "s", sqlInput("three", "three")); err != nil {
		t.Fatal(err)
	}
	page, err := other.Items(ctx, "s", 0, 10)
	if err != nil || len(page.Items) != 3 {
		t.Fatal(page, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = other.AppendInput(canceled, "s", sqlInput("canceled", "no")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestSQLiteCapturesSurviveCheckpointFailure(t *testing.T) {
	s := sqliteStore(t, t.TempDir())
	ctx := t.Context()
	_, err := s.Create(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AppendTurn(ctx, "s", session.Turn{ID: "t"}); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(s.directory, "operations", "s")
	dir := filepath.Join(base, "op")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	out, stderr := filepath.Join(dir, "out"), filepath.Join(dir, "err")
	for _, p := range []string{out, stderr} {
		if err = os.WriteFile(p, []byte("full capture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	spec, err := operation.NewShellSpec(operation.ShellInput{Command: "echo", Shell: "/bin/sh"}, base, 100)
	if err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: "op", Type: spec.Type, Version: spec.Version, State: spec.State, Status: operation.StatusReady, MaxOutputLength: 100}
	if err = s.AppendToolCallStatus(ctx, "s", sessionstore.ToolCallStatus{TurnID: "t", CallID: "c", Status: tool.CallStatus{WaitingFor: []operation.ID{"op"}}, Operations: []operation.Operation{op}}); err != nil {
		t.Fatal(err)
	}
	state, err := operation.DecodeShellState(op)
	if err != nil {
		t.Fatal(err)
	}
	state.OutPath = out
	state.ErrPath = stderr
	state.Result = &operation.ShellResult{Out: "preview"}
	op.Status = operation.StatusCompleted
	op.State, _ = json.Marshal(state)
	if _, err = s.database.Exec("CREATE TRIGGER fail_checkpoint BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveOperation(ctx, "s", op); err == nil {
		t.Fatal("checkpoint should fail")
	}
	if _, err = os.Stat(out); err != nil {
		t.Fatal("deleted before checkpoint", err)
	}
	if _, err = s.database.Exec("DROP TRIGGER fail_checkpoint"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveOperation(ctx, "s", op); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("spool retained", err)
	}
	// Simulate a durable checkpoint whose unlink was interrupted by a crash.
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(out, []byte("full capture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.CleanupCaptures(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("recovery did not clean spool", err)
	}
	data, err := s.database.Bytes(ctx, storage.Reference(out))
	if err != nil || string(data) != "full capture" {
		t.Fatal(string(data), err)
	}
}
func TestSQLiteLegacyMigrationIsIdempotentAndNonDestructive(t *testing.T) {
	source := t.TempDir()
	legacy, err := New(source)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err = legacy.Create(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if err = legacy.AppendInput(ctx, "old", sqlInput("i", strings.Repeat("repeated", 10000))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "old.session.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"torn":`)
	_ = f.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sqliteStore(t, t.TempDir())
	if err = s.ImportLegacy(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err = s.ImportLegacy(ctx, source); err != nil {
		t.Fatal("retry", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("source changed", err)
	}
	page, err := s.Items(ctx, "old", 0, 10)
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	if err = os.WriteFile(path, append(before, 'x'), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.ImportLegacy(ctx, source); err == nil {
		t.Fatal("accepted changed source")
	}
}
