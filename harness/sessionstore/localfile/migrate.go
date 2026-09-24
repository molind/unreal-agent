package localfile

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

// ImportLegacy is the non-destructive, offline import primitive. Callers must
// exclude source writers; MigrateLegacy and the CLI also provide directory locks
// and checks for pre-lock binaries. Source files are never changed or deleted,
// including torn trailing records. Each history and its fingerprint commit
// together; retries never duplicate history.
func (s *Store) ImportLegacy(ctx context.Context, directory string) error {
	if s.database == nil {
		return errors.New("migration requires SQLite destination")
	}
	source, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), sessionFileSuffix)
		if !ok {
			continue
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("legacy session is not regular: %s", entry.Name())
		}
		if err = validateSessionID(session.ID(name)); err != nil {
			return err
		}
		path := filepath.Join(source, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		state, committed, err := decodeLog(data)
		if err != nil {
			return fmt.Errorf("migrate %s: %w", entry.Name(), err)
		}
		if string(state.Snapshot.Session.ID) != name {
			return fmt.Errorf("legacy session filename/header mismatch")
		}
		digest := storage.Hash(data)
		var previous string
		err = s.database.QueryRowContext(ctx, "SELECT digest FROM imports WHERE source=?", path).Scan(&previous)
		if err == nil {
			if previous != digest {
				return fmt.Errorf("legacy source changed after import: %s; do not mix legacy and SQLite writers", path)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// Import only generated auxiliary files under this session's private tree.
		// Receipts must be available before an interrupted file action can resume.
		if err = s.importAuxiliary(ctx, source, name); err != nil {
			return err
		}
		tx, err := s.database.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		err = func() error {
			defer tx.Rollback()
			var existing int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE id=?", name).Scan(&existing); err != nil {
				return err
			}
			if existing != 0 {
				return fmt.Errorf("session %s already exists in destination; refusing overwrite", name)
			}
			lines := bytes.Split(bytes.TrimSuffix(data[:committed], []byte{'\n'}), []byte{'\n'})
			for i, line := range lines {
				if err := s.sqlRecord(ctx, tx, session.ID(name), int64(i), line); err != nil {
					return err
				}
			}
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			current, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if storage.Hash(current) != digest {
				return errors.New("legacy session changed during migration; stop its writer first")
			}
			if _, err = tx.ExecContext(ctx, "UPDATE sessions SET updated=? WHERE id=?", info.ModTime().UTC().Format(time.RFC3339Nano), name); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO imports VALUES(?,?)", path, digest); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			return err
		}
		// Verify the imported history using the same decoder before reporting success.
		restored, _, err := s.sqlState(ctx, session.ID(name))
		if err != nil {
			return err
		}
		if len(restored.Items) != len(state.Items) {
			return errors.New("migration verification failed")
		}
	}
	return s.importLegacyLogs(ctx, source)
}

func (s *Store) importAuxiliary(ctx context.Context, source, id string) error {
	oldBase := filepath.Join(source, "operations", id)
	newBase := filepath.Join(s.directory, "operations", id)
	err := filepath.WalkDir(oldBase, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) && path == oldBase {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in legacy operation storage: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("nonregular legacy operation file: %s", path)
		}
		rel, err := filepath.Rel(oldBase, path)
		if err != nil {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 2 || !knownMigrationFile([]string{"operations", id, parts[0], parts[1]}, map[string]bool{id: true}) {
			return nil
		}
		if parts[0] == "revisions" && strings.HasSuffix(parts[1], ".json") {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var revision struct {
				Version      int
				Path, SHA256 string
			}
			if err = json.Unmarshal(data, &revision, json.RejectUnknownMembers(true)); err != nil {
				return err
			}
			token := strings.TrimSuffix(parts[1], ".json")
			if revision.Version != 1 || !filepath.IsAbs(revision.Path) || len(revision.SHA256) != 64 || len(token) != 16 {
				return errors.New("invalid legacy revision")
			}
			for _, base := range []string{oldBase, newBase} {
				if err = s.importMetadata(ctx, "revision:"+base, token, data); err != nil {
					return err
				}
			}
		} else if parts[1] == "transaction.json" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var receipt map[string]any
			if err = json.Unmarshal(data, &receipt); err != nil {
				return err
			}
			if err = s.importMetadata(ctx, "receipt:"+oldBase, parts[0], data); err != nil {
				return err
			}
		} else if parts[1] == "change.diff" || parts[1] == "out" || parts[1] == "err" {
			if err = s.database.RetainCapture(ctx, path); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

func (s *Store) importLegacyLogs(ctx context.Context, source string) error {
	// Preserve redacted diagnostic/command JSONL as artifacts with path aliases.
	// No transcript or credential discovery is performed here.
	logs := filepath.Join(source, "logs")
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	sessions := map[string]bool{}
	for _, entry := range entries {
		if id, ok := strings.CutSuffix(entry.Name(), sessionFileSuffix); ok {
			sessions[id] = true
		}
	}
	return filepath.WalkDir(logs, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) && path == logs {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in legacy logs")
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Type().IsRegular() && knownMigrationFile(strings.Split(rel, string(filepath.Separator)), sessions) {
			return s.database.RetainCapture(ctx, path)
		}
		return nil
	})
}
func (s *Store) importMetadata(ctx context.Context, namespace, key string, data []byte) error {
	old, err := s.database.GetMetadata(ctx, namespace, key)
	if err == nil {
		if !bytes.Equal(old, data) {
			return errors.New("legacy metadata conflicts with destination")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.database.PutMetadata(ctx, namespace, key, data)
}
