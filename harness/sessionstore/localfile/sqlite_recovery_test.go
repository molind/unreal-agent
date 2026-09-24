package localfile

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func TestSQLiteUnknownProcessStartNeverArchivesOrDeletes(t *testing.T) {
	s := sqliteStore(t, t.TempDir())
	base := filepath.Join(s.directory, "operations", "s")
	dir := filepath.Join(base, "op")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := os.WriteFile(out, []byte("possibly still growing"), 0600); err != nil {
		t.Fatal(err)
	}
	// Process start may have happened before its PID reached durable history.
	// PID zero does NOT prove that the capture is closed.
	state := operation.ShellState{BaseDirectory: base, Input: operation.ShellInput{Shell: "/bin/sh"}, OutPath: out, TerminalError: "execution outcome is unknown"}
	raw, _ := json.Marshal(state)
	op := operation.Operation{ID: "op", Type: operation.TypeShell, Version: operation.VersionShell, Status: operation.StatusFailed, MaxOutputLength: 100, State: raw}
	for _, remove := range []bool{false, true} {
		if err := s.captureOperation(t.Context(), op, remove); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.database.QueryRow("SELECT count(*) FROM captures").Scan(&n); err != nil || n != 0 {
		t.Fatal("archived unknown outcome", n, err)
	}
}
func TestInPlaceMigrationRetainsAuxiliarySources(t *testing.T) {
	source := t.TempDir()
	legacy, err := New(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Create(t.Context(), "s"); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(source, "operations", "s")
	dir := filepath.Join(base, "op")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "out")
	if err = os.WriteFile(path, []byte("legacy output"), 0600); err != nil {
		t.Fatal(err)
	}
	s := sqliteStore(t, source)
	if err = s.ImportLegacy(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if err = s.database.RemoveCapture(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("deleted migration backup", err)
	}
	got, err := s.database.Bytes(t.Context(), storage.Reference(path))
	if err != nil || string(got) != "legacy output" {
		t.Fatal(string(got), err)
	}
}
func TestLegacyMetadataMigratesRevisionIntoBothNamespaces(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	legacy, err := New(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Create(t.Context(), "s"); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(source, "operations", "s")
	dir := filepath.Join(base, "revisions")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"Version":1,"Path":"/workspace/source","SHA256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if err = os.WriteFile(filepath.Join(dir, "0123456789abcdef.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	s := sqliteStore(t, destination)
	if err = s.ImportLegacy(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{base, filepath.Join(destination, "operations", "s")} {
		got, err := s.database.GetMetadata(t.Context(), "revision:"+scope, "0123456789abcdef")
		if err != nil || string(got) != string(raw) {
			t.Fatal(scope, err)
		}
	}
}
