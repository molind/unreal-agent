package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostSessionAndMaintenanceLeases(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, err := db.LockHost()
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.LockHost()
	if err != nil {
		t.Fatal(err)
	}
	if release, err := db.LockWriter(); err == nil {
		_ = release()
		t.Fatal("maintenance ignored shared leases")
	}
	session, err := db.LockSession("s")
	if err != nil {
		t.Fatal(err)
	}
	if release, err := db.LockSession("s"); err == nil {
		_ = release()
		t.Fatal("duplicate session acquired")
	}
	other, err := db.LockSession("other")
	if err != nil {
		t.Fatal(err)
	}
	for _, release := range []func() error{session, other, a, b} {
		if err := release(); err != nil {
			t.Fatal(err)
		}
	}
	maintenance, err := db.LockWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance()
	if release, err := db.LockHost(); err == nil {
		_ = release()
		t.Fatal("host ignored maintenance/old writer lease")
	}
}

func TestStartupLeaseAliasesCancellationAndUnsafeFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	first, err := LockStartup(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	alias := filepath.Join(t.TempDir(), "alias")
	if err = os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if release, err := LockStartup(ctx, alias); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			_ = release()
		}
		t.Fatal("canceled startup acquired lease", err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = os.Symlink(filepath.Join(dir, Filename), filepath.Join(dir, "writer.lock")); err != nil {
		t.Fatal(err)
	}
	if release, err := db.LockHost(); err == nil {
		_ = release()
		t.Fatal("followed symlinked lock")
	}
}
