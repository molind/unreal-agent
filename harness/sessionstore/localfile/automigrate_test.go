package localfile

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func legacyFixture(t *testing.T) (string, *Store, *Store) {
	t.Helper()
	source := filepath.Join(t.TempDir(), ".harness", "sessions")
	old, err := New(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.Create(t.Context(), "old"); err != nil {
		t.Fatal(err)
	}
	if err = old.AppendInput(t.Context(), "old", sqlInput("input", "retain original history")); err != nil {
		t.Fatal(err)
	}
	return source, old, sqliteStore(t, t.TempDir())
}
func sourceFile(t *testing.T, source, rel, text string) string {
	t.Helper()
	path := filepath.Join(source, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestAutomaticMigrationCleansAndArchivesExactSources(t *testing.T) {
	source, _, dest := legacyFixture(t)
	journal := filepath.Join(source, "old.session.jsonl")
	f, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"torn":`)
	_ = f.Close()
	original, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	out := sourceFile(t, source, "operations/old/op/out", "full output")
	sourceFile(t, source, "operations/old/op/err", "")
	sourceFile(t, source, "operations/old/op/lock", "")
	sourceFile(t, source, "logs/diagnostic-old.jsonl", "{}\n")
	sourceFile(t, source, "logs/commands/old/op.jsonl", "{}\n")
	if err = dest.MigrateLegacy(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(source)); !os.IsNotExist(err) {
		t.Fatal(".harness remains", err)
	}
	for path, want := range map[string][]byte{journal: original, out: []byte("full output")} {
		got, err := dest.database.Bytes(t.Context(), storage.Reference(path))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("lost original bytes", path, err)
		}
	}
	page, err := dest.Items(t.Context(), "old", 0, 100)
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	if err = dest.MigrateLegacy(t.Context(), source); err != nil {
		t.Fatal("retry", err)
	}
}
func TestAutomaticMigrationPreservesSkillsUnknownFilesAndDestination(t *testing.T) {
	for _, inPlace := range []bool{false, true} {
		t.Run(map[bool]string{false: "XDG", true: "in-place"}[inPlace], func(t *testing.T) {
			source, _, dest := legacyFixture(t)
			if inPlace {
				dest = sqliteStore(t, source)
			}
			skill := sourceFile(t, filepath.Dir(source), "skills/custom/SKILL.md", "keep skill")
			unknown := sourceFile(t, source, "unknown.txt", "keep unknown")
			if err := dest.MigrateLegacy(t.Context(), source); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{skill, unknown, dest.database.Path} {
				if _, err := os.Stat(path); err != nil {
					t.Fatal("removed unrelated file", path, err)
				}
			}
			if _, err := os.Stat(filepath.Join(source, "old.session.jsonl")); !os.IsNotExist(err) {
				t.Fatal("journal not cleaned", err)
			}
			if err := dest.MigrateLegacy(t.Context(), source); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestAutomaticMigrationPreservesCorruptOrActiveSources(t *testing.T) {
	source, _, dest := legacyFixture(t)
	path := filepath.Join(source, "old.session.jsonl")
	lease, err := storage.LockLegacyDirectory(source, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = dest.MigrateLegacy(t.Context(), source); err == nil {
		t.Fatal("active legacy writer ignored")
	}
	_ = lease.Close()
	if _, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("corrupt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = dest.MigrateLegacy(t.Context(), source); err == nil {
		t.Fatal("corrupt history accepted")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "corrupt\n" {
		t.Fatal("corrupt source removed", err)
	}
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err = dest.MigrateLegacy(t.Context(), source); err != nil {
		t.Fatal("repair retry failed", err)
	}
}
func TestAutomaticMigrationPreservesSymlinkTargets(t *testing.T) {
	source, _, dest := legacyFixture(t)
	target := t.TempDir()
	outside := sourceFile(t, target, "old/op/out", "outside")
	if err := os.Symlink(target, filepath.Join(source, "operations")); err != nil {
		t.Fatal(err)
	}
	if err := dest.MigrateLegacy(t.Context(), source); err == nil {
		t.Fatal("symlink followed")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "outside" {
		t.Fatal("outside changed", err)
	}
}
func TestAutomaticCleanupPreviouslyImportedHistoryWithNewerEvents(t *testing.T) {
	source, _, dest := legacyFixture(t)
	if err := dest.ImportLegacy(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if err := dest.AppendInput(t.Context(), "old", sqlInput("new", "new SQLite work")); err != nil {
		t.Fatal(err)
	}
	if err := dest.MigrateLegacy(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	page, err := dest.Items(t.Context(), "old", 0, 100)
	if err != nil || len(page.Items) != 2 {
		t.Fatal("overwrote newer history", page, err)
	}
}

// Set up the exact durable boundary reached after verification and before unlink.
func verifiedManifest(t *testing.T, source string, dest *Store) migrationManifest {
	t.Helper()
	files, dirs, err := discoverMigrationFiles(source, migrationManifest{})
	if err != nil {
		t.Fatal(err)
	}
	manifest := migrationManifest{Version: 1, Verified: true, Directories: dirs}
	for _, file := range files {
		path := filepath.Join(source, file)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = dest.database.RetainCapture(t.Context(), path); err != nil {
			t.Fatal(err)
		}
		manifest.Files = append(manifest.Files, migrationFile{Path: file, Hash: storage.Hash(data)})
	}
	if err = dest.ImportLegacy(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if err = dest.saveManifest(t.Context(), source, manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}
func TestAutomaticCleanupResumesAfterJournalWasRemoved(t *testing.T) {
	source, _, dest := legacyFixture(t)
	sourceFile(t, source, "operations/old/op/out", "output")
	verifiedManifest(t, source, dest)
	if err := os.Remove(filepath.Join(source, "old.session.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := dest.MigrateLegacy(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(source)); !os.IsNotExist(err) {
		t.Fatal("partial cleanup not completed", err)
	}
}
func TestAutomaticCleanupRejectsChangedFileOrCorruptArchive(t *testing.T) {
	for _, corruptArchive := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed-source", true: "corrupt-archive"}[corruptArchive], func(t *testing.T) {
			source, _, dest := legacyFixture(t)
			out := sourceFile(t, source, "operations/old/op/out", "original output")
			verifiedManifest(t, source, dest)
			if corruptArchive {
				if _, err := dest.database.Exec("UPDATE chunks SET data=x'00'"); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(out, []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := dest.MigrateLegacy(t.Context(), source); err == nil {
				t.Fatal("unsafe cleanup allowed")
			}
			if _, err := os.Stat(filepath.Join(source, "old.session.jsonl")); err != nil {
				t.Fatal("deleted before full verification", err)
			}
			if _, err := os.Stat(out); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestAutomaticMigrationCanceledBeforeDelete(t *testing.T) {
	source, _, dest := legacyFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := dest.MigrateLegacy(ctx, source); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source, "old.session.jsonl")); err != nil {
		t.Fatal(err)
	}
	// Manifests are ordinary versioned database metadata, not loose files.
	manifest := migrationManifest{Version: 999}
	data, _ := json.Marshal(manifest)
	if err := dest.database.PutMetadata(t.Context(), cleanupNamespace, source, data); err != nil {
		t.Fatal(err)
	}
	if err := dest.MigrateLegacy(t.Context(), source); err == nil {
		t.Fatal("future manifest accepted")
	}
}
