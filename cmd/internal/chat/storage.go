package chat

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func openStorage(ctx context.Context, c config) (*localfile.Store, func() error, error) {
	noop := func() error { return nil }
	if c.storageFormat == "jsonl" {
		s, err := localfile.New(c.directory)
		return s, noop, err
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
		_ = release()
		_ = s.Close()
		return nil, noop, err
	}
	if err = s.Database().BindWorkspace(ctx, c.workspace); err != nil {
		return fail(err)
	}
	// Never silently strand a pre-XDG history. Migration is deliberately offline
	// and explicit: the old host may still have active processes and open captures.
	for _, dir := range []string{filepath.Join(c.workspace, ".harness", "sessions"), c.directory} {
		matches, err := filepath.Glob(filepath.Join(dir, "*.session.jsonl"))
		if err != nil {
			return fail(err)
		}
		for _, path := range matches {
			var imported int
			if err = s.Database().QueryRowContext(ctx, "SELECT count(*) FROM imports WHERE source=?", path).Scan(&imported); err != nil {
				return fail(err)
			}
			if imported == 0 {
				return fail(fmt.Errorf("legacy history found in %s; stop old chat processes, then run unreal-storage -workspace %q -session-directory %q migrate %q (sources are retained); use -storage-format jsonl to stay on the old store", dir, c.workspace, c.directory, dir))
			}
		}
	}
	return s, release, nil
}
