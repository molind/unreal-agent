package lineeditor

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestClearDraftPreservesHistoryStatusAndSelection(t *testing.T) {
	var wire bytes.Buffer
	term := NewTerminal(&wire, "you> ")
	if err := term.SetSize(24, 12); err != nil {
		t.Fatal(err)
	}
	term.History.Add("submitted")
	term.historyPending, term.historyIndex = "unsent earlier draft", 0
	term.line, term.pos = []rune(strings.Repeat("б", 50)), 7
	term.writePrompt()
	term.writeLine(term.line)
	term.moveCursorToPos(term.pos)
	term.outBuf = nil
	if err := term.SetStatus([]string{"task running"}); err != nil {
		t.Fatal(err)
	}
	cleared, err := term.ClearDraft()
	if err != nil || !cleared || len(term.line) != 0 || term.pos != 0 || term.cursorX != 5 || term.cursorY != 1 {
		t.Fatalf("clear failed: %v, %v; cursor=%d,%d", cleared, err, term.cursorX, term.cursorY)
	}
	if term.History.Len() != 1 || term.historyIndex != -1 || term.historyPending != "" || len(term.status) != 1 {
		t.Fatal("clear damaged history/status or retained pending history")
	}
	term.lock.Lock()
	term.handleKey(keyDown)
	term.lock.Unlock()
	if len(term.line) != 0 {
		t.Fatal("Down resurrected the discarded draft")
	}
	term.lock.Lock()
	term.handleKey(keyUp)
	term.lock.Unlock()
	if string(term.line) != "submitted" {
		t.Fatal("submitted history was lost")
	}
	if err := term.OpenSelection("Pick", []Choice{{Value: "one"}}); err != nil {
		t.Fatal(err)
	}
	if cleared, err := term.ClearDraft(); err != nil || !cleared || term.selection == nil {
		t.Fatal("clearing a draft changed the selection", err)
	}
	if cleared, err := term.ClearDraft(); err != nil || cleared {
		t.Fatal("empty input was reported as cleared", err)
	}
}

func TestClearDraftPartialKeysAndOutputFailure(t *testing.T) {
	var wire bytes.Buffer
	term := NewTerminal(&wire, "you> ")
	term.remainder = []byte{0xd0}
	if cleared, err := term.ClearDraft(); err != nil || !cleared || len(term.remainder) != 0 {
		t.Fatal("partial input retained", err)
	}
	term.line = []rune("draft")
	term.c = brokenWire{}
	if cleared, err := term.ClearDraft(); !cleared || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("redraw failure hidden", err)
	}
}
