package coordinator

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"uuid"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Reconstructed from durable overflow/approval controls, compaction turns and
// ordinary responses. Only a successful ordinary response ends an overflow
// episode. A stop, new input or process restart must not grant another attempt.
type contextRecovery struct {
	used            bool
	needsCompaction bool
	approvalID      string
	maxPrefixItems  int
}

func (c *coordinator) applyContextControl(control inbox.ControlMessage) {
	switch control.Mode {
	case inbox.ContextOverflow:
		p := control.Parameters.(inbox.ContextRecovery)
		if p.FailedPrefixItems > 0 {
			c.recovery.maxPrefixItems = p.FailedPrefixItems - 1
			if c.recovery.maxPrefixItems == 0 {
				c.recovery.maxPrefixItems = -1
			}
		}
		if !c.recovery.used {
			c.recovery.used, c.recovery.needsCompaction = true, true
		} else {
			c.recovery.approvalID, c.recovery.needsCompaction = p.RequestID, false
		}
	case inbox.ApproveCompaction:
		p := control.Parameters.(inbox.ContextRecovery)
		if c.recovery.approvalID != "" && p.RequestID == c.recovery.approvalID && !c.state.discardPending {
			c.recovery.approvalID, c.recovery.needsCompaction = "", true
		}
	case inbox.StopAndDiscard, inbox.StopHard:
		c.recovery.needsCompaction, c.recovery.approvalID = false, ""
	}
}

func (c *coordinator) contextEstimate(request llm.Request) int {
	if b, ok := c.dependencies.ContextBuilder.(contextbuilder.Compactor); ok {
		return b.Estimate(request)
	}
	return contextbuilder.EstimateTokens(request)
}
func (c *coordinator) notifyContext() error {
	if c.dependencies.OnContextChange == nil {
		return nil
	}
	built, err := c.dependencies.ContextBuilder.Build()
	if err != nil {
		return err
	}
	status := contextbuilder.Status{
		EstimatedTokens: c.contextEstimate(built.Request),
		Compacting:      c.cancelModel != nil && c.state.currentTurnType == session.TurnCompaction,
		ApprovalID:      c.recovery.approvalID,
	}
	if b, ok := c.dependencies.ContextBuilder.(contextbuilder.Compactor); ok {
		usage, count := b.ContextUsage()
		status.LastInputTokens, status.CachedInputTokens, status.Compactions = usage.InputTokens, usage.CachedInputTokens, count
	}
	if !c.contextNotified || status != c.contextStatus {
		c.contextStatus, c.contextNotified = status, true
		c.dependencies.OnContextChange(status)
	}
	return nil
}

func (c *coordinator) recoveryRequest(request llm.Request) (llm.Request, *session.ContextCompaction, error) {
	if !c.dependencies.RecoverContext || !c.recovery.needsCompaction {
		return request, nil, nil
	}
	if b, ok := c.dependencies.ContextBuilder.(contextbuilder.Compactor); ok {
		plan, compactRequest, err := b.PlanCompaction(c.recovery.maxPrefixItems)
		if err != nil {
			return llm.Request{}, nil, err
		}
		if plan != nil {
			return compactRequest, plan, nil
		}
	}
	return llm.Request{}, nil, &contextbuilder.LimitError{Reason: "provider rejected the context, but no safely compactable older prefix remains. Recent/unanswered messages and active call/result pairs were not discarded. Use /new with a shorter handoff"}
}

// A checkpoint is validated and persisted BEFORE replacing the in-memory prefix.
// Replay uses exactly the same descriptor and summary, never a new model call.
func (c *coordinator) saveCompaction(ctx context.Context, response sessionstore.ModelResponse, plan session.ContextCompaction) error {
	b, ok := c.dependencies.ContextBuilder.(contextbuilder.Compactor)
	if !ok {
		return fmt.Errorf("context builder cannot restore compaction checkpoints")
	}
	text, err := contextbuilder.CompactionSummary(response.Response)
	if err == nil {
		err = b.ValidateCompaction(plan, text)
	}
	if err != nil {
		return &contextbuilder.LimitError{Reason: "context compaction failed; original history retained", Cause: err}
	}
	item := sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: response}
	if err := c.storeItemInSessionStore(ctx, item); err != nil {
		return err
	}
	_, err = c.addItemToLocalState(item)
	return err
}

func (c *coordinator) contextFailure(ctx context.Context, err error) (handled bool, result error) {
	if !c.dependencies.RecoverContext {
		return false, nil
	}
	var limit interface{ ContextLimitExceeded() bool }
	if errors.As(err, &limit) && limit.ContextLimitExceeded() {
		params := inbox.ContextRecovery{RequestID: string(c.state.currentTurnID)}
		if plan, ok := c.contextPlans[c.state.currentTurnID]; ok {
			params.FailedPrefixItems = plan.PrefixItems
		}
		// Record the gate before notifying the UI. Restart/replay must not reset it.
		input := newContextControl(inbox.ContextOverflow, params)
		item := sessionstore.Item{Kind: sessionstore.ItemInput, Data: input}
		if err := c.storeItemInSessionStore(ctx, item); err != nil {
			return true, err
		}
		if _, err := c.addItemToLocalState(item); err != nil {
			return true, err
		}
		c.state.callModel = c.recovery.needsCompaction
		return true, nil
	}
	if _, compacting := c.contextPlans[c.state.currentTurnID]; compacting {
		return true, &contextbuilder.LimitError{Reason: "context compaction request failed; original history retained. Use /new or send a new message after fixing the cause", Cause: err}
	}
	return false, nil
}

func newContextControl(mode inbox.ControlMode, params inbox.ContextRecovery) inbox.Input {
	payload, err := json.Marshal(inbox.ControlMessage{Mode: mode, Parameters: params})
	if err != nil {
		panic(err)
	} // fixed structs containing strings and integers
	return inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload}
}
