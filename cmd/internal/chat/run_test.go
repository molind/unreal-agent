package chat

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

var testPanic = errors.New("test provider panic")

type modelCall struct {
	ctx     context.Context
	request llm.Request
	reply   chan llm.Response
	failure chan error
}
type testClient struct {
	calls  chan modelCall
	closed chan struct{}
	fail   bool
}

func (c *testClient) Respond(ctx context.Context, request llm.Request, _ llm.RequestOptions) (llm.Response, error) {
	call := modelCall{ctx, request, make(chan llm.Response, 1), make(chan error, 1)}
	select {
	case c.calls <- call:
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
	if c.fail {
		return llm.Response{}, errors.New("sk-PRIVATE upstream refused the request")
	}
	select {
	case reply := <-call.reply:
		return reply, nil
	case err := <-call.failure:
		if err == testPanic {
			panic("specific provider panic sk-PRIVATE")
		}
		return llm.Response{}, err
	case <-ctx.Done():
		return llm.Response{}, ctx.Err()
	}
}
func (c *testClient) Close() error { close(c.closed); return nil }

type recordingWriter struct {
	mu      sync.Mutex
	text    strings.Builder
	changed chan struct{}
	fail    bool
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail {
		return 0, errors.New("output unavailable")
	}
	n, err := w.text.Write(p)
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return n, err
}
func (w *recordingWriter) snapshot() string { w.mu.Lock(); defer w.mu.Unlock(); return w.text.String() }

type chatTest struct {
	t          *testing.T
	workspace  string
	input      *io.PipeWriter
	output     *recordingWriter
	client     *testClient
	done       chan error
	interrupts chan os.Signal
	exited     chan struct{}
}

func launch(t *testing.T, workspace string, fail bool, args ...string) *chatTest {
	t.Helper()
	c := &chatTest{t: t, workspace: workspace, output: &recordingWriter{changed: make(chan struct{}, 1)}, client: &testClient{calls: make(chan modelCall, 16), closed: make(chan struct{}), fail: fail}, done: make(chan error, 1), interrupts: make(chan os.Signal, 1), exited: make(chan struct{})}
	reader, writer := io.Pipe()
	c.input = writer
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(func() {
		cancel()
		_ = writer.Close()
		select {
		case <-c.exited:
		case <-time.After(8 * time.Second):
			t.Error("cleanup did not join chat")
		}
	})
	provider := agentrunner.Provider{Name: "openai-codex", NewClient: func(string, string, int, func(string) string) (agentrunner.Client, error) { return c.client, nil }}
	args = append(args, workspace)
	go func() {
		defer close(c.exited)
		c.done <- Run(ctx, args, func(string) string { return "" }, reader, c.output, c.output, c.interrupts, []agentrunner.Provider{provider})
	}()
	c.wait("Type /help")
	return c
}
func (c *chatTest) send(s string) {
	c.t.Helper()
	if _, err := fmt.Fprintln(c.input, s); err != nil {
		c.t.Fatal(err)
	}
}
func (c *chatTest) wait(s string) string {
	c.t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		text := c.output.snapshot()
		if strings.Contains(text, s) {
			return text
		}
		select {
		case <-c.output.changed:
		case err := <-c.done:
			c.t.Fatalf("chat exited (%v) waiting for %q: %s", err, s, text)
		case <-timer.C:
			c.t.Fatalf("timeout waiting for %q: %s", s, text)
		}
	}
}
func (c *chatTest) call() modelCall {
	c.t.Helper()
	select {
	case call := <-c.client.calls:
		return call
	case <-time.After(8 * time.Second):
		c.t.Fatal("model was not called")
		return modelCall{}
	}
}
func (c *chatTest) finish(command string) {
	c.t.Helper()
	if command == "interrupt" {
		c.interrupts <- os.Interrupt
	} else if command == "EOF" {
		_ = c.input.Close()
	} else {
		c.send(command)
	}
	select {
	case err := <-c.done:
		if err != nil {
			c.t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		c.t.Fatal("chat did not exit")
	}
	select {
	case <-c.client.closed:
	default:
		c.t.Fatal("client not closed")
	}
}
func reply(text string) llm.Response {
	return llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: text}}}}
}
func messages(request llm.Request, role llm.Role) []string {
	var result []string
	for _, item := range request.Input {
		if item.Type == llm.ItemMessage {
			m := item.Data.(llm.Message)
			if m.Role == role {
				result = append(result, m.Text)
			}
		}
	}
	return result
}
func assertMessages(t *testing.T, request llm.Request, role llm.Role, want ...string) {
	t.Helper()
	got := messages(request, role)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%s messages = %q, want %q", role, got, want)
	}
}
func waitCanceled(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("request not canceled")
	}
}
func sessionID(t *testing.T, output string) string {
	t.Helper()
	m := regexp.MustCompile(`Session: ([^\n]+)`).FindAllStringSubmatch(output, -1)
	if len(m) == 0 {
		t.Fatal(output)
	}
	return m[len(m)-1][1]
}

