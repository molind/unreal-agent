package chat

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/coordinator"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/bash"
	"github.com/unreallabsai/unreal-agent/harness/tool/files"
	"github.com/unreallabsai/unreal-agent/harness/tool/viewimage"
)

type event struct {
	context *contextbuilder.Status
	idle    *bool
	item    *sessionstore.Item
	op      *operation.Operation
}

type runtime struct {
	// Application-owned projection of coordinator idle notifications. A newly
	// started runtime is conservatively busy until its first notification.
	idle          bool
	pendingInputs int // Submitted here but not yet observed as durable inputs.

	inputs     *inbox.Inbox
	operations *operation.LocalOperationManager
	cancel     context.CancelFunc
	events     chan event
	done       chan error
}

// SaveOperation is the observation boundary: the coordinator remains the ONLY
// consumer of manager updates, and all terminal notices follow durable writes.
type observedStore struct {
	sessionstore.Store
	emit func(event)
	logs *logs
}

func (s observedStore) SaveOperation(ctx context.Context, id session.ID, op operation.Operation) error {
	if err := s.Store.SaveOperation(ctx, id, op); err != nil {
		return err
	}
	if err := s.logs.command(id, op); err != nil {
		return boundary("command log", err)
	}
	s.emit(event{op: &op})
	return nil
}

func newInput(kind inbox.InputKind, value any) inbox.Input {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(err)
	} // Only strings and fixed control structs are used.
	return inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: kind, Payload: payload}
}

func startRuntime(parent context.Context, c config, id session.ID, store *localfile.Store, adapter llm.Adapter, log *logs) (*runtime, error) {
	restored, err := store.Resume(parent, id)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(c.directory, "operations", string(id))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	// Persist settings before restore so the very first recovered request uses
	// the selected effort, not the previous launch settings.
	if err := store.AppendInput(parent, id, newInput(inbox.InputControl, inbox.ControlMessage{
		Mode: inbox.UpdateSettings, Parameters: inbox.Settings{ReasoningEffort: llm.ReasoningEffort(c.effort)},
	})); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	inputs, err := inbox.New(ctx, restored.ExternalInputIDs)
	if err != nil {
		cancel()
		return nil, err
	}
	r := &runtime{inputs: inputs, operations: operation.NewLocalOperationManager(ctx), cancel: cancel, events: make(chan event, 128), done: make(chan error, 1)}
	emit := func(e event) {
		select {
		case r.events <- e:
		case <-ctx.Done():
		}
	}
	observer := store.AddObserver(func(_ session.ID, item sessionstore.Item) { emit(event{item: &item}) })
	fileConfig := files.Config{Directory: c.workspace, BaseDirectory: directory}
	registry := tool.NewRegistry(tool.StaticTranslators{
		Read: files.NewRead(fileConfig), Edit: files.NewEdit(fileConfig), Write: files.NewWrite(fileConfig),
		Bash:      bash.New(bash.Config{Shell: "/bin/sh", Directory: c.workspace, BaseDirectory: directory}),
		ViewImage: viewimage.New(viewimage.Config{Directory: c.workspace}),
	}, tool.BashName, tool.ViewImageName, tool.ReadName, tool.EditName, tool.WriteName)
	builder := contextbuilder.NewBuilder()
	builder.SetSystemPrompt(c.prompt)
	builder.SetModel(llm.Model{ID: c.model, ReasoningEffort: llm.ReasoningEffort(c.effort)})
	for _, definition := range registry.StaticDefinitions() {
		builder.AddTool(definition.Tool)
	}
	current := coordinator.New(coordinator.Dependencies{
		OnIdleChange:    func(idle bool) { emit(event{idle: &idle}) },
		RecoverContext:  true,
		OnContextChange: func(status contextbuilder.Status) { emit(event{context: &status}) },
		JoinModels:      true, SessionID: id, Inbox: inputs, Restored: restored,
		Sessions: observedStore{Store: store, emit: emit, logs: log}, ContextBuilder: builder,
		LLM: adapter, Tools: registry, Operations: r.operations,
	})
	go func() {
		err := func() (result error) {
			defer func() {
				if p := recover(); p != nil {
					result = boundary("coordinator panic", fmt.Errorf("%v", p))
				}
			}()
			return current.Run(ctx)
		}()
		err = errors.Join(err, log.event("coordinator", "ended", id, "", err))
		cancel()
		// Run has joined model calls. Cancel and join operations (including process
		// groups) and inbox before any session mutation or runtime replacement.
		for range r.operations.Updates() {
		}
		for range inputs.Output() {
		}
		store.RemoveObserver(observer)
		if err != nil {
			// An output/provider/context failure must not leave resumable live jobs.
			// All children are now joined; retire unfinished durable checkpoints.
			cleanup := context.Background()
			state, resumeErr := store.Resume(cleanup, id)
			if resumeErr == nil {
				for _, op := range state.Operations {
					if terminal(op.Status) {
						continue
					}
					op.Status = operation.StatusCanceled
					resumeErr = errors.Join(resumeErr, (observedStore{Store: store, emit: emit, logs: log}).SaveOperation(cleanup, id, op))
				}
				resumeErr = errors.Join(resumeErr, store.AppendInput(cleanup, id, newInput(inbox.InputControl, inbox.ControlMessage{Mode: inbox.StopAndDiscard, Reason: "Chat runtime ended"})))
			}
			err = errors.Join(err, resumeErr)
		}
		close(r.events)
		r.done <- err
	}()
	return r, nil
}

func terminal(status operation.Status) bool {
	return status == operation.StatusCompleted || status == operation.StatusFailed || status == operation.StatusCanceled
}
