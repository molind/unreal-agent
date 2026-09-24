package operation

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fileFixture(t *testing.T, text string) (FileInput, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(path, []byte(text), 0750); err != nil {
		t.Fatal(err)
	}
	return FileInput{Action: "Read", Path: path, BaseDirectory: filepath.Join(dir, "state"), Offset: 1, Limit: 200}, path
}
func readRevision(t *testing.T, in FileInput) FileResult {
	t.Helper()
	in.Action = "Read"
	in.Offset, in.Limit = 1, 200
	r, err := executeFile(t.Context(), "read", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Revision) != 16 {
		t.Fatalf("revision length %d", len(r.Revision))
	}
	return r
}
func TestStructuredReadEditWriteAndShortRevisions(t *testing.T) {
	in, path := fileFixture(t, "one\nбеларуская\nthree\n")
	in.Offset, in.Limit = 2, 1
	read, err := executeFile(t.Context(), "read", in)
	if err != nil || read.Text != "2: беларуская\n" || read.NextOffset != 3 || read.TotalLines != 3 {
		t.Fatalf("read %+v, %v", read, err)
	}
	var binding fileRevision
	data, err := os.ReadFile(filepath.Join(in.BaseDirectory, "revisions", read.Revision+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &binding); err != nil || len(binding.SHA256) != 64 {
		t.Fatal("short revision did not bind a full digest", err)
	}
	in.Action, in.Revision, in.OldText, in.NewText = "Edit", read.Revision, "беларуская", "новая"
	result, err := executeFile(t.Context(), "edit", in)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "one\nновая\nthree\n" || result.Replacements != 1 || len(result.Revision) != 16 || !strings.Contains(result.Diff, "-беларуская\n+новая") {
		t.Fatalf("edit %+v: %q", result, data)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0750 {
		t.Fatal("file permissions lost")
	}
	before, _ := os.ReadFile(path)
	if _, err := executeFile(t.Context(), "stale", in); err == nil {
		t.Fatal("stale read overwrote changed contents")
	}
	data, _ = os.ReadFile(path)
	if string(data) != string(before) {
		t.Fatal("conflict changed file")
	}
	in.Action, in.Revision, in.Content = "Write", result.Revision, "replacement\n"
	written, err := executeFile(t.Context(), "write", in)
	if err != nil || len(written.Revision) != 16 {
		t.Fatal(err)
	}
	in.Path = filepath.Join(filepath.Dir(path), "new.txt")
	in.Revision = "missing"
	in.Content = ""
	created, err := executeFile(t.Context(), "create", in)
	if err != nil || !created.Created {
		t.Fatalf("create %+v %v", created, err)
	}
	if _, err := executeFile(t.Context(), "create-again", in); err == nil {
		t.Fatal("create-only overwrote a file")
	}
	in.Revision = written.Revision
	in.Content = "bad"
	if _, err := executeFile(t.Context(), "wrong-path", in); err == nil {
		t.Fatal("revision for another file accepted")
	}
}
func TestFileEditRefusesAmbiguityAndPreservesExactBytes(t *testing.T) {
	in, path := fileFixture(t, "same\r\nsame\r\nend")
	r := readRevision(t, in)
	in.Action, in.Revision, in.OldText, in.NewText = "Edit", r.Revision, "same", "changed"
	if _, err := executeFile(t.Context(), "ambiguous", in); err == nil {
		t.Fatal("ambiguous edit accepted")
	}
	in.ReplaceAll = true
	r, err := executeFile(t.Context(), "all", in)
	if err != nil || r.Replacements != 2 {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "changed\r\nchanged\r\nend" {
		t.Fatalf("line endings changed: %q", data)
	}
	in.Revision = r.Revision
	in.OldText = "absent"
	if _, err := executeFile(t.Context(), "absent", in); err == nil {
		t.Fatal("missing match accepted")
	}
}
func TestFileOperationsRejectSpecialAndBinaryFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "binary", "hardlink", "readonly", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			in, path := fileFixture(t, "old\n")
			switch kind {
			case "symlink":
				in.Path = path + "-link"
				if err := os.Symlink(path, in.Path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				in.Path = filepath.Dir(path)
			case "binary":
				if err := os.WriteFile(path, []byte{0, 1, 2}, 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.WriteFile(path, []byte(strings.Repeat("x", MaxFileBytes+1)), 0600); err != nil {
					t.Fatal(err)
				}
			case "hardlink", "readonly":
				r := readRevision(t, in)
				in.Action, in.Revision, in.OldText, in.NewText = "Edit", r.Revision, "old", "new"
				if kind == "hardlink" {
					if err := os.Link(path, path+"-alias"); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Chmod(path, 0400); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := executeFile(t.Context(), "reject", in); err == nil {
				t.Fatal("unsafe file accepted")
			}
		})
	}
}
func TestFileMutationReceiptsNeverBlindlyRepeat(t *testing.T) {
	for _, stage := range []string{"complete", "prepared-applied", "prepared-not-applied"} {
		t.Run(stage, func(t *testing.T) {
			in, path := fileFixture(t, "old\n")
			r := readRevision(t, in)
			in.Action, in.Revision, in.OldText, in.NewText = "Edit", r.Revision, "old", "new"
			first, err := executeFile(t.Context(), "edit", in)
			if err != nil {
				t.Fatal(err)
			}
			if stage != "complete" {
				receiptPath := filepath.Join(in.BaseDirectory, "edit", "transaction.json")
				data, _ := os.ReadFile(receiptPath)
				var rec fileReceipt
				if err := json.Unmarshal(data, &rec); err != nil {
					t.Fatal(err)
				}
				rec.Complete = false
				data, _ = json.Marshal(rec)
				if err := os.WriteFile(receiptPath, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "complete" {
				_ = os.WriteFile(path, []byte("external change"), 0750)
			}
			if stage == "prepared-not-applied" {
				_ = os.WriteFile(path, []byte("old\n"), 0750)
			}
			before, _ := os.ReadFile(path)
			again, err := executeFile(t.Context(), "edit", in)
			if stage == "prepared-not-applied" {
				if err == nil {
					t.Fatal("uncertain mutation repeated")
				}
			} else if err != nil || !again.Recovered || again.Revision != first.Revision {
				t.Fatalf("recovery %+v %v", again, err)
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("replay modified current file")
			}
		})
	}
}
func TestConcurrentFileEditsHaveOneWinner(t *testing.T) {
	in, path := fileFixture(t, "original\n")
	r := readRevision(t, in)
	in.Action, in.Revision, in.OldText = "Edit", r.Revision, "original"
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		wg.Go(func() {
			next := in
			next.NewText = name
			_, err := executeFile(t.Context(), ID(name), next)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("winners=%d", success)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "first\n" && string(data) != "second\n" {
		t.Fatalf("torn write %q", data)
	}
}
func TestFileDiffIsAnApplicablePatch(t *testing.T) {
	for _, pair := range [][2]string{{"a\nb\nc\n", "a\nB\nc\n"}, {"old", "new"}, {"one\r\ntwo\r\n", "one\r\nthree\r\n"}, {"", "new\n"}, {"old\n", ""}} {
		dir := t.TempDir()
		path := filepath.Join(dir, "f.txt")
		if err := os.WriteFile(path, []byte(pair[0]), 0600); err != nil {
			t.Fatal(err)
		}
		patch := fileDiff("f.txt", pair[0], pair[1], false)
		cmd := exec.CommandContext(t.Context(), "git", "apply", "--check", "--unsafe-paths", "-p0", "-")
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(patch)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("invalid diff %s: %v\n%s", patch, err, out)
		}
	}
}
func TestLocalManagerRunsFileOperationsAndCancellation(t *testing.T) {
	in, _ := fileFixture(t, "hello\n")
	spec, err := NewFileSpec(in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	manager := NewLocalOperationManager(ctx)
	if err := manager.Add(Operation{ID: "read", Type: spec.Type, Version: spec.Version, State: spec.State, Status: StatusReady}); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(3 * time.Second)
	for {
		select {
		case op := <-manager.Updates():
			if op.Status == StatusCompleted {
				state, err := DecodeFileState(op)
				if err != nil || state.Result == nil || state.Result.Revision == "" {
					t.Fatal("no durable read result", err)
				}
				cancel()
				for range manager.Updates() {
				}
				return
			}
		case <-timeout:
			t.Fatal("file operation did not finish")
		}
	}
}
func TestCanceledFileMutationDoesNotWrite(t *testing.T) {
	in, path := fileFixture(t, "old\n")
	r := readRevision(t, in)
	in.Action, in.Revision, in.OldText, in.NewText = "Edit", r.Revision, "old", "new"
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := executeFile(ctx, "canceled", in); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "old\n" {
		t.Fatal("canceled request wrote the file")
	}
}
