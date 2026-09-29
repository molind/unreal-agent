package chat

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

// Headless owns a selected session independently of any client connection.
// It shares the CLI's configuration, runtime, logging and session leases.
// Each owner has its own Store: store observers are deliberately single-owner.
type Headless struct {
	actions                 sync.Mutex
	mu                      sync.Mutex
	ctx                     context.Context
	config                  config
	store                   *localfile.Store
	release, releaseSession func() error
	client                  agentrunner.Client
	newClient               func() (agentrunner.Client, error)
	logs                    *logs
	safe                    func(string) string
	notify                  func()
	run                     *headlessRun
	closed                  bool
	status                  HeadlessStatus
	lastMessage             time.Time // Under mu; opening an owner starts a fresh grace period.
	inputs                  map[string]*delivery
	modelChoices            []llm.ModelOption // Under actions.
	modelsFetched           time.Time
	modelsWarning           string
	settingsID              inbox.ID // Pending durable receipt; under mu.
	settingsSaved           chan struct{}
}

type headlessRun struct {
	runtime *runtime
	done    chan struct{}
	err     error
}

type delivery struct {
	hash    [32]byte
	done    chan struct{}
	err     error
	durable bool
}

type HeadlessStatus struct {
	ModelSettings
	State      string                `json:"state"`
	Error      string                `json:"error,omitempty"`
	Warning    string                `json:"warning,omitempty"`
	Context    contextbuilder.Status `json:"context"`
	Operations []WebOperation        `json:"operations"`
}

// OpenHeadless acquires ownership but never starts tools or model requests.
// args are the same flags/workspace accepted by unreal_chat, including -session.
// notify must be nonblocking; it signals that clients should refresh durable data.
func OpenHeadless(ctx context.Context, args []string, getenv func(string) string, providers []agentrunner.Provider, notify func()) (_ *Headless, result error) {
	c, err := parse(args, getenv, io.Discard)
	if err != nil {
		return nil, err
	}
	if c.session == "" || c.storageFormat != "sqlite" {
		return nil, errors.New("headless sessions require a saved SQLite session")
	}
	s, release, err := openStorage(ctx, c)
	if err != nil {
		return nil, err
	}
	h := &Headless{ctx: ctx, config: c, store: s, release: release, safe: newDisplay(io.Discard, getenv).safe, notify: notify, inputs: make(map[string]*delivery), status: HeadlessStatus{State: "stopped"}, lastMessage: time.Now()}
	h.newClient = func() (agentrunner.Client, error) { return c.client(providers, getenv) }
	defer func() {
		if result != nil {
			result = errors.Join(result, h.Close())
		}
	}()
	_, h.releaseSession, err = openSession(ctx, s, c.session)
	if err != nil {
		return nil, err
	}
	h.logs = openDatabaseLogs(s.Database(), h.safe)
	// Retain payload hashes so a retry cannot reuse an ID for new text without
	// keeping another copy of every historical message in memory.
	var after sessionstore.Sequence
	for {
		page, err := s.Items(ctx, session.ID(c.session), after, 128)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if input, ok := item.Data.(inbox.Input); ok && input.Kind == inbox.InputExternal {
				var text string
				if err := json.Unmarshal(input.Payload, &text); err != nil {
					return nil, err
				}
				d := &delivery{hash: sha256.Sum256([]byte(text)), done: make(chan struct{}), durable: true}
				close(d.done)
				h.inputs[string(input.ID)] = d
			}
		}
		if !page.More {
			break
		}
		after = page.NextAfter
	}
	selected, err := savedModelSettings(ctx, s, session.ID(c.session))
	if err != nil {
		return nil, err
	}
	if selected.Model != "" {
		h.config.model, h.config.effort = selected.Model, string(selected.ReasoningEffort)
	}
	h.status.ModelSettings = ModelSettings{h.config.model, llm.ReasoningEffort(h.config.effort)}
	return h, nil
}

func (h *Headless) changed() {
	if h.notify != nil {
		h.notify()
	}
}

func (h *Headless) Status() HeadlessStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.status
	s.Operations = append([]WebOperation{}, s.Operations...)
	return s
}

// State is the lightweight sidebar projection; full status can contain previews.
func (h *Headless) State() string { h.mu.Lock(); defer h.mu.Unlock(); return h.status.State }

