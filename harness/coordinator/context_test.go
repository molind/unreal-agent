package coordinator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func seedContextHistory(t *testing.T, run *stopTestRun) {
	t.Helper()
	var items []sessionstore.Item
	var previous session.TurnID
	for i := 0; i < 7; i++ {
		turn := session.Turn{ID: session.TurnID(fmt.Sprintf("history-%d", i)), PreviousTurnID: previous, Type: session.TurnRegular}
		items = append(items,
			sessionstore.Item{Kind: sessionstore.ItemInput, Data: externalEvent(t, 0, inbox.ID(fmt.Sprintf("input-%d", i)), fmt.Sprintf("old-task-%d %s", i, strings.Repeat("x", 3500)))},
			sessionstore.Item{Kind: sessionstore.ItemTurn, Data: turn},
			sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: turn.ID, Response: textResponse(fmt.Sprintf("completed-%d", i))}},
		)
		previous = turn.ID
	}
	// Keep any existing unfinished tool fixture after the historical prefix.
	if len(run.store.resume.Operations) > 0 {
		turn := run.store.items[0].Data.(session.Turn)
		turn.PreviousTurnID = previous
		run.store.items[0].Data = turn
		items = append(items, run.store.items...)
	}
	for i := range items {
		items[i] = storedItem(sessionstore.Sequence(i+1), items[i].Kind, items[i].Data)
	}
	run.store.items = items
	run.current.dependencies.RecoverContext = true
	// Existing checkpoint tests start after one real provider overflow, never a
	// guessed local threshold. The failed ordinary call has no response to persist.
	adapter := run.current.dependencies.LLM
	overflow := true
	run.current.dependencies.LLM = &fakeAdapter{respond: func(ctx context.Context, request llm.Request) (llm.Response, error) {
		if overflow {
			overflow = false
			return llm.Response{}, contextOverflowError()
		}
		return adapter.Respond(ctx, request, llm.RequestOptions{})
	}}
}
func persistContextHistory(t *testing.T, run *stopTestRun) *localfile.Store {
	t.Helper()
	store, err := localfile.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create(t.Context(), "session-1"); err != nil {
		t.Fatal(err)
	}
	run.current.dependencies.Sessions = store
	for _, item := range run.store.items {
		if err := run.current.storeItemInSessionStore(t.Context(), item); err != nil {
			t.Fatal(err)
		}
	}
	restoreTestRun(t, run, store)
	return store
}
func isSummary(request llm.Request) bool {
	return len(request.Tools) == 0 && len(request.Input) == 2 && strings.Contains(request.Input[0].Data.(llm.Message).Text, "context-maintenance request")
}

func TestContextCompactionDurableResumeAndPendingInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		seedContextHistory(t, run)
		store := persistContextHistory(t, run)
		var statuses []contextbuilder.Status
		run.current.dependencies.OnContextChange = func(s contextbuilder.Status) { statuses = append(statuses, s) }
		run.start(t)
		run.input(t, externalEvent(t, 0, "pending", "finish the current task"))
		settleContext()
		if len(run.calls) != 1 || !isSummary(run.calls[0].request) {
			t.Fatalf("expected maintenance request, got %d calls", len(run.calls))
		}
		if run.current.pendingInputs() != 1 {
			t.Fatal("summary consumed pending input")
		}
		run.respond(t, 0, textResponse("Earlier tasks complete. Continue with the current task."))
		if len(run.calls) != 2 || isSummary(run.calls[1].request) {
			t.Fatal("did not resume ordinary work after compaction")
		}
		ordinary := run.calls[1].request
		if len(ordinary.Input) >= 20 || run.current.pendingInputs() != 1 {
			t.Fatal("checkpoint did not shrink context or consumed input")
		}
		run.input(t, stopInput(t, "hard", inbox.StopHard))
		run.assertStopped(t)
		page, err := store.Items(t.Context(), "session-1", 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		oldInput, checkpoints := 0, 0
		for _, item := range page.Items {
			if turn, ok := item.Data.(session.Turn); ok && turn.Compaction != nil {
				checkpoints++
			}
			if input, ok := item.Data.(inbox.Input); ok && strings.Contains(string(input.Payload), "old-task-") {
				oldInput++
			}
		}
		if oldInput != 7 || checkpoints != 1 {
			t.Fatalf("original history/checkpoint lost: %d/%d", oldInput, checkpoints)
		}
		resumed := newStopTestRun(t, 0)
		resumed.current.dependencies.RecoverContext = true
		restoreTestRun(t, resumed, store)
		resumed.start(t)
		if len(resumed.calls) != 1 || isSummary(resumed.calls[0].request) || !reflect.DeepEqual(ordinary.Input, resumed.calls[0].request.Input) {
			t.Fatal("resume failed to reuse checkpoint verbatim")
		}
		resumed.respond(t, 0, textResponse("done"))
		resumed.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
		resumed.assertStopped(t)
		if resumed.current.pendingInputs() != 0 {
			t.Fatal("ordinary response did not deliver input")
		}
		compacting, compacted := false, false
		for _, s := range statuses {
			compacting = compacting || s.Compacting
			compacted = compacted || s.Compactions == 1
		}
		if !compacting || !compacted {
			t.Fatal("context observer missed lifecycle")
		}
	})
}

func TestContextCompactionDoesNotBlockToolCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 1)
		seedContextHistory(t, run)
		run.start(t)
		run.input(t, externalEvent(t, 0, "pending", "check progress"))
		settleContext()
		if len(run.calls) != 1 || !isSummary(run.calls[0].request) {
			t.Fatal("no maintenance request")
		}
		run.update(t, 0, operation.StatusCompleted)
		if len(run.calls) != 1 || len(run.store.savedOperations) != 1 {
			t.Fatal("completion interrupted summary or was not saved")
		}
		run.respond(t, 0, textResponse("Earlier tasks complete."))
		if len(run.calls) != 2 {
			t.Fatalf("requests %d", len(run.calls))
		}
		assertStopResult(t, run.calls[1].request, "call-0", "completed")
		callFound := false
		for _, item := range run.calls[1].request.Input {
			if v, ok := item.Data.(llm.ToolCall); ok && v.CallID == "call-0" {
				callFound = true
			}
		}
		if !callFound {
			t.Fatal("completed result lost its originating call")
		}
		run.respond(t, 1, textResponse("done"))
		run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

func TestContextCompactionStopAndQueuedSteering(t *testing.T) {
	for _, mode := range []string{"stop", "steer"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 0)
				seedContextHistory(t, run)
				store := persistContextHistory(t, run)
				run.current.dependencies.JoinModels = true
				overflow := true
				run.current.dependencies.LLM = &fakeAdapter{respond: func(ctx context.Context, r llm.Request) (llm.Response, error) {
					if overflow {
						overflow = false
						return llm.Response{}, contextOverflowError()
					}
					call := stopTestCall{ctx: ctx, request: r, response: make(chan llm.Response)}
					run.calls = append(run.calls, call)
					return <-call.response, nil // A late success must not survive a stop.
				}}
				run.start(t)
				run.input(t, externalEvent(t, 0, "pending", "do work"))
				settleContext()
				if mode == "stop" {
					run.input(t, stopInput(t, "stop", inbox.StopAndDiscard))
					if run.calls[0].ctx.Err() == nil {
						t.Fatal("stop did not cancel maintenance")
					}
					run.respond(t, 0, textResponse("late summary must not apply"))
				} else {
					run.input(t, externalEvent(t, 0, "steer", "change direction"))
					if run.calls[0].ctx.Err() != nil || len(run.calls) != 1 {
						t.Fatal("steering spent another maintenance attempt")
					}
					run.respond(t, 0, textResponse("Earlier work complete."))
					if len(run.calls) != 2 || isSummary(run.calls[1].request) {
						t.Fatal("no ordinary request after maintenance")
					}
					seen := false
					for _, item := range run.calls[1].request.Input {
						if m, ok := item.Data.(llm.Message); ok && m.Text == "change direction" {
							seen = true
						}
					}
					if !seen {
						t.Fatal("steering lost")
					}
					run.respond(t, 1, textResponse("done"))
					run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
				}
				run.assertStopped(t)
				page, err := store.Items(t.Context(), "session-1", 0, 1000)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range page.Items {
					if r, ok := item.Data.(sessionstore.ModelResponse); ok {
						for _, output := range r.Response.Output {
							if m, ok := output.Data.(llm.Message); ok && m.Text == "late summary must not apply" {
								t.Fatal("late summary persisted")
							}
						}
					}
				}
				resumed := newStopTestRun(t, 0)
				resumed.current.dependencies.RecoverContext = true
				restoreTestRun(t, resumed, store)
				resumed.start(t)
				if len(resumed.calls) != 0 {
					t.Fatal("resume restarted stopped/answered work")
				}
				resumed.input(t, stopInput(t, "idle-again", inbox.StopWhenIdle))
				resumed.assertStopped(t)
			})
		})
	}
}