func TestSequentialSteeringResumeAndCommands(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("Workspace test policy"), 0600); err != nil {
		t.Fatal(err)
	}
	c := launch(t, workspace, false)
	c.send("first")
	first := c.call()
	id := sessionID(t, c.output.snapshot())
	if first.request.Model.ID != "gpt-6-astra" || first.request.Model.ReasoningEffort != llm.ReasoningEffortXHigh {
		t.Fatal(first.request.Model)
	}
	prompt := strings.Join(messages(first.request, llm.RoleSystem), "\n")
	if !strings.Contains(prompt, "Workspace test policy") || strings.Contains(prompt, "inside an isolated sandbox container") {
		t.Fatal(prompt)
	}
	first.reply <- reply("one")
	c.wait("assistant> one")
	c.send("second")
	second := c.call()
	assertMessages(t, second.request, llm.RoleUser, "first", "second")
	assertMessages(t, second.request, llm.RoleAssistant, "one")
	c.send("steer")
	third := c.call()
	waitCanceled(t, second.ctx)
	assertMessages(t, third.request, llm.RoleUser, "first", "second", "steer")
	third.reply <- reply("three")
	c.wait("assistant> three")
	for _, command := range []string{"/help", "/status", "/sessions", "/cancel", "/new bad", "/cancel missing", "/resume missing"} {
		c.send(command)
	}
	c.wait("Current session retained")
	c.send("after failure")
	next := c.call()
	next.reply <- reply("still usable")
	c.wait("assistant> still usable")
	c.finish("/exit")
	c = launch(t, workspace, false, "-session", id)
	if text := c.output.snapshot(); strings.Count(text, "assistant> one\n") != 1 || strings.Count(text, "you> first\n") != 1 {
		t.Fatal(text)
	}
	c.send("resumed")
	next = c.call()
	assertMessages(t, next.request, llm.RoleUser, "first", "second", "steer", "after failure", "resumed")
	assertMessages(t, next.request, llm.RoleAssistant, "one", "three", "still usable")
	next.reply <- reply("resumed OK")
	c.wait("resumed OK")
	c.send("/new")
	c.wait("Session:")
	c.send("new conversation")
	next = c.call()
	assertMessages(t, next.request, llm.RoleUser, "new conversation")
	next.reply <- reply("new OK")
	c.wait("new OK")
	c.finish("EOF")
}

func TestStopGenerationAndReplay(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupt), func(t *testing.T) {
			workspace := t.TempDir()
			c := launch(t, workspace, false)
			c.send("interrupted request")
			call := c.call()
			id := sessionID(t, c.output.snapshot())
			if interrupt {
				c.interrupts <- os.Interrupt
			} else {
				c.send("/stop")
			}
			waitCanceled(t, call.ctx)
			c.send("/status")
			c.wait("Status: idle")
			select {
			case <-c.client.calls:
				t.Fatal("stop automatically retried")
			default:
			}
			c.finish("/quit")
			c = launch(t, workspace, false, "-session", id)
			c.send("/status")
			c.wait("Status: idle")
			select {
			case <-c.client.calls:
				t.Fatal("resume replayed stopped request")
			default:
			}
			c.send("next")
			call = c.call()
			assertMessages(t, call.request, llm.RoleUser, "interrupted request", "next")
			assertMessages(t, call.request, llm.RoleAssistant)
			// The resumed model must know the old request was stopped, not merely
			// see two unanswered user messages and treat both as active work.
			interrupted, stopped := false, false
			for _, item := range call.request.Input {
				if item.Type != llm.ItemMessage {
					continue
				}
				message := item.Data.(llm.Message)
				if message.Role == llm.RoleUser && message.Text == "interrupted request" {
					interrupted = true
				}
				if interrupted && message.Role == llm.RoleSystem && strings.Contains(message.Text, "The preceding work was stopped.") {
					stopped = true
				}
				if message.Role == llm.RoleUser && message.Text == "next" && !stopped {
					t.Fatal("resumed context omits the stop between old and new user requests")
				}
			}
			call.reply <- reply("next works")
			c.wait("next works")
			c.finish("/exit")
		})
	}
}

