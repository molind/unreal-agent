package chat

import (
	"bytes"
	stdjson "encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func emulateNormalizedSQLite(t *testing.T, directory string, removeOriginal bool) {
	t.Helper()
	store, err := localfile.NewSQLite(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.Database()
	rows, err := db.Query("SELECT session,number,payload FROM events")
	if err != nil {
		t.Fatal(err)
	}
	type record struct {
		id     string
		number int64
		ref    string
	}
	var records []record
	for rows.Next() {
		var r record
		if err = rows.Scan(&r.id, &r.number, &r.ref); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	for _, r := range records {
		raw, err := db.JSON(t.Context(), r.ref)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		decoder := stdjson.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err = decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		encoded, err := stdjson.Marshal(map[string]any{"value": value})
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		ref, _, err := storage.PutTx(t.Context(), tx, bytes.NewReader(encoded))
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if _, err = tx.Exec("UPDATE events SET payload=? WHERE session=? AND number=?", ref, r.id, r.number); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if removeOriginal {
		if _, err = db.Exec("DELETE FROM captures WHERE path LIKE '%.session.jsonl'"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSQLiteCompactionReopenAndOldCodecRecovery(t *testing.T) {
	for _, mode := range []string{"exact", "old-import-with-original", "old-without-original"} {
		t.Run(mode, func(t *testing.T) {
			workspace := t.TempDir()
			directory := filepath.Join(workspace, "state")
			seedContextChat(t, workspace) // Includes nonalphabetic opaque provider JSON.
			args := []string{"-storage-format", "sqlite", "-session-directory", directory, "-session", "context-case"}
			c := launch(t, workspace, false, args...)
			c.send("continue the next task")
			c.call().failure <- overflowError()
			summary := "VERIFIED-HANDOFF: original tasks done; keep all constraints."
			summaryCall(t, c).reply <- reply(summary)
			call := c.call()
			call.reply <- reply("done before restart")
			c.wait("done before restart")
			c.finish("/exit")
			if mode != "exact" {
				emulateNormalizedSQLite(t, directory, mode == "old-without-original")
			}
			c = launch(t, workspace, false, args...)
			c.send("after restart")
			call = c.call()
			user := strings.Join(messages(call.request, llm.RoleUser), "\n")
			if mode == "old-without-original" {
				c.wait("History recovery:")
				if !strings.Contains(user, "old-task-0") || strings.Contains(user, summary) {
					t.Fatal("stale summary applied or original context dropped")
				}
			} else if !strings.Contains(user, summary) || strings.Contains(c.output.snapshot(), "History recovery:") {
				t.Fatal("valid saved checkpoint was not reused")
			}
			call.reply <- reply("reopened successfully")
			c.wait("reopened successfully")
			c.finish("/exit")
		})
	}
}
