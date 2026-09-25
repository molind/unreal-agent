package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"golang.org/x/sys/unix"
)

func seedPickerSessions(t *testing.T, workspace string, count int) {
	t.Helper()
	directory := filepath.Join(workspace, ".harness/sessions")
	store, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		id := session.ID(fmt.Sprintf("seed-%02d", i))
		if _, err := store.Create(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal("Беларуская паўторная тэма")
		if err := store.AppendInput(t.Context(), id, inbox.Input{ID: inbox.ID("input-" + id), Kind: inbox.InputExternal, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		turn := session.Turn{ID: session.TurnID("turn-" + id), Type: session.TurnRegular}
		if err := store.AppendTurn(t.Context(), id, turn); err != nil {
			t.Fatal(err)
		}
		response := sessionstore.ModelResponse{TurnID: turn.ID, Response: llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "SAVED ANSWER " + string(id)}}}}}
		if err := store.AppendModelResponse(t.Context(), id, response); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-time.Hour).Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(directory, string(id)+".session.jsonl"), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
}

func pickerCount(t *testing.T, workspace string, want int) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(workspace, ".harness/sessions/*.session.jsonl"))
	if err != nil || len(files) != want {
		t.Fatalf("session count %d want %d (%v)", len(files), want, err)
	}
}

type pickerScreen struct {
	t   *testing.T
	cli *ttyCLI
	*liveScreen
}

