package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// The parent retains the SAME open description as all three child descriptors.
// Independent pipes cannot reproduce the stdout poller/file-status mismatch.
func TestSharedTTYLongReplay(t *testing.T) {
	fixture := "../../.harness/experiments/unreal-chat-terminal/repro-sessions/07b7620a-8299-4202-8d60-98db68726f80.session.jsonl"
	data, err := os.ReadFile(fixture)
	if os.IsNotExist(err) {
		t.Skip("local read-only reproduction fixture not installed")
	}
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	directory := filepath.Join(workspace, "sessions")
	store, err := localfile.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	const id = "07b7620a-8299-4202-8d60-98db68726f80"
	if err := os.WriteFile(filepath.Join(directory, id+".session.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	page, err := store.Items(t.Context(), id, sessionstore.BeforeFirst, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, item := range page.Items {
		if response, ok := item.Data.(sessionstore.ModelResponse); ok {
			for _, output := range response.Response.Output {
				if message, ok := output.Data.(llm.Message); ok && message.Role == llm.RoleAssistant {
					want = append(want, message.Text)
				}
			}
		}
	}
	if len(strings.Join(want, "")) < 4000 {
		t.Fatal("fixture lacks long response")
	}
	cli := startTTY(t, []string{"TERM=dumb"}, "-provider", "ollama", "-session-directory", directory, workspace)
	cli.wait("Type /help")
	cli.pause.Lock() // Backpressure while replay writes many kilobytes.
	cli.send("/resume " + id + "\n")
	time.Sleep(300 * time.Millisecond)
	cli.pause.Unlock()
	cli.wait("Session: " + id)
	// Check the full final assistant response, not just its leading paragraph.
	last := strings.ReplaceAll(want[len(want)-1], "\n", "\r\n")
	cli.wait(last)
	cli.send("/status\n")
	cli.wait("Status: idle")
	cli.send("/exit\n")
	cli.finish(0)
}

type ttyCLI struct {
	t             *testing.T
	master, slave *os.File
	cmd           *exec.Cmd
	flags         int
	state         *term.State
	mu, pause     sync.Mutex
	text          bytes.Buffer
	done          chan error
	readerDone    chan struct{}
}

func startTTY(t *testing.T, env []string, args ...string) *ttyCLI {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	binary := filepath.Join(t.TempDir(), "unreal_chat")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		cancel()
		t.Fatalf("build: %v %s", err, out)
	}
	master, slave, err := pty.Open()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &ttyCLI{t: t, master: master, slave: slave, done: make(chan error, 1), readerDone: make(chan struct{})}
	c.flags, err = unix.FcntlInt(slave.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.state, err = term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 24, Cols: 40}); err != nil {
		t.Fatal(err)
	}
	c.cmd = exec.CommandContext(ctx, binary, args...)
	c.cmd.Env = append([]string{"PATH=/usr/bin:/bin", "OPENAI_API_KEY=local-fake-key"}, env...)
	c.cmd.Stdin, c.cmd.Stdout, c.cmd.Stderr = slave, slave, slave
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { c.done <- c.cmd.Wait() }()
	go func() {
		defer close(c.readerDone)
		b := make([]byte, 512)
		for {
			n, err := master.Read(b)
			c.pause.Lock()
			c.mu.Lock()
			c.text.Write(b[:n])
			c.mu.Unlock()
			c.pause.Unlock()
			if err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	t.Cleanup(func() { cancel(); _ = c.cmd.Process.Kill(); _ = slave.Close(); _ = master.Close(); <-c.readerDone })
	return c
}
func (c *ttyCLI) send(s string) {
	c.t.Helper()
	if _, err := io.WriteString(c.master, s); err != nil {
		c.t.Fatal(err)
	}
}
func (c *ttyCLI) snapshot() string { c.mu.Lock(); defer c.mu.Unlock(); return c.text.String() }
func (c *ttyCLI) wait(s string) {
	c.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		text := c.snapshot()
		if strings.Contains(text, s) {
			return
		}
		select {
		case err := <-c.done:
			time.Sleep(100 * time.Millisecond)
			text = c.snapshot()
			c.t.Fatalf("CLI exited: %v; waiting for %.100q; tail: %.1500s", err, s, tail(text, 1500))
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("timeout waiting for %.100q; tail: %s", s, tail(c.snapshot(), 1500))
}
func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
func (c *ttyCLI) finish(code int) {
	c.t.Helper()
	select {
	case err := <-c.done:
		got := 0
		if err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				got = e.ExitCode()
			} else {
				c.t.Fatal(err)
			}
		}
		if got != code {
			c.t.Fatalf("exit=%d, want %d: %s", got, code, tail(c.snapshot(), 1500))
		}
	case <-time.After(10 * time.Second):
		c.t.Fatalf("CLI did not exit: %s", tail(c.snapshot(), 3000))
	}
	// Darwin may add internal bookkeeping bits after terminal writes. Compare
	// the public mutable flags, including nonblocking, not those kernel bits.
	flags, err := unix.FcntlInt(c.slave.Fd(), unix.F_GETFL, 0)
	if err != nil || flags&(unix.O_NONBLOCK|unix.O_APPEND|unix.O_ASYNC|unix.O_SYNC) != c.flags&(unix.O_NONBLOCK|unix.O_APPEND|unix.O_ASYNC|unix.O_SYNC) {
		c.t.Fatalf("file flags: %x -> %x (%v)", c.flags, flags, err)
	}
	state, err := term.GetState(int(c.slave.Fd()))
	if err != nil || fmt.Sprint(state) != fmt.Sprint(c.state) {
		c.t.Fatalf("terminal state not restored: %v", err)
	}
}

