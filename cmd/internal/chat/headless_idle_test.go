package chat

import (
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
)

func TestCloseIfInactive(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name    string
		state   string
		age     time.Duration
		context contextbuilder.Status
		ops     []WebOperation
		pending bool
		want    bool
	}{
		{name: "idle at boundary", state: "idle", age: time.Hour, want: true},
		{name: "stopped", state: "stopped", age: 2 * time.Hour, want: true},
		{name: "error", state: "error", age: 2 * time.Hour, want: true},
		{name: "recent reply", state: "idle", age: time.Hour - time.Nanosecond},
		{name: "model working", state: "working", age: 2 * time.Hour},
		{name: "waiting", state: "waiting", age: 2 * time.Hour},
		{name: "compaction approval", state: "idle", age: 2 * time.Hour, context: contextbuilder.Status{ApprovalID: "approval"}},
		{name: "compacting", state: "idle", age: 2 * time.Hour, context: contextbuilder.Status{Compacting: true}},
		{name: "background tool", state: "idle", age: 2 * time.Hour, ops: []WebOperation{{State: "running"}}},
		{name: "joining tool", state: "idle", age: 2 * time.Hour, ops: []WebOperation{{State: "canceling"}}},
		{name: "shell approval", state: "idle", age: 2 * time.Hour, ops: []WebOperation{{State: "awaiting permission", ApprovalID: "approval"}}},
		{name: "completed tool", state: "idle", age: 2 * time.Hour, ops: []WebOperation{{State: "completed"}}, want: true},
		{name: "uncommitted input", state: "idle", age: 2 * time.Hour, pending: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			released := false
			h := &Headless{lastMessage: now.Add(-test.age), status: HeadlessStatus{State: test.state, Context: test.context, Operations: test.ops}, releaseSession: func() error { released = true; return nil }}
			if test.pending {
				h.inputs = map[string]*delivery{"pending": {}}
			}
			got, err := h.CloseIfInactive(now.Add(-time.Hour))
			if err != nil || got != test.want || released != test.want || h.closed != test.want {
				t.Fatalf("released=%v lease=%v closed=%v err=%v", got, released, h.closed, err)
			}
		})
	}
}

func TestCloseIfInactiveDoesNotWaitForAction(t *testing.T) {
	h := &Headless{lastMessage: time.Now().Add(-2 * time.Hour), status: HeadlessStatus{State: "idle"}}
	h.actions.Lock()
	defer h.actions.Unlock()
	if released, err := h.CloseIfInactive(time.Now().Add(-time.Hour)); released || err != nil {
		t.Fatalf("closed an action's owner: %v %v", released, err)
	}
}
