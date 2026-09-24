package chat

import (
	"context"
	"path/filepath"

	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func openStorage(ctx context.Context, c config) (*localfile.Store, func() error, error) {
	noop := func() error { return nil }
	if c.storageFormat == "jsonl" {
		s, err := localfile.New(c.directory)
		if err != nil {
			return nil, noop, err
		}
		lease, err := storage.LockLegacyDirectory(c.directory, false)
		if err != nil {
			_ = s.Close()
			return nil, noop, err
		}
		return s, lease.Close, nil
	}
	s, err := localfile.NewSQLite(c.directory)
	if err != nil {
		return nil, noop, err
	}
	release, err := s.Database().LockWriter()
	if err != nil {
		_ = s.Close()
		return nil, noop, err
	}
	fail := func(err error) (*localfile.Store, func() error, error) {
		_ = s.Close()
		_ = release()
		return nil, noop, err
	}
	if err = s.Database().BindWorkspace(ctx, c.workspace); err != nil {
		return fail(err)
	}
	// Missing sources are a no-op. Both fresh legacy stores and previously
	// imported/partially cleaned stores are handled by the durable manifest.
	for _, directory := range []string{filepath.Join(c.workspace, ".harness", "sessions"), c.directory} {
		if err = s.MigrateLegacy(ctx, directory); err != nil {
			return fail(err)
		}
	}
	return s, release, nil
}
