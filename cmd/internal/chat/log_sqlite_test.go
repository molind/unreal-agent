package chat

import (
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func TestSQLiteLifecycleLogDeduplicatesAcrossRuns(t *testing.T) {
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	safe := func(s string) string { return s }
	spec, err := operation.NewFileSpec(operation.FileInput{Action: "Read", Path: "/workspace/source", BaseDirectory: "/private", Offset: 1, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	op := operation.Operation{ID: "op", Type: spec.Type, Version: spec.Version, Status: operation.StatusAwaiting, State: spec.State}
	first := openDatabaseLogs(db, safe)
	if err = first.command("s", op); err != nil {
		t.Fatal(err)
	}
	second := openDatabaseLogs(db, safe)
	if err = second.command("s", op); err != nil {
		t.Fatal(err)
	}
	op.Status = operation.StatusCompleted
	if err = second.command("s", op); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT payload FROM diagnostics ORDER BY number")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var records []logRecord
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			t.Fatal(err)
		}
		var r logRecord
		if err = json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Started.IsZero() || !records[0].Started.Equal(records[1].Started) {
		t.Fatal(records)
	}
}
