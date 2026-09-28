package localfile

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

const sessionTitleNamespace = "session-title"
const MaxSessionTitleLength = 200

var ErrInvalidSessionTitle = errors.New("session title must be at most 200 characters without control characters")

// SetSessionTitle stores display-only metadata, never a model input or history
// event. Empty/whitespace-only titles restore the automatically derived title.
// No runtime lease is needed: the SQLite transaction serializes with deletion
// and history writes, and does not change session timestamps or record counts.
func (s *Store) SetSessionTitle(ctx context.Context, id session.ID, title string) error {
	if s.database == nil {
		return errors.New("session titles require SQLite storage")
	}
	if err := validateSessionID(id); err != nil {
		return err
	}
	if !utf8.ValidString(title) {
		return ErrInvalidSessionTitle
	}
	for _, r := range title {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return ErrInvalidSessionTitle
		}
	}
	title = strings.Join(strings.Fields(title), " ")
	if utf8.RuneCountInString(title) > MaxSessionTitleLength {
		return ErrInvalidSessionTitle
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id=?", id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return os.ErrNotExist
		}
		return err
	}
	if title == "" {
		_, err = tx.ExecContext(ctx, "DELETE FROM metadata WHERE namespace=? AND key=?", sessionTitleNamespace, id)
	} else {
		_, err = tx.ExecContext(ctx, "INSERT INTO metadata(namespace,key,value) VALUES(?,?,?) ON CONFLICT(namespace,key) DO UPDATE SET value=excluded.value", sessionTitleNamespace, id, []byte(title))
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SessionTitles reads overrides in one query, independently of the cached
// history summary. A rename must be visible even if no history has changed.
func (s *Store) SessionTitles(ctx context.Context) (map[session.ID]string, error) {
	if s.database == nil {
		return nil, errors.New("session titles require SQLite storage")
	}
	rows, err := s.database.QueryContext(ctx, "SELECT m.key,m.value FROM metadata m JOIN sessions s ON s.id=m.key WHERE m.namespace=?", sessionTitleNamespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	titles := make(map[session.ID]string)
	for rows.Next() {
		var id session.ID
		var title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		titles[id] = title
	}
	return titles, rows.Err()
}
