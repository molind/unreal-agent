package localfile

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

// NewSQLite reuses the tested history validation/replay engine, but persists
// indexed immutable records and operation projections in a workspace database.
// New continues to open legacy JSONL stores for compatibility and migration.
func NewSQLite(directory string) (*Store, error) {
	db, err := storage.Open(directory)
	if err != nil {
		return nil, err
	}
	return &Store{directory: filepath.Dir(db.Path), database: db, writeStateCache: make(map[session.ID]cachedWriteState), observers: make(map[sessionstore.ObserverID]sessionstore.Observer)}, nil
}
func (s *Store) Database() *storage.DB { return s.database }
func (s *Store) Close() error {
	if s.database != nil {
		return s.database.Close()
	}
	return nil
}

func (s *Store) sqlCreate(state storedState) error {
	ctx := context.Background()
	encoded, err := encodeInitialLog(state.Snapshot.Session, state.Items)
	if err != nil {
		return err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE id=?", state.Snapshot.Session.ID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("create session: %w", os.ErrExist)
	}
	lines := bytes.Split(bytes.TrimSuffix(encoded, []byte{'\n'}), []byte{'\n'})
	for i, line := range lines {
		if err = s.sqlRecord(ctx, tx, state.Snapshot.Session.ID, int64(i), line); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.putCachedWriteState(state.Snapshot.Session.ID, state.sessionHead, int64(len(lines)))
	return nil
}

func (s *Store) sqlRecord(ctx context.Context, tx *sql.Tx, id session.ID, number int64, line []byte) error {
	// The JSONL delimiter belongs to export, not to the stored JSON value.
	line = bytes.TrimSuffix(line, []byte{'\n'})
	var record logRecord
	if err := json.Unmarshal(line, &record); err != nil {
		return err
	}
	payload, err := storage.PutJSON(ctx, tx, line)
	if err != nil {
		return err
	}
	if number == 0 {
		_, err = tx.ExecContext(ctx, "INSERT INTO sessions VALUES(?,?,1,?)", id, payload, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}
	var sequence any
	var ops []operation.Operation
	if record.Type == recordItem {
		var item itemRecord
		if err = json.Unmarshal(record.Data, &item); err != nil {
			return err
		}
		sequence = uint64(item.Item.Sequence)
		ops = item.Operations
	} else if record.Type == recordOperation {
		var op operationRecord
		if err = json.Unmarshal(record.Data, &op); err != nil {
			return err
		}
		ops = []operation.Operation{op.Operation}
	} else {
		return fmt.Errorf("invalid SQL record type %q", record.Type)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO events VALUES(?,?,?,?,?)", id, number, sequence, record.Type, payload); err != nil {
		return err
	}
	for _, op := range ops {
		raw, err := json.Marshal(op)
		if err != nil {
			return err
		}
		ref, err := storage.PutJSON(ctx, tx, raw)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO operations VALUES(?,?,?) ON CONFLICT(session,id) DO UPDATE SET payload=excluded.payload", id, op.ID, ref); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE sessions SET records=?,updated=? WHERE id=?", number+1, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func (s *Store) sqlAppend(id session.ID, head sessionHead, expected int64, kind recordType, value any) error {
	ctx := context.Background()
	encoded, err := encodeRecord(kind, value)
	if err != nil {
		return err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Optimistic concurrency check: never truncate another writer's history.
	result, err := tx.ExecContext(ctx, "UPDATE sessions SET records=records WHERE id=? AND records=?", id, expected)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("session changed by another writer; reopen before retrying")
	}
	if err = s.sqlRecord(ctx, tx, id, expected, encoded); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.putCachedWriteState(id, head, expected+1)
	return nil
}

func (s *Store) sqlHeader(ctx context.Context, id session.ID) (sessionstore.Snapshot, int64, error) {
	if err := validateSessionID(id); err != nil {
		return sessionstore.Snapshot{}, 0, err
	}
	var ref string
	var count int64
	err := s.database.QueryRowContext(ctx, "SELECT header,records FROM sessions WHERE id=?", id).Scan(&ref, &count)
	if errors.Is(err, sql.ErrNoRows) {
		err = os.ErrNotExist
	}
	if err != nil {
		return sessionstore.Snapshot{}, 0, err
	}
	raw, err := s.database.JSON(ctx, ref)
	if err != nil {
		return sessionstore.Snapshot{}, 0, err
	}
	var record logRecord
	var header sessionRecord
	if err = json.Unmarshal(raw, &record); err == nil {
		err = json.Unmarshal(record.Data, &header)
	}
	if err == nil && (header.Version != formatVersion || header.Session.ID != id) {
		err = fmt.Errorf("invalid SQLite session header")
	}
	return sessionstore.Snapshot{Session: header.Session}, count, err
}
func (s *Store) sqlRefs(ctx context.Context, id session.ID, query string, args ...any) ([]string, error) {
	rows, err := s.database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err = rows.Scan(&ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}
func (s *Store) sqlState(ctx context.Context, id session.ID) (storedState, int64, error) {
	header, count, records, opRefs, err := s.sqlStateSnapshot(ctx, id)
	if err != nil {
		return storedState{}, 0, err
	}
	// Only immutable artifact references are hydrated after releasing the read
	// transaction, avoiding nested queries on our single connection.
	encoded, err := s.database.JSON(ctx, header)
	if err != nil {
		return storedState{}, 0, err
	}
	encoded = append(encoded, '\n')
	for _, record := range records {
		raw, err := s.historyJSON(ctx, id, record)
		if err != nil {
			return storedState{}, 0, err
		}
		encoded = append(encoded, raw...)
		encoded = append(encoded, '\n')
	}
	for _, ref := range opRefs {
		raw, err := s.database.JSON(ctx, ref)
		if err != nil {
			return storedState{}, 0, err
		}
		var op operation.Operation
		if err = json.Unmarshal(raw, &op); err != nil {
			return storedState{}, 0, err
		}
		line, err := encodeRecord(recordOperation, operationRecord{Operation: op})
		if err != nil {
			return storedState{}, 0, err
		}
		encoded = append(encoded, line...)
	}
	state, _, err := decodeLog(encoded)
	if err == nil && state.Snapshot.Session.ID != id {
		err = fmt.Errorf("invalid SQLite session header")
	}
	return state, count, err
}
func (s *Store) sqlItems(ctx context.Context, id session.ID, after sessionstore.Sequence, limit int) (sessionstore.Page, error) {
	if _, _, err := s.sqlHeader(ctx, id); err != nil {
		return sessionstore.Page{}, err
	}
	if uint64(after) > uint64(1<<63-1) {
		return sessionstore.Page{NextAfter: after}, nil
	}
	queryLimit := limit
	if limit < int(^uint(0)>>1) {
		queryLimit++
	}
	records, err := s.sqlHistory(ctx, "SELECT number,payload FROM events WHERE session=? AND sequence>? ORDER BY sequence LIMIT ?", id, int64(after), queryLimit)
	if err != nil {
		return sessionstore.Page{}, err
	}
	page := sessionstore.Page{NextAfter: after, More: len(records) > limit}
	if page.More {
		records = records[:limit]
	}
	for _, record := range records {
		raw, err := s.historyJSON(ctx, id, record)
		if err != nil {
			return sessionstore.Page{}, err
		}
		var record logRecord
		var item itemRecord
		if err = json.Unmarshal(raw, &record); err == nil {
			err = json.Unmarshal(record.Data, &item)
		}
		if err != nil {
			return sessionstore.Page{}, err
		}
		if item.Item.Kind == sessionstore.ItemToolCallStatus {
			status := item.Item.Data.(sessionstore.ToolCallStatus)
			status.Operations = item.Operations
			item.Item.Data = status
		}
		page.Items = append(page.Items, item.Item)
		page.NextAfter = item.Item.Sequence
	}
	return page, nil
}
func (s *Store) sqlList(ctx context.Context) ([]sessionstore.SessionInfo, error) {
	rows, err := s.database.QueryContext(ctx, "SELECT id,updated FROM sessions ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []sessionstore.SessionInfo{}
	for rows.Next() {
		var info sessionstore.SessionInfo
		var stamp string
		if err = rows.Scan(&info.ID, &stamp); err != nil {
			return nil, err
		}
		info.LastUpdatedAt, err = time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return nil, err
		}
		result = append(result, info)
	}
	return result, rows.Err()
}

// Terminal captures are imported before the checkpoint, unlinked only after it.
// Only paths inside this store's own operation directory may be removed. Legacy
// captures are copied but retained, even after a successful migration.
func (s *Store) captureOperation(ctx context.Context, op operation.Operation, remove bool) error {
	if s.database == nil || !terminalOperationStatus(op.Status) || op.Type != operation.TypeShell {
		return nil
	}
	state, err := operation.DecodeShellState(op)
	if err != nil {
		return err
	}
	// An interrupted process may still own these descriptors. Never archive or
	// delete them until the process primitive has reported a joined exit/cancel.
	if state.Result == nil && !state.CapturesClosed {
		return nil
	}
	for _, path := range []string{state.OutPath, state.ErrPath} {
		if path == "" || storage.IsReference(path) {
			continue
		}
		rel, err := filepath.Rel(filepath.Join(s.directory, "operations"), path)
		if err != nil {
			return err
		}
		owned := filepath.IsLocal(rel)
		if remove {
			if owned {
				if err = s.database.RemoveCapture(ctx, path); err != nil {
					return err
				}
			}
			continue
		}
		if _, err = s.database.Capture(ctx, path); err != nil {
			// Failure before stdout/stderr creation is a valid terminal operation.
			if errors.Is(err, os.ErrNotExist) && state.Result == nil {
				continue
			}
			return err
		}
	}
	return nil
}
