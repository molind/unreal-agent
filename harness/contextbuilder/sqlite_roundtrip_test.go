package contextbuilder

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func TestSQLiteRoundTripPreservesCompactionPrefixHash(t *testing.T) {
	// Synthetic opaque provider state, deliberately not alphabetically ordered.
	// Never use real provider state or conversation contents in test diagnostics.
	items := []llm.Item{{Type: llm.ItemReasoning, Data: llm.Reasoning{Raw: jsontext.Value(`{"type":"reasoning","id":"fixture","summary":[],"encrypted_content":"opaque-fixture"}`)}}}
	before, err := prefixHash(items)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ref, err := storage.PutJSON(t.Context(), tx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	restored, err := db.JSON(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	var replay []llm.Item
	if err = json.Unmarshal(restored, &replay); err != nil {
		t.Fatal(err)
	}
	after, err := prefixHash(replay)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("SQLite JSON round-trip changed the compaction prefix hash")
	}
}
