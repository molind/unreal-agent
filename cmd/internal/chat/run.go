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
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
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
	store, release, err := openStorage(ctx, c)
	if err != nil {
		return boundary("storage", err)
	}
	defer func() { result = errors.Join(result, store.Close(), release()) }()
	var log *logs
	if store.Database() != nil {
		log = openDatabaseLogs(store.Database(), d.safe)
	} else {
		log, err = openLogs(c.directory, d.safe)
	}
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
		result = errors.Join(result, log.event(stage, event, id, "", result), log.close())
		if result != nil {
			result = fmt.Errorf("%w (diagnostic log: %s)", result, log.diagnostic)
		}
	}()
	if err := log.event("application", "startup", id, "", nil); err != nil {
		return err
	}
	stage = "storage"
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
		d.promptInfo = d.safe(info)
		if err := d.updatePrompt(); err != nil {
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
				if recoverableContextError(err) {
					if ui != nil && a.approvalMenuShown != "" {
						if outputErr := ui.editor.CloseSelectionID(compactionMenuID(a.approvalMenuShown)); outputErr != nil {
							return outputErr
						}
					}
					a.approvalMenuShown = ""
					d.generating = false
					d.contextStatus.Compacting = false
					d.contextStatus.ApprovalID = ""
					if outputErr := d.print("Context work stopped: %v\nHistory retained. Use /new for a shorter conversation, /resume to switch, or a new message to retry compaction.\n", err); outputErr != nil {
						return outputErr
					}
					if outputErr := d.tick(0); outputErr != nil {
						return outputErr
					}
					continue
				}
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
			if err := a.offerCompactionMenu(); err != nil {
				return err
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
	viewer    *lineeditor.ViewerClosed
	interrupt bool
	cleared   bool
	selection *lineeditor.Selection
	literal   bool // A multiline pasted body is content, not a chat command.
	text      string
	eof       bool
	err       error
}
type application struct {
	ctx               context.Context
	config            config
	store             *localfile.Store
	id                session.ID
	display           *display
	client            agentrunner.Client
	runtime           *runtime
	logs              *logs
	approvalMenuShown string
	stopping          bool
}

func runtimeError(err error) error {
	return fmt.Errorf("chat runtime failed; work stopped and history retained. See the cause below and the diagnostic log; no automatic retry: %w", err)
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
	if e.warning != "" {
		return a.display.print("History recovery: %s\n", e.warning)
	}
	if e.context != nil {
		oldID := a.display.contextStatus.ApprovalID
		if oldID != "" && oldID != e.context.ApprovalID && a.display.ui != nil {
			if err := a.display.ui.editor.CloseSelectionID(compactionMenuID(oldID)); err != nil {
				return err
			}
		}
		if err := a.display.context(*e.context); err != nil {
			return err
		}
		return a.offerCompactionMenu()
	}
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
	if a.display.ui != nil {
		if closed, err := a.display.ui.editor.CloseViewer(); err != nil {
			return false, err
		} else if closed {
			return false, a.offerCompactionMenu()
		}
	}
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
	a.stopping = true
	defer func() { a.stopping = false }()
	var menuErr error
	if a.display.ui != nil && a.approvalMenuShown != "" {
		menuErr = a.display.ui.editor.CloseSelectionID(compactionMenuID(a.approvalMenuShown))
	}
	a.approvalMenuShown = ""
	r := a.runtime
	if r == nil {
		return menuErr
	}
	logErr := errors.Join(menuErr, a.logs.event("runtime", "stop_requested", a.id, "", nil))
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
	a.display.contextStatus.Compacting = false
	a.display.contextStatus.ApprovalID = ""
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
	commandLogs := filepath.Join(a.logs.directory, "commands")
	if a.logs.database != nil {
		commandLogs = a.logs.database.Path + " (diagnostics table; use unreal-storage logs)"
	}
	text := fmt.Sprintf("Session: %s\nWorkspace: %s\nProvider: %s | Model: %s | Reasoning effort: %s\nCommand logs: %s\nDiagnostic log: %s\n", id, a.config.workspace, a.config.provider, a.config.model, a.config.effort, commandLogs, a.logs.diagnostic)
	text += "Context: recover on provider overflow; repeated compaction requires approval.\n"
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
		if err := a.display.tick(0); err != nil {
			return err
		}
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
	if err := a.display.tick(0); err != nil {
		return err
	}
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
	case "/help", "/status", "/sessions", "/new", "/resume", "/cancel", "/compact", "/stop", "/exit", "/quit":
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
	if (name == "/resume" || name == "/compact") && len(fields) == 2 {
		want = 2
	}
	if len(fields) != want {
		return false, a.display.print("Invalid arguments for %s. Type /help.\n", name)
	}
	switch name {
	case "/help":
		return false, a.report("Help", func(view *application) error { return view.display.print("%s", help) })
	case "/status":
		return false, a.report("Status", func(view *application) error {
			if a.display.ui == nil || a.display.ui.viewerDisabled {
				if err := view.announce(); err != nil {
					return err
				}
				return view.display.status()
			}
			if err := view.display.status(); err != nil {
				return err
			}
			return view.announce()
		})
	case "/sessions":
		return false, a.report("Saved sessions", func(view *application) error { return view.listSessions() })
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
	case "/compact":
		decision := ""
		if len(fields) == 2 {
			decision = fields[1]
		}
		return false, a.compactApproval(decision)
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
	if l.viewer != nil {
		if l.viewer.Overflow {
			return false, a.display.print("Viewer closed because queued output reached 1 MiB; all output was returned to the transcript.\n")
		}
		return false, nil
	}
	if l.interrupt {
		if l.cleared {
			return false, a.display.print("Draft cleared.\n")
		}
		return a.interrupt()
	}
	if l.selection != nil {
		if strings.HasPrefix(l.selection.Context, "compaction:") {
			return false, a.acceptCompactionSelection(l.selection)
		}
		if l.selection.Context != "" {
			return false, a.display.print("Ignored an obsolete menu selection.\n")
		}
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
	if a.display.contextStatus.ApprovalID != "" {
		if a.display.ui != nil {
			return false, a.display.print("Message queued. Compaction is awaiting your choice; /compact reopens the menu.\n")
		}
		return false, a.display.print("Message queued. Further compaction still requires /compact yes; /compact no stops the work.\n")
	}
	return false, boundary("output", a.display.working())
}

// Do not swallow a simultaneous storage/log/output failure just because another
// branch of errors.Join contains a recoverable context failure.
func recoverableContextError(err error) bool {
	var diagnostic *diagnosticWriteError
	if errors.As(err, &diagnostic) {
		return false
	}
	for err != nil {
		if _, ok := err.(*contextbuilder.LimitError); ok {
			return true
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) != 1 {
				return false
			}
			err = children[0]
		} else {
			err = errors.Unwrap(err)
		}
	}
	return false
}

// Only an explicit local command can grant a single request-scoped permission.
// Approval is a durable control, never a model/user message or an inferred yes.
func (a *application) compactApproval(decision string) error {
	if decision != "" && decision != "yes" && decision != "no" {
		return a.display.print("Usage: /compact yes or /compact no.\n")
	}
	id := a.display.contextStatus.ApprovalID
	if id == "" || a.runtime == nil {
		return a.display.print("No compaction approval pending.\n")
	}
	switch decision {
	case "":
		if a.display.ui != nil {
			a.approvalMenuShown = ""
			return a.offerCompactionMenu()
		}
		return a.display.print("Context still exceeds the provider limit. Compress another older chunk? /compact yes approves one attempt; /compact no stops work.\n")
	case "no":
		if err := a.stop(); err != nil {
			return err
		}
		return a.display.print("Further compaction declined. Work stopped; history retained. Use /new for a fresh conversation.\n")
	default:
		control := newInput(inbox.InputControl, inbox.ControlMessage{Mode: inbox.ApproveCompaction, Parameters: inbox.ContextRecovery{RequestID: id}})
		if err := a.runtime.inputs.Submit(a.ctx, control); err != nil {
			return runtimeError(err)
		}
		return a.display.print("Approval submitted for one additional compaction attempt.\n")
	}
}

func compactionMenuID(requestID string) string { return "compaction:" + requestID }

func (a *application) offerCompactionMenu() error {
	id := a.display.contextStatus.ApprovalID
	if a.display.ui == nil || a.runtime == nil || a.stopping || id == "" || a.approvalMenuShown == id {
		return nil
	}
	opened, err := a.display.ui.editor.TryOpenSelection(compactionMenuID(id), "Compact again?", []lineeditor.Choice{
		{Value: "no", Label: "No - stop work", Detail: "Keep history; do not compress again. Esc defers."},
		{Value: "yes", Label: "Yes - compact once", Detail: "Approve one attempt; recent work stays unchanged."},
	})
	if opened {
		a.approvalMenuShown = id
	}
	return err
}

func (a *application) acceptCompactionSelection(selected *lineeditor.Selection) error {
	id := strings.TrimPrefix(selected.Context, "compaction:")
	if id == "" || id != a.display.contextStatus.ApprovalID || a.runtime == nil {
		return a.display.print("Ignored an obsolete compaction choice.\n")
	}
	if selected.Canceled {
		return a.display.print("Compaction decision deferred. No permission granted; /compact reopens the menu.\n")
	}
	if selected.Value != "yes" && selected.Value != "no" {
		return a.display.print("Invalid compaction choice.\n")
	}
	return a.compactApproval(selected.Value)
}
