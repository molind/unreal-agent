package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
)

func TestTTYRollingContextApprovalMenu(t *testing.T) {
	for _, choice := range []string{"yes", "no", "escape-stop"} {
		t.Run(choice, func(t *testing.T) { testTTYContextMenu(t, choice) })
	}
}
func testTTYContextMenu(t *testing.T, choice string) {
	workspace := t.TempDir()
	store, err := localfile.New(filepath.Join(workspace, ".harness/sessions"))
	if err != nil {
		t.Fatal(err)
	}
	const id session.ID = "context-case"
	if _, err := store.Create(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	var previous session.TurnID
	for i := 0; i < 7; i++ {
		payload, _ := json.Marshal(fmt.Sprintf("old-task-%d %s", i, strings.Repeat("x", 3500)))
		if err := store.AppendInput(t.Context(), id, inbox.Input{ID: inbox.ID(fmt.Sprintf("user-%d", i)), Kind: inbox.InputExternal, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		turn := session.Turn{ID: session.TurnID(fmt.Sprintf("turn-%d", i)), PreviousTurnID: previous, Type: session.TurnRegular}
		if err := store.AppendTurn(t.Context(), id, turn); err != nil {
			t.Fatal(err)
		}
		response := llm.Response{Output: []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: fmt.Sprintf("completed-%d", i)}}}}
		if err := store.AppendModelResponse(t.Context(), id, sessionstore.ModelResponse{TurnID: turn.ID, Response: response}); err != nil {
			t.Fatal(err)
		}
		previous = turn.ID
	}
	type exchange struct {
		body     string
		response chan []any
		overflow chan struct{}
	}
	requests := make(chan exchange, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		call := exchange{body: string(b), response: make(chan []any, 1), overflow: make(chan struct{}, 1)}
		select {
		case requests <- call:
		case <-r.Context().Done():
			return
		}
		select {
		case <-call.overflow:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"context too large"}}`)
		case output := <-call.response:
			writeResponse(w, output)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	request := func() exchange {
		t.Helper()
		select {
		case r := <-requests:
			return r
		case <-time.After(8 * time.Second):
			t.Fatal("no request")
			return exchange{}
		}
	}
	cli := startTTY(t, []string{"TERM=xterm", "NO_COLOR=1"}, "-provider", "openai", "-base-url", server.URL, "-session", string(id), workspace)
	cli.wait("Type /help")
	screen := newLiveScreen(40, 24)
	waitScreen := func(text string) {
		t.Helper()
		until := time.Now().Add(8 * time.Second)
		for time.Now().Before(until) {
			screen.update(cli.snapshot())
			if strings.Contains(screen.text(), text) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("missing %q on screen:\n%s", text, screen.text())
	}
	waitScreen("ctx ~")
	cli.send("continue\r")
	original := request()
	if strings.Contains(original.body, "context-maintenance request") {
		t.Fatal("compacted before provider overflow")
	}
	original.overflow <- struct{}{}
	compact := request()
	if !strings.Contains(compact.body, "context-maintenance request") {
		t.Fatal("ordinary request sent instead of compaction")
	}
	waitScreen("compacting context")
	draft := "чарнавік ABC"
	cli.send(draft + "\x1b[D\x1b[D")
	time.Sleep(150 * time.Millisecond)
	screen.update(cli.snapshot())
	screen.draft(t, draft, 2)
	const summary = "PRIVATE-MAINTENANCE-HANDOFF: prior work complete."
	compact.response <- []any{messageOutput(summary)}
	ordinary := request()
	if !strings.Contains(ordinary.body, summary) || strings.Contains(ordinary.body, "old-task-0") {
		t.Fatal("wire request did not replace old prefix")
	}
	ordinary.overflow <- struct{}{}
	waitScreen("Compact again?")
	waitScreen("> 1 No - stop work")
	select {
	case <-requests:
		t.Fatal("unapproved additional request")
	default:
	}
	if choice == "no" {
		// Enter alone selects the safe default, never another compression.
		cli.send("\r")
		cli.wait("Further compaction declined.")
		time.Sleep(150 * time.Millisecond)
		screen.update(cli.snapshot())
		screen.draft(t, draft, 2)
		select {
		case <-requests:
			t.Fatal("default No sent another model request")
		default:
		}
		cli.send("\x03")
		cli.wait("Draft cleared.")
		cli.send("/exit\r")
		cli.finish(0)
		return
	}
	cli.send("\x1b")
	cli.wait("Compaction decision deferred.")
	waitScreen("ctx approval")
	time.Sleep(150 * time.Millisecond)
	screen.update(cli.snapshot())
	screen.draft(t, draft, 2)
	select {
	case <-requests:
		t.Fatal("Escape granted permission")
	default:
	}
	// Clearing the restored draft is not approval and does not stop work.
	cli.send("\x03")
	cli.wait("Draft cleared.")
	waitScreen("ctx approval")
	if choice == "escape-stop" {
		cli.send("\x03")
		cli.wait("Stopped.")
		select {
		case <-requests:
			t.Fatal("stop granted permission")
		default:
		}
		cli.send("/exit\r")
		cli.finish(0)
		return
	}
	cli.send("/compact\r")
	waitScreen("Compact again?")
	waitScreen("> 1 No - stop work")
	cli.send("\x1b[B\r")
	approved := request()
	if !strings.Contains(approved.body, "context-maintenance request") {
		t.Fatal("approval did not authorize summary")
	}
	cli.send(draft + "\x1b[D\x1b[D")
	approved.response <- []any{messageOutput("SECOND-HANDOFF: old work summarized again.")}
	next := request()
	next.response <- []any{messageOutput("ROLLING DONE")}
	waitScreen("ROLLING DONE")
	time.Sleep(150 * time.Millisecond)
	screen.update(cli.snapshot())
	screen.draft(t, draft, 2)
	if strings.Contains(cli.snapshot(), summary) {
		t.Fatal("maintenance reply displayed")
	}
	for _, row := range screen.history {
		if strings.HasPrefix(row, "── ") {
			t.Fatalf("context indicator escaped into scrollback: %q", row)
		}
	}
	cli.send("\x03")
	cli.wait("Draft cleared.")
	cli.send("/new\r")
	waitScreen("ctx --")
	cli.send("/exit\r")
	cli.finish(0)
}