// start is called under actions. Completion of done joins every runtime effect.
func (h *Headless) start() error {
	if h.closed {
		return errors.New("session is closed")
	}
	if h.run != nil {
		select {
		case <-h.run.done:
			h.run = nil
		default:
			return nil
		}
	}
	if h.client == nil {
		return errors.New("provider is not initialized")
	}
	r, err := startRuntime(h.ctx, h.config, session.ID(h.config.session), h.store, loggedAdapter{h.client, h.logs, session.ID(h.config.session)}, h.logs)
	if err != nil {
		return err
	}
	run := &headlessRun{runtime: r, done: make(chan struct{})}
	h.run = run
	h.mu.Lock()
	h.status.State, h.status.Error = "working", ""
	h.mu.Unlock()
	go h.observe(run)
	h.changed()
	return nil
}

func (h *Headless) prepare() error {
	if h.closed {
		return errors.New("session is closed")
	}
	if h.client == nil {
		client, err := h.newClient()
		if err != nil {
			return err
		}
		h.client = client
	}
	return nil
}

func (h *Headless) observe(run *headlessRun) {
	defer close(run.done)
	for e := range run.runtime.events {
		h.mu.Lock()
		if e.context != nil {
			h.status.Context = *e.context
			if e.context.ApprovalID != "" {
				h.status.State = "waiting"
			}
		}
		if e.warning != "" {
			h.status.Warning = h.safe(e.warning)
		}
		if e.idle != nil {
			h.status.State = "working"
			if *e.idle {
				h.status.State = "idle"
			}
			if h.status.Context.ApprovalID != "" {
				h.status.State = "waiting"
			}
		}
		if e.item != nil {
			if input, ok := e.item.Data.(inbox.Input); ok && input.ID == h.settingsID && h.settingsSaved != nil {
				select {
				case h.settingsSaved <- struct{}{}:
				default:
				}
			}
			// Record activity with the status update: a just-completed reply must
			// not be reaped using an older history/sidebar timestamp.
			if _, ok := e.item.Data.(sessionstore.ModelResponse); ok {
				h.lastMessage = time.Now()
			}
			if input, ok := e.item.Data.(inbox.Input); ok && input.Kind == inbox.InputExternal {
				h.lastMessage = time.Now()
				if d := h.inputs[string(input.ID)]; d != nil && !d.durable {
					d.durable = true
					close(d.done)
				}
				h.status.State = "working"
				if h.status.Context.ApprovalID != "" {
					h.status.State = "waiting"
				}
			}
		}
		if e.op != nil {
			value := ProjectOperation(*e.op, h.safe)
			found := false
			for i := range h.status.Operations {
				if h.status.Operations[i].ID == value.ID {
					h.status.Operations[i] = value
					found = true
					break
				}
			}
			if !found {
				h.status.Operations = append(h.status.Operations, value)
			}
			// Completed operations are also in durable history. Keep a bounded
			// recent overlay for late terminal checkpoints, plus every live job.
			if len(h.status.Operations) > 100 {
				for i, op := range h.status.Operations {
					if op.State != "running" && op.State != "canceling" && op.ApprovalID == "" {
						h.status.Operations = append(h.status.Operations[:i], h.status.Operations[i+1:]...)
						break
					}
				}
			}
		}
		if h.status.State == "waiting" {
			h.status.State = "working"
		}
		if h.status.Context.ApprovalID != "" {
			h.status.State = "waiting"
		}
		for _, op := range h.status.Operations {
			if op.ApprovalID != "" {
				h.status.State = "waiting"
				break
			}
		}
		h.mu.Unlock()
		h.changed()
	}
	run.err = <-run.runtime.done
	h.mu.Lock()
	h.status.State = "stopped"
	h.status.Context.ApprovalID = ""
	h.status.Context.Compacting = false
	if run.err != nil {
		h.status.State, h.status.Error = "error", h.safe(run.err.Error())
	}
	for id, d := range h.inputs {
		if !d.durable {
			d.err = errors.New("runtime stopped before saving the message; retry with the same ID")
			close(d.done)
			delete(h.inputs, id)
		}
	}
	h.mu.Unlock()
	h.changed()
}

