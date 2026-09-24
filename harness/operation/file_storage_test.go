package operation

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func TestSQLiteFileRevisionsReceiptsAndArtifacts(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	db, err := storage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	base := filepath.Join(dir, "operations", "session")
	path := filepath.Join(t.TempDir(), "file.txt")
	input := FileInput{Action: "Write", Path: path, BaseDirectory: base, Revision: "missing", Content: "first\n"}
	written, err := executeFileStored(ctx, "write", input, db)
	if err != nil || !written.Applied || written.Revision == "" || !strings.HasPrefix(written.DiffPath, "artifact:") {
		t.Fatal(written, err)
	}
	if _, err = os.Stat(base); !os.IsNotExist(err) {
		t.Fatal("file action created auxiliary files", err)
	}
	// A completed receipt prevents replaying the mutation, even if the user later
	// changes the file. The recovered result describes that original operation.
	if err = os.WriteFile(path, []byte("external\n"), 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := executeFileStored(ctx, "write", input, db)
	if err != nil || !recovered.Recovered {
		t.Fatal(recovered, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "external\n" {
		t.Fatal("repeated file write")
	}
	edit := FileInput{Action: "Edit", Path: path, BaseDirectory: base, Revision: written.Revision, OldText: "first", NewText: "second"}
	if _, err = executeFileStored(ctx, "conflict", edit, db); err == nil {
		t.Fatal("ignored revision conflict")
	}
	read, err := executeFileStored(ctx, "read", FileInput{Action: "Read", Path: path, BaseDirectory: base, Offset: 1, Limit: 200}, db)
	if err != nil {
		t.Fatal(err)
	}
	edit.Revision = read.Revision
	edit.OldText = "external"
	if _, err = executeFileStored(ctx, "edit", edit, db); err != nil {
		t.Fatal(err)
	}
	artifact, err := readArtifact(ctx, FileInput{Action: "Read", Path: written.DiffPath, Offset: 1, Limit: 2000}, db)
	if err != nil || artifact.Revision != "" || !strings.Contains(artifact.Text, "+first") {
		t.Fatal(artifact, err)
	}
}
func TestSQLiteInterruptedFileReceiptRecovery(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncertain", true: "applied"}[applied], func(t *testing.T) {
			db, err := storage.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			path := filepath.Join(t.TempDir(), "file")
			base := filepath.Join(filepath.Dir(db.Path), "operations", "s")
			input := FileInput{Action: "Write", Path: path, BaseDirectory: base, Revision: "missing", Content: "after"}
			encoded, _ := json.Marshal(input, json.Deterministic(true))
			receipt := fileReceipt{Version: 1, InputHash: fileHash(encoded), Before: "missing", After: fileHash([]byte("after")), Result: FileResult{Path: path}}
			data, _ := json.Marshal(receipt)
			if err = db.PutMetadata(t.Context(), "receipt:"+base, "op", data); err != nil {
				t.Fatal(err)
			}
			if applied {
				if err = os.WriteFile(path, []byte("after"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := executeFileStored(t.Context(), "op", input, db)
			if applied {
				if err != nil || !got.Recovered || !got.Applied {
					t.Fatal(got, err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "uncertain") {
					t.Fatal(err)
				}
				if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("repeated ambiguous edit")
				}
			}
		})
	}
}
func TestSQLiteArtifactReadsRejectMutationAndPreserveBinary(t *testing.T) {
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ref, err := db.Put(t.Context(), strings.NewReader("\xff\x00binary"))
	if err != nil {
		t.Fatal(err)
	}
	in := FileInput{Action: "Read", Path: ref, BaseDirectory: "/private", Offset: 1, Limit: 2}
	if _, err = NewFileSpec(in); err != nil {
		t.Fatal(err)
	}
	result, err := readArtifact(t.Context(), in, db)
	if err != nil || result.Encoding != "base64" || result.Text != "/wA=" || result.NextOffset != 3 {
		t.Fatal(result, err)
	}
	in.Action = "Write"
	in.Revision = "missing"
	if _, err = NewFileSpec(in); err == nil {
		t.Fatal("allowed artifact write")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	in.Action = "Read"
	if _, err = readArtifact(canceled, in, db); err == nil {
		t.Fatal("ignored cancellation")
	}
}
