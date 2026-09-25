package chat

import (
	"context"

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
	return localfile.OpenWorkspace(ctx, c.directory, c.workspace)
}
