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
	refs, err := s.sqlRefs(ctx, id, "SELECT payload FROM events WHERE session=? AND number<? ORDER BY number", id, count)
	if err != nil {
		return err
	}
	refs = append([]string{header}, refs...)
	if int64(len(refs)) != count {
		return fmt.Errorf("incomplete session event journal")
	}
	for _, ref := range refs {
		raw, err := s.database.JSON(ctx, ref)
		if err != nil {
			return err
		}
		if _, err = w.Write(append(raw, '\n')); err != nil {
			return err
		}
	}
	return nil
}
