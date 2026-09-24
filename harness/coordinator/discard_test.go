package coordinator

import (
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func TestStopAndDiscardRetiresDeliveryAcrossReplay(t *testing.T) {
	for _, pending := range []int{0, 2} {
		t.Run(strconv.Itoa(pending), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, pending)
				store := persistTestRun(t, run)
				restoreTestRun(t, run, store)
				run.current.dependencies.JoinModels = true
				run.start(t)
				run.input(t, externalEvent(t, 0, "first", "unfinished request"))
				run.input(t, stopInput(t, "discard", inbox.StopAndDiscard))
				for index := range pending {
					run.update(t, index, operation.StatusCanceled)
				}
				run.assertStopped(t)
				if len(run.calls) != 1 || run.calls[0].ctx.Err() == nil || run.current.pendingInputs() != 0 {
					t.Fatal("discard did not retire pending delivery")
				}
				resumed := newStopTestRun(t, 0)
				restoreTestRun(t, resumed, store)
				resumed.start(t)
				if len(resumed.calls) != 0 || len(resumed.operations.adds) != 0 || resumed.current.pendingInputs() != 0 {
					t.Fatal("resume restarted interrupted work")
				}
				resumed.input(t, heartbeatInput(t, "ignored-heartbeat"))
				if len(resumed.calls) != 0 {
					t.Fatal("control restarted discarded work")
				}
				resumed.input(t, externalEvent(t, 1, "next", "next message"))
				if len(resumed.calls) != 1 || resumed.current.pendingInputs() != 1 {
					t.Fatal("new input was swallowed by discard marker")
				}
				resumed.respond(t, 0, textResponse("new answer"))
				resumed.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
				resumed.assertStopped(t)
				page, err := store.Items(t.Context(), "session-1", 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				responses := 0
				for _, item := range page.Items {
					if item.Kind == sessionstore.ItemModelResponse {
						responses++
					}
				}
				if responses != 2 {
					t.Fatalf("responses = %d; stop must not fabricate an answer", responses)
				}
			})
		})
	}
}

func TestDiscardRecoveryBeforeCancellationCheckpoints(t *testing.T) {
	for _, translated := range []bool{false, true} {
		t.Run(map[bool]string{false: "untranslated", true: "pending"}[translated], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 1)
				if !translated {
					run.store.items = run.store.items[:2]
				}
				store := persistTestRun(t, run)
				// Simulate termination just after the durable stop marker, before the
				// cancellation snapshots or even the tool translation were persisted.
				if err := store.AppendInput(t.Context(), "session-1", stopInput(t, "discard", inbox.StopAndDiscard)); err != nil {
					t.Fatal(err)
				}
				restoreTestRun(t, run, store)
				run.start(t)
				if len(run.calls) != 0 || len(run.operations.adds) != 0 || run.current.pendingInputs() != 0 || len(run.current.state.toolCalls) != 0 {
					t.Fatal("discard recovery launched work or left pending delivery")
				}
				run.input(t, externalEvent(t, 0, "new", "do something else"))
				if len(run.calls) != 1 {
					t.Fatal("new request not delivered")
				}
				run.respond(t, 0, textResponse("done"))
				run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
				run.assertStopped(t)
			})
		})
	}
}

// A new input can be persisted while cancellation is still in flight. It must
// reopen model delivery without reviving the calls preceding the durable stop.
func TestDiscardRecoveryWithLaterInput(t *testing.T) {
	for _, translated := range []bool{false, true} {
		t.Run(map[bool]string{false: "untranslated", true: "pending"}[translated], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 1)
				if !translated {
					run.store.items = run.store.items[:2]
				}
				store := persistTestRun(t, run)
				for _, input := range []inbox.Input{
					stopInput(t, "discard", inbox.StopAndDiscard),
					externalEvent(t, 0, "new", "do something else"),
				} {
					if err := store.AppendInput(t.Context(), "session-1", input); err != nil {
						t.Fatal(err)
					}
				}
				restoreTestRun(t, run, store)
				run.start(t)
				run.assertRunning(t)
				if len(run.operations.adds) != 0 || len(run.current.state.toolCalls) != 0 {
					t.Fatal("recovery revived pre-stop work")
				}
				if len(run.calls) != 1 || run.current.pendingInputs() != 1 {
					t.Fatal("recovery must deliver only the new input")
				}
				if translated {
					assertStopResult(t, run.calls[0].request, "call-0", string(operation.StatusCanceled))
				}
				run.respond(t, 0, textResponse("new answer"))
				run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
				run.assertStopped(t)
				// Recovery writes must survive another replay without another turn.
				resumed := newStopTestRun(t, 0)
				restoreTestRun(t, resumed, store)
				resumed.start(t)
				if len(resumed.calls) != 0 || len(resumed.operations.adds) != 0 || resumed.current.pendingInputs() != 0 {
					t.Fatal("second recovery revived discarded work or delivered input")
				}
				resumed.input(t, stopInput(t, "idle-again", inbox.StopWhenIdle))
				resumed.assertStopped(t)
			})
		})
	}
}

