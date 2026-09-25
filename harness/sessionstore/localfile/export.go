package localfile

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

func (s *Store) ExportJSONL(ctx context.Context, id session.ID, w io.Writer) error {
	if err := validateSessionID(id); err != nil {
		return err
	}
	if s.database == nil {
		f, err := os.Open(s.sessionPath(id))
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	}
	// Fix the high-water mark before fetching immutable rows, so concurrent
	// appends cannot produce a backup with a changing tail.
	_, count, err := s.sqlHeader(ctx, id)
	if err != nil {
		return err
	}
	var header string
	if err = s.database.QueryRowContext(ctx, "SELECT header FROM sessions WHERE id=?", id).Scan(&header); err != nil {
		return err
	}
	records, err := s.sqlHistory(ctx, "SELECT number,payload FROM events WHERE session=? AND number<? ORDER BY number", id, count)
	if err != nil {
		return err
	}
	records = append([]historyRecord{{Number: 0, Ref: header}}, records...)
	if int64(len(records)) != count {
		return fmt.Errorf("incomplete session event journal")
	}
	for _, record := range records {
		raw, err := s.historyJSON(ctx, id, record)
		if err != nil {
			return err
		}
		if _, err = w.Write(append(raw, '\n')); err != nil {
			return err
		}
	}
	return nil
}
