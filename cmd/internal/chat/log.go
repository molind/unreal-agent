package chat

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

type logRecord struct {
	Time      time.Time           `json:"time"`
	Stage     string              `json:"stage"`
	Event     string              `json:"event"`
	Session   session.ID          `json:"session,omitempty"`
	Operation operation.ID        `json:"operation,omitempty"`
	Error     string              `json:"error,omitempty"`
	Stack     string              `json:"stack,omitempty"`
	Command   string              `json:"command,omitempty"`
	Directory string              `json:"directory,omitempty"`
	Started   time.Time           `json:"started,omitzero"`
	Duration  float64             `json:"duration_seconds,omitempty"`
	ExitCode  *int                `json:"exit_code,omitempty"`
	Stdout    string              `json:"stdout,omitempty"`
	Stderr    string              `json:"stderr,omitempty"`
	Transport *llm.TransportUsage `json:"transport,omitempty"`
}

// App, coordinator and provider goroutines share this writer. No request,
// response, auth object, reasoning, or frame is ever passed to it.
type logs struct {
	mu                    sync.Mutex
	directory, diagnostic string
	file                  *os.File
	safe                  func(string) string
	commands              map[string]logRecord
}

func openLogs(directory string, safe func(string) string) (*logs, error) {
	directory = filepath.Join(directory, "logs")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(directory, "diagnostic-*.jsonl")
	if err != nil {
		return nil, err
	}
	return &logs{directory: directory, diagnostic: f.Name(), file: f, safe: safe, commands: make(map[string]logRecord)}, nil
}
func (l *logs) write(f *os.File, r logRecord) error {
	r.Time = time.Now().UTC()
	r.Session = session.ID(l.safe(string(r.Session)))
	r.Operation = operation.ID(l.safe(string(r.Operation)))
	r.Error = l.safe(r.Error)
	r.Stack = l.safe(r.Stack)
	r.Command = l.safe(r.Command)
	r.Directory = l.safe(r.Directory)
	r.Stdout = l.safe(r.Stdout)
	r.Stderr = l.safe(r.Stderr)
	// Error bodies can be enormous; the cause and boundary stack are useful,
	// megabytes of server HTML are not.
	if len(r.Error) > 8192 {
		r.Error = string([]rune(r.Error)[:min(2048, len([]rune(r.Error)))]) + "… [truncated]"
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, 10))
	return err
}
func (l *logs) event(stage, event string, id session.ID, op operation.ID, err error) error {
	if l == nil {
		return nil
	}
	r := logRecord{Stage: stage, Event: event, Session: id, Operation: op}
	if err != nil {
		r.Error = err.Error()
		r.Stack = string(debug.Stack())
		var failure *boundaryError
		if errors.As(err, &failure) {
			r.Stack = failure.stack
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.write(l.file, r)
}
func (l *logs) command(id session.ID, op operation.Operation) error {
	if l != nil && op.Type == operation.TypeFile {
		return l.fileOperation(id, op)
	}
	if l == nil || op.Type != operation.TypeShell || op.Status == operation.StatusReady {
		return nil
	}
	state, err := operation.DecodeShellState(op)
	if err != nil {
		return err
	}
	status := operationState(op)
	key := string(id) + "/" + string(op.ID)
	l.mu.Lock()
	defer l.mu.Unlock()
	path := filepath.Join(l.directory, "commands", string(id), url.PathEscape(string(op.ID))+".jsonl")
	previous, known := l.commands[key]
	if !known {
		b, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line != "" {
				if err := json.Unmarshal([]byte(line), &previous); err != nil {
					return fmt.Errorf("read command log: %w", err)
				}
			}
		}
	}
	if previous.Event == status {
		l.commands[key] = previous
		return nil
	}
	start := previous.Started
	if start.IsZero() {
		start = time.Now().UTC()
	}
	r := logRecord{Stage: "command", Event: status, Session: id, Operation: op.ID, Started: start, Command: state.Input.Command, Directory: state.Input.Directory, Stdout: state.OutPath, Stderr: state.ErrPath}
	if terminal(op.Status) {
		r.Duration = time.Since(start).Seconds()
		r.Error = state.TerminalError
	}
	if state.Result != nil {
		exit := state.Result.ExitCode
		r.ExitCode = &exit
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if err = f.Chmod(0600); err == nil {
		err = l.write(f, r)
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		l.commands[key] = r
		notice := logRecord{Stage: "operation", Event: status, Session: id, Operation: op.ID, Error: state.TerminalError}
		if op.Status == operation.StatusFailed {
			notice.Stack = string(debug.Stack())
		}
		err = l.write(l.file, notice)
	}
	return err
}

type boundaryError struct {
	stage string
	cause error
	stack string
}

func (e *boundaryError) Error() string { return e.stage + ": " + e.cause.Error() }
func (e *boundaryError) Unwrap() error { return e.cause }
func boundary(stage string, err error) error {
	if err == nil {
		return nil
	}
	var existing *boundaryError
	if errors.As(err, &existing) {
		return fmt.Errorf("%s: %w", stage, err)
	}
	return &boundaryError{stage, err, string(debug.Stack())}
}

// Preserve errors.Is/As while keeping terminal diagnostics redacted.
type safeError struct {
	error
	text string
}

func (e safeError) Error() string { return e.text }
func (e safeError) Unwrap() error { return e.error }

// Respond is the provider goroutine boundary, not an arbitrary-goroutine panic
// catcher. Recovery here lets the coordinator cancel/join existing operations.
type loggedAdapter struct {
	llm.Adapter
	logs *logs
	id   session.ID
}

// Mark local audit failures separately from recoverable provider/context errors.
type diagnosticWriteError struct{ error }

func (e *diagnosticWriteError) Unwrap() error { return e.error }

func (a loggedAdapter) Respond(ctx context.Context, r llm.Request, o llm.RequestOptions) (response llm.Response, result error) {
	if err := a.logs.event("provider", "request_started", a.id, "", nil); err != nil {
		return response, &diagnosticWriteError{err}
	}
	defer func() {
		if p := recover(); p != nil {
			result = boundary("provider panic", fmt.Errorf("%v", p))
		}
		event := "request_completed"
		if result != nil {
			event = "request_failed"
			result = boundary("provider", result)
		}
		if ctx.Err() != nil {
			event = "request_canceled"
		}
		if response.Transport != nil && result == nil && a.logs != nil {
			a.logs.mu.Lock()
			statsErr := a.logs.write(a.logs.file, logRecord{Stage: "provider", Event: "transport", Session: a.id, Transport: response.Transport})
			a.logs.mu.Unlock()
			if statsErr != nil {
				result = errors.Join(result, &diagnosticWriteError{statsErr})
			}
		}
		if err := a.logs.event("provider", event, a.id, "", result); err != nil {
			result = errors.Join(result, &diagnosticWriteError{err})
		}
	}()
	return a.Adapter.Respond(ctx, r, o)
}

// File lifecycle records contain paths/revisions/capture locations, never source
// text or replacements. Full file data lives only in private canonical captures.
func (l *logs) fileOperation(id session.ID, op operation.Operation) error {
	if op.Status == operation.StatusReady {
		return nil
	}
	state, err := operation.DecodeFileState(op)
	if err != nil {
		return err
	}
	key := string(id) + "/" + string(op.ID)
	l.mu.Lock()
	defer l.mu.Unlock()
	path := filepath.Join(l.directory, "commands", string(id), url.PathEscape(string(op.ID))+".jsonl")
	previous, known := l.commands[key]
	if !known {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line != "" {
				if err := json.Unmarshal([]byte(line), &previous); err != nil {
					return err
				}
			}
		}
	}
	status := operationState(op)
	if previous.Event == status {
		return nil
	}
	start := previous.Started
	if start.IsZero() {
		start = time.Now().UTC()
	}
	record := logRecord{Stage: "file", Event: status, Session: id, Operation: op.ID, Started: start, Command: state.Input.Action + ": " + state.Input.Path, Directory: filepath.Dir(state.Input.Path)}
	if terminal(op.Status) {
		record.Duration = time.Since(start).Seconds()
	}
	if state.Result != nil {
		record.Stdout = state.Result.DiffPath
		record.Error = state.Result.Error
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	err = l.write(f, record)
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	l.commands[key] = record
	return l.write(l.file, logRecord{Stage: "file", Event: status, Session: id, Operation: op.ID, Error: record.Error})
}