func TestDiscardPreservesInputAcceptedDuringCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 1)
		store := persistTestRun(t, run)
		restoreTestRun(t, run, store)
		run.start(t)
		run.input(t, stopInput(t, "discard", inbox.StopAndDiscard), externalEvent(t, 0, "new", "new work"))
		run.assertRunning(t)
		if len(run.calls) != 0 || len(run.operations.cancels) != 1 {
			t.Fatal("new input interrupted the stop")
		}
		run.update(t, 0, operation.StatusCanceled)
		run.assertStopped(t)
		if run.current.pendingInputs() != 1 {
			t.Fatal("discarded completion changed delivery of the later user input")
		}
		resumed := newStopTestRun(t, 0)
		restoreTestRun(t, resumed, store)
		resumed.start(t)
		if len(resumed.calls) != 1 || len(resumed.operations.adds) != 0 || resumed.current.pendingInputs() != 1 {
			t.Fatal("replay lost the later input or revived discarded work")
		}
		assertStopResult(t, resumed.calls[0].request, "call-0", string(operation.StatusCanceled))
		resumed.respond(t, 0, textResponse("new answer"))
		resumed.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
		resumed.assertStopped(t)
	})
}

func TestDiscardRecoveryPreservesLaterOperationsAndTerminalCheckpoints(t *testing.T) {
	for _, status := range []operation.Status{operation.StatusReady, operation.StatusAwaiting, operation.StatusCanceling, operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled} {
		t.Run(string(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 1)
				store := persistTestRun(t, run)
				if err := store.AppendInput(t.Context(), "session-1", stopInput(t, "discard", inbox.StopAndDiscard)); err != nil {
					t.Fatal(err)
				}
				old := run.store.resume.Operations[0]
				old.Status = status
				if err := store.SaveOperation(t.Context(), "session-1", old); err != nil {
					t.Fatal(err)
				}
				if err := store.AppendInput(t.Context(), "session-1", externalEvent(t, 0, "new", "new work")); err != nil {
					t.Fatal(err)
				}
				turn := session.Turn{ID: "new-turn", PreviousTurnID: "turn-1", Type: session.TurnRegular}
				if err := store.AppendTurn(t.Context(), "session-1", turn); err != nil {
					t.Fatal(err)
				}
				response := run.store.items[1].Data.(sessionstore.ModelResponse)
				call := response.Response.Output[0].Data.(llm.ToolCall)
				call.CallID = "new-call"
				response = sessionstore.ModelResponse{TurnID: turn.ID, Response: llm.Response{Output: []llm.Item{{Type: llm.ItemToolCall, Data: call}}}}
				if err := store.AppendModelResponse(t.Context(), "session-1", response); err != nil {
					t.Fatal(err)
				}
				pending := run.store.items[2].Data.(sessionstore.ToolCallStatus)
				pending.TurnID, pending.CallID = turn.ID, call.CallID
				next := run.store.resume.Operations[0]
				next.ID = "new-operation"
				pending.Status.WaitingFor = []operation.ID{next.ID}
				pending.Operations = []operation.Operation{next}
				if err := store.AppendToolCallStatus(t.Context(), "session-1", pending); err != nil {
					t.Fatal(err)
				}
				restoreTestRun(t, run, store)
				run.start(t)
				if len(run.operations.adds) != 1 || run.operations.adds[0].ID != next.ID || len(run.calls) != 0 || run.current.pendingInputs() != 0 {
					t.Fatal("discard affected new work or revived old work/delivery")
				}
				want := status
				if !operationIsTerminal(want) {
					want = operation.StatusCanceled
				}
				next.Status = operation.StatusCompleted
				run.operations.updates <- next
				synctest.Wait()
				synctest.Sleep(2 * slurpIdleTimeout)
				synctest.Wait()
				if len(run.calls) != 1 {
					t.Fatal("new tool result was not delivered")
				}
				assertStopResult(t, run.calls[0].request, "call-0", string(want))
				assertStopResult(t, run.calls[0].request, call.CallID, string(operation.StatusCompleted))
				run.respond(t, 0, textResponse("done"))
				run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
				run.assertStopped(t)
			})
		})
	}
}

func TestDiscardHeartbeatDoesNotPreventIdleStop(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(strconv.FormatBool(replay), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				run := newStopTestRun(t, 0)
				store := persistTestRun(t, run)
				if err := store.AppendInput(t.Context(), "session-1", stopInput(t, "discard", inbox.StopAndDiscard)); err != nil {
					t.Fatal(err)
				}
				heartbeat := heartbeatInput(t, "ignored-heartbeat")
				if replay {
					if err := store.AppendInput(t.Context(), "session-1", heartbeat); err != nil {
						t.Fatal(err)
					}
				}
				restoreTestRun(t, run, store)
				run.start(t)
				if !replay {
					run.input(t, heartbeat)
				}
				synctest.Sleep(time.Minute)
				synctest.Wait()
				if len(run.calls) != 0 || run.current.pendingInputs() != 0 {
					t.Fatal("heartbeat created work after discard")
				}
				built, err := run.current.dependencies.ContextBuilder.Build()
				if err != nil {
					t.Fatal(err)
				}
				if countHeartbeatMessages(built.Request) != 0 {
					t.Fatal("ignored heartbeat leaked into model context")
				}
				run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
				run.assertStopped(t)
			})
		})
	}
}