// Slow real terminal output must neither lose bytes nor prevent the next input.
// This regression is portable even when the private saved-session fixture is
// absent. It also exercises the editor writer, not only TERM=dumb replay.
func TestSharedTTYLongOutput(t *testing.T) {
	for _, noColor := range []bool{false, true} {
		t.Run(fmt.Sprint(noColor), func(t *testing.T) {
			answer := strings.Repeat("Беларуская мова: complete output. ", 2200) + "THE-END"
			requests := make(chan string, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				requests <- string(b)
				writeResponse(w, []any{messageOutput(answer)})
			}))
			defer server.Close()
			env := []string{"TERM=xterm-256color"}
			if noColor {
				env = append(env, "NO_COLOR=1")
			}
			cli := startTTY(t, env, "-provider", "openai", "-base-url", server.URL, t.TempDir())
			cli.wait("you> ")
			cli.pause.Lock()
			cli.send("long answer\r")
			time.Sleep(250 * time.Millisecond)
			cli.pause.Unlock()
			cli.wait("THE-END")
			plain := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(cli.snapshot(), "")
			if !strings.Contains(strings.Join(strings.Fields(plain), " "), strings.Join(strings.Fields(answer), " ")) {
				t.Fatal("word-wrapped long response was truncated or changed")
			}
			cli.send("/status\r")
			cli.wait("Status: idle")
			if regexp.MustCompile(`\x1b\[[0-9;]*m`).MatchString(cli.snapshot()) == noColor {
				t.Fatal("NO_COLOR/color not respected")
			}
			cli.send("/exit\r")
			cli.finish(0)
		})
	}
}

