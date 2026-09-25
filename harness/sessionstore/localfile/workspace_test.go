package localfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

func TestWorkspaceDefersNewMigrationWhileHostsAreActive(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(t.TempDir(), "state")
	s, host, err := OpenWorkspace(t.Context(), directory, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer host()
	defer s.Close()
	source := filepath.Join(workspace, ".harness", "sessions")
	legacy, err := New(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Create(t.Context(), "old"); err != nil {
		t.Fatal(err)
	}
	other, release, err := OpenWorkspace(t.Context(), directory, workspace)
	if err == nil {
		_ = other.Close()
		_ = release()
		t.Fatal("migration ran alongside active host")
	}
	if !strings.Contains(err.Error(), "legacy migration required") {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(source, "old.session.jsonl")); err != nil {
		t.Fatal("busy source was removed", err)
	}
	// Failed startup must leave the existing host usable.
	if _, err = s.Create(t.Context(), "live"); err != nil {
		t.Fatal(err)
	}
	_ = host()
	other, release, err = OpenWorkspace(t.Context(), directory, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer other.Close()
	if _, err = other.Resume(t.Context(), "old"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceUpgradesPartialManifestBeforeParallelUse(t *testing.T) {
	workspace := t.TempDir()
	source := filepath.Join(workspace, ".harness", "sessions")
	legacy, err := New(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Create(t.Context(), "old"); err != nil {
		t.Fatal(err)
	}
	out := sourceFile(t, source, "operations/old/op/out", "retained output")
	dir := filepath.Join(t.TempDir(), "state")
	dest := sqliteStore(t, dir)
	verifiedManifest(t, source, dest)
	if err = os.Remove(filepath.Join(source, "old.session.jsonl")); err != nil {
		t.Fatal(err)
	}
	if pending, err := dest.migrationPending(t.Context(), source); err != nil || !pending {
		t.Fatal("partial cleanup mistaken for completed migration", pending, err)
	}
	a, releaseA, err := OpenWorkspace(t.Context(), dir, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()
	defer a.Close()
	if _, err = os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("partial cleanup was not completed", err)
	}
	b, releaseB, err := OpenWorkspace(t.Context(), dir, workspace)
	if err != nil {
		t.Fatal("completed migration blocked peer", err)
	}
	defer releaseB()
	defer b.Close()
	if pending, err := b.migrationPending(t.Context(), source); err != nil || pending {
		t.Fatal("completed migration still pending", pending, err)
	}
}

func TestSQLiteConcurrentSessionCreates(t *testing.T) {
	dir := t.TempDir()
	var stores []*Store
	for range 4 {
		stores = append(stores, sqliteStore(t, dir))
	}
	start := make(chan struct{})
	done := make(chan error, len(stores))
	for i, s := range stores {
		go func() {
			<-start
			for n := range 16 {
				id := session.ID(fmt.Sprintf("%d-%d", i, n))
				if _, err := s.Create(t.Context(), id); err != nil {
					done <- err
					return
				}
				if err := s.AppendInput(t.Context(), id, sqlInput("input", "message")); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	close(start)
	for range stores {
		if err := <-done; err != nil {
			t.Error("concurrent creation failed", err)
		}
	}
	list, err := stores[0].ListSessions(t.Context())
	if err != nil || len(list) != 64 {
		t.Fatal(len(list), err)
	}
}
