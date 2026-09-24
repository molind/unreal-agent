// Package storage owns the workspace SQLite database and immutable artifacts.
// It has no dependency on coordinator, operation, or session state machines.
package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

const Filename = "state.sqlite3"
const schemaVersion = 1

type DB struct {
	*sql.DB
	Path string
}

const schema = `
CREATE TABLE IF NOT EXISTS artifacts(id TEXT PRIMARY KEY, size INTEGER NOT NULL CHECK(size>=0));
CREATE TABLE IF NOT EXISTS chunks(id TEXT PRIMARY KEY, codec TEXT NOT NULL, size INTEGER NOT NULL, data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS artifact_chunks(artifact TEXT NOT NULL REFERENCES artifacts(id), offset INTEGER NOT NULL, chunk TEXT NOT NULL REFERENCES chunks(id), PRIMARY KEY(artifact,offset));
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY, header TEXT NOT NULL REFERENCES artifacts(id), records INTEGER NOT NULL, updated TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS events(session TEXT NOT NULL REFERENCES sessions(id), number INTEGER NOT NULL, sequence INTEGER, kind TEXT NOT NULL, payload TEXT NOT NULL REFERENCES artifacts(id), PRIMARY KEY(session,number));
CREATE UNIQUE INDEX IF NOT EXISTS events_sequence ON events(session,sequence) WHERE sequence IS NOT NULL;
CREATE TABLE IF NOT EXISTS operations(session TEXT NOT NULL REFERENCES sessions(id), id TEXT NOT NULL, payload TEXT NOT NULL REFERENCES artifacts(id), PRIMARY KEY(session,id));
CREATE TABLE IF NOT EXISTS metadata(namespace TEXT NOT NULL, key TEXT NOT NULL, value BLOB NOT NULL, PRIMARY KEY(namespace,key));
CREATE TABLE IF NOT EXISTS captures(path TEXT PRIMARY KEY, ref TEXT NOT NULL UNIQUE, artifact TEXT NOT NULL REFERENCES artifacts(id), size INTEGER NOT NULL, retained INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS diagnostics(number INTEGER PRIMARY KEY, session TEXT NOT NULL, operation TEXT NOT NULL, run TEXT NOT NULL, payload BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS diagnostic_session ON diagnostics(session,number);
CREATE INDEX IF NOT EXISTS diagnostic_operation ON diagnostics(session,operation,number);
CREATE TABLE IF NOT EXISTS imports(source TEXT PRIMARY KEY, digest TEXT NOT NULL);
PRAGMA user_version=1;
`

func Open(directory string) (*DB, error) {
	if strings.TrimSpace(directory) == "" {
		return nil, errors.New("storage directory is empty")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(absolute, Filename)
	// Refuse symlink/nonregular database and sidecars, and create the main file
	// privately before SQLite creates its journals (which inherit its mode).
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		st, e := os.Lstat(p)
		if e == nil && !st.Mode().IsRegular() {
			return nil, fmt.Errorf("not a regular database file: %s", p)
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	err = errors.Join(f.Chmod(0600), f.Close())
	if err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: path}
	query := uri.Query()
	// Apply these to every replacement connection, including after cancellation.
	for _, pragma := range []string{"busy_timeout(5000)", "foreign_keys(1)", "synchronous(FULL)"} {
		query.Add("_pragma", pragma)
	}
	uri.RawQuery = query.Encode()
	handle, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	// One connection serializes short transactions. Do not hold rows open while
	// issuing another query: readers hydrate their payloads after closing rows.
	handle.SetMaxOpenConns(1)
	db := &DB{DB: handle, Path: path}
	fail := func(e error) (*DB, error) { _ = handle.Close(); return nil, e }
	if _, err = handle.Exec(`PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON; PRAGMA synchronous=FULL;`); err != nil {
		return fail(err)
	}
	var version int
	if err = handle.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version != 0 && version != schemaVersion {
		return fail(fmt.Errorf("unsupported storage schema %d", version))
	}
	var mode string
	if err = handle.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fail(err)
	}
	if mode != "wal" {
		return fail(fmt.Errorf("SQLite WAL unavailable: %s", mode))
	}
	if version == schemaVersion {
		return db, nil
	}
	tx, err := handle.Begin()
	if err != nil {
		return fail(err)
	}
	if _, err = tx.Exec(schema); err != nil {
		_ = tx.Rollback()
		return fail(err)
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	return db, nil
}

func (db *DB) Close() error {
	_, err := db.Exec("PRAGMA wal_checkpoint(PASSIVE)")
	return errors.Join(err, db.DB.Close())
}

// LockWriter permits readers/exports but only one harness process per workspace.
// SQLite's transaction lock alone cannot protect a filesystem Edit/Write that
// spans two database commits. Never hold an SQL transaction across that edit.
func (db *DB) LockWriter() (func() error, error) {
	f, err := os.OpenFile(filepath.Join(filepath.Dir(db.Path), "writer.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("workspace storage already has a writer: %w", err)
	}
	return f.Close, nil
}

func Hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// Directory uses XDG's absolute-path rule and a canonical workspace identity.
func Directory(workspace string, getenv func(string) string) (string, error) {
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", err
	}
	home := getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(home) {
		home = getenv("HOME")
		if home == "" {
			home, err = os.UserHomeDir()
			if err != nil {
				return "", err
			}
		}
		if !filepath.IsAbs(home) {
			return "", errors.New("set absolute XDG_STATE_HOME or HOME, or use -session-directory")
		}
		home = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(home, "unreal-agent", "workspaces", Hash([]byte(canonical))), nil
}

func (db *DB) BindWorkspace(ctx context.Context, workspace string) error {
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "INSERT OR IGNORE INTO metadata VALUES('workspace','path',?)", []byte(canonical))
	if err != nil {
		return err
	}
	var stored []byte
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE namespace='workspace' AND key='path'").Scan(&stored); err != nil {
		return err
	}
	if string(stored) != canonical {
		return fmt.Errorf("storage belongs to workspace %s, not %s", stored, canonical)
	}
	return nil
}

func (db *DB) GetMetadata(ctx context.Context, namespace, key string) ([]byte, error) {
	var data []byte
	err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE namespace=? AND key=?", namespace, key).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		err = os.ErrNotExist
	}
	return data, err
}
func (db *DB) PutMetadata(ctx context.Context, namespace, key string, data []byte) error {
	_, err := db.ExecContext(ctx, "INSERT INTO metadata VALUES(?,?,?) ON CONFLICT(namespace,key) DO UPDATE SET value=excluded.value", namespace, key, data)
	return err
}

// Backup creates a consistent, standalone snapshot, including committed WAL
// contents. VACUUM INTO refuses an existing nonempty target; we additionally
// reserve a private file with O_EXCL so exports never overwrite user data.
func (db *DB) Backup(ctx context.Context, path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(absolute, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if _, err = db.ExecContext(ctx, "VACUUM INTO ?", absolute); err != nil {
		_ = os.Remove(absolute)
		return err
	}
	f, err = os.Open(absolute)
	if err != nil {
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(absolute))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
