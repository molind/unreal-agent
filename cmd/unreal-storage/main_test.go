package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestStorageCLIWithoutProviderAndSafeExports(t *testing.T) {
	work := t.TempDir()
	source := t.TempDir()
	directory := filepath.Join(work, "state")
	legacy, err := localfile.New(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Create(t.Context(), "s"); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal("hello history")
	if err = legacy.AppendInput(t.Context(), "s", inbox.Input{ID: "i", Kind: inbox.InputExternal, Payload: data}); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) (string, error) {
		var out bytes.Buffer
		all := append([]string{"-workspace", work, "-session-directory", directory}, args...)
		err := run(t.Context(), all, func(string) string { return "" }, &out, io.Discard)
		return out.String(), err
	}
	if _, err = invoke("path"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("path command created database")
	}
	if _, err = invoke("-keep-source", "migrate", source); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(source, "s.session.jsonl")); err != nil {
		t.Fatal("keep-source removed journal", err)
	}
	if _, err = invoke("migrate", source); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(source); !os.IsNotExist(err) {
		t.Fatal("default migration did not clean source", err)
	}
	if _, err = invoke("migrate", source); err != nil {
		t.Fatal("cleanup retry failed", err)
	}
	if got, err := invoke("history", "s"); err != nil || !strings.Contains(got, "hello history") {
		t.Fatal(got, err)
	}
	db, err := localfile.NewSQLite(directory)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := db.Database().Put(t.Context(), strings.NewReader("artifact bytes"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if got, err := invoke("read", ref); err != nil || got != "artifact bytes" {
		t.Fatal(got, err)
	}
	target := filepath.Join(work, "export.txt")
	if _, err = invoke("export", ref, target); err != nil {
		t.Fatal(err)
	}
	if _, err = invoke("export", ref, target); !errors.Is(err, os.ErrExist) {
		t.Fatal("overwrote export", err)
	}
	if got, err := invoke("check"); err != nil || got != "ok\n" {
		t.Fatal(got, err)
	}
	if _, err = invoke("backup", filepath.Join(work, "backup.sqlite3")); err != nil {
		t.Fatal(err)
	}
}