func TestContextCompactionFailureKeepsOriginalPrefix(t *testing.T) {
	for _, failure := range []string{"empty", "oversized", "tool", "storage"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 0)
				seedContextHistory(t, run)
				run.start(t)
				run.input(t, externalEvent(t, 0, "pending", "do work"))
				settleContext()
				before, _ := run.current.dependencies.ContextBuilder.Build()
				response := textResponse("valid summary")
				var want error
				switch failure {
				case "empty":
					response = llm.Response{}
				case "oversized":
					response = textResponse(strings.Repeat("too large", 4000))
				case "tool":
					response = llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "evil", Name: "Bash", Arguments: `{"command":"exit 9"}`}}}}
				case "storage":
					want = errors.New("disk unavailable")
					run.store.appendModelResponseErr = want
				}
				run.respond(t, 0, response)
				err := <-run.done
				if err == nil {
					t.Fatal("failure ignored")
				}
				if want != nil && !errors.Is(err, want) {
					t.Fatal(err)
				}
				after, _ := run.current.dependencies.ContextBuilder.Build()
				if !reflect.DeepEqual(before.Request.Input, after.Request.Input) {
					t.Fatal("failed summary changed prefix")
				}
				if len(run.operations.adds) != 0 || len(run.calls) != 1 {
					t.Fatal("failed summary executed tools or started another request")
				}
			})
		})
	}
}

func contextOverflowError() error {
	return &responsesapi.APIError{Code: "context_length_exceeded", Message: "too many tokens"}
}
func settleContext() {
	for range 6 {
		synctest.Sleep(2 * slurpIdleTimeout)
		synctest.Wait()
	}
}

func TestContextSizeAloneNeverCompacts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.current.dependencies.RecoverContext = true
		run.start(t)
		run.input(t, externalEvent(t, 0, "huge", strings.Repeat("x", 600000)))
		if len(run.calls) != 1 || isSummary(run.calls[0].request) {
			t.Fatal("size heuristic blocked or compacted an ordinary request")
		}
		run.respond(t, 0, textResponse("accepted by provider"))
		run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

