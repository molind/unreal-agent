package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Own all three Go files, not just stdin: dup shares open-file status flags,
// and stdin/stdout/stderr may themselves be duplicates of one terminal. The
// original os.Stdout was constructed while blocking and cannot poll on EAGAIN.
// NewFile observes O_NONBLOCK and registers each duplicate with the Go poller.
// This also makes Close unblock a pending stdin Read on Darwin.
func chatStdio() (files [3]*os.File, closeFiles func() error, err error) {
	var flags [3]int
	for i := range flags {
		flags[i], err = unix.FcntlInt(uintptr(i), unix.F_GETFL, 0)
		if err != nil {
			return files, nil, err
		}
	}
	closeFiles = func() (err error) {
		for _, f := range files {
			if f != nil {
				_ = f.Close()
			}
		}
		// Restore only after reads/writes have joined. Snapshot ALL flags before
		// changing any of them, since any subset of the descriptors may alias.
		for i, flag := range flags {
			_, e := unix.FcntlInt(uintptr(i), unix.F_SETFL, flag)
			err = errors.Join(err, e)
		}
		return err
	}
	for i := range files {
		fd, e := unix.Dup(i)
		if e != nil {
			return files, closeFiles, errors.Join(e, closeFiles())
		}
		unix.CloseOnExec(fd)
		if e = unix.SetNonblock(fd, true); e != nil {
			_ = unix.Close(fd)
			return files, closeFiles, errors.Join(e, closeFiles())
		}
		files[i] = os.NewFile(uintptr(fd), fmt.Sprintf("chat stdio %d", i))
	}
	return files, closeFiles, nil
}
