package contextbuilder

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
)

func historyBuilder(t *testing.T, rounds int, size int) *builder {
	t.Helper()
	b := NewBuilder().(*builder)
	b.SetModel(llm.Model{ID: "test", ReasoningEffort: llm.ReasoningEffortXHigh})
	b.AddTool(llm.Tool{Name: "Bash", Parameters: map[string]any{"type": "object"}})
	for i := 0; i < rounds; i++ {
		addText(t, b, fmt.Sprintf("task-%d %s", i, strings.Repeat("абв", size)))
		b.Commit()
		b.AddModelResponse(llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: fmt.Sprintf("answer-%d", i)}}}})
	}
	return b
}
func addText(t *testing.T, b Builder, text string) {
	t.Helper()
	data, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AddExternalInput(inbox.Input{Kind: inbox.InputExternal, Payload: data}); err != nil {
		t.Fatal(err)
	}
}
func mustPlan(t *testing.T, b *builder, maxPrefixItems int) (*session.ContextCompaction, llm.Request) {
	t.Helper()
	p, r, err := b.PlanCompaction(maxPrefixItems)
	if err != nil || p == nil {
		t.Fatalf("plan = %+v, %v", p, err)
	}
	return p, r
}

func TestCompactionReplacesOnlyAnsweredPrefixAndKeepsInstructions(t *testing.T) {
	b := historyBuilder(t, 6, 300)
	addText(t, b, "unanswered exact text\n  indentation")
	before, _ := b.Build()
	plan, request := mustPlan(t, b, 0)
	afterPlan, _ := b.Build()
	if !reflect.DeepEqual(before, afterPlan) {
		t.Fatal("planning mutated context")
	}
	if len(request.Tools) != 0 || request.Model.ID != "test" || request.Model.ReasoningEffort != llm.ReasoningEffortLow {
		t.Fatal("summary can execute tools or changed model")
	}
	if strings.Contains(request.Input[1].Data.(llm.Message).Text, "task-4") {
		t.Fatal("last two responses summarized")
	}
	b.Commit() // Same submission boundary as a persisted compaction turn.
	addText(t, b, "arrived while summarizing")
	if err := b.ApplyCompaction(*plan, "Goal: keep the project working. Completed four tasks."); err != nil {
		t.Fatal(err)
	}
	after, _ := b.Build()
	if !reflect.DeepEqual(after.Request.Input[0], before.Request.Input[0]) {
		t.Fatal("system instructions replaced")
	}
	retained := before.Request.Input[plan.PrefixItems+1:]
	if !reflect.DeepEqual(after.Request.Input[2:len(after.Request.Input)-1], retained) {
		t.Fatal("retained suffix changed")
	}
	if got := after.Request.Input[len(after.Request.Input)-1].Data.(llm.Message).Text; got != "arrived while summarizing" {
		t.Fatal("concurrent input lost")
	}
	if b.Estimate(after.Request) >= b.Estimate(before.Request) {
		t.Fatal("context did not shrink")
	}
	if before.Request.Input[1].Data.(llm.Message).Text == after.Request.Input[1].Data.(llm.Message).Text {
		t.Fatal("old built request was mutated")
	}
}

func TestCompactionProtectsActiveAndCrossBoundaryToolPairs(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			b := historyBuilder(t, 3, 300)
			b.Commit()
			b.AddModelResponse(llm.Response{Output: []llm.Item{
				{Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{"private"}}},
				{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: "active", Name: "Bash", Arguments: `{"command":"test"}`}},
			}})
			callStart := len(b.committedPrefix) - 2
			b.AddToolResult("active", nil, true)
			for range 4 {
				b.Commit()
				b.AddModelResponse(llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: "more progress"}}}})
			}
			if complete {
				b.AddToolResult("active", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "finished"}}, false)
			}
			p, _ := mustPlan(t, b, 0)
			if p.PrefixItems+1 > callStart {
				t.Fatal("prefix cut an active call or a pair spanning the suffix")
			}
			b.Commit()
			// Completion during the summary request stays a separately delivered result.
			if !complete {
				b.AddToolResult("active", []llm.ToolResultOutput{{Kind: llm.ToolResultText, Value: "finished"}}, false)
			}
			if err := b.ApplyCompaction(*p, "Earlier tasks complete."); err != nil {
				t.Fatal(err)
			}
			next, _ := b.Build()
			call, result := false, false
			for _, item := range next.Request.Input {
				switch v := item.Data.(type) {
				case llm.ToolCall:
					call = call || v.CallID == "active"
				case llm.ToolResult:
					result = result || (v.CallID == "active" && v.Output[0].Value == "finished")
				}
			}
			if !call || !result {
				t.Fatal("call/result pair lost")
			}
		})
	}
}