func shellResponse(commands ...string) llm.Response {
	var r llm.Response
	for index, command := range commands {
		args, _ := json.Marshal(map[string]string{"command": command})
		r.Output = append(r.Output, llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: fmt.Sprintf("call-%d", index), Name: "Bash", Arguments: string(args)}})
	}
	return r
}
func (c *chatTest) toolID(command string) string {
	c.t.Helper()
	c.wait(command)
	re := regexp.MustCompile(`tool ([^ ]+) running — Bash: ` + regexp.QuoteMeta(command))
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(c.output.snapshot()); len(m) == 2 {
			return m[1]
		}
		time.Sleep(time.Millisecond)
	}
	c.t.Fatal("tool ID missing")
	return ""
}
func TestPendingToolSteeringCancelAndStop(t *testing.T) {
	workspace := t.TempDir()
	c := launch(t, workspace, false)
	c.send("run tools")
	call := c.call()
	commands := []string{"echo $$ > first.pid; exec sleep 60", "echo $$ > second.pid; exec sleep 61"}
	call.reply <- shellResponse(commands...)
	first, second := c.toolID(commands[0]), c.toolID(commands[1])
	c.waitFile("first.pid")
	c.waitFile("second.pid")
	c.send("steer while tools pending")
	call = c.call()
	assertMessages(t, call.request, llm.RoleUser, "run tools", "steer while tools pending")
	results := 0
	for _, item := range call.request.Input {
		if item.Type == llm.ItemToolResult {
			results++
		}
	}
	if results != 2 {
		t.Fatalf("pending tool results = %d", results)
	}
	call.reply <- reply("both pending")
	c.wait("both pending")
	c.send("/cancel " + first)
	c.wait("tool " + first + " canceled")
	call = c.call()
	call.reply <- reply("one canceled")
	c.wait("one canceled")
	c.send("/status")
	c.wait(second + " running")
	if strings.Contains(c.output.snapshot(), "tool "+second+" canceled") {
		t.Fatal("canceled unrelated operation")
	}
	c.send("/stop")
	c.wait("tool " + second + " canceled")
	c.wait("Stopped.")
	select {
	case <-c.client.calls:
		t.Fatal("stop called model")
	default:
	}
	if strings.Count(c.output.snapshot(), "tool "+first+" running") != 1 || strings.Count(c.output.snapshot(), "tool "+second+" running") != 1 {
		t.Fatal("duplicate start notices")
	}
	c.send("next")
	call = c.call()
	call.reply <- reply("alive")
	c.wait("assistant> alive")
	id := sessionID(t, c.output.snapshot())
	c.finish("EOF")
	assertNoProcesses(t, workspace, id)
	c = launch(t, workspace, false, "-session", id)
	if strings.Contains(c.output.snapshot(), "tool ") {
		t.Fatal("replayed tool notices")
	}
	c.finish("/exit")
}
func (c *chatTest) waitFile(name string) {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(c.workspace, name)); err == nil && len(data) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	c.t.Fatal("shell did not start")
}
func assertNoProcesses(t *testing.T, workspace, id string) {
	t.Helper()
	store, err := localfile.New(filepath.Join(workspace, ".harness/sessions"))
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.Items(t.Context(), session.ID(id), sessionstore.BeforeFirst, 1000)
	if err != nil {
		t.Fatal(err)
	}
	groups := map[int]bool{}
	restored, err := store.Resume(t.Context(), session.ID(id))
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range restored.Operations {
		var state struct{ ProcessGroupID int }
		_ = json.Unmarshal(op.State, &state)
		if state.ProcessGroupID > 1 {
			groups[state.ProcessGroupID] = true
		}
		if !terminal(op.Status) {
			t.Fatal("failure left a live operation checkpoint")
		}
	}
	for _, item := range page.Items {
		if status, ok := item.Data.(sessionstore.ToolCallStatus); ok {
			for _, op := range status.Operations {
				var state struct{ ProcessGroupID int }
				_ = json.Unmarshal(op.State, &state)
				if state.ProcessGroupID > 1 {
					groups[state.ProcessGroupID] = true
				}
			}
		}
	}
	if len(groups) == 0 {
		t.Fatal("no started process groups were checked")
	}
	for group := range groups {
		if err := syscall.Kill(-group, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("process group %d still exists: %v", group, err)
		}
	}
}

