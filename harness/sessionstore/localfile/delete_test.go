package localfile

import (
	"errors"
	"os"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestDeleteSessionLeaseAtomicityAndFork(t *testing.T) {
	s := sqliteStore(t, t.TempDir())
	ctx := t.Context()
	if _, err := s.Create(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendInput(ctx, "parent", sqlInput("input", "keep this in the fork")); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTurn(ctx, "parent", session.Turn{ID: "turn", Type: session.TurnRegular}); err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: "op", Type: "test", Version: 1, Status: operation.StatusReady, State: []byte(`{}`)}
	if err := s.AppendToolCallStatus(ctx, "parent", sessionstore.ToolCallStatus{TurnID: "turn", CallID: "call", Status: tool.CallStatus{WaitingFor: []operation.ID{"op"}}, Operations: []operation.Operation{op}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fork(ctx, "child", "parent", "turn"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.database.Exec("INSERT INTO diagnostics(session,operation,run,payload) VALUES('parent','op','run','{}')"); err != nil {
		t.Fatal(err)
	}

	other := sqliteStore(t, s.directory)
	release, err := other.LockSession("parent")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteSession(ctx, "parent"); err == nil {
		t.Fatal("deleted another runtime's session")
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	// An error after deleting dependent rows must roll back the entire change.
	if _, err = s.database.Exec("CREATE TRIGGER refuse_delete BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT,'injected'); END"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteSession(ctx, "parent"); err == nil {
		t.Fatal("injected failure ignored")
	}
	for _, table := range []string{"events", "operations", "diagnostics"} {
		var n int
		if err = s.database.QueryRow("SELECT count(*) FROM " + table + " WHERE session='parent'").Scan(&n); err != nil || n == 0 {
			t.Fatalf("lost %s on rollback: %d, %v", table, n, err)
		}
	}
	if _, err = s.database.Exec("DROP TRIGGER refuse_delete"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteSession(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteSession(ctx, "parent"); err != nil {
		t.Fatal("retry", err)
	}
	if _, err = other.Resume(ctx, "parent"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted session resumed", err)
	}
	for _, table := range []string{"events", "operations", "diagnostics"} {
		var n int
		if err = s.database.QueryRow("SELECT count(*) FROM " + table + " WHERE session='parent'").Scan(&n); err != nil || n != 0 {
			t.Fatalf("retained %s: %d, %v", table, n, err)
		}
	}
	page, err := other.Items(ctx, "child", 0, 64)
	if err != nil || len(page.Items) == 0 {
		t.Fatal("fork damaged", err)
	}
	if _, err = other.Resume(ctx, "child"); err != nil {
		t.Fatal(err)
	}
	if err = s.database.Check(ctx); err != nil {
		t.Fatal(err)
	}
}
