package localfile

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/session"
)

// A real host process, not another goroutine sharing Go mutexes. Commands on
// stdin let the parent hold both leases while exercising writes and crashes.
func TestSQLiteWorkspaceProcess(t *testing.T) {
	if os.Getenv("UNREAL_STORAGE_TEST_CHILD") != "1" {
		return
	}
	args := os.Args[len(os.Args)-3:]
	s, host, err := OpenWorkspace(t.Context(), args[1], args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer host()
	defer s.Close()
	id := session.ID(args[2])
	release, err := s.LockSession(id)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = s.Create(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	scanner := bufio.NewScanner(os.Stdin)
	n := 0
	for scanner.Scan() {
		if scanner.Text() == "exit" {
			return
		}
		for range 24 {
			n++
			if err = s.AppendInput(t.Context(), id, sqlInput(fmt.Sprint(n), fmt.Sprintf("%s-%d", id, n))); err != nil {
				t.Fatal(err)
			}
			// Concurrent deduplicated artifacts share one DB, too.
			if _, err = s.database.Put(t.Context(), strings.NewReader(strings.Repeat("shared artifact", 1000))); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Println("appended")
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

type workspaceProcess struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Reader
	stderr bytes.Buffer
	wait   func() error
}

func startWorkspaceProcess(t *testing.T, workspace, directory, id string) *workspaceProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	p := &workspaceProcess{}
	p.cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteWorkspaceProcess$", "--", workspace, directory, id)
	p.cmd.Env = append(os.Environ(), "UNREAL_STORAGE_TEST_CHILD=1")
	p.cmd.Stderr = &p.stderr
	var err error
	p.input, err = p.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.output = bufio.NewReader(out)
	if err = p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.wait = sync.OnceValue(p.cmd.Wait)
	t.Cleanup(func() {
		cancel()
		_ = p.input.Close()
		_ = p.wait()
	})
	return p
}
func (p *workspaceProcess) send(t *testing.T, command string) {
	t.Helper()
	if _, err := fmt.Fprintln(p.input, command); err != nil {
		t.Fatal(err)
	}
}
func (p *workspaceProcess) expect(t *testing.T, want string) {
	t.Helper()
	line, err := p.output.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != want {
		_ = p.cmd.Process.Kill()
		_ = p.wait()
		t.Fatalf("child: got %q (%v), want %q; stderr: %s", line, err, want, p.stderr.String())
	}
}

func TestSQLiteParallelWorkspaceProcessesAndCrash(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		t.Run(fmt.Sprintf("in-place-migration=%t", migrate), func(t *testing.T) {
			workspace := t.TempDir()
			directory := filepath.Join(workspace, "state")
			if migrate {
				directory = filepath.Join(workspace, ".harness", "sessions")
				legacy, err := New(directory)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = legacy.Create(t.Context(), "legacy"); err != nil {
					t.Fatal(err)
				}
			}
			// Start simultaneously, including first-ever schema initialization and
			// the migration gate. Neither process has opened SQLite beforehand.
			a := startWorkspaceProcess(t, workspace, directory, "a")
			b := startWorkspaceProcess(t, workspace, directory, "b")
			a.expect(t, "ready")
			b.expect(t, "ready")
			a.send(t, "append")
			b.send(t, "append")
			a.expect(t, "appended")
			b.expect(t, "appended")
			s, host, err := OpenWorkspace(t.Context(), directory, workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer host()
			defer s.Close()
			for _, id := range []session.ID{"a", "b"} {
				if release, err := s.LockSession(id); err == nil {
					_ = release()
					t.Fatal("accepted duplicate session owner", id)
				}
				page, err := s.Items(t.Context(), id, 0, 100)
				if err != nil || len(page.Items) != 24 {
					t.Fatal("lost parallel history", id, len(page.Items), err)
				}
			}
			if release, err := s.database.LockWriter(); err == nil {
				_ = release()
				t.Fatal("maintenance entered live workspace")
			}
			if migrate {
				if _, err := s.Resume(t.Context(), "legacy"); err != nil {
					t.Fatal("lost imported session", err)
				}
			}
			if err := a.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = a.wait()
			release, err := s.LockSession("a")
			if err != nil {
				t.Fatal("crashed owner retained its lock", err)
			}
			if err := s.AppendInput(t.Context(), "a", sqlInput("recovered", "recovered")); err != nil {
				t.Fatal(err)
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
			// A crashed host/checkpoint must not stop its independent peer.
			b.send(t, "append")
			b.expect(t, "appended")
			b.send(t, "exit")
			if err := b.wait(); err != nil {
				t.Fatal(err, b.stderr.String())
			}
			for id, want := range map[session.ID]int{"a": 25, "b": 48} {
				state, count, err := s.sqlState(t.Context(), id)
				if err != nil || len(state.Items) != want || count != int64(want+1) {
					t.Fatal(id, count, len(state.Items), err)
				}
			}
			if err := s.database.Check(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSQLiteSessionLeaseRefreshesCachedState(t *testing.T) {
	dir := t.TempDir()
	a, b := sqliteStore(t, dir), sqliteStore(t, dir)
	for i, s := range []*Store{a, b, a} {
		release, err := s.LockSession("s")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err = s.Create(t.Context(), "s"); err != nil {
				t.Fatal(err)
			}
		}
		if err = s.AppendInput(t.Context(), "s", sqlInput(fmt.Sprint(i), "message")); err != nil {
			t.Fatal("stale cache after ownership handoff", err)
		}
		if err = release(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSQLiteSnapshotDuringConcurrentAppends(t *testing.T) {
	dir := t.TempDir()
	writer, reader := sqliteStore(t, dir), sqliteStore(t, dir)
	if _, err := writer.Create(t.Context(), "s"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := range 120 {
			if err := writer.AppendInput(t.Context(), "s", sqlInput(fmt.Sprint(i), "snapshot")); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for range 120 {
		state, count, err := reader.sqlState(t.Context(), "s")
		if err != nil || count != int64(len(state.Items)+1) {
			t.Errorf("mixed snapshot: %d records, %d items, %v", count, len(state.Items), err)
			break
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
