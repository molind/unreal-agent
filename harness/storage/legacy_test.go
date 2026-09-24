package storage

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyDirectoryLease(t *testing.T) {
	directory := t.TempDir()
	first, err := LockLegacyDirectory(directory, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LockLegacyDirectory(directory, false)
	if err != nil {
		t.Fatal(err)
	}
	if exclusive, err := LockLegacyDirectory(directory, true); err == nil {
		_ = exclusive.Close()
		t.Fatal("migration did not exclude legacy writers")
	}
	_ = first.Close()
	_ = second.Close()
	exclusive, err := LockLegacyDirectory(directory, true)
	if err != nil {
		t.Fatal(err)
	}
	if writer, err := LockLegacyDirectory(directory, false); err == nil {
		_ = writer.Close()
		t.Fatal("legacy writer entered migrating directory")
	}
	_ = exclusive.Close()
}

func TestLegacyIdleRejectsPreLockProcess(t *testing.T) {
	if os.Getenv("UNREAL_TEST_LEGACY_OPEN") != "" {
		f, err := os.Open(os.Getenv("UNREAL_TEST_LEGACY_OPEN"))
		if err != nil {
			os.Exit(2)
		}
		defer f.Close()
		fmt.Println("ready")
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		return
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "diagnostic.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLegacyIdleRejectsPreLockProcess$")
	cmd.Env = append(os.Environ(), "UNREAL_TEST_LEGACY_OPEN="+path)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if ready, err := bufio.NewReader(output).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatal(ready, err)
	}
	if err = LegacyIdle(ctx, directory); err == nil {
		t.Fatal("old process with an open source file was not detected")
	}
	_ = input.Close()
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err = LegacyIdle(ctx, directory); err != nil {
		t.Fatal("idle source refused", err)
	}
}
