package chat

import (
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func TestSQLiteChatEndToEndArtifactsFilesResumeAndLogs(t *testing.T) {
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "state")
	c := launch(t, workspace, false, "-storage-format", "sqlite", "-session-directory", directory)
	c.send("run a command and edit a file")
	call := c.call()
	id := sessionID(t, c.output.snapshot())
	call.reply <- fileCall("bash", "Bash", map[string]any{"command": "printf 'a long captured output for testing\\n'", "max_output_length": 8})
	call = c.call()
	var text string
	for _, item := range call.request.Input {
		if r, ok := item.Data.(llm.ToolResult); ok && r.CallID == "bash" {
			text = r.Output[0].Value
		}
	}
	ref := regexp.MustCompile(`capture:[0-9a-f]{64}`).FindString(text)
	if ref == "" {
		t.Fatal("no artifact reference", text)
	}
	call.reply <- fileCall("capture-read", "Read", map[string]any{"path": ref, "offset": 1, "limit": 2000})
	call = c.call()
	capture := fileResult(t, call.request, "capture-read")
	if capture.Text != "a long captured output for testing\n" || capture.Unit != "bytes" {
		t.Fatal(capture)
	}
	call.reply <- fileCall("write", "Write", map[string]any{"path": "source.txt", "revision": "missing", "content": "hello\n"})
	call = c.call()
	written := fileResult(t, call.request, "write")
	if !written.Applied || !strings.HasPrefix(written.DiffPath, "artifact:") {
		t.Fatal(written)
	}
	call.reply <- reply("SQLite work done")
	c.wait("SQLite work done")
	c.finish("/exit")
	// There are no per-command logs, revision JSON, diff or capture files at rest.
	var files []string
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files = append(files, entry.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if name != storage.Filename && name != storage.Filename+"-wal" && name != storage.Filename+"-shm" && name != "writer.lock" {
			t.Fatal("loose session file", name)
		}
	}
	store, err := localfile.NewSQLite(directory)
	if err != nil {
		t.Fatal(err)
	}
	data, err := store.Database().Bytes(t.Context(), ref)
	if err != nil || string(data) != capture.Text {
		t.Fatal(string(data), err)
	}
	var logs int
	if err = store.Database().QueryRow("SELECT count(*) FROM diagnostics WHERE session=?", id).Scan(&logs); err != nil || logs == 0 {
		t.Fatal(logs, err)
	}
	_ = store.Close()
	resumed := launch(t, workspace, false, "-storage-format", "sqlite", "-session-directory", directory, "-session", id)
	resumed.wait("SQLite work done")
	resumed.send("next message")
	next := resumed.call()
	if len(messages(next.request, llm.RoleUser)) != 2 {
		t.Fatal("lost history")
	}
	next.reply <- fileCall("edit", "Edit", map[string]any{"path": "source.txt", "revision": written.Revision, "old_text": "hello", "new_text": "world"})
	next = resumed.call()
	if result := fileResult(t, next.request, "edit"); !result.Applied {
		t.Fatal("revision lost across reopen", result)
	}
	next.reply <- reply("resumed done")
	resumed.wait("resumed done")
	resumed.finish("/exit")
}
func TestSQLiteConfigAndLegacyMigrationGuard(t *testing.T) {
	workspace := t.TempDir()
	state := t.TempDir()
	env := func(name string) string {
		if name == "XDG_STATE_HOME" {
			return state
		}
		return ""
	}
	c, err := parse([]string{workspace}, env, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want, err := storage.Directory(workspace, env)
	if err != nil || c.directory != want || c.storageFormat != "sqlite" {
		t.Fatal(c, err)
	}
	source := filepath.Join(workspace, ".harness", "sessions")
	legacy, err := localfile.New(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Create(t.Context(), "old"); err != nil {
		t.Fatal(err)
	}
	if s, release, err := openStorage(t.Context(), c); err == nil {
		_ = s.Close()
		_ = release()
		t.Fatal("silently stranded legacy history")
	}
	s, err := localfile.NewSQLite(want)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ImportLegacy(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	opened, release, err := openStorage(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer opened.Close()
	if _, err = opened.Resume(t.Context(), session.ID("old")); err != nil {
		t.Fatal(err)
	}
}
func TestSQLiteDiagnosticsAreRedacted(t *testing.T) {
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	redact := func(text string) string { return strings.ReplaceAll(text, "secret", "[redacted]") }
	l := openDatabaseLogs(db, redact)
	if err = l.event("test", "failure", "s", "", io.ErrUnexpectedEOF); err != nil {
		t.Fatal(err)
	}
	if err = l.write(nil, logRecord{Stage: "test", Command: "secret"}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(context.Background(), "SELECT payload FROM diagnostics ORDER BY number")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "secret") {
			t.Fatal("unredacted log")
		}
		var record logRecord
		if err = json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}
