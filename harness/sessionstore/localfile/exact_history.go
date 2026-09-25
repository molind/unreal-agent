package localfile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

type historyRecord struct {
	Number int64
	Ref    string
}

func (s *Store) sqlHistory(ctx context.Context, query string, args ...any) ([]historyRecord, error) {
	rows, err := s.database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []historyRecord
	for rows.Next() {
		var record historyRecord
		if err = rows.Scan(&record.Number, &record.Ref); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// historyJSON repairs the read view of old normalized imports, not the event
// journal itself. Exact originals must be independently authenticated by the
// import fingerprint and semantically equal to the current immutable event.
// New SQLite events after the imported prefix are never replaced by the archive.
func (s *Store) historyJSON(ctx context.Context, id session.ID, record historyRecord) ([]byte, error) {
	raw, exact, err := s.database.JSONExact(ctx, record.Ref)
	if err != nil || exact {
		return raw, err
	}
	original, err := s.originalJournal(ctx, id)
	if err != nil {
		return nil, err
	}
	if record.Number < 0 {
		return nil, errors.New("invalid history record number")
	}
	if record.Number >= int64(len(original)) {
		return raw, nil
	}
	before, err := canonicalMigrationJSON(original[record.Number])
	if err != nil {
		return nil, err
	}
	after, err := canonicalMigrationJSON(raw)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(before, after) {
		return nil, fmt.Errorf("archived session %q differs from stored history at record %d; refusing to replace it", id, record.Number)
	}
	return bytes.Clone(original[record.Number]), nil
}

func (s *Store) originalJournal(ctx context.Context, id session.ID) ([][]byte, error) {
	s.originalsMutex.Lock()
	defer s.originalsMutex.Unlock()
	if lines, ok := s.originals[id]; ok {
		return lines, nil
	}
	// Query only a known imported session journal, never arbitrary capture paths.
	// The caller closes rows before hydrating blobs on the shared SQL connection.
	rows, err := s.database.QueryContext(ctx, `SELECT i.source,i.digest,c.artifact
FROM imports i JOIN captures c ON c.path=i.source
WHERE substr(i.source, -length(?))=?`, "/"+sessionFilename(id), "/"+sessionFilename(id))
	if err != nil {
		return nil, err
	}
	type archived struct{ source, digest, ref string }
	var archives []archived
	for rows.Next() {
		var a archived
		if err = rows.Scan(&a.source, &a.digest, &a.ref); err != nil {
			break
		}
		archives = append(archives, a)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	var selected [][]byte
	var digest string
	for _, archive := range archives {
		if filepath.Base(archive.source) != sessionFilename(id) {
			continue
		}
		if digest != "" && digest != archive.digest {
			return nil, errors.New("conflicting original session archives")
		}
		if selected != nil {
			continue
		}
		raw, err := s.database.Bytes(ctx, archive.ref)
		if err != nil {
			return nil, err
		}
		if storage.Hash(raw) != archive.digest {
			return nil, errors.New("original session archive does not match its import fingerprint")
		}
		state, committed, err := decodeLog(raw)
		if err != nil {
			return nil, fmt.Errorf("validate original session archive: %w", err)
		}
		if state.Snapshot.Session.ID != id {
			return nil, errors.New("original session archive ID mismatch")
		}
		selected = bytes.Split(bytes.TrimSuffix(raw[:committed], []byte{'\n'}), []byte{'\n'})
		digest = archive.digest
	}
	// Do not cache absence: automatic migration may archive a copy-only import
	// later in this same store instance. Available original bytes are immutable.
	if selected != nil {
		if s.originals == nil {
			s.originals = make(map[session.ID][][]byte)
		}
		s.originals[id] = selected
	}
	return selected, nil
}
