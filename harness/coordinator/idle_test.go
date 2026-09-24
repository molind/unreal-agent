package coordinator

import (
	"slices"
	"testing"
	"testing/synctest"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/operation"
)

func TestIdleObserverIncludesPendingToolResultsAndFollowUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newToolGraceTestRun(t)
		var states []bool
		run.current.dependencies.OnIdleChange = func(idle bool) { states = append(states, idle) }
		run.start(t)
		if !slices.Equal(states, []bool{true}) {
			t.Fatalf("initial idle states: %v", states)
		}
		run.input(t, externalEvent(t, 0, "input", "run tools"))
		run.respond(t, 0, toolGraceResponse("A", "B"))
		updateToolGraceCall(t, run, "A", operation.StatusCompleted)
		if !slices.Equal(states, []bool{true, false}) {
			t.Fatalf("tool grace was reported idle: %v", states)
		}
		updateToolGraceCall(t, run, "B", operation.StatusCompleted)
		if !slices.Equal(states, []bool{true, false}) || len(run.calls) != 2 {
			t.Fatalf("pending follow-up was reported idle: %v", states)
		}
		run.respond(t, 1, textResponse("done"))
		if !slices.Equal(states, []bool{true, false, true}) {
			t.Fatalf("final idle states: %v", states)
		}
		run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}

func TestIdleObserverStartsBusyForRestoredWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		run := newStopTestRun(t, 1)
		var states []bool
		run.current.dependencies.OnIdleChange = func(idle bool) { states = append(states, idle) }
		run.start(t)
		if !slices.Equal(states, []bool{false}) {
			t.Fatalf("restored operation was reported idle: %v", states)
		}
		run.update(t, 0, operation.StatusCompleted)
		run.respond(t, 0, textResponse("done"))
		if !slices.Equal(states, []bool{false, true}) {
			t.Fatalf("restored completion states: %v", states)
		}
		run.input(t, stopInput(t, "idle", inbox.StopWhenIdle))
		run.assertStopped(t)
	})
}
