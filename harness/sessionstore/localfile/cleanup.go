package localfile

import (
	"context"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

// CleanupCaptures completes a checkpoint-committed/unlink-interrupted cleanup.
// Hosts call it while holding this session's lease, before starting its jobs.
// Read-only Inspect/Items/Resume never perform this filesystem mutation.
func (s *Store) CleanupCaptures(ctx context.Context, id session.ID) error {
	if s.database == nil {
		return nil
	}
	state, _, err := s.readState(ctx, id)
	if err != nil {
		return err
	}
	for _, op := range state.Operations {
		if err = s.captureOperation(ctx, op, true); err != nil {
			return err
		}
	}
	return nil
}
