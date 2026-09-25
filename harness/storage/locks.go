package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Lock files are permanent: unlinking one would allow two owners of different
// inodes. File descriptors are close-on-exec and the kernel releases them on exit.
func lock(ctx context.Context, path string, mode int, wait bool) (func() error, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (func() error, error) { _ = f.Close(); return nil, err }
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() {
		return fail(fmt.Errorf("not a regular lock file: %s", path))
	}
	for {
		if err = ctx.Err(); err != nil {
			return fail(err)
		}
		err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return sync.OnceValue(f.Close), nil
		}
		if !wait || (!errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN)) {
			return fail(err)
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// LockStartup serializes host startup and migration before opening SQLite. In
// particular, an in-place migration must not see another starting host's open DB
// descriptors as evidence of a live legacy writer. It is never held during chat.
func LockStartup(ctx context.Context, directory string) (func() error, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	// Keep the gate outside the source tree: a waiting host's open lock FD
	// must not make LegacyIdle reject the in-place migration holding the gate.
	path := filepath.Join(filepath.Dir(canonical), ".unreal-startup-"+Hash([]byte(canonical))+".lock")
	return lock(ctx, path, unix.LOCK_EX, true)
}

// LockHost excludes maintenance and older exclusive-writer hosts, but permits
// independent sessions. Hold it until all tools, logs and database writes stop.
func (db *DB) LockHost() (func() error, error) {
	release, err := lock(context.Background(), filepath.Join(filepath.Dir(db.Path), "writer.lock"), unix.LOCK_SH, false)
	if err != nil {
		return nil, fmt.Errorf("workspace storage is in use by maintenance or an older exclusive writer: %w", err)
	}
	return release, nil
}

// LockSession excludes a second runtime, not just simultaneous SQL commits:
// restoring the same operation in two hosts could repeat an external effect.
func (db *DB) LockSession(id string) (func() error, error) {
	release, err := lock(context.Background(), filepath.Join(filepath.Dir(db.Path), "session-"+Hash([]byte(id))+".lock"), unix.LOCK_EX, false)
	if err != nil {
		return nil, fmt.Errorf("session %q is already in use or cannot be locked: %w", id, err)
	}
	return release, nil
}
