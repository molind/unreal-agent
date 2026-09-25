package localfile

import (
	"bytes"
	stdjson "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

// Emulate the released format-1 writer without retaining that writer in
// production. Fixtures are entirely synthetic; never print raw provider state.
func normalizeHistoryFixture(t *testing.T, s *Store, id session.ID) {
	t.Helper()
	records, err := s.sqlHistory(t.Context(), "SELECT number,payload FROM events WHERE session=? ORDER BY number", id)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		raw, err := s.database.JSON(t.Context(), record.Ref)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		decoder := stdjson.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err = decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		encoded, err := stdjson.Marshal(struct {
			Value any `json:"value"`
		}{value})
		if err != nil {
			t.Fatal(err)
		}
		tx, err := s.database.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		ref, _, err := storage.PutTx(t.Context(), tx, bytes.NewReader(encoded))
		if err == nil {
			_, err = tx.Exec("UPDATE events SET payload=? WHERE session=? AND number=?", ref, id, record.Number)
		}
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOldNormalizedImportReadsVerifiedOriginalWithoutRewritingEvents(t *testing.T) {
	source, old, dest := legacyFixture(t)
	if err := old.AppendTurn(t.Context(), "old", session.Turn{ID: "turn"}); err != nil {
		t.Fatal(err)
	}
	state := jsontext.Value(`{"type":"reasoning","id":"fixture","summary":[],"encrypted_content":"opaque-fixture"}`)
	response := sessionstore.ModelResponse{TurnID: "turn", Response: llm.Response{Output: []llm.Item{{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: state}}}}}
	if err := old.AppendModelResponse(t.Context(), "old", response); err != nil {
		t.Fatal(err)
	}
	if err := dest.ImportLegacy(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "old.session.jsonl")
	if err := dest.database.RetainCapture(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	normalizeHistoryFixture(t, dest, "old")
	if err := dest.AppendInput(t.Context(), "old", sqlInput("tail", "new SQLite work")); err != nil {
		t.Fatal(err)
	}
	before, err := dest.sqlRefs(t.Context(), "old", "SELECT payload FROM events WHERE session=? ORDER BY number", "old")
	if err != nil {
		t.Fatal(err)
	}
	page, err := dest.Items(t.Context(), "old", 0, 100)
	if err != nil || len(page.Items) != 4 {
		t.Fatal("read failed", err)
	}
	restored := page.Items[2].Data.(sessionstore.ModelResponse).Response.Output[0].Data.(llm.Reasoning).Raw
	if !bytes.Equal(restored, state) {
		t.Fatal("original provider representation not restored")
	}
	if _, err = dest.Resume(t.Context(), "old"); err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if err = dest.ExportJSONL(t.Context(), "old", &exported); err != nil {
		t.Fatal(err)
	}
	decoded, _, err := decodeLog(exported.Bytes())
	if err != nil || len(decoded.Items) != 4 {
		t.Fatal(err)
	}
	exportedState := decoded.Items[2].Data.(sessionstore.ModelResponse).Response.Output[0].Data.(llm.Reasoning).Raw
	if !bytes.Equal(exportedState, state) {
		t.Fatal("export changed original state")
	}
	after, err := dest.sqlRefs(t.Context(), "old", "SELECT payload FROM events WHERE session=? ORDER BY number", "old")
	if err != nil {
		t.Fatal(err)
	}
	x, _ := json.Marshal(before)
	y, _ := json.Marshal(after)
	if !bytes.Equal(x, y) {
		t.Fatal("read recovery rewrote immutable events")
	}
}

func TestOriginalReadViewRejectsCorruptArchive(t *testing.T) {
	source, _, dest := legacyFixture(t)
	if err := dest.ImportLegacy(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "old.session.jsonl")
	if err := dest.database.RetainCapture(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	normalizeHistoryFixture(t, dest, "old")
	ref, err := dest.database.Put(t.Context(), bytes.NewReader([]byte("not the imported journal")))
	if err != nil {
		t.Fatal(err)
	}
	// A valid artifact with the wrong content must not be treated as an absent
	// original and silently bypassed by the stale-summary fallback.
	if _, err = dest.database.Exec("UPDATE captures SET artifact=? WHERE path=?", ref[len("artifact:"):], path); err != nil {
		t.Fatal(err)
	}
	if _, err = dest.Items(t.Context(), "old", 0, 100); err == nil {
		t.Fatal("ignored archive fingerprint mismatch")
	}
}

func TestOriginalReadViewRejectsDifferentStoredHistory(t *testing.T) {
	source, _, dest := legacyFixture(t)
	if err := dest.ImportLegacy(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if err := dest.database.RetainCapture(t.Context(), filepath.Join(source, "old.session.jsonl")); err != nil {
		t.Fatal(err)
	}
	normalizeHistoryFixture(t, dest, "old")
	rows, err := dest.sqlHistory(t.Context(), "SELECT number,payload FROM events WHERE session=?", "old")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := dest.database.JSON(t.Context(), rows[0].Ref)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte("retain original history"), []byte("different canonical history"), 1)
	var value any
	if err = stdjson.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	encoded, _ := stdjson.Marshal(map[string]any{"value": value})
	tx, err := dest.database.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := storage.PutTx(t.Context(), tx, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE events SET payload=? WHERE session='old'", ref); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = dest.Items(t.Context(), "old", 0, 100); err == nil {
		t.Fatal("replaced a changed event with an old archive")
	}
}
