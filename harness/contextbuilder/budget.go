package contextbuilder

import (
	"encoding/json/v2"
	"math"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

// Status reports context size, not a percentage of an assumed model limit.
// ApprovalID identifies one pending request for another compaction attempt.
type Status struct {
	EstimatedTokens   int
	LastInputTokens   int64
	CachedInputTokens int64
	Compacting        bool
	Compactions       int
	ApprovalID        string
}

// LimitError is recoverable by the interactive host. No history is deleted;
// the host should stop/join work but leave session selection and /new available.
type LimitError struct {
	Reason string
	Cause  error
}

func (e *LimitError) Error() string {
	if e.Cause != nil {
		return e.Reason + ": " + e.Cause.Error()
	}
	return e.Reason
}
func (e *LimitError) Unwrap() error { return e.Cause }

// Compactor is optional: existing Builder implementations need not implement it.
// Planning/validation are pure; ApplyCompaction is called only after a durable
// checkpoint and also during replay. All methods belong to the coordinator.
type Compactor interface {
	PlanCompaction(maxPrefixItems int) (*session.ContextCompaction, llm.Request, error)
	ValidateCompaction(session.ContextCompaction, string) error
	ApplyCompaction(session.ContextCompaction, string) error
	Estimate(llm.Request) int
	ContextUsage() (llm.Usage, int)
}

// EstimateTokens deliberately uses a conservative UTF-8 byte heuristic, including
// tool schemas and protocol overhead. It is NOT a tokenizer or a billing count.
// Image bytes are not text tokens; a fixed allowance is still only an estimate.
func EstimateTokens(request llm.Request) int {
	tokens := 256
	for _, item := range request.Input {
		tokens += estimateItem(item)
	}
	data, _ := json.Marshal(request.Tools)
	return tokens + (len(data)+2)/3
}
func estimateItem(item llm.Item) int {
	images := 0
	if result, ok := item.Data.(llm.ToolResult); ok {
		result.Output = append([]llm.ToolResultOutput(nil), result.Output...)
		for i := range result.Output {
			if result.Output[i].Kind == llm.ToolResultImage {
				images++
				result.Output[i].Value = "[image]"
			}
		}
		item.Data = result
	}
	data, _ := json.Marshal(item)
	return 16 + (len(data)+2)/3 + images*8192
}
func (b *builder) Estimate(r llm.Request) int {
	// Malformed provider usage must not overflow an informational estimate
	// into a negative value. Leave headroom for downstream display arithmetic.
	ceiling := float64(int(^uint(0)>>1) / 1024)
	return int(min(ceiling, math.Ceil(float64(EstimateTokens(r))*max(1, b.tokenScale))))
}
func (b *builder) ContextUsage() (llm.Usage, int) { return b.lastUsage, b.compactions }