func TestCompactionRollsThroughOldPrefixAndReplays(t *testing.T) {
	b := historyBuilder(t, 30, 300)
	replay := historyBuilder(t, 30, 300)
	b.Commit()
	replay.Commit()
	before, _ := b.Build()
	originalTokens := b.Estimate(before.Request)
	count := 0
	for {
		p, r, err := b.PlanCompaction(0)
		if err != nil {
			t.Fatal(err)
		}
		if p == nil {
			break
		}
		if b.Estimate(r) >= originalTokens {
			t.Fatal("summary did not use a smaller historical chunk")
		}
		b.Commit()
		replay.Commit()
		// Different current system instructions must not invalidate an old checkpoint.
		replay.SetSystemPrompt("New workspace instructions")
		if err := b.ApplyCompaction(*p, "Completed earlier tasks; continue remaining work."); err != nil {
			t.Fatal(err)
		}
		if err := replay.ApplyCompaction(*p, "Completed earlier tasks; continue remaining work."); err != nil {
			t.Fatal(err)
		}
		a, _ := b.Build()
		z, _ := replay.Build()
		if !reflect.DeepEqual(a.Request.Input[1:], z.Request.Input[1:]) {
			t.Fatal("replay differs")
		}
		count++
		if count > 30 {
			t.Fatal("compaction did not converge")
		}
	}
	if count < 2 {
		t.Fatal("fixture did not exercise multiple checkpoints")
	}
	final, _ := b.Build()
	if b.Estimate(final.Request) >= originalTokens/2 {
		t.Fatal("large history did not shrink")
	}
}

func TestCompactionRejectsInvalidCheckpointsWithoutMutation(t *testing.T) {
	for _, kind := range []string{"hash", "version", "oversized", "empty", "count", "overflow", "boundary"} {
		t.Run(kind, func(t *testing.T) {
			b := historyBuilder(t, 6, 300)
			p, _ := mustPlan(t, b, 0)
			summary := "Good summary"
			switch kind {
			case "hash":
				p.PrefixHash = "bad"
			case "version":
				p.Version = 2
			case "oversized":
				summary = strings.Repeat("x", p.SummaryTokens*3+1)
			case "empty":
				summary = " "
			case "count":
				p.PrefixItems = -1
			case "overflow":
				p.PrefixItems = int(^uint(0) >> 1)
			case "boundary":
				p.PrefixItems--
				p.PrefixHash, _ = prefixHash(b.committedPrefix[1 : p.PrefixItems+1])
			}
			before, _ := b.Build()
			if err := b.ApplyCompaction(*p, summary); err == nil {
				t.Fatal("invalid checkpoint accepted")
			}
			after, _ := b.Build()
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed compaction mutated history")
			}
		})
	}
}

func TestCompactionSummaryRejectsPartialAndExecutableOutput(t *testing.T) {
	for _, response := range []llm.Response{
		{}, {Stop: llm.StopMaxOutputTokens}, {Stop: llm.StopRefused}, {Failure: &llm.Failure{Code: "failed"}},
		{Output: []llm.Item{{Type: llm.ItemToolCall, Data: llm.ToolCall{Name: "Bash"}}}},
		{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleUser, Text: "not a summary"}}}},
	} {
		if _, err := CompactionSummary(response); err == nil {
			t.Fatal("invalid response accepted")
		}
	}
	response := llm.Response{Output: []llm.Item{
		{Type: llm.ItemReasoning, Data: llm.Reasoning{Summary: []string{"private reasoning"}}},
		{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: " public handoff "}},
	}}
	text, err := CompactionSummary(response)
	if err != nil || text != "public handoff" {
		t.Fatalf("summary %q, %v", text, err)
	}
	history, err := summaryHistory(response.Output)
	if err != nil || strings.Contains(history, "private reasoning") {
		t.Fatal("reasoning included in text summary input")
	}
}

func TestContextEstimateIncludesSchemasUnicodeAndCalibratesUsage(t *testing.T) {
	b := NewBuilder().(*builder)
	ascii := EstimateTokens(llm.Request{Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: strings.Repeat("a", 100)}}}})
	unicode := EstimateTokens(llm.Request{Input: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Text: strings.Repeat("б", 100)}}}})
	if unicode <= ascii {
		t.Fatal("UTF-8 cost ignored")
	}
	r := llm.Request{Tools: []llm.Tool{{Description: strings.Repeat("schema", 1000)}}}
	if EstimateTokens(r) <= ascii {
		t.Fatal("schema cost ignored")
	}
	addText(t, b, "hello")
	b.Commit()
	before, _ := b.Build()
	raw := EstimateTokens(before.Request)
	b.AddModelResponse(llm.Response{Usage: llm.Usage{InputTokens: int64(raw * 2), CachedInputTokens: int64(raw)}})
	after, _ := b.Build()
	if b.Estimate(after.Request) < raw*2 {
		t.Fatal("provider calibration ignored, or cached tokens subtracted")
	}
}

func TestContextEstimateCannotOverflowFromProviderUsage(t *testing.T) {
	b := NewBuilder().(*builder)
	b.Commit()
	b.AddModelResponse(llm.Response{Usage: llm.Usage{InputTokens: 1<<63 - 1}})
	built, _ := b.Build()
	estimated := b.Estimate(built.Request)
	if estimated <= 10000000 || estimated*100 <= 0 {
		t.Fatal("provider usage overflow bypassed the guard")
	}
}
