package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTTYInterruptClearsStopsThenExits(t *testing.T) {
	type call struct {
		ctx   context.Context
		body  string
		reply chan []any
	}
	requests := make(chan call, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := call{r.Context(), string(body), make(chan []any, 1)}
		requests <- c
		select {
		case response := <-c.reply:
			writeResponse(w, response)
		case <-r.Context().Done():
		}
	}))
	// Register before startTTY so process cleanup precedes closing the server
	// even when an assertion fails with a deliberately pending request.
	t.Cleanup(server.Close)
	workspace := t.TempDir()
	cli := startTTY(t, []string{"TERM=xterm"}, "-provider", "openai", "-base-url", server.URL, workspace)
	cli.wait("you> ")
	s := &pickerScreen{t, cli, newLiveScreen(40, 24)}
	s.resizeTo(80, 14)
	request := func() call {
		t.Helper()
		select {
		case c := <-requests:
			return c
		case <-time.After(8 * time.Second):
			t.Fatal("missing model request")
			return call{}
		}
	}
	cli.send("generate slowly\r")
	generating := request()
	cli.send("discard-this-draft беларускі\x1b[200~/tmp/path\x1b[201~\x1b[200~\nfolded\nblock\x1b[201~\x1b[D\x03")
	cli.wait("Draft cleared.")
	s.waitPrompt()
	s.draft(t, "", 0)
	select {
	case <-generating.ctx.Done():
		t.Fatal("clearing input canceled model generation")
	default:
	}
	if strings.Contains(cli.snapshot(), "Stopped.") {
		t.Fatal("clearing input stopped work")
	}
	cli.send("\x03")
	cli.wait("Stopped.")
	select {
	case <-generating.ctx.Done():
	case <-time.After(8 * time.Second):
		t.Fatal("empty-draft interrupt did not cancel the model")
	}
	// The chat remains usable after stopping; cleared text was never submitted.
	cli.send("start shell\r")
	tools := request()
	if lastUserText(t, tools.body) != "start shell" || strings.Contains(tools.body, "discard-this-draft") || strings.Contains(tools.body, "folded") {
		t.Fatal("cleared draft reached the model", tools.body)
	}
	tools.reply <- []any{toolOutput("interrupt-task", "echo $$ > interrupt.pid; exec sleep 60")}
	until := time.Now().Add(8 * time.Second)
	for {
		if b, err := os.ReadFile(filepath.Join(workspace, "interrupt.pid")); err == nil && len(b) != 0 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("shell did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Three keys in one write must not be coalesced or read past the stop/join.
	cli.send("second draft\x03\x03\x03")
	cli.finish(0)
	assertPickerProcessGone(t, workspace, "interrupt.pid")
	if strings.Count(cli.snapshot(), "Draft cleared.") != 2 || strings.Count(cli.snapshot(), "Stopped.") != 2 {
		t.Fatalf("interrupt ladder was skipped or repeated: %s", tail(cli.snapshot(), 3000))
	}
	select {
	case c := <-requests:
		t.Fatal("interrupt submitted or resumed work", c.body)
	default:
	}
	pickerCount(t, workspace, 1)
}

func TestTTYInterruptIdleDraftAndChooser(t *testing.T) {
	for _, mode := range []string{"empty", "draft", "chooser"} {
		t.Run(mode, func(t *testing.T) {
			workspace := t.TempDir()
			wantSessions := 0
			if mode == "chooser" {
				seedPickerSessions(t, workspace, 2)
				wantSessions = 2
			}
			cli := startTTY(t, []string{"TERM=xterm", "NO_COLOR=1"}, "-provider", "ollama", workspace)
			cli.wait("you> ")
			s := &pickerScreen{t, cli, newLiveScreen(40, 24)}
			switch mode {
			case "draft":
				cli.send("   \x1b[200~one\ntwo\x1b[201~\x03")
				cli.wait("Draft cleared.")
				s.waitPrompt()
				s.draft(t, "", 0)
			case "chooser":
				cli.send("/resume\r")
				s.selected(1)
			}
			cli.send("\x03")
			cli.finish(0)
			pickerCount(t, workspace, wantSessions)
		})
	}
}
