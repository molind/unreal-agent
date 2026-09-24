package chat

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/internal/lineeditor"
)

func seedContextChat(t *testing.T, workspace string) *localfile.Store {
	t.Helper()
	store, err := localfile.New(filepath.Join(workspace, ".harness/sessions"))
	if err != nil {
		t.Fatal(err)
	}
	id := session.ID("context-case")
	if _, err := store.Create(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	var previous session.TurnID
	for i := 0; i < 7; i++ {
		turn := session.Turn{ID: session.TurnID(fmt.Sprintf("history-%d", i)), PreviousTurnID: previous, Type: session.TurnRegular}
		input := newInput(inbox.InputExternal, fmt.Sprintf("old-task-%d %s", i, strings.Repeat("x", 3500)))
		if err := store.AppendInput(t.Context(), id, input); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendTurn(t.Context(), id, turn); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendModelResponse(t.Context(), id, sessionstore.ModelResponse{TurnID: turn.ID, Response: reply(fmt.Sprintf("completed-%d", i))}); err != nil {
			t.Fatal(err)
		}
		previous = turn.ID
	}
	return store
}
func summaryCall(t *testing.T, c *chatTest) modelCall {
	t.Helper()
	call := c.call()
	if len(call.request.Tools) != 0 || len(call.request.Input) != 2 || !strings.Contains(call.request.Input[0].Data.(llm.Message).Text, "context-maintenance request") {
		t.Fatal("expected a tool-free summary request")
	}
	return call
}

func TestChatContextCompactionAndRestart(t *testing.T) {
	workspace := t.TempDir()
	store := seedContextChat(t, workspace)
	c := launch(t, workspace, false, "-session", "context-case")
	c.send("continue with the next task")
	c.call().failure <- overflowError()
	compact := summaryCall(t, c)
	c.wait("Compacting context")
	c.send("/status")
	c.wait("Context: ~")
	summary := "HANDOFF-CHECKPOINT: earlier tasks done; retain user constraints."
	compact.reply <- reply(summary)
	ordinary := c.call()
	if len(ordinary.request.Tools) == 0 {
		t.Fatal("did not return to ordinary tools")
	}
	user := strings.Join(messages(ordinary.request, llm.RoleUser), "\n")
	if !strings.Contains(user, summary) || !strings.Contains(user, "continue with the next task") || strings.Contains(user, "old-task-0") {
		t.Fatal("ordinary context did not use checkpoint plus pending message")
	}
	ordinary.reply <- reply("new task done")
	c.wait("new task done")
	c.wait("Context compacted:")
	c.send("/status")
	c.wait("compactions 1")
	if strings.Contains(c.output.snapshot(), summary) {
		t.Fatal("maintenance summary leaked into assistant transcript")
	}
	c.finish("/exit")
	page, err := store.Items(t.Context(), "context-case", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	originals := 0
	for _, item := range page.Items {
		if input, ok := item.Data.(inbox.Input); ok && strings.Contains(string(input.Payload), "old-task-") {
			originals++
		}
	}
	if originals != 7 {
		t.Fatal("compaction deleted original transcript")
	}
	c = launch(t, workspace, false, "-session", "context-case")
	c.send("after restart")
	ordinary = c.call()
	if len(ordinary.request.Tools) == 0 || !strings.Contains(strings.Join(messages(ordinary.request, llm.RoleUser), "\n"), summary) {
		t.Fatal("restart did not reuse summary")
	}
	ordinary.reply <- reply("restart done")
	c.wait("restart done")
	if strings.Contains(c.output.snapshot(), summary) {
		t.Fatal("replay exposed maintenance summary")
	}
	c.finish("/exit")
}

func overflowError() error {
	return &responsesapi.APIError{Code: "context_length_exceeded", Message: "provider context exhausted"}
}
func waitCount(t *testing.T, c *chatTest, text string, count int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(c.output.snapshot(), text) >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("missing %d occurrences of %q: %s", count, text, c.output.snapshot())
}

func TestChatContextFailuresKeepChatOpen(t *testing.T) {
	for _, kind := range []string{"oversized input", "summary failure", "summary network failure"} {
		t.Run(kind, func(t *testing.T) {
			workspace := t.TempDir()
			var args []string
			if kind != "oversized input" {
				seedContextChat(t, workspace)
				args = []string{"-session", "context-case"}
			}
			c := launch(t, workspace, false, args...)
			if kind == "oversized input" {
				c.send(strings.Repeat("x", 600000))
			} else {
				c.send("continue")
			}
			call := c.call()
			if len(call.request.Tools) == 0 {
				t.Fatal("compacted before a provider error")
			}
			call.failure <- overflowError()
			if kind != "oversized input" {
				call = summaryCall(t, c)
				if kind == "summary failure" {
					call.reply <- llm.Response{}
				} else {
					call.failure <- errors.New("summary connection failed")
				}
			}
			c.wait("Context work stopped:")
			c.send("/status")
			c.wait("Status: idle")
			select {
			case <-c.client.calls:
				t.Fatal("failed context kept issuing requests")
			default:
			}
			c.send("/new")
			c.wait("Selected session: new unsaved chat")
			c.send("fresh task")
			call = c.call()
			assertMessages(t, call.request, llm.RoleUser, "fresh task")
			call.reply <- reply("fresh answer")
			c.wait("fresh answer")
			c.finish("/exit")
		})
	}
}

func TestChatRepeatCompactionRequiresExplicitApproval(t *testing.T) {
	workspace := t.TempDir()
	seedContextChat(t, workspace)
	c := launch(t, workspace, false, "-session", "context-case")
	c.send("continue")
	c.call().failure <- overflowError()
	first := summaryCall(t, c)
	first.reply <- reply("First handoff: older tasks completed.")
	c.call().failure <- overflowError()
	c.wait("Context still too large.")
	c.send("/status")
	c.wait("Status: awaiting compaction approval")
	c.send("yes")
	c.wait("Message queued.")
	select {
	case <-c.client.calls:
		t.Fatal("ordinary yes bypassed approval")
	default:
	}
	c.send("/compact yes")
	second := summaryCall(t, c)
	second.reply <- reply("Second handoff: older work summarized further.")
	retried := c.call()
	if !strings.Contains(strings.Join(messages(retried.request, llm.RoleUser), "\n"), "yes") {
		t.Fatal("queued input lost")
	}
	retried.failure <- overflowError()
	waitCount(t, c, "Context still too large.", 2)
	select {
	case <-c.client.calls:
		t.Fatal("one approval authorized multiple compactions")
	default:
	}
	c.send("/compact no")
	c.wait("Further compaction declined.")
	c.finish("/exit")
	// Declining and restarting does not give the same overflow episode a new
	// automatic attempt. A new user message still is not compaction approval.
	c = launch(t, workspace, false, "-session", "context-case")
	c.send("continue without discarding more history")
	call := c.call()
	if len(call.request.Tools) == 0 {
		t.Fatal("restart launched an unapproved summary")
	}
	call.failure <- overflowError()
	c.wait("Context still too large.")
	select {
	case <-c.client.calls:
		t.Fatal("restart reset consent")
	default:
	}
	c.finish("/exit")
}

func TestChatSummaryOverflowAsksBeforeTryingSmallerPrefix(t *testing.T) {
	workspace := t.TempDir()
	seedContextChat(t, workspace)
	c := launch(t, workspace, false, "-session", "context-case")
	c.send("continue")
	c.call().failure <- overflowError()
	first := summaryCall(t, c)
	first.failure <- overflowError()
	c.wait("Context still too large.")
	select {
	case <-c.client.calls:
		t.Fatal("retried summary without approval")
	default:
	}
	c.send("/compact yes")
	smaller := summaryCall(t, c)
	if len(smaller.request.Input[1].Data.(llm.Message).Text) >= len(first.request.Input[1].Data.(llm.Message).Text) {
		t.Fatal("summary retry did not choose a smaller prefix")
	}
	smaller.reply <- reply("Oldest work completed.")
	call := c.call()
	call.reply <- reply("fits now")
	c.wait("fits now")
	c.send("/compact yes")
	c.wait("No compaction approval pending.")
	c.finish("/exit")
}

func TestContextConfigurationHasNoInventedWindow(t *testing.T) {
	workspace := t.TempDir()
	// Obsolete environment values cannot restrict otherwise valid requests.
	if _, err := parse([]string{workspace}, func(string) string { return "" }, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"-context-window", "-context-reserve", "-auto-compact"} {
		if _, err := parse([]string{name, "1", workspace}, func(string) string { return "" }, io.Discard); err == nil {
			t.Fatalf("obsolete flag %s still accepted", name)
		}
	}
	var help bytes.Buffer
	_, _ = parse([]string{"-h"}, func(string) string { return "" }, &help)
	if strings.Contains(help.String(), "128000") || strings.Contains(help.String(), "85%") {
		t.Fatal("invented context budget in help")
	}
	if !strings.Contains(help.String(), "/compact yes") {
		t.Fatal("approval command missing from help")
	}
}

func TestContextDisplayApprovalCountsAndDraft(t *testing.T) {
	var transcript, screen bytes.Buffer
	d := newDisplay(&transcript, func(string) string { return "" })
	d.ui = &terminalUI{editor: lineeditor.NewTerminal(&screen, "you> ")}
	d.promptInfo = "test model"
	s := contextbuilder.Status{EstimatedTokens: 6000, LastInputTokens: 5000, CachedInputTokens: 4000, ApprovalID: "request-1"}
	for range 3 {
		if err := d.context(s); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(transcript.String(), "Context still too large.") != 1 {
		t.Fatal("approval request spammed")
	}
	s.ApprovalID = ""
	s.Compacting = true
	if err := d.context(s); err != nil {
		t.Fatal(err)
	}
	s.Compacting = false
	s.Compactions = 1
	s.EstimatedTokens = 2500
	if err := d.context(s); err != nil {
		t.Fatal(err)
	}
	if err := d.status(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"~2500 tokens", "provider limit unknown", "5000 tokens (4000 cached, already included)", "Context compacted:"} {
		if !strings.Contains(transcript.String(), want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(transcript.String(), "%") {
		t.Fatal("percentage shown without a known limit")
	}
	d.reset()
	if d.contextKnown || d.contextStatus.ApprovalID != "" {
		t.Fatal("new session retained approval/metrics")
	}
}

func TestContextRecoveryDoesNotHideStorageFailures(t *testing.T) {
	limit := &contextbuilder.LimitError{Reason: "too big", Cause: errors.New("provider rejected")}
	if !recoverableContextError(fmt.Errorf("runtime: %w", errors.Join(limit))) {
		t.Fatal("lost recoverable error")
	}
	if recoverableContextError(&contextbuilder.LimitError{Reason: "summary failed", Cause: &diagnosticWriteError{errors.New("audit disk full")}}) {
		t.Fatal("hid provider-boundary audit failure inside compaction error")
	}
	if recoverableContextError(errors.Join(limit, errors.New("disk full"))) {
		t.Fatal("hid storage failure")
	}
}

func TestCompactionChooserScopesSelectionAndDoesNotHijackResume(t *testing.T) {
	var output, wire bytes.Buffer
	d := newDisplay(&output, func(string) string { return "" })
	d.ui = &terminalUI{editor: lineeditor.NewTerminal(&wire, "you> ")}
	d.contextStatus.ApprovalID = "pending-request"
	a := &application{display: d, runtime: &runtime{}}
	if err := d.ui.editor.OpenSelection("Resume", []lineeditor.Choice{{Value: "session-1", Label: "Saved session"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.offerCompactionMenu(); err != nil {
		t.Fatal(err)
	}
	if a.approvalMenuShown != "" {
		t.Fatal("approval displaced the resume picker")
	}
	if err := d.ui.editor.CloseSelection(); err != nil {
		t.Fatal(err)
	}
	if err := a.offerCompactionMenu(); err != nil {
		t.Fatal(err)
	}
	if a.approvalMenuShown != "pending-request" {
		t.Fatal("deferred chooser did not open")
	}
	// A stale selection cannot authorize a newer request or select a session.
	if exit, err := a.accept(line{selection: &lineeditor.Selection{Context: compactionMenuID("obsolete"), Value: "yes"}}); exit || err != nil {
		t.Fatal(exit, err)
	}
	if !strings.Contains(output.String(), "Ignored an obsolete compaction choice") {
		t.Fatal(output.String())
	}
	if exit, err := a.accept(line{selection: &lineeditor.Selection{Context: compactionMenuID("pending-request"), Canceled: true}}); exit || err != nil {
		t.Fatal(exit, err)
	}
	if d.contextStatus.ApprovalID != "pending-request" {
		t.Fatal("Escape cleared consent gate")
	}
}
