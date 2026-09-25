package localfile

import (
	"context"
	"database/sql"
	"errors"
	"os"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

// Capture all mutable projections in one WAL snapshot. No SQL transaction is
// held while decompressing artifacts, invoking observers or doing external I/O.
func (s *Store) sqlStateSnapshot(ctx context.Context, id session.ID) (header string, count int64, records []historyRecord, operations []string, err error) {
	tx, err := s.database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "SELECT header,records FROM sessions WHERE id=?", id).Scan(&header, &count); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = os.ErrNotExist
		}
		return
	}
	rows, err := tx.QueryContext(ctx, "SELECT number,payload FROM events WHERE session=? AND kind='item' ORDER BY number", id)
	if err != nil {
		return
	}
	for rows.Next() {
		var record historyRecord
		if err = rows.Scan(&record.Number, &record.Ref); err != nil {
			break
		}
		records = append(records, record)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return
	}
	rows, err = tx.QueryContext(ctx, "SELECT payload FROM operations WHERE session=? ORDER BY id", id)
	if err != nil {
		return
	}
	for rows.Next() {
		var ref string
		if err = rows.Scan(&ref); err != nil {
			break
		}
		operations = append(operations, ref)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err == nil {
		err = tx.Commit()
	}
	return
}