func (s *pickerScreen) check() {
	s.t.Helper()
	s.update(s.cli.snapshot())
	for _, row := range s.history {
		if regexp.MustCompile(`^Resume [0-9]+/|^[> ] [0-9]+ [* ] |^ID: seed-|^.*running.*PICKER_TASK|^── `).MatchString(row) {
			s.t.Fatalf("menu/live row in scrollback: %q", row)
		}
	}
}
func (s *pickerScreen) wait(want string) {
	s.t.Helper()
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		s.check()
		if strings.Contains(s.text(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.t.Fatalf("missing %q on screen:\n%s", want, s.text())
}
func (s *pickerScreen) selected(n int) {
	s.t.Helper()
	prefix := fmt.Sprintf("> %d ", n)
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		s.check()
		// The selected row and the final cursor movement can arrive in
		// separate PTY reads. Wait for both, not just the row's text.
		if s.x == 0 && strings.HasPrefix(string(s.rows[s.y]), prefix) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.t.Fatalf("hardware cursor not on selection %d: %d,%d\n%s", n, s.x, s.y, s.text())
}
func (s *pickerScreen) waitPrompt() {
	s.t.Helper()
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		s.check()
		if s.x == 5 && strings.HasPrefix(string(s.rows[s.y]), "you> ") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.t.Fatalf("no input prompt at cursor: %s", s.text())
}

func (s *pickerScreen) resizeTo(w, h int) {
	s.t.Helper()
	s.check()
	if err := s.cli.resize(uint16(h), uint16(w)); err != nil {
		s.t.Fatal(err)
	}
	s.resize(w, h)
	_ = s.cli.cmd.Process.Signal(unix.SIGWINCH)
	time.Sleep(150 * time.Millisecond)
	s.check()
}

func TestTTYSessionPickerSelectionContextAndLifecycle(t *testing.T) {
	workspace := t.TempDir()
	seedPickerSessions(t, workspace, 24)
	requests := make(chan string, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		requests <- string(b)
		writeResponse(w, []any{messageOutput("PICKER REPLY")})
	}))
	defer server.Close()
	cli := startTTY(t, []string{"TERM=xterm", "NO_COLOR=1"}, "-provider", "openai", "-base-url", server.URL, workspace)
	cli.wait("you> ")
	pickerCount(t, workspace, 24)
	s := &pickerScreen{t, cli, newLiveScreen(40, 24)}
	s.resizeTo(80, 12)
	cli.send("/resume\r")
	s.selected(1)
	s.wait("ID: seed-23")
	s.wait("Беларуская паўторная тэма")
	cli.send("\x1b[B")
	s.selected(2)
	s.wait("ID: seed-22")
	// Bare Escape (no following input) must cancel promptly and select nothing.
	cli.send("\x1b")
	s.waitPrompt()
	s.draft(t, "", 0)
	pickerCount(t, workspace, 24)
	cli.send("/resume\r" + strings.Repeat("\x1b[B", 19))
	s.selected(20)
	s.wait("ID: seed-04")
	// This item survives narrow/short resize and growing the viewport again.
	s.resizeTo(32, 5)
	s.selected(20)
	s.resizeTo(80, 12)
	s.selected(20)
	s.wait("ID: seed-04")
	// Text, paste (even command/newline/escape), and horizontal keys are menu
	// input, not messages or a draft waiting to be submitted after selection.
	cli.send("not-a-message\x1b[D\x1b[200~/exit\n\x1b[B\x1b[201~")
	time.Sleep(100 * time.Millisecond)
	s.selected(20)
	select {
	case body := <-requests:
		t.Fatalf("selector called model: %s", body)
	default:
	}
	cli.send("\r")
	cli.wait("Selected session seed-04")
	cli.wait("SAVED ANSWER seed-04")
	cli.wait("Session: seed-04")
	pickerCount(t, workspace, 24)
	s.waitPrompt()
	s.draft(t, "", 0)
	cli.send("after picker\r")
	select {
	case body := <-requests:
		if lastUserText(t, body) != "after picker" || !strings.Contains(body, "SAVED ANSWER seed-04") || strings.Contains(body, "SAVED ANSWER seed-22") {
			t.Fatal("wrong restored context", body)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("no subsequent model request")
	}
	cli.wait("PICKER REPLY")
	// Resumed current session is now the most recent and explicitly marked.
	cli.send("/resume\r")
	s.selected(1)
	s.wait("> 1 *")
	s.wait("ID: seed-04")
	cli.send("\x1b")
	s.waitPrompt()
	cli.send("/new\r/resume\r")
	s.selected(1)
	cli.send("\r")
	time.Sleep(200 * time.Millisecond)
	pickerCount(t, workspace, 24)
	// Explicit-ID path remains available, with no invisible selector state.
	cli.send("/resume seed-02\r")
	cli.wait("Session: seed-02")
	cli.wait("SAVED ANSWER seed-02")
	for range 5 {
		before := strings.Count(cli.snapshot(), "\x1b[?1049h")
		cli.send("/status\r")
		until := time.Now().Add(8 * time.Second)
		for strings.Count(cli.snapshot(), "\x1b[?1049h") == before {
			if time.Now().After(until) {
				t.Fatal("status viewer did not open")
			}
			time.Sleep(5 * time.Millisecond)
		}
		cli.closeViewer()
	}
	time.Sleep(300 * time.Millisecond)
	s.check()
	cli.send("/exit\r")
	cli.finish(0)
	pickerCount(t, workspace, 24)
	if regexp.MustCompile(`\x1b\[[0-9;]*m`).MatchString(cli.snapshot()) {
		t.Fatal("NO_COLOR emitted colors")
	}
}

func TestTTYSessionPickerWorkErrorsAndEOF(t *testing.T) {
	workspace := t.TempDir()
	seedPickerSessions(t, workspace, 3)
	requests := make(chan string, 16)
	responses := make(chan []any, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		requests <- string(b)
		select {
		case out := <-responses:
			writeResponse(w, out)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	request := func() string {
		t.Helper()
		select {
		case b := <-requests:
			return b
		case <-time.After(8 * time.Second):
			t.Fatal("no model request")
			return ""
		}
	}
	cli := startTTY(t, []string{"TERM=xterm"}, "-provider", "openai", "-base-url", server.URL, workspace)
	cli.wait("you> ")
	s := &pickerScreen{t, cli, newLiveScreen(40, 24)}
	s.resizeTo(100, 12)
	cli.send("run picker tasks\r")
	request()
	responses <- []any{toolOutput("picker-one", "printf PICKER_TASK; while [ ! -f picker-release ]; do sleep .05; done"), toolOutput("picker-two", "echo $$ > picker-confirm.pid; exec sleep 60")}
	s.wait("PICKER_TASK")
	pickerCount(t, workspace, 4)
	cli.send("/resume\r")
	s.selected(1)
	cli.send("\x1b[B")
	s.selected(2)
	// The tool completes, its colored notice and model follow-up appear while
	// the menu remains open. Opening and navigating did not stop either task.
	if err := os.WriteFile(filepath.Join(workspace, "picker-release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	cli.wait("\x1b[32m✓\x1b[0m Bash: printf PICKER_TASK")
	request()
	responses <- []any{messageOutput("ASYNC WHILE CHOOSING")}
	s.wait("ASYNC WHILE CHOOSING")
	s.selected(2)
	cli.send("\x1b")
	s.waitPrompt()
	s.wait("running") // Remaining tool survives cancellation of the menu.

	// Confirming the current exact ID still joins its running tool before
	// replay; opening/canceling above did not. No alternate switch path.
	cli.send("/resume\r")
	s.selected(1)
	cli.send("\r")
	cli.wait("canceled ·")
	s.waitPrompt()
	assertPickerProcessGone(t, workspace, "picker-confirm.pid")
	cli.send("ctrl-c picker task\r")
	request()
	responses <- []any{toolOutput("ctrl-c-task", "echo $$ > picker-interrupt.pid; exec sleep 60")}
	s.wait("picker-interrupt.pid")
	cli.send("/resume\r")
	s.selected(1)
	cli.send("\x03")
	cli.wait("Stopped.")
	cli.wait("canceled ·")
	s.selected(1) // Ctrl-C stops work, not the chooser or process.
	cli.send("\x1b")
	s.waitPrompt()
	// Delete the exact snapshot choice between open and confirmation.
	cli.send("/resume\r\x1b[B")
	s.selected(2)
	s.wait("ID: seed-02")
	if err := os.Remove(filepath.Join(workspace, ".harness/sessions/seed-02.session.jsonl")); err != nil {
		t.Fatal(err)
	}
	cli.send("\r")
	cli.wait("Cannot switch:")
	cli.wait("Current session retained")
	cli.send("/resume\r\x1b[B")
	s.selected(2)
	s.wait("ID: seed-01")
	if err := os.WriteFile(filepath.Join(workspace, ".harness/sessions/seed-01.session.jsonl"), []byte("corrupt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cli.send("\r")
	cli.wait("decode record")
	cli.send("still original\r")
	body := request()
	if lastUserText(t, body) != "still original" || !strings.Contains(body, "run picker tasks") {
		t.Fatal("failed switch lost session", body)
	}
	responses <- []any{toolOutput("eof-task", "echo $$ > picker-eof.pid; exec sleep 60")}
	s.wait("picker-eof.pid")
	cli.send("/resume\r")
	s.selected(1)
	cli.send("\x04")
	cli.finish(0)
	s.check()
	assertPickerProcessGone(t, workspace, "picker-interrupt.pid")
	assertPickerProcessGone(t, workspace, "picker-eof.pid")
}

func TestTTYEmptyPickerExitDoesNotSave(t *testing.T) {
	workspace := t.TempDir()
	cli := startTTY(t, []string{"TERM=xterm"}, "-provider", "ollama", workspace)
	cli.wait("you> ")
	cli.send("/resume\r")
	cli.wait("No saved sessions.")
	cli.send("/new\r\x04")
	cli.finish(0)
	pickerCount(t, workspace, 0)
}

func assertPickerProcessGone(t *testing.T, workspace, file string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workspace, file))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid < 2 {
		t.Fatal("invalid pid", string(data), err)
	}
	if err := unix.Kill(-pid, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("process group %d survived: %v", pid, err)
	}
}

func TestPickerSelectionWaitsForCompleteFrame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cli := &ttyCLI{t: t}
		// A PTY read can expose the selected row before the frame's final
		// cursor movement. This is not a broken hardware cursor position.
		cli.text.WriteString("> 1 selected\r\nID: seed")
		s := &pickerScreen{t, cli, newLiveScreen(40, 4)}
		done := make(chan struct{})
		defer func() { <-done }()
		go func() {
			defer close(done)
			time.Sleep(50 * time.Millisecond)
			cli.mu.Lock()
			cli.text.WriteString("\x1b[1A\x1b[8D")
			cli.mu.Unlock()
		}()
		s.selected(1)
		if s.x != 0 || s.y != 0 {
			t.Fatal("returned before the cursor reached the selected row")
		}
	})
}
