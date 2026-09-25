package operation

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

	"github.com/unreallabsai/unreal-agent/harness/storage"
)

func TestSQLiteFileProcess(t *testing.T) {
	if os.Getenv("UNREAL_FILE_TEST_CHILD") != "1" {
		return
	}
	args := os.Args[len(os.Args)-4:]
	db, err := storage.Open(args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	host, err := db.LockHost()
	if err != nil {
		t.Fatal(err)
	}
	defer host()
	lease, err := db.LockSession(args[2])
	if err != nil {
		t.Fatal(err)
	}
	defer lease()
	in := FileInput{Path: args[1], BaseDirectory: filepath.Join(args[0], "operations", args[2]), Offset: 1, Limit: 200, Action: "Read"}
	in.Revision = "missing"
	if args[3] != "create" {
		read, err := executeFileStored(t.Context(), "read", in, db)
		if err != nil {
			t.Fatal(err)
		}
		in.Revision = read.Revision
	}
	in.Action, in.Content = "Write", args[2]+"\n"
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	result, err := executeFileStored(t.Context(), "write", in, db)
	if err != nil {
		if !strings.Contains(err.Error(), "file changed since Read") && !strings.Contains(err.Error(), "file identity changed") && !strings.Contains(err.Error(), "file already exists") && !os.IsExist(err) {
			t.Fatal("unexpected mutation failure", err)
		}
		fmt.Println("conflict")
		return
	}
	if !result.Applied {
		t.Fatal("successful write not applied")
	}
	// A replay must use this session's receipt, not its peer's same operation ID.
	again, err := executeFileStored(t.Context(), "write", in, db)
	if err != nil || !again.Recovered || again.Revision != result.Revision {
		t.Fatal("lost receipt", again, err)
	}
	fmt.Println("applied")
}

func TestSQLiteFileMutationsAcrossProcesses(t *testing.T) {
	for _, mode := range []string{"same-file", "different-files", "create"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			path := filepath.Join(dir, "source")
			if mode != "create" {
				if err := os.WriteFile(path, []byte("original\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			type child struct {
				cmd    *exec.Cmd
				input  io.WriteCloser
				output *bufio.Reader
				stderr bytes.Buffer
				wait   func() error
			}
			children := []*child{}
			for _, id := range []string{"a", "b"} {
				target := path
				if mode == "different-files" && id == "b" {
					target = path + "-b"
					if err := os.WriteFile(target, []byte("original\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				c := &child{cmd: exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteFileProcess$", "--", state, target, id, mode)}
				c.cmd.Env = append(os.Environ(), "UNREAL_FILE_TEST_CHILD=1")
				c.cmd.Stderr = &c.stderr
				var err error
				c.input, err = c.cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				out, err := c.cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				c.output = bufio.NewReader(out)
				if err = c.cmd.Start(); err != nil {
					t.Fatal(err)
				}
				c.wait = sync.OnceValue(c.cmd.Wait)
				t.Cleanup(func() { cancel(); _ = c.input.Close(); _ = c.wait() })
				children = append(children, c)
			}
			// Both reads must finish before either edit begins.
			for _, c := range children {
				line, err := c.output.ReadString('\n')
				if err != nil || line != "ready\n" {
					t.Fatal("child did not read original revision", line, err)
				}
			}
			for _, c := range children {
				if _, err := fmt.Fprintln(c.input, "write"); err != nil {
					t.Fatal(err)
				}
			}
			winners := 0
			for _, c := range children {
				line, err := c.output.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if err = c.wait(); err != nil {
					t.Fatal(line, err, c.stderr.String())
				}
				switch line {
				case "applied\n":
					winners++
				case "conflict\n":
				default:
					t.Fatal("unexpected child result", line)
				}
			}
			want := 1
			if mode == "different-files" {
				want = 2
			}
			if winners != want {
				t.Fatalf("successful writers=%d, want %d", winners, want)
			}
			data, err := os.ReadFile(path)
			if err != nil || (string(data) != "a\n" && string(data) != "b\n") {
				t.Fatal("torn or missing write", string(data), err)
			}
		})
	}
}
