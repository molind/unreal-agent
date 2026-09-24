package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type diskFailureReader struct{}

func (diskFailureReader) Read([]byte) (int, error) { return 0, os.ErrPermission }
func TestArtifactReaderFailureRollsBackAllChunks(t *testing.T) {
	db := testDB(t)
	_, err := db.Put(t.Context(), io.MultiReader(strings.NewReader(strings.Repeat("x", ChunkSize*2)), diskFailureReader{}))
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	for _, table := range []string{"chunks", "artifacts", "artifact_chunks"} {
		var count int
		if err = db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
}
func TestDatabaseURIAndReplacementConnectionPragmas(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "space ?#state")
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = os.Stat(filepath.Join(dir, Filename)); err != nil {
		t.Fatal(err)
	}
	// Force the next query to use a replacement connection, as can happen after
	// cancellation or an underlying connection failure.
	db.SetMaxIdleConns(0)
	for _, pragma := range []struct {
		name string
		want int
	}{{"foreign_keys", 1}, {"synchronous", 2}, {"busy_timeout", 5000}} {
		var got int
		if err = db.QueryRowContext(context.Background(), "PRAGMA "+pragma.name).Scan(&got); err != nil || got != pragma.want {
			t.Fatal(pragma.name, got, err)
		}
	}
}
