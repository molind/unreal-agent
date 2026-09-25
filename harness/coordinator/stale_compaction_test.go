package coordinator

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

func staleReplayFixture(t *testing.T, recover bool, warning func(session.TurnID, error) error) (*coordinator, sessionstore.Item) {
	t.Helper()
	builder := contextbuilder.NewBuilder()
	c := New(Dependencies{ContextBuilder: builder, RecoverStaleCompactions: recover, OnCompactionSkipped: warning}).(*coordinator)
	for i := 0; i < 7; i++ {
		payload, _ := json.Marshal(fmt.Sprintf("original-task-%d %s", i, strings.Repeat("x", 3500)))
		turn := session.TurnID(fmt.Sprintf("t%d", i))
		for _, item := range []sessionstore.Item{
			{Kind: sessionstore.ItemInput, Data: inbox.Input{ID: inbox.ID(fmt.Sprintf("i%d", i)), Kind: inbox.InputExternal, Payload: payload}},
			{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: turn, Type: session.TurnRegular}},
			{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: turn, Response: textResponse("answered")}},
		} {
			if err := c.restoreItem(item); err != nil {
				t.Fatal(err)
			}
		}
	}
	plan, _, err := builder.(contextbuilder.Compactor).PlanCompaction(0)
	if err != nil || plan == nil {
		t.Fatal("no plan", err)
	}
	plan.PrefixHash = "stale-fixture-hash"
	if err = c.restoreItem(sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "compact", Type: session.TurnCompaction, Compaction: plan}}); err != nil {
		t.Fatal(err)
	}
	response := sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "compact", Response: textResponse("UNVERIFIED SUMMARY")}}
	return c, response
}
func assertFullReplay(t *testing.T, c *coordinator) {
	t.Helper()
	built, err := c.dependencies.ContextBuilder.Build()
	if err != nil {
		t.Fatal(err)
	}
	text := ""
	for _, item := range built.Request.Input {
		if m, ok := item.Data.(llm.Message); ok {
			text += m.Text
		}
	}
	if !strings.Contains(text, "original-task-0") || !strings.Contains(text, "original-task-6") || strings.Contains(text, "UNVERIFIED SUMMARY") {
		t.Fatal("full canonical context was not preserved")
	}
}
func TestStaleCompactionRecoveryIsReplayOnlyAndOptIn(t *testing.T) {
	for _, recover := range []bool{false, true} {
		warnings := 0
		c, item := staleReplayFixture(t, recover, func(session.TurnID, error) error { warnings++; return nil })
		err := c.restoreItem(item)
		if recover {
			if err != nil || warnings != 1 {
				t.Fatal(warnings, err)
			}
		} else if !errors.Is(err, contextbuilder.ErrCompactionMismatch) || warnings != 0 {
			t.Fatal(warnings, err)
		}
		assertFullReplay(t, c)
	}
	c, item := staleReplayFixture(t, true, nil)
	if _, err := c.addItemToLocalState(item); !errors.Is(err, contextbuilder.ErrCompactionMismatch) {
		t.Fatal("live validation was bypassed", err)
	}
	assertFullReplay(t, c)
}
func TestStaleCompactionWarningFailureRemainsFatal(t *testing.T) {
	want := errors.New("audit failure")
	c, item := staleReplayFixture(t, true, func(session.TurnID, error) error { return want })
	if err := c.restoreItem(item); !errors.Is(err, want) {
		t.Fatal(err)
	}
	assertFullReplay(t, c)
}
func TestValidLaterCheckpointStillAppliesAfterStaleOne(t *testing.T) {
	c, item := staleReplayFixture(t, true, nil)
	if err := c.restoreItem(item); err != nil {
		t.Fatal(err)
	}
	plan, _, err := c.dependencies.ContextBuilder.(contextbuilder.Compactor).PlanCompaction(0)
	if err != nil || plan == nil {
		t.Fatal(err)
	}
	if err = c.restoreItem(sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: "new-compact", Type: session.TurnCompaction, Compaction: plan}}); err != nil {
		t.Fatal(err)
	}
	if err = c.restoreItem(sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: "new-compact", Response: textResponse("VERIFIED NEW HANDOFF")}}); err != nil {
		t.Fatal(err)
	}
	built, err := c.dependencies.ContextBuilder.Build()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range built.Request.Input {
		if m, ok := item.Data.(llm.Message); ok && strings.Contains(m.Text, "VERIFIED NEW HANDOFF") {
			found = true
		}
	}
	if !found {
		t.Fatal("a later valid checkpoint was skipped")
	}
}
