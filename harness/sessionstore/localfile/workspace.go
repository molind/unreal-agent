package localfile

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

// OpenWorkspace is the writable host entry point. Startup/migration is serialized
// before opening the DB; normal operation holds only a shared maintenance lease.
// Close the store before releasing the returned lease, after joining all tools.
func OpenWorkspace(ctx context.Context, directory, workspace string) (*Store, func() error, error) {
	startup, err := storage.LockStartup(ctx, directory)
	if err != nil {
		return nil, nil, err
	}
	defer startup()
	s, err := NewSQLite(directory)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*Store, func() error, error) {
		return nil, nil, errors.Join(err, s.Close())
	}
	// Establish exclusion from offline maintenance even on the no-migration path.
	host, err := s.database.LockHost()
	if err != nil {
		return fail(err)
	}
	if err = s.database.BindWorkspace(ctx, workspace); err != nil {
		_ = host()
		return fail(err)
	}
	sources := []string{filepath.Join(workspace, ".harness", "sessions"), s.directory}
	needed := false
	for _, source := range sources {
		pending, e := s.migrationPending(ctx, source)
		if e != nil {
			_ = host()
			return fail(e)
		}
		needed = needed || pending
	}
	if needed {
		// Startup serializes cooperating hosts/CLI migrations. Older hosts still
		// use writer.lock exclusively, so even in this handoff they cannot overlap.
		if err = host(); err != nil {
			return fail(err)
		}
		maintenance, err := s.database.LockWriter()
		if err != nil {
			return fail(fmt.Errorf("legacy migration required: %w", err))
		}
		for _, source := range sources {
			if err = s.MigrateLegacy(ctx, source); err != nil {
				_ = maintenance()
				return fail(err)
			}
		}
		if err = maintenance(); err != nil {
			return fail(err)
		}
		host, err = s.database.LockHost()
		if err != nil {
			return fail(err)
		}
	}
	return s, host, nil
}

// LockSession must surround creation/restore, all runtime effects and cleanup.
// It intentionally does not make the low-level Store methods require a lease:
// read-only tools, offline import and embedded single-owner users also use them.
func (s *Store) LockSession(id session.ID) (func() error, error) {
	if err := validateSessionID(id); err != nil {
		return nil, err
	}
	if s.database == nil {
		return func() error { return nil }, nil
	}
	release, err := s.database.LockSession(string(id))
	if err != nil {
		return nil, err
	}
	// A selected session may have been used by another process since /new.
	s.evictCachedWriteState(id)
	return sync.OnceValue(func() error {
		s.evictCachedWriteState(id)
		return release()
	}), nil
}

// A completed manifest is a durable boundary AFTER unlink/pruning, unlike
// Verified (which is saved BEFORE cleanup). Never scan live SQLite spool trees
// or run LegacyIdle on a completed in-place migration during normal startup.
func (s *Store) migrationPending(ctx context.Context, directory string) (bool, error) {
	source, err := filepath.Abs(directory)
	if err != nil {
		return false, err
	}
	var manifest migrationManifest
	data, err := s.database.GetMetadata(ctx, cleanupNamespace, source)
	known := err == nil
	if known {
		if err = json.Unmarshal(data, &manifest); err != nil {
			return false, err
		}
		if manifest.Version != 1 {
			return false, errors.New("unsupported legacy cleanup manifest")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return known && !manifest.Completed, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("legacy source is not a real directory: %s", source)
	}
	if parent := filepath.Dir(source); filepath.Base(parent) == ".harness" {
		info, err := os.Lstat(parent)
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, errors.New("refusing symlinked .harness directory")
		}
	}
	if known && !manifest.Completed {
		return true, nil
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return false, err
	}
	if known {
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), sessionFileSuffix) {
				return false, fmt.Errorf("legacy journal appeared after completed migration: %s; source preserved", entry.Name())
			}
		}
		return false, nil
	}
	files, _, err := discoverMigrationFiles(source, manifest)
	return len(files) != 0 || len(entries) == 0, err
}
