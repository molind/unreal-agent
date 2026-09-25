package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}
func TestArtifactsRangesDedupCompressionAndCorruption(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	raw := bytes.Repeat([]byte("hello Belarus!\n"), 40000)
	ref, err := db.Put(ctx, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	again, err := db.Put(ctx, bytes.NewReader(raw))
	if err != nil || ref != again {
		t.Fatal(ref, again, err)
	}
	for _, offset := range []int64{0, 1, ChunkSize - 2, ChunkSize, int64(len(raw)) - 9, int64(len(raw)), int64(len(raw)) + 1} {
		data, size, err := db.Read(ctx, ref, offset, 25)
		if err != nil || size != int64(len(raw)) {
			t.Fatal(size, err)
		}
		start := min(offset, int64(len(raw)))
		if !bytes.Equal(data, raw[start:min(start+25, int64(len(raw)))]) {
			t.Fatal("range", offset)
		}
	}
	var saved int
	if err = db.QueryRow("SELECT sum(length(data)) FROM chunks").Scan(&saved); err != nil || saved >= len(raw)/10 {
		t.Fatal("not compressed", saved, err)
	}
	if err = db.Check(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE chunks SET data=x'00'")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = db.Read(ctx, ref, 0, 5); err == nil {
		t.Fatal("accepted corrupt chunk")
	}
}
func TestJSONManifestPreservesNumbersAndUserObjects(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	text := strings.Repeat("abc", 10000)
	data := []byte(fmt.Sprintf(`{"a/b~c":%q,"number":18446744073709551615,"decimal":1.234567890123456789,"user":{"$blob":"untrusted"},"array":[null,%q]}`, text, text))
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := PutJSON(ctx, tx, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := db.JSON(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	var want, actual any
	for _, pair := range []struct {
		data []byte
		to   *any
	}{{data, &want}, {got, &actual}} {
		dec := json.NewDecoder(bytes.NewReader(pair.data))
		dec.UseNumber()
		if err = dec.Decode(pair.to); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(actual)
	if !bytes.Equal(a, b) {
		t.Fatal("JSON changed")
	}
	if !bytes.Equal(got, data) {
		t.Fatal("JSON representation changed")
	}
	encodedText, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRow("SELECT count(*) FROM artifacts WHERE id=?", Hash(encodedText)).Scan(&n); err != nil || n != 1 {
		t.Fatal("missing shared string", n, err)
	}
}
func TestCaptureCommitBeforeUnlinkAndRetry(t *testing.T) {
	db := testDB(t)
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "out")
	raw := []byte("complete command output")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := db.Capture(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("import removed uncheckpointed source", err)
	}
	again, err := db.Capture(ctx, path)
	if err != nil || again != ref {
		t.Fatal(again, err)
	}
	if err = db.RemoveCapture(ctx, path); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	again, err = db.Capture(ctx, path)
	if err != nil || again != ref {
		t.Fatal("retry after unlink", again, err)
	}
	got, err := db.Bytes(ctx, ref)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal(got, err)
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = db.RemoveCapture(ctx, path); err == nil {
		t.Fatal("removed changed source")
	}
	if _, err = db.Capture(ctx, path); err == nil {
		t.Fatal("rebound stable capture")
	}
}
func TestCanceledPutRollbackAndBackup(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := db.Put(ctx, strings.NewReader("never committed")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM artifacts").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	ref, err := db.Put(t.Context(), strings.NewReader("committed in WAL"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), Filename)
	if err = db.Backup(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if err = db.Backup(t.Context(), target); !errors.Is(err, os.ErrExist) {
		t.Fatal("overwrote backup", err)
	}
	copy, err := Open(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	data, err := copy.Bytes(t.Context(), ref)
	if err != nil || string(data) != "committed in WAL" {
		t.Fatal(string(data), err)
	}
}
func TestDirectoriesIdentityWriterAndPermissions(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	env := func(name string) string {
		if name == "HOME" {
			return home
		}
		return "relative"
	}
	dir, err := Directory(workspace, env)
	if err != nil || !strings.HasPrefix(dir, filepath.Join(home, ".local", "state")) {
		t.Fatal(dir, err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err = os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	other, err := Directory(alias, env)
	if err != nil || other != dir {
		t.Fatal(other, err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.BindWorkspace(t.Context(), workspace); err != nil {
		t.Fatal(err)
	}
	if err = db.BindWorkspace(t.Context(), t.TempDir()); err == nil {
		t.Fatal("accepted wrong workspace")
	}
	release, err := db.LockWriter()
	if err != nil {
		t.Fatal(err)
	}
	if second, err := db.LockWriter(); err == nil {
		_ = second()
		t.Fatal("accepted second writer")
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{db.Path, db.Path + "-wal", db.Path + "-shm"} {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(path, info, err)
		}
	}
}
func TestUnknownSchemaAndSymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("PRAGMA user_version=999"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if db, err = Open(dir); err == nil {
		_ = db.Close()
		t.Fatal("opened future schema")
	}
	linkDir := t.TempDir()
	if err = os.Symlink(filepath.Join(dir, Filename), filepath.Join(linkDir, Filename)); err != nil {
		t.Fatal(err)
	}
	if db, err = Open(linkDir); err == nil {
		_ = db.Close()
		t.Fatal("opened symlink")
	}
}