func TestContextApprovalGatePersistsAndIsRequestScoped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		seedContextHistory(t, run)
		store := persistContextHistory(t, run)
		run.current.dependencies.LLM = &fakeAdapter{respond: func(_ context.Context, r llm.Request) (llm.Response, error) {
			run.calls = append(run.calls, stopTestCall{request: r})
			if isSummary(r) {
				return textResponse("Old tasks completed."), nil
			}
			return llm.Response{}, contextOverflowError()
		}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { run.done <- run.current.Run(ctx) }()
		synctest.Wait()
		run.input(t, externalEvent(t, 0, "pending", "continue"))
		settleContext()
		id := run.current.recovery.approvalID
		if id == "" || len(run.calls) != 3 || !isSummary(run.calls[1].request) {
			t.Fatalf("not gated after one summary: %d requests, id %q", len(run.calls), id)
		}
		run.input(t, externalEvent(t, 0, "queued", "yes"), newContextControl(inbox.ApproveCompaction, inbox.ContextRecovery{RequestID: "wrong-request"}))
		settleContext()
		if len(run.calls) != 3 || run.current.recovery.approvalID != id {
			t.Fatal("ordinary text or stale approval bypassed the gate")
		}
		// Simulate process loss: no user stop marker is written by the coordinator.
		cancel()
		synctest.Wait()
		if err := <-run.done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		resumed := newStopTestRun(t, 0)
		resumed.current.dependencies.RecoverContext = true
		restoreTestRun(t, resumed, store)
		resumed.start(t)
		if len(resumed.calls) != 0 || resumed.current.recovery.approvalID != id {
			t.Fatal("restart reset the approval gate")
		}
		resumed.input(t, newContextControl(inbox.ApproveCompaction, inbox.ContextRecovery{RequestID: id}), newContextControl(inbox.ApproveCompaction, inbox.ContextRecovery{RequestID: id}))
		if len(resumed.calls) != 1 || !isSummary(resumed.calls[0].request) {
			t.Fatal("one approval did not authorize one summary")
		}
		resumed.respond(t, 0, textResponse("More old tasks completed."))
		if len(resumed.calls) != 2 || isSummary(resumed.calls[1].request) {
			t.Fatal("approved summary did not retry the ordinary request")
		}
		// A second failure after approval requires another fresh, matching approval.
		resumed.calls[1].failure <- contextOverflowError()
		settleContext()
		newID := resumed.current.recovery.approvalID
		resumed.input(t, newContextControl(inbox.ApproveCompaction, inbox.ContextRecovery{RequestID: id}), externalEvent(t, 0, "queued-again", "more context"))
		settleContext()
		if newID == "" || newID == id || len(resumed.calls) != 2 || resumed.current.recovery.approvalID != newID {
			t.Fatal("permission was reused after another overflow")
		}
		resumed.input(t, stopInput(t, "decline", inbox.StopAndDiscard))
		resumed.assertStopped(t)
		again := newStopTestRun(t, 0)
		again.current.dependencies.RecoverContext = true
		restoreTestRun(t, again, store)
		again.start(t)
		if len(again.calls) != 0 || !again.current.recovery.used || again.current.recovery.approvalID != "" {
			t.Fatal("decline resumed work or granted another automatic attempt")
		}
		again.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
		again.assertStopped(t)
	})
}

// Simulate losing the process after the durable summary, before the next
// ordinary turn can be recorded. Replay must apply the checkpoint once and
// deliver the still-pending input, not send the entire history again.
type failPostCompactionTurnStore struct {
	sessionstore.Store
	summarySaved bool
}

func (s *failPostCompactionTurnStore) AppendModelResponse(ctx context.Context, id session.ID, response sessionstore.ModelResponse) error {
	if err := s.Store.AppendModelResponse(ctx, id, response); err != nil {
		return err
	}
	s.summarySaved = true
	return nil
}
func (s *failPostCompactionTurnStore) AppendTurn(ctx context.Context, id session.ID, turn session.Turn) error {
	if s.summarySaved && turn.Type == session.TurnRegular {
		return errors.New("simulated failure after checkpoint")
	}
	return s.Store.AppendTurn(ctx, id, turn)
}
func TestContextReplayImmediatelyAfterDurableCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		seedContextHistory(t, run)
		store := persistContextHistory(t, run)
		run.current.dependencies.Sessions = &failPostCompactionTurnStore{Store: store}
		run.start(t)
		run.input(t, externalEvent(t, 0, "pending", "unfinished user task"))
		settleContext()
		run.respond(t, 0, textResponse("Earlier tasks done; preserve current task."))
		select {
		case err := <-run.done:
			if err == nil || !strings.Contains(err.Error(), "simulated failure") {
				t.Fatal(err)
			}
		default:
			t.Fatal("post-checkpoint failure did not exit")
		}
		resumed := newStopTestRun(t, 0)
		resumed.current.dependencies.RecoverContext = true
		restoreTestRun(t, resumed, store)
		resumed.start(t)
		if len(resumed.calls) != 1 || isSummary(resumed.calls[0].request) {
			t.Fatal("checkpoint was not reused")
		}
		summary, pending := false, false
		for _, item := range resumed.calls[0].request.Input {
			if m, ok := item.Data.(llm.Message); ok {
				summary = summary || strings.Contains(m.Text, "Earlier tasks done; preserve current task.")
				pending = pending || m.Text == "unfinished user task"
			}
		}
		if !summary || !pending || resumed.current.pendingInputs() != 1 {
			t.Fatal("checkpoint lost pending delivery")
		}
		resumed.respond(t, 0, textResponse("done"))
		resumed.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
		resumed.assertStopped(t)
	})
}

func TestContextApprovalWaitStillRecordsToolCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 1)
		seedContextHistory(t, run)
		run.start(t)
		run.input(t, externalEvent(t, 0, "pending", "check work"))
		settleContext()
		run.respond(t, 0, textResponse("Earlier work summarized."))
		run.calls[1].failure <- contextOverflowError()
		settleContext()
		id := run.current.recovery.approvalID
		if id == "" {
			t.Fatal("missing approval gate")
		}
		run.update(t, 0, operation.StatusCompleted)
		settleContext()
		if len(run.calls) != 2 || len(run.store.savedOperations) != 1 {
			t.Fatal("tool completion bypassed gate or was not persisted")
		}
		run.input(t, newContextControl(inbox.ApproveCompaction, inbox.ContextRecovery{RequestID: id}))
		if len(run.calls) != 3 || !isSummary(run.calls[2].request) {
			t.Fatal("approval did not start exactly one maintenance request")
		}
		run.respond(t, 2, textResponse("More earlier work summarized."))
		assertStopResult(t, run.calls[3].request, "call-0", "completed")
		run.respond(t, 3, textResponse("done"))
		if run.current.recovery.used {
			t.Fatal("successful ordinary response did not end overflow episode")
		}
		run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

func TestContextGateWithoutRecoveryHostDoesNotHang(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		run.store.items = append(run.store.items,
			sessionstore.Item{Kind: sessionstore.ItemInput, Data: newContextControl(inbox.ContextOverflow, inbox.ContextRecovery{RequestID: "first"})},
			sessionstore.Item{Kind: sessionstore.ItemInput, Data: newContextControl(inbox.ContextOverflow, inbox.ContextRecovery{RequestID: "second"})},
		)
		for i := range run.store.items {
			item := run.store.items[i]
			run.store.items[i] = storedItem(sessionstore.Sequence(i+1), item.Kind, item.Data)
		}
		run.start(t)
		select {
		case err := <-run.done:
			var limit *contextbuilder.LimitError
			if !errors.As(err, &limit) || !strings.Contains(err.Error(), "approval") {
				t.Fatal(err)
			}
		default:
			t.Fatal("noninteractive host hung on an unanswerable gate")
		}
	})
}

func TestContextOverflowInsideTerminalHTTPResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 0)
		adapter := run.current.dependencies.LLM
		seedContextHistory(t, run)
		run.current.dependencies.LLM = adapter
		run.start(t)
		run.input(t, externalEvent(t, 0, "pending", "continue"))
		run.respond(t, 0, llm.Response{Failure: &llm.Failure{Code: "context_length_exceeded", Message: "input too long"}})
		if len(run.calls) != 2 || !isSummary(run.calls[1].request) {
			t.Fatal("in-band failure was swallowed instead of compacting")
		}
		run.respond(t, 1, textResponse("Old work summarized."))
		run.respond(t, 2, textResponse("done"))
		run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}
