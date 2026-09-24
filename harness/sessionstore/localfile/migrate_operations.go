package localfile

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/storage"
	"golang.org/x/sys/unix"
)

// Rebase only unfinished shell checkpoints. Historical events/results remain
// byte-for-byte equivalent on replay, preserving saved compaction prefix hashes.
// File-tool receipts live in SQLite and may keep their old namespace: they no
// longer create private filesystem files, so they do not need a path rewrite.
func (s *Store) relocateLegacyOperations(ctx context.Context, source string, manifest migrationManifest, inPlace bool) error {
	archives := map[string]string{}
	for _, file := range manifest.Files {
		archives[filepath.Join(source, file.Path)] = file.Hash
	}
	for _, file := range manifest.Files {
		name, ok := strings.CutSuffix(file.Path, sessionFileSuffix)
		if !ok || filepath.Base(name) != name {
			continue
		}
		id := session.ID(name)
		state, _, err := s.readState(ctx, id)
		if err != nil {
			return err
		}
		oldBase := filepath.Join(source, "operations", name)
		newBase := filepath.Join(s.directory, "operations", name)
		if inPlace {
			// Even an in-place migration needs fresh paths: ready/create-phase
			// processes may rewrite their spools, whose old aliases are immutable.
			newBase = filepath.Join(s.directory, "operations", ".migrated", storage.Hash([]byte(source))[:16], name)
		}
		for _, op := range state.Operations {
			if terminalOperationStatus(op.Status) || op.Type != operation.TypeShell {
				continue
			}
			shell, err := operation.DecodeShellState(op)
			if err != nil {
				return err
			}
			if shell.BaseDirectory != oldBase {
				continue
			}
			if err = validateSessionID(session.ID(op.ID)); err != nil {
				return fmt.Errorf("cannot relocate operation ID: %w", err)
			}
			for _, stream := range []string{operation.ShellOutFilename, operation.ShellErrFilename} {
				oldPath := filepath.Join(oldBase, string(op.ID), stream)
				if hash, exists := archives[oldPath]; exists {
					newPath := filepath.Join(newBase, string(op.ID), stream)
					if err = s.materializeMigrationCapture(ctx, oldPath, newPath, hash); err != nil {
						return err
					}
				}
			}
			for _, field := range []*string{&shell.OutPath, &shell.ErrPath} {
				if *field == "" {
					continue
				}
				rel, err := filepath.Rel(oldBase, *field)
				if err != nil || !filepath.IsLocal(rel) {
					return errors.New("unfinished shell references a capture outside its legacy directory")
				}
				*field = filepath.Join(newBase, rel)
			}
			shell.BaseDirectory = newBase
			op.State, err = json.Marshal(shell)
			if err != nil {
				return err
			}
			if err = s.SaveOperation(ctx, id, op); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) materializeMigrationCapture(ctx context.Context, oldPath, target, hash string) error {
	root, err := os.OpenRoot(s.directory)
	if err != nil {
		return err
	}
	defer root.Close()
	rel, err := filepath.Rel(s.directory, target)
	if err != nil || !filepath.IsLocal(rel) {
		return errors.New("invalid relocated spool path")
	}
	prefix := "."
	for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		prefix = filepath.Join(prefix, part)
		info, err := root.Lstat(prefix)
		if errors.Is(err, os.ErrNotExist) {
			if err = root.Mkdir(prefix, 0700); err != nil {
				return err
			}
			if err = syncMigrationDirectory(root, filepath.Dir(prefix)); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !info.IsDir() {
			return errors.New("relocated spool directory is a symlink or non-directory")
		}
	}
	f, err := openMigrationFile(root, rel)
	if err == nil {
		got, err := hashMigrationFile(f)
		_ = f.Close()
		if err != nil {
			return err
		}
		if got != hash {
			return fmt.Errorf("relocated spool already exists with different bytes: %s", target)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Publish a fully synced temporary file with create-only Link. A crash may
	// leave a private temporary, never a partially written canonical spool.
	tmp := filepath.Join(filepath.Dir(rel), ".migration-spool-"+uuid.New().String())
	temporary, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if err = s.database.Copy(ctx, storage.Reference(oldPath), temporary); err == nil {
		err = temporary.Sync()
	}
	err = errors.Join(err, temporary.Close())
	if err != nil {
		return err
	}
	if err = root.Link(tmp, rel); err != nil {
		return err
	}
	return syncMigrationDirectory(root, filepath.Dir(rel))
}
