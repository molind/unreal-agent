package chat

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestSQLiteParallelChatsAndSessionHandoff(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "state")
	args := []string{"-storage-format", "sqlite", "-session-directory", directory}
	a := launch(t, workspace, false, args...)
	a.send("task a")
	callA := a.call()
	idA := sessionID(t, a.output.snapshot())
	b := launch(t, workspace, false, args...)
	b.send("task b")
	callB := b.call()
	idB := sessionID(t, b.output.snapshot())
	if idA == idB {
		t.Fatal("parallel chats reused session identity")
	}
	// Duplicate startup must fail before provider setup/recovery or effects.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := Run(ctx, append(append([]string{}, args...), "-session", idA, workspace), func(string) string { return "" }, io.NopCloser(strings.NewReader("")), io.Discard, io.Discard, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatal("duplicate startup was not rejected", err)
	}
	// Failed selection cancels/joins only B, keeps its lease, and never affects A.
	b.send("/resume " + idA)
	b.wait("Cannot switch")
	select {
	case <-callB.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("old runtime was not joined")
	}
	select {
	case <-callA.ctx.Done():
		t.Fatal("peer's runtime was canceled")
	default:
	}
	b.send("still task b")
	callB = b.call()
	assertMessages(t, callB.request, llm.RoleUser, "task b", "still task b")
	callB.reply <- reply("answer b")
	callA.reply <- reply("answer a")
	a.wait("answer a")
	b.wait("answer b")

	probe, err := localfile.NewSQLite(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	a.send("/stop")
	a.wait("Stopped. Send a new message")
	if release, err := probe.LockSession(session.ID(idA)); err == nil {
		_ = release()
		t.Fatal("/stop released the selected session")
	}
	a.send("/new")
	// The fresh-chat banner also exists at startup; /status is an ordered ack.
	a.send("/status")
	a.wait("Session: (unsaved; first message saves)")
	b.send("/resume " + idA)
	b.wait("Selected session " + idA)
	b.send("continue task a")
	resumed := b.call()
	assertMessages(t, resumed.request, llm.RoleUser, "task a", "continue task a")
	resumed.reply <- reply("handoff a complete")
	b.wait("handoff a complete")
	a.send("/resume " + idB)
	a.wait("Selected session " + idB)
	a.send("continue task b")
	resumed = a.call()
	assertMessages(t, resumed.request, llm.RoleUser, "task b", "still task b", "continue task b")
	resumed.reply <- reply("handoff b complete")
	a.wait("handoff b complete")
	a.finish("/exit")
	b.finish("/exit")
	for _, id := range []string{idA, idB} {
		release, err := probe.LockSession(session.ID(id))
		if err != nil {
			t.Fatal("shutdown leaked session lease", err)
		}
		_ = release()
	}
	maintenance, err := probe.Database().LockWriter()
	if err != nil {
		t.Fatal("shutdown leaked workspace lease", err)
	}
	_ = maintenance()
}

func TestSQLiteFailedSessionSetupReleasesOnlyTarget(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "state")
	c := launch(t, workspace, false, "-storage-format", "sqlite", "-session-directory", directory)
	c.send("original")
	call := c.call()
	call.reply <- reply("original done")
	c.wait("original done")
	id := sessionID(t, c.output.snapshot())
	probe, err := localfile.NewSQLite(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if _, err = probe.Create(t.Context(), "broken"); err != nil {
		t.Fatal(err)
	}
	// A low-level SQL fault in setup exercises the rollback path after locking.
	if _, err = probe.Database().Exec("CREATE TRIGGER reject_settings BEFORE INSERT ON events WHEN NEW.session='broken' BEGIN SELECT RAISE(ABORT,'setup failure'); END"); err != nil {
		t.Fatal(err)
	}
	c.send("/resume broken")
	c.wait("Cannot start selected session")
	release, err := probe.LockSession("broken")
	if err != nil {
		t.Fatal("failed target retained lease", err)
	}
	_ = release()
	if release, err = probe.LockSession(session.ID(id)); err == nil {
		_ = release()
		t.Fatal("failed switch lost original lease")
	}
	c.send("still original")
	call = c.call()
	assertMessages(t, call.request, llm.RoleUser, "original", "still original")
	call.reply <- reply("retained original")
	c.wait("retained original")
	c.finish("/exit")
}