func TestTTYEditingDuringEventsResizePasteAndStop(t *testing.T) {
	requests := make(chan string, 16)
	responses := make(chan []any, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		requests <- string(b)
		select {
		case response := <-responses:
			writeResponse(w, response)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	cli := startTTY(t, []string{"TERM=xterm-256color"}, "-provider", "openai", "-base-url", server.URL, t.TempDir())
	cli.wait("you> ")
	cli.send("begin\r")
	cli.wait("model / input pending")
	request := func() string {
		t.Helper()
		select {
		case r := <-requests:
			return r
		case <-time.After(8 * time.Second):
			t.Fatal("missing model request")
			return ""
		}
	}
	request()
	draft := "Прывітанне, доўгі беларускі тэкст для вузкага акна ABC"
	cli.send(draft + "\x1b[D\x1b[D")
	pastedCode := "\n\tif ready {\n\t\twork()\n\t}\n"
	cli.send("\x1b[200~" + pastedCode + "\x1b[201~")
	time.Sleep(300 * time.Millisecond)
	responses <- []any{messageOutput("Async answer while editing."), toolOutput("one", "sleep 60"), toolOutput("two", "sleep 61")}
	cli.wait("Async answer while editing.")
	cli.wait("Bash: sleep 61")
	time.Sleep(500 * time.Millisecond)
	assertDraftOnScreen(t, cli.snapshot(), strings.TrimSuffix(draft, "BC")+"▣BC", 40, 2)
	if err := cli.resize(24, 24); err != nil {
		t.Fatal(err)
	}
	if err := cli.cmd.Process.Signal(unix.SIGWINCH); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	// The insertion must still occur before B, after async messages, per-command
	// animation frames, wrapping and a width change.
	cli.send("Ж\r")
	body := request()
	want := strings.TrimSuffix(draft, "BC") + pastedCode + "ЖBC"
	if lastUserText(t, body) != want {
		t.Fatalf("edited draft/cursor lost: want %q; request %s", want, body)
	}
	responses <- []any{messageOutput("Draft received.")}
	cli.wait("Draft received.")
	// Up retrieves the prior Unicode line, Ctrl-A/Home and Ctrl-E/End edit it.
	cli.send("\x1b[A\x01X\x05Y\r")
	if body = request(); lastUserText(t, body) != "X"+want+"Y" {
		t.Fatalf("history/home/end: %s", body)
	}
	responses <- []any{messageOutput("History received.")}
	cli.wait("History received.")
	// Pasted newlines do not submit or execute slash commands without Enter.
	cli.send("\x1b[200~першы\n/exit\nдругі\x1b[201~")
	time.Sleep(300 * time.Millisecond)
	select {
	case body := <-requests:
		t.Fatalf("paste auto-submitted: %s", body)
	default:
	}
	cli.send("\r")
	if body = request(); lastUserText(t, body) != "першы\n/exit\nдругі" {
		t.Fatalf("paste: %s", body)
	}
	responses <- []any{messageOutput("Paste received.")}
	cli.wait("Paste received.")
	cli.send("\x04")
	cli.finish(0)
}

func TestTTYFailureRestoresModes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, `{"error":{"message":"specific failure API_KEY=private","type":"server_error"}}`, http.StatusBadRequest)
	}))
	defer server.Close()
	cli := startTTY(t, []string{"TERM=xterm"}, "-provider", "openai", "-base-url", server.URL, t.TempDir())
	cli.wait("you> ")
	cli.send("fail\r")
	cli.finish(1)
}
func messageOutput(text string) any {
	return map[string]any{"type": "message", "id": "msg", "role": "assistant", "status": "completed", "content": []any{map[string]string{"type": "output_text", "text": text}}}
}
func toolOutput(id, command string) any {
	args, _ := json.Marshal(map[string]string{"command": command})
	return map[string]any{"type": "function_call", "id": "fc-" + id, "call_id": id, "name": "Bash", "arguments": string(args)}
}
func writeResponse(w http.ResponseWriter, output []any) {
	response, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "response", "status": "completed", "output": output}})
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\n", response)
}

func TestTTYShorterPromptClearsWrappedTail(t *testing.T) {
	responses := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-responses:
			writeResponse(w, []any{messageOutput("Async done.")})
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	cli := startTTY(t, []string{"TERM=xterm"}, "-provider", "openai", "-base-url", server.URL, t.TempDir())
	cli.wait("you> ")
	cli.send("work\r")
	cli.wait("model / input pending")
	draft := strings.Repeat("беларускі тэкст ", 7)
	cli.send(draft + "\x01") // Cursor on the first of several wrapped input rows.
	time.Sleep(400 * time.Millisecond)
	close(responses)
	cli.wait("Async done.")
	time.Sleep(400 * time.Millisecond)
	assertDraftOnScreen(t, cli.snapshot(), draft, 40, len([]rune(draft)))
	cli.send("\x05\x15/exit\r")
	cli.finish(0)
}

