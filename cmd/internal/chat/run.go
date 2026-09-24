package chat

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"uuid"

	"github.com/unreallabsai/unreal-agent/internal/lineeditor"
	"golang.org/x/term"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

// Run takes ownership of input. Close must unblock Read (as with an os.File
// opened for polling, or io.PipeReader). Only this goroutine renders harness
// events. The terminal library serializes those writes with input echo; the
// input reader never touches coordinator, store, or display state.
func Run(ctx context.Context, args []string, getenv func(string) string, input io.ReadCloser, output, flagOutput io.Writer, interrupts <-chan os.Signal, providers []agentrunner.Provider) (result error) {
	d := newDisplay(output, getenv)
	defer func() {
		if result != nil {
			result = safeError{result, d.safe(result.Error())}
		}
	}()
	defer input.Close()
	stage := "configuration"
	var id session.ID

	c, err := parse(args, getenv, flagOutput)
	if err != nil {
		return err
	}
	log, err := openLogs(c.directory, d.safe)
	if err != nil {
		return boundary("diagnostic storage", err)
	}
	defer func() {
		if p := recover(); p != nil {
			result = boundary(stage+" panic", fmt.Errorf("%v", p))
		}
		event := "shutdown"
		if result != nil {
			event = "failed"
			result = boundary(stage, result)
		}
		result = errors.Join(result, log.event(stage, event, id, "", result), log.file.Close())
		if result != nil {
			result = fmt.Errorf("%w (diagnostic log: %s)", result, log.diagnostic)
		}
	}()
	if err := log.event("application", "startup", id, "", nil); err != nil {
		return err
	}
	stage = "storage"
	store, err := localfile.New(c.directory)
	if err != nil {
		return err
	}
	id = session.ID(c.session)
	opened, err := openSession(ctx, store, c.session)
	if err != nil {
		return err
	}
	id = opened
	stage = "provider setup"
	client, err := c.client(providers, getenv)
	if err != nil {
		return err
	}
	defer func() {
		if err := client.Close(); err != nil {
			result = errors.Join(result, boundary("provider close", err))
		}
	}()
	stage = "terminal setup"
	ui, err := openTerminal(input, output, getenv)
	if err != nil {
		return err
	}
	var resize <-chan os.Signal
	var terminalErrors <-chan error
	var ticks <-chan time.Time
	if ui != nil {
		d.ui = ui
		d.out = ui.editor
		defer func() { result = errors.Join(result, ui.close()) }()
		resize, terminalErrors = ui.resize, ui.failures
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		ticks = ticker.C
	}
	if file, ok := output.(*os.File); ok {
		d.color = term.IsTerminal(int(file.Fd())) && getenv("NO_COLOR") == "" && getenv("TERM") != "dumb" && getenv("TERM") != ""
	}
	if ui != nil {
		info := fmt.Sprintf("%s / %s | %s | /help", c.model, c.effort, filepath.Base(c.workspace))
		if err := ui.editor.SetPromptInfo(d.safe(info), d.color); err != nil {
			return boundary("terminal prompt", err)
		}
	}
	a := &application{ctx: ctx, config: c, store: store, id: id, display: d, client: client, logs: log}
	defer func() { result = errors.Join(result, a.stop()) }()
	stage = "output startup"
	if err := a.announce(); err != nil {
		return err
	}
	if err := a.replay(); err != nil {
		return err
	}
	if err := d.print("%s\n", "Type /help for commands. Tools run locally, without an approval sandbox."); err != nil {
		return err
	}
	stage = "runtime start"
	if err := a.start(); err != nil {
		return err
	}
	readerCtx, cancelReader := context.WithCancel(ctx)
	readerDone := make(chan struct{})
	lines := make(chan line)
	readNext := make(chan struct{})
	go func() {
		defer close(readerDone)
		send := func(l line) bool {
			select {
			case lines <- l:
				return true
			case <-readerCtx.Done():
				return false
			}
		}
		if ui != nil {
			for {
				l, err := ui.readLine()
				if err != nil {
					if errors.Is(err, io.EOF) {
						err = nil
					}
					send(line{eof: true, err: err})
					return
				}
				if !send(l) {
					return
				}
				// Do not read selector keys until the event owner has opened
				// the menu (or finished handling the previous command).
				select {
				case <-readNext:
				case <-readerCtx.Done():
					return
				}
			}
		}
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), maxMessageBytes+2)
		for scanner.Scan() {
			if len(scanner.Text()) > maxMessageBytes {
				send(line{eof: true, err: errors.New("input line exceeds 1 MiB; nothing from this line was sent")})
				return
			}
			if !send(line{text: scanner.Text()}) {
				return
			}
		}
		scanErr := scanner.Err()
		if errors.Is(scanErr, bufio.ErrTooLong) {
			scanErr = fmt.Errorf("input line exceeds 1 MiB; nothing from this line was sent: %w", scanErr)
		}
		send(line{eof: true, err: scanErr})
	}()
	defer func() { cancelReader(); _ = input.Close(); <-readerDone }()
	frame := 0
	stage = "runtime"
	for {
		var events <-chan event
		var done <-chan error
		if a.runtime != nil {
			events, done = a.runtime.events, a.runtime.done
		}
		select {
		case err := <-terminalErrors:
			return boundary("output terminal", err)
		case <-resize:
			if err := ui.size(); err != nil {
				return boundary("terminal resize", err)
			}
		case <-ticks:
			frame++
			if err := d.tick(frame); err != nil {
				return boundary("output status", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-interrupts:
			if !ok {
				interrupts = nil
				continue
			}
			// OS signals (including plain/piped input) have no editor key
			// event. Keyboard Ctrl-C uses the ordered line handoff below.
			exit, err := a.interrupt()
			if err != nil || exit {
				return err
			}
		case e, ok := <-events:
			if ok {
				if err := a.event(e); err != nil {
					return err
				}
			}
		case err := <-done:
			// Preserve final queued output before dropping the joined runtime.
			for e := range a.runtime.events {
				if outputErr := a.event(e); outputErr != nil {
					err = errors.Join(err, outputErr)
				}
			}
			a.runtime = nil
			if err != nil {
				return runtimeError(err)
			}
		case l := <-lines:
			if l.eof {
				return l.err
			}
			exit, err := a.accept(l)
			id = a.id
			if err != nil {
				return err
			}
			if exit {
				return nil
			}
			if ui != nil {
				select {
				case readNext <- struct{}{}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	}
}

type line struct {
	interrupt bool
	cleared   bool
	selection *lineeditor.Selection
	literal   bool // A multiline pasted body is content, not a chat command.
	text      string
	eof       bool
	err       error
}
type application struct {
	ctx     context.Context
	config  config
	store   *localfile.Store
	id      session.ID
	display *display
	client  agentrunner.Client
	runtime *runtime
	logs    *logs
}

func runtimeError(err error) error {
	return fmt.Errorf("chat runtime failed; work stopped and history retained. Check provider/model configuration, connection, missing or expired credentials, and session storage. Renew credentials externally; no automatic retry: %w", err)
}
func (a *application) start() error {
	if a.runtime != nil || a.id == "" {
		return nil
	}
	r, err := startRuntime(a.ctx, a.config, a.id, a.store, loggedAdapter{a.client, a.logs, a.id}, a.logs)
	if err != nil {
		return err
	}
	a.runtime = r
	return a.logs.event("runtime", "started", a.id, "", nil)
}
func (a *application) event(e event) error {
	if e.idle != nil {
		a.runtime.idle = *e.idle
		return nil
	}
	if e.item != nil {
		if input, ok := e.item.Data.(inbox.Input); ok && input.Kind == inbox.InputExternal {
			a.runtime.pendingInputs--
			a.runtime.idle = false // Ack precedes the next settled-state notification.
		}
		if status, ok := e.item.Data.(sessionstore.ToolCallStatus); ok && status.Status.Error != "" {
			if err := a.logs.event("tool translation", "failed", a.id, "", errors.New(status.Status.Error)); err != nil {
				return err
			}
		}
		return boundary("output item", a.display.item(*e.item, false))
	}
	if e.op != nil {
		return boundary("output operation", a.display.operation(*e.op, "", false))
	}
	return nil
}

// Drain already-observed state before deciding whether a second-tier interrupt
// stops work or exits. In particular, UI/model-response timing is not an idle
// signal: tools can be translating, in grace, or awaiting a follow-up turn.
func (a *application) interrupt() (bool, error) {
	if r := a.runtime; r != nil {
		// Bound the drain to this snapshot: continuously arriving progress
		// must not postpone the stop control indefinitely.
		for range len(r.events) {
			if err := a.event(<-r.events); err != nil {
				return false, err
			}
		}
		// An older idle notification must not hide input still in the inbox.
		if !r.idle || r.pendingInputs != 0 {
			if err := a.stop(); err != nil {
				return false, err
			}
			return false, a.display.print("Stopped. Send a new message to continue. Ctrl-C on an empty prompt exits.\n")
		}
	}
	return true, nil
}

func (a *application) stop() error {
	r := a.runtime
	if r == nil {
		return nil
	}
	logErr := a.logs.event("runtime", "stop_requested", a.id, "", nil)
	if logErr != nil {
		r.cancel()
	}
	// FIFO control follows every accepted user line. Drain display events while
	// joining so backpressure from the bounded observation queue cannot deadlock.
	_ = r.inputs.Submit(a.ctx, newInput(inbox.InputControl, inbox.ControlMessage{Mode: inbox.StopAndDiscard, Reason: "User stopped chat work"}))
	outputErr := logErr
	for e := range r.events {
		if outputErr == nil {
			outputErr = a.event(e)
			if outputErr != nil {
				r.cancel()
			}
		}
	}
	err := <-r.done
	a.runtime = nil
	a.display.generating = false
	outputErr = errors.Join(outputErr, a.display.tick(0))
	if err != nil && a.ctx.Err() == nil {
		return errors.Join(outputErr, runtimeError(err))
	}
	return errors.Join(outputErr, a.logs.event("runtime", "stopped", a.id, "", err))
}
func (a *application) announce() error {
	id := string(a.id)
	if id == "" {
		id = "(unsaved; first message saves)"
	}
	text := fmt.Sprintf("Session: %s\nWorkspace: %s\nProvider: %s | Model: %s | Reasoning effort: %s\nCommand logs: %s\nDiagnostic log: %s\n", id, a.config.workspace, a.config.provider, a.config.model, a.config.effort, filepath.Join(a.logs.directory, "commands"), a.logs.diagnostic)
	if a.display.ui != nil {
		return a.display.write("\n" + paint(a.display.color, "1", "  unreal chat") + "\n" + paint(a.display.color, "2", a.display.safe(text)) + "\n")
	}
	return a.display.print("%s", text)
}

func openSession(ctx context.Context, store *localfile.Store, requested string) (session.ID, error) {
	if requested != "" {
		id := session.ID(requested)
		if _, err := store.Resume(ctx, id); err != nil {
			return "", fmt.Errorf("resume session %q: %w", id, err)
		}
		return id, nil
	}
	return "", nil
}
func (a *application) replay() error {
	if a.id == "" {
		a.display.reset()
		return a.display.print("Selected session: new unsaved chat (no user messages).\n")
	}
	topic, err := sessionTopic(a.ctx, a.store, a.id, a.display.safe)
	if err != nil {
		return boundary("storage replay", err)
	}
	if err := a.display.print("Selected session %s — %s\n", a.id, topic); err != nil {
		return err
	}
	a.display.reset()
	after := sessionstore.BeforeFirst
	for {
		page, err := a.store.Items(a.ctx, a.id, after, 256)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			if err := a.display.item(item, true); err != nil {
				return err
			}
		}
		if !page.More {
			restored, err := a.store.Resume(a.ctx, a.id)
			if err != nil {
				return boundary("storage replay", err)
			}
			for _, op := range restored.Operations {
				if err := a.display.operation(op, "", true); err != nil {
					return err
				}
			}
			return nil
		}
		after = page.NextAfter
	}
}

// A slash is also the first character of an absolute path. Only our explicit
// command vocabulary is interpreted; other slash-leading input is user text.
func isCommand(text string) bool {
	text = strings.TrimSpace(text)
	if strings.ContainsAny(text, "\r\n") {
		return false
	}
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "/help", "/status", "/sessions", "/new", "/resume", "/cancel", "/stop", "/exit", "/quit":
		return true
	}
	return false
}

func (a *application) command(text string) (bool, error) {
	if !isCommand(text) {
		return false, a.display.print("Unknown command %q. Type /help.\n", text)
	}
	fields := strings.Fields(text)
	name := fields[0]
	want := 1
	switch name {
	case "/cancel":
		want = 2
	}
	if name == "/resume" && len(fields) == 2 {
		want = 2
	}
	if len(fields) != want {
		return false, a.display.print("Invalid arguments for %s. Type /help.\n", name)
	}
	switch name {
	case "/help":
		return false, a.display.print("%s", help)
	case "/status":
		if err := a.announce(); err != nil {
			return false, err
		}
		return false, a.display.status()
	case "/sessions":
		return false, a.listSessions()
	case "/cancel":
		id := operation.ID(fields[1])
		notice, ok := a.display.operations[id]
		if !ok || notice.state == "" || a.runtime == nil {
			return false, a.display.print("No active operation %q. Use /status.\n", id)
		}
		if notice.state != "running" && notice.state != "canceling" {
			return false, a.display.print("Operation %s is already %s.\n", id, notice.state)
		}
		if err := a.runtime.operations.Cancel(id, "User canceled operation"); err != nil {
			if logErr := a.logs.event("operation", "cancel_failed", a.id, id, err); logErr != nil {
				return false, logErr
			}
			return false, a.display.print("Cannot cancel operation %s: %v\n", id, err)
		}
		return false, a.display.print("Cancellation requested for %s.\n", id)
	case "/stop":
		if err := a.stop(); err != nil {
			return false, err
		}
		return false, a.display.print("Stopped. Send a new message to continue.\n")
	case "/new", "/resume":
		if name == "/resume" && len(fields) == 1 {
			return false, a.chooseSession()
		}
		// Always stop and JOIN before touching session state. A failed switch keeps
		// the old session selected and usable, though its work has been stopped.
		if err := a.stop(); err != nil {
			return false, err
		}
		requested := ""
		if name == "/resume" {
			requested = fields[1]
		}
		id, err := openSession(a.ctx, a.store, requested)
		if err != nil {
			if logErr := a.logs.event("storage", "resume_failed", session.ID(requested), "", err); logErr != nil {
				return false, logErr
			}
			return false, a.display.print("Cannot switch: %v. Current session retained (work stopped).\n", err)
		}
		old, oldDisplay := a.id, *a.display
		a.id = id
		if err := a.replay(); err != nil {
			a.id, *a.display = old, oldDisplay
			if logErr := a.logs.event("storage", "replay_failed", id, "", err); logErr != nil {
				return false, logErr
			}
			return false, a.display.print("Cannot replay selected session: %v. Current session retained (work stopped).\n", err)
		}
		if err := a.announce(); err != nil {
			return false, err
		}
		if err := a.start(); err != nil {
			a.id, *a.display = old, oldDisplay
			if logErr := a.logs.event("runtime", "start_failed", id, "", err); logErr != nil {
				return false, logErr
			}
			return false, a.display.print("Cannot start selected session: %v. Current session retained (work stopped).\n", err)
		}
		return false, nil
	case "/exit", "/quit":
		return true, nil
	}
	return false, nil
}

// The empty ID is the unsaved conversation, not a second persistence flag.
// Only acceptance of real user content crosses the durable creation boundary.
func (a *application) accept(l line) (bool, error) {
	if l.interrupt {
		if l.cleared {
			return false, a.display.print("Draft cleared.\n")
		}
		return a.interrupt()
	}
	if l.selection != nil {
		if l.selection.Canceled {
			return false, nil
		}
		return a.command("/resume " + l.selection.Value)
	}
	text := strings.TrimSpace(l.text)
	if text == "" && !l.literal {
		return false, nil
	}
	if !l.literal && isCommand(text) {
		return a.command(text)
	}
	input := newInput(inbox.InputExternal, l.text)
	if a.id == "" {
		id := session.ID(uuid.New().String())
		if _, err := a.store.Create(a.ctx, id); err != nil {
			return false, boundary("storage create", err)
		}
		// Persist the first input before starting a runtime: restore consumes
		// it exactly once, even if setup fails or EOF immediately follows.
		if err := a.store.AppendInput(a.ctx, id, input); err != nil {
			return false, boundary("storage first input", err)
		}
		a.id = id
		if a.display.ui != nil {
			// Saving the first message changes only identity; do not repeat
			// the entire startup/configuration/log banner in the conversation.
			if err := a.display.write(paint(a.display.color, "2", "  Session: "+string(id)+"\n")); err != nil {
				return false, err
			}
		} else if err := a.announce(); err != nil {
			return false, err
		}
		if err := a.start(); err != nil {
			return false, err
		}
	} else {
		if err := a.start(); err != nil {
			return false, err
		}
		if err := a.runtime.inputs.Submit(a.ctx, input); err != nil {
			return false, runtimeError(err)
		}
		a.runtime.pendingInputs++
	}
	return false, boundary("output", a.display.working())
}
