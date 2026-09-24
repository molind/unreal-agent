package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// LockLegacyDirectory coordinates new JSONL hosts (shared) with migration
// (exclusive), without leaving a lock file in the directory being removed.
// Keep the descriptor alive until all history/log/tool writes have stopped.
func LockLegacyDirectory(directory string, exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(directory, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) { _ = f.Close(); return nil, err }
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	if err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB); err != nil {
		return fail(fmt.Errorf("legacy storage is in use; close its chat/runner before migration: %w", err))
	}
	opened, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	named, err := os.Lstat(directory)
	if err != nil || !os.SameFile(opened, named) {
		return fail(fmt.Errorf("legacy storage directory changed; reopen it"))
	}
	return f, nil
}

// LegacyIdle also checks old binaries that predate the directory lock. It reads
// process metadata only, never command-line arguments or file contents. Failure
// to inspect is not evidence of inactivity: automatic cleanup fails closed.
// Arbitrary third-party writers must still be stopped; no advisory lock can
// exclude software that deliberately ignores it and starts after this check.
func LegacyIdle(ctx context.Context, directory string) error {
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		return legacyIdleProc(ctx, canonical)
	}
	return legacyIdleLSOF(ctx, canonical)
}

func legacyBusy(pid int) error {
	return fmt.Errorf("legacy storage may be in use by process %d; close old chat/runner processes and retry (nothing deleted)", pid)
}
func underDirectory(path, directory string) bool {
	path = strings.TrimSuffix(path, " (deleted)")
	return path == directory || strings.HasPrefix(path, directory+string(filepath.Separator))
}

func legacyIdleProc(ctx context.Context, directory string) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("cannot inspect legacy writers: %w", err)
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		base := filepath.Join("/proc", entry.Name())
		var stat unix.Stat_t
		if err = unix.Stat(base, &stat); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if stat.Uid != uint32(os.Getuid()) {
			continue
		}
		// Older one-shot runners may hold no storage descriptor while waiting
		// on the provider. Conservatively defer migration while any is running.
		if executable, err := os.Readlink(filepath.Join(base, "exe")); err == nil && filepath.Base(strings.TrimSuffix(executable, " (deleted)")) == "unreal-agent-runner" {
			return legacyBusy(pid)
		}
		fds, err := os.ReadDir(filepath.Join(base, "fd"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot inspect legacy writer process %d: %w", pid, err)
		}
		paths := []string{filepath.Join(base, "cwd")}
		for _, fd := range fds {
			paths = append(paths, filepath.Join(base, "fd", fd.Name()))
		}
		for _, path := range paths {
			target, err := os.Readlink(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("cannot inspect legacy writer process %d: %w", pid, err)
			}
			if underDirectory(target, directory) {
				return legacyBusy(pid)
			}
		}
	}
	return nil
}

func legacyIdleLSOF(ctx context.Context, directory string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// comm contains executable names, not arguments (which may contain secrets).
	ps, err := exec.CommandContext(ctx, "ps", "-U", strconv.Itoa(os.Getuid()), "-o", "pid=", "-o", "comm=").Output()
	if err != nil {
		return fmt.Errorf("cannot inspect legacy runner processes: %w", err)
	}
	for _, line := range strings.Split(string(ps), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err == nil && pid != os.Getpid() && filepath.Base(strings.Join(fields[1:], " ")) == "unreal-agent-runner" {
			return legacyBusy(pid)
		}
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "lsof", "-nP", "-Fp", "-a", "-u", strconv.Itoa(os.Getuid()), "+D", directory)
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.HasPrefix(line, "p") {
			continue
		}
		pid, parseErr := strconv.Atoi(strings.TrimPrefix(line, "p"))
		if parseErr == nil && pid != os.Getpid() {
			return legacyBusy(pid)
		}
	}
	var exit *exec.ExitError
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if stderr.Len() != 0 || (err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1)) {
		return fmt.Errorf("cannot verify legacy storage is idle; lsof must be available and able to inspect the source directory")
	}
	return nil
}
