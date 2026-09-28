package localfile

import (
	"context"
	"errors"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

// DeleteSession removes the session's indexed history, operation checkpoints and
// diagnostics. It acquires the runtime lease; callers must close their owner
// first. Immutable shared artifacts and backups are retained (not secure erasure).
// An absent session is a success, so a lost HTTP acknowledgement can be retried.
func (s *Store) DeleteSession(ctx context.Context, id session.ID) error {
	if s.database == nil {
		return errors.New("deleting sessions requires SQLite storage")
	}
	release, err := s.LockSession(id)
	if err != nil {
		return err
	}
	defer release()
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{
		"DELETE FROM diagnostics WHERE session=?",
		"DELETE FROM operations WHERE session=?",
		"DELETE FROM events WHERE session=?",
		"DELETE FROM sessions WHERE id=?",
	} {
		if _, err = tx.ExecContext(ctx, query, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