// Send acknowledges only a durable input. Canceling the caller's wait does not
// cancel the runtime or forget an accepted delivery. IDs survive server restart.
func (h *Headless) Send(ctx context.Context, id, text string) error {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) || strings.TrimSpace(text) == "" || len(text) > maxMessageBytes || !utf8.ValidString(text) {
		return errors.New("message requires an ID (at most 128 bytes) and nonempty UTF-8 text (at most 1 MiB)")
	}
	h.actions.Lock()
	defer h.actions.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.closed {
		return errors.New("session is closed")
	}
	h.mu.Lock()
	d := h.inputs[id]
	h.mu.Unlock()
	if d != nil {
		if d.hash != sha256.Sum256([]byte(text)) {
			return errors.New("message ID already belongs to different text")
		}
	} else {
		if err := h.prepare(); err != nil {
			return err
		}
		if err := h.start(); err != nil {
			return err
		}
		d = &delivery{hash: sha256.Sum256([]byte(text)), done: make(chan struct{})}
		h.mu.Lock()
		h.inputs[id] = d
		h.mu.Unlock()
		input := newInput(inbox.InputExternal, text)
		input.ID = inbox.ID(id)
		if err := h.run.runtime.inputs.Submit(h.ctx, input); err != nil {
			h.mu.Lock()
			delete(h.inputs, id)
			h.mu.Unlock()
			return err
		}
	}
	select {
	case <-d.done:
		return d.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Headless) Resume() error {
	h.actions.Lock()
	defer h.actions.Unlock()
	if err := h.prepare(); err != nil {
		return err
	}
	return h.start()
}

func (h *Headless) stop() error {
	if h.run == nil {
		return nil
	}
	r := h.run
	select {
	case <-r.done:
	default:
		if err := r.runtime.inputs.Submit(h.ctx, newInput(inbox.InputControl, inbox.ControlMessage{Mode: inbox.StopAndDiscard, Reason: "User stopped chat work"})); err != nil {
			r.runtime.cancel()
		}
		<-r.done
	}
	h.run = nil
	return r.err
}

func (h *Headless) Stop() error { h.actions.Lock(); defer h.actions.Unlock(); return h.stop() }

func (h *Headless) Approve(requestID string) error {
	h.actions.Lock()
	defer h.actions.Unlock()
	s := h.Status()
	if h.closed || h.run == nil || requestID == "" || requestID != s.Context.ApprovalID {
		return errors.New("compaction request is no longer pending")
	}
	return h.run.runtime.inputs.Submit(h.ctx, newInput(inbox.InputControl, inbox.ControlMessage{Mode: inbox.ApproveCompaction, Parameters: inbox.ContextRecovery{RequestID: requestID}}))
}

func (h *Headless) Permit(id, requestID string, allow bool) error {
	h.actions.Lock()
	defer h.actions.Unlock()
	if h.closed || h.run == nil {
		return errors.New("session is not running")
	}
	return h.run.runtime.operations.ResolveShellApproval(operation.ID(id), requestID, allow)
}

func (h *Headless) Cancel(id string) error {
	h.actions.Lock()
	defer h.actions.Unlock()
	if h.closed || h.run == nil {
		return errors.New("session is not running")
	}
	return h.run.runtime.operations.Cancel(operation.ID(id), "User canceled operation")
}

// CloseIfInactive releases resources without deleting history. The caller must
// also serialize owner lookup/removal with requests that can acquire this owner.
// Waiting for permission is not inactivity, nor is a live background operation.
func (h *Headless) CloseIfInactive(before time.Time) (bool, error) {
	if !h.actions.TryLock() {
		return false, nil
	}
	defer h.actions.Unlock()
	h.mu.Lock()
	eligible := !h.closed && !h.lastMessage.IsZero() && !h.lastMessage.After(before) &&
		(h.status.State == "idle" || h.status.State == "stopped" || h.status.State == "error") &&
		h.status.Context.ApprovalID == "" && !h.status.Context.Compacting
	for _, op := range h.status.Operations {
		if op.State == "running" || op.State == "canceling" || op.ApprovalID != "" {
			eligible = false
		}
	}
	for _, input := range h.inputs {
		if !input.durable {
			eligible = false
		}
	}
	h.mu.Unlock()
	if !eligible {
		return false, nil
	}
	return true, h.close()
}

func (h *Headless) Close() error {
	h.actions.Lock()
	defer h.actions.Unlock()
	return h.close()
}

// close is called under actions.
func (h *Headless) close() error {
	if h.closed {
		return nil
	}
	h.closed = true
	err := h.stop()
	if h.client != nil {
		err = errors.Join(err, h.client.Close())
	}
	if h.logs != nil {
		err = errors.Join(err, h.logs.close())
	}
	if h.releaseSession != nil {
		err = errors.Join(err, h.releaseSession())
	}
	if h.store != nil {
		err = errors.Join(err, h.store.Close())
	}
	if h.release != nil {
		err = errors.Join(err, h.release())
	}
	return err
}