func TestTerminalNoticesAndRedaction(t *testing.T) {
	c := launch(t, t.TempDir(), false)
	c.send("run")
	call := c.call()
	r := shellResponse("printf OK", "exit 7")
	r.Output = append(r.Output, llm.Item{Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{"PRIVATE REASONING"}}})
	call.reply <- r
	c.wait("failed (exit 7)")
	c.wait("completed")
	call = c.call()
	call.reply <- reply("sk-abcdef API_KEY=secret done")
	c.wait("[redacted] done")
	if strings.Contains(c.output.snapshot(), "PRIVATE") || strings.Contains(c.output.snapshot(), "sk-abcdef") || strings.Contains(c.output.snapshot(), "API_KEY=secret") {
		t.Fatal(c.output.snapshot())
	}
	c.finish("/exit")
}

func TestFailuresCloseRuntime(t *testing.T) {
	for _, mode := range []string{"provider", "provider-with-tool", "panic-with-tool", "output", "switch", "exit", "EOF"} {
		t.Run(mode, func(t *testing.T) {
			workspace := t.TempDir()
			c := launch(t, workspace, mode == "provider")
			c.send("work")
			call := c.call()
			id := sessionID(t, c.output.snapshot())
			if mode == "provider" {
				select {
				case err := <-c.done:
					if err == nil || strings.Contains(err.Error(), "sk-PRIVATE") {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("provider error hung")
				}
				<-c.client.closed
				return
			}
			call.reply <- shellResponse("echo $$ > running.pid; exec sleep 60")
			c.toolID("echo $$ > running.pid; exec sleep 60")
			c.waitFile("running.pid")
			c.send("while running")
			call = c.call()
			switch mode {
			case "provider-with-tool", "panic-with-tool":
				if mode == "panic-with-tool" {
					call.failure <- testPanic
				} else {
					call.failure <- errors.New("sk-PRIVATE upstream refused the request")
				}
				select {
				case err := <-c.done:
					if err == nil || strings.Contains(err.Error(), "sk-PRIVATE") {
						t.Fatal(err)
					}
				case <-time.After(8 * time.Second):
					t.Fatal("provider failure hung")
				}
			case "output":
				c.output.mu.Lock()
				c.output.fail = true
				c.output.mu.Unlock()
				call.reply <- reply("fail output")
				select {
				case err := <-c.done:
					if err == nil {
						t.Fatal("missing output failure")
					}
				case <-time.After(8 * time.Second):
					t.Fatal("output failure hung")
				}
			case "switch":
				c.send("/new")
				c.send("new session input")
				next := c.call()
				assertMessages(t, next.request, llm.RoleUser, "new session input")
				next.reply <- reply("switched")
				c.wait("assistant> switched")
				c.finish("/exit")
			case "exit":
				c.finish("/exit")
			case "EOF":
				c.finish("EOF")
			}
			waitCanceled(t, call.ctx)
			assertNoProcesses(t, workspace, id)
		})
	}
}

func TestMissingStartupSessionAndConfiguration(t *testing.T) {
	workspace := t.TempDir()
	reader := io.NopCloser(strings.NewReader(""))
	err := Run(t.Context(), []string{"-session", "missing", workspace}, func(string) string { return "" }, reader, io.Discard, io.Discard, nil, nil)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing session: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(workspace, ".harness/sessions"))
	if err != nil || (len(entries) != 1 || entries[0].Name() != "logs") {
		t.Fatalf("created missing session: %v %v", entries, err)
	}
	for _, args := range [][]string{{"-reasoning-effort", "bad"}, {"-max-attempts", "0"}, {"-session", ""}, {workspace, workspace}} {
		if _, err := parse(args, func(string) string { return "" }, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	c, err := parse([]string{"-provider", "ollama", "-model", "test", workspace}, func(name string) string {
		if name == "UNREAL_HARNESS_LLM_PROVIDER" {
			return "openai-codex"
		}
		return ""
	}, io.Discard)
	if err != nil || c.provider != "ollama" || c.model != "test" {
		t.Fatalf("provider override: %+v, %v", c, err)
	}
}

func TestSwitchSetupFailureKeepsCurrentSession(t *testing.T) {
	workspace := t.TempDir()
	store, err := localfile.New(filepath.Join(workspace, ".harness/sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(t.Context(), "broken"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".harness/sessions/operations"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".harness/sessions/operations/broken"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	c := launch(t, workspace, false)
	c.send("original")
	call := c.call()
	call.reply <- reply("original answer")
	c.wait("original answer")
	c.send("/resume broken")
	c.wait("Cannot start selected session")
	c.send("still here")
	call = c.call()
	assertMessages(t, call.request, llm.RoleUser, "original", "still here")
	call.reply <- reply("retained")
	c.wait("assistant> retained")
	c.finish("/exit")
}

func TestSessionTopicsEmptyCurrentAndExactResume(t *testing.T) {
	workspace := t.TempDir()
	c := launch(t, workspace, false)
	c.wait("new unsaved chat")
	c.send("review the original task")
	call := c.call()
	call.reply <- reply("original context")
	c.wait("original context")
	original := sessionID(t, c.output.snapshot())
	c.finish("/exit")
	// Startup MUST remain fresh and unsaved, never auto-resume the last one.
	c = launch(t, workspace, false)
	c.send("continue")
	call = c.call()
	assertMessages(t, call.request, llm.RoleUser, "continue")
	call.reply <- reply("different conversation")
	c.wait("different conversation")
	other := sessionID(t, c.output.snapshot())
	if other == original {
		t.Fatal("auto-resumed")
	}
	c.send("/sessions")
	c.wait("First prompt: continue")
	text := c.wait("First prompt: review the original task")
	if !strings.Contains(text, "First prompt: review the original task") || !strings.Contains(text, "* "+other) {
		t.Fatal(text)
	}
	c.send("/resume " + original)
	c.wait("Selected session " + original + " — First prompt: review the original task")
	c.send("genuine continuation")
	call = c.call()
	assertMessages(t, call.request, llm.RoleUser, "review the original task", "genuine continuation")
	assertMessages(t, call.request, llm.RoleAssistant, "original context")
	call.reply <- reply("resumed exact context")
	c.wait("resumed exact context")
	c.finish("/exit")
}

func TestPipedMessageSizeBoundary(t *testing.T) {
	c := launch(t, t.TempDir(), false)
	text := strings.Repeat("x", maxMessageBytes-4) + "-END"
	c.send(text)
	call := c.call()
	assertMessages(t, call.request, llm.RoleUser, text)
	call.reply <- reply("boundary accepted")
	c.wait("boundary accepted")
	c.finish("/exit")
	c = launch(t, t.TempDir(), false)
	_, _ = fmt.Fprintln(c.input, text+"too much")
	select {
	case err := <-c.done:
		if err == nil {
			t.Fatal("oversize piped line accepted")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("oversize piped line hung")
	}
	select {
	case <-c.client.calls:
		t.Fatal("partial piped line reached model")
	default:
	}
}

func TestSlashPathsAreUserMessages(t *testing.T) {
	c := launch(t, t.TempDir(), false)
	for i, text := range []string{"/Users/evgen/my project/main.go", "/tmp", "/not-a-command аргумент", "/"} {
		c.send(text)
		call := c.call()
		got := messages(call.request, llm.RoleUser)
		if len(got) != i+1 || got[i] != text {
			t.Fatalf("slash-leading user input changed: %q", got)
		}
		answer := fmt.Sprintf("path received %d", i)
		call.reply <- reply(answer)
		c.wait(answer)
	}
	c.send("/status")
	c.wait("Status: idle")
	select {
	case <-c.client.calls:
		t.Fatal("known command was sent to the model")
	default:
	}
	c.finish("/exit")
}

func TestCommandRecognition(t *testing.T) {
	for _, text := range []string{"/help", " /status\n", "/resume saved-id", "/cancel operation-id", "/stop invalid-argument"} {
		if !isCommand(text) {
			t.Errorf("command treated as text: %q", text)
		}
	}
	for _, text := range []string{"", "/tmp", "/Users/name/project", "read /stop", "/exit\nrm file", "/resume\nsaved-id", "/unknown"} {
		if isCommand(text) {
			t.Errorf("user text treated as command: %q", text)
		}
	}
}

func TestInterruptStopsThenExits(t *testing.T) {
	workspace := t.TempDir()
	c := launch(t, workspace, false)
	c.finish("interrupt")
	durableCount(t, workspace, 0)

	c = launch(t, workspace, false)
	c.send("work")
	call := c.call()
	c.interrupts <- os.Interrupt
	c.wait("Stopped.")
	waitCanceled(t, call.ctx)
	c.finish("interrupt")
	durableCount(t, workspace, 1)
}

func TestInterruptUsesSettledRuntimeStateInsteadOfDisplay(t *testing.T) {
	var out strings.Builder
	d := newDisplay(&out, func(string) string { return "" })
	r := &runtime{events: make(chan event, 2)}
	a := &application{display: d, runtime: r}
	// A just-finished response can leave the display behind the event queue.
	// The authoritative idle event is drained before deciding to exit.
	d.generating = true
	idle := true
	r.events <- event{idle: &idle}
	if exit, err := a.interrupt(); err != nil || !exit {
		t.Fatalf("settled idle did not exit: %v, %v", exit, err)
	}
	// Clearing a draft must not inspect activity or touch this runtime at all.
	r.idle = false
	if exit, err := a.accept(line{interrupt: true, cleared: true}); err != nil || exit || a.runtime != r {
		t.Fatalf("draft clear changed runtime: %v, %v", exit, err)
	}
}

func TestInterruptPendingInputOverridesEarlierIdleEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	inputs, err := inbox.New(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	r := &runtime{inputs: inputs, idle: true, pendingInputs: 1, events: make(chan event, 1), done: make(chan error, 1), cancel: cancel}
	a := &application{ctx: ctx, display: newDisplay(&out, func(string) string { return "" }), runtime: r}
	idle := true
	r.events <- event{idle: &idle}
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		select {
		case input := <-inputs.Output():
			control, err := input.DecodeControlMessage()
			if err != nil || control.Mode != inbox.StopAndDiscard {
				r.done <- fmt.Errorf("expected discard: %v", err)
			} else {
				r.done <- nil
			}
			close(r.events)
		case <-ctx.Done():
		}
	}()
	defer func() { cancel(); <-joined }()
	if exit, err := a.interrupt(); err != nil || exit || a.runtime != nil || !strings.Contains(out.String(), "Stopped.") {
		t.Fatalf("queued input was mistaken for idle: %v, %v", exit, err)
	}
}

func TestDurableInputAckCannotExposeEarlierIdleState(t *testing.T) {
	var out strings.Builder
	r := &runtime{idle: true, pendingInputs: 1}
	a := &application{runtime: r, display: newDisplay(&out, func(string) string { return "" })}
	input := newInput(inbox.InputExternal, "new work")
	item := sessionstore.Item{Kind: sessionstore.ItemInput, Data: input}
	if err := a.event(event{item: &item}); err != nil {
		t.Fatal(err)
	}
	if r.pendingInputs != 0 || r.idle {
		t.Fatal("ack exposed stale idle before scheduling the new input")
	}
}