// Compare the actual HTTP user content, not a substring of serialized JSON
// (which can conceal whitespace changes and truncated messages).
func lastUserText(t *testing.T, body string) string {
	t.Helper()
	var request struct {
		Input []struct {
			Role    string         `json:"role"`
			Content jsontext.Value `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatal(err)
	}
	for i := len(request.Input) - 1; i >= 0; i-- {
		item := request.Input[i]
		if item.Role != "user" {
			continue
		}
		var text string
		if err := json.Unmarshal(item.Content, &text); err == nil {
			return text
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(item.Content, &parts); err != nil {
			t.Fatal(err)
		}
		for _, part := range parts {
			text += part.Text
		}
		return text
	}
	t.Fatal("no user message in model request")
	return ""
}

func TestTTYFaithfulPastesAndLimits(t *testing.T) {
	requests := make(chan string, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		requests <- string(b)
		writeResponse(w, []any{messageOutput("Input received.")})
	}))
	defer server.Close()
	workspace := t.TempDir()
	store, err := localfile.New(filepath.Join(workspace, ".harness/sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "paste-target"); err != nil {
		t.Fatal(err)
	}
	cli := startTTY(t, []string{"TERM=xterm"}, "-provider", "openai", "-base-url", server.URL, workspace)
	cli.wait("you> ")
	request := func(want string) {
		t.Helper()
		select {
		case body := <-requests:
			got := lastUserText(t, body)
			if got != want {
				t.Fatalf("user content changed: got %d bytes (tail %q), want %d bytes (tail %q)", len(got), tail(got, 100), len(want), tail(want, 100))
			}
		case <-time.After(8 * time.Second):
			t.Fatal("missing model request")
		}
	}
	noRequest := func() {
		t.Helper()
		select {
		case body := <-requests:
			t.Fatalf("unsubmitted/rejected input reached model (%d bytes)", len(body))
		case <-time.After(100 * time.Millisecond):
		}
	}
	paste := func(text string) { cli.send("\x1b[200~" + text + "\x1b[201~") }
	cli.send("/resume ")
	paste("paste-target\n")
	cli.send("\r")
	cli.wait("Session: paste-target")
	noRequest()
	for _, r := range cli.snapshot() {
		if r >= 0xe000 && r <= 0xf8ff {
			t.Fatal("private attachment marker leaked")
		}
	}
	paste("/Users/evgen/my project/file.go")
	noRequest()
	cli.send("\x7fX\r")
	request("/Users/evgen/my project/file.gX")
	for _, path := range []string{"/tmp", "/Users/name/path\n", "/tmp/" + strings.Repeat("long-path", 30)} {
		paste(path)
		noRequest()
		cli.send("\r")
		request(path)
	}
	code := "\tif ready {\n\t\tprintln(\"прывітанне\")\n\t}\n  tail  \n"
	long := "LONG-PASTE-" + strings.Repeat("б", 6000) + "-END"
	for _, text := range []string{code, long, "/exit\n\t/not a command\n"} {
		paste(text)
		noRequest()
		cli.send("\r")
		request(text)
	}
	// A pasted single-line command is not executed until explicit Enter.
	paste("/status\n")
	noRequest()
	cli.send("\r")
	cli.wait("Status: idle")
	noRequest()
	// Recall still contains the full long block; insert typed text around it.
	paste(long)
	cli.send("\r")
	request(long)
	cli.send("\x1b[A\x01[\x05]\r")
	request("[" + long + "]")
	cli.send("prefix X\x1b[D")
	paste(code)
	cli.send("suffix\r")
	request("prefix " + code + "suffixX")
	// Ctrl-C clears the entire folded draft without submitting its bytes.
	paste(code)
	cli.send("\x03")
	cli.wait("Draft cleared.")
	noRequest()
	cli.send("trailing constraint\r")
	request("trailing constraint")
	// At least 1 MiB is supported as one paste, without a lost tail sentinel.
	maximum := strings.Repeat("x", 1024*1024-4) + "-END"
	paste(maximum)
	cli.send("\r")
	request(maximum)
	paste(maximum + "!")
	cli.send("\r")
	cli.wait("Input rejected: message exceeds 1 MiB")
	noRequest()
	cli.send("after rejected paste\r")
	request("after rejected paste")
	cli.send(strings.Repeat("б", 4097) + "\r")
	cli.wait("Input rejected: editable draft exceeds 4096 cells")
	noRequest()
	cli.send("after rejected typing\r")
	request("after rejected typing")
	cli.send("/exit\r")
	cli.finish(0)
}

// Do not use pty.Setsize/File.Fd while the child is running: File.Fd restores
// blocking mode on the shared open description and can strand the child reader.
func (c *ttyCLI) resize(rows, cols uint16) error {
	raw, err := c.slave.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	}); err != nil {
		return err
	}
	return ioctlErr
}

func TestTTYReadableTranscriptAndInlinePaste(t *testing.T) {
	for _, noColor := range []bool{false, true} {
		t.Run(fmt.Sprint(noColor), func(t *testing.T) {
			markdown := "## Summary\n\n**Ready** for `go test`.\n\n- first result\n- second result\n\n```sh\nif true; then\n    printf '**literal**'\nfi\n```\n\nMarkdown done."
			requests := make(chan string, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				requests <- string(body)
				writeResponse(w, []any{messageOutput(markdown)})
			}))
			defer server.Close()
			env := []string{"TERM=xterm"}
			if noColor {
				env = append(env, "NO_COLOR=1")
			}
			cli := startTTY(t, env, "-provider", "openai", "-base-url", server.URL, t.TempDir())
			cli.wait("you> ")
			s := &pickerScreen{t, cli, newLiveScreen(40, 24)}
			s.resizeTo(80, 28)
			cli.send("render Markdown\r")
			select {
			case <-requests:
			case <-time.After(8 * time.Second):
				t.Fatal("no model request")
			}
			cli.wait("Markdown done.")
			s.waitPrompt()
			for _, want := range []string{"Summary", "Ready", "• first result", "```sh", "printf '**literal**'", "gpt-6-astra / xhigh"} {
				if !strings.Contains(s.text(), want) {
					t.Fatalf("missing formatted content %q:\n%s", want, s.text())
				}
			}
			// These are the actual terminal cells a mouse selection will copy,
			// not just the source bytes before the editor has rendered them.
			for _, want := range []string{"Assistant", "Summary", "if true; then", "    printf '**literal**'", "fi"} {
				found := false
				for _, row := range strings.Split(s.text(), "\n") {
					if row == want {
						found = true
					}
				}
				if !found {
					t.Fatalf("copyable row %q has a gutter or altered indentation:\n%s", want, s.text())
				}
			}
			for _, raw := range []string{"## Summary", "**Ready**", "assistant>"} {
				if strings.Contains(s.text(), raw) {
					t.Fatalf("raw formatting %q:\n%s", raw, s.text())
				}
			}
			if strings.Contains(cli.snapshot(), "\x1b[1mReady\x1b[0m") == noColor || strings.Contains(cli.snapshot(), "\x1b[35m") {
				t.Fatal("emphasis/neutral palette/NO_COLOR contract")
			}
			cli.send("\x1b[200~/Users/demo/file.go\x1b[201~\x7fX")
			s.wait("/Users/demo/file.gX")
			s.draft(t, "/Users/demo/file.gX", 0)
			select {
			case <-requests:
				t.Fatal("paste submitted before Enter")
			case <-time.After(100 * time.Millisecond):
			}
			cli.send("\r")
			select {
			case request := <-requests:
				if lastUserText(t, request) != "/Users/demo/file.gX" {
					t.Fatal("inline path was not delivered literally")
				}
				if !strings.Contains(request, "## Summary") || !strings.Contains(request, "**Ready**") {
					t.Fatal("presentation modified canonical model context")
				}
			case <-time.After(8 * time.Second):
				t.Fatal("pasted path treated as a command")
			}
			cli.send("/exit\r")
			cli.finish(0)
			for _, forbidden := range []string{"[Copy", "\x1b[?1000h", "\x1b[?1003h", "\x1b[?1006h", "\x1b]52;"} {
				if strings.Contains(cli.snapshot(), forbidden) {
					t.Fatalf("native selection still intercepted: %q", forbidden)
				}
			}
		})
	}
}
