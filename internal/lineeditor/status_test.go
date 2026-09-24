package lineeditor

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestStatusBoundsDraftGrowthAndEnter(t *testing.T) {
	var wire bytes.Buffer
	term := NewTerminal(&wire, "you> ")
	if err := term.SetSize(20, 6); err != nil {
		t.Fatal(err)
	}
	// Seed the library-owned editing state, as ReadLine does, without an input
	// goroutine. All subsequent transitions use the actual editor key handlers.
	term.writePrompt()
	term.outBuf = nil
	rows := []string{"* one running", "+ two canceling", "- three running", ". four running", "* five running"}
	if err := term.SetStatus(rows); err != nil {
		t.Fatal(err)
	}
	if term.statusHeight != 4 || term.cursorY != 4 || !strings.Contains(wire.String(), "+2 more pending") {
		t.Fatalf("bounds: %q", wire.String())
	}
	if string(term.prompt) != "you> " {
		t.Fatal("status changed prompt")
	}
	wire.Reset()
	// Input growth takes rows away before a live frame can scroll offscreen.
	for range 37 {
		term.handleKey(98)
	}
	if term.statusHeight != 2 {
		t.Fatalf("growing draft did not reserve rows: %d", term.statusHeight)
	}
	// Home then resize exercises a cursor above the wrapped tail.
	term.handleKey(keyHome)
	if err := term.SetSize(12, 6); err != nil {
		t.Fatal(err)
	}
	if term.statusHeight != 1 || term.cursorY != 1 || term.cursorX != 5 {
		t.Fatalf("resized cursor/rows: %d %d %d", term.statusHeight, term.cursorY, term.cursorX)
	}
	term.outBuf = nil
	got, ok := term.handleKey(keyEnter)
	if !ok || got != strings.Repeat("b", 37) {
		t.Fatal("submission changed")
	}
	if term.statusHeight != 0 || strings.Contains(string(term.outBuf), "running") || strings.Contains(string(term.outBuf), "pending") {
		t.Fatalf("Enter retained live rows: %q", term.outBuf)
	}
	if len(term.status) != 5 {
		t.Fatal("submission lost pending tasks")
	}
}

func TestStatusAnimationIdempotenceClearingAndErrors(t *testing.T) {
	var wire bytes.Buffer
	term := NewTerminal(&wire, "you> ")
	term.writePrompt()
	term.outBuf = nil
	if err := term.SetStatus([]string{"* model pending"}); err != nil {
		t.Fatal(err)
	}
	wire.Reset()
	if err := term.SetStatus([]string{"* model pending"}); err != nil {
		t.Fatal(err)
	}
	if wire.Len() != 0 {
		t.Fatal("unchanged status redrawn")
	}
	if err := term.SetStatus([]string{"+ model pending"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wire.String(), "+ model pending") {
		t.Fatal("no animation")
	}
	wire.Reset()
	if err := term.SetStatus(nil); err != nil {
		t.Fatal(err)
	}
	if term.statusHeight != 0 || term.cursorY != 0 || term.cursorX != 5 || strings.Contains(wire.String(), "pending") {
		t.Fatal("idle clearing")
	}
	term.c = brokenWire{}
	if err := term.SetStatus([]string{"* failure"}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lost output error: %v", err)
	}
}

type brokenWire struct{}

func (brokenWire) Read([]byte) (int, error)  { return 0, io.EOF }
func (brokenWire) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestPromptInfoSharesViewportAndDisappearsOnSubmit(t *testing.T) {
	for _, color := range []bool{false, true} {
		var wire bytes.Buffer
		term := NewTerminal(&wire, "you> ")
		if err := term.SetSize(32, 6); err != nil {
			t.Fatal(err)
		}
		if err := term.SetPromptInfo("model / high | workspace", color); err != nil {
			t.Fatal(err)
		}
		term.writePrompt()
		term.outBuf = nil
		if err := term.SetStatus([]string{"one", "two", "three", "four", "five"}); err != nil {
			t.Fatal(err)
		}
		if term.statusHeight != 4 || term.cursorY != 4 || term.cursorX != 5 || !strings.Contains(wire.String(), "+3 more pending") {
			t.Fatalf("context did not reserve a row: %d,%d,%d %q", term.statusHeight, term.cursorX, term.cursorY, wire.String())
		}
		if strings.Contains(wire.String(), "\x1b[2m") != color || strings.Contains(wire.String(), "\x1b[1;36m") != color {
			t.Fatal("prompt/status styling did not respect the color setting")
		}
		for range 130 {
			term.handleKey('x')
		}
		if term.statusHeight != 0 {
			t.Fatal("context scrolled out when the draft filled the viewport")
		}
		term.handleKey(keyCtrlU)
		term.fitStatus(len(term.line))
		if term.statusHeight != 4 {
			t.Fatal("context did not return after draft shrink")
		}
		term.outBuf = nil
		term.handleKey(keyEnter)
		if term.statusHeight != 0 || strings.Contains(string(term.outBuf), "model") || strings.Contains(string(term.outBuf), "pending") {
			t.Fatalf("submitted context into transcript: %q", term.outBuf)
		}
	}
}

func TestPromptInfoSafeNarrowResizeAndSelection(t *testing.T) {
	var wire bytes.Buffer
	term := NewTerminal(&wire, "you> ")
	if err := term.SetPromptInfo("model\x1b[2J / Беларуская\u202e", true); err != nil {
		t.Fatal(err)
	}
	term.writePrompt()
	if strings.Contains(string(term.outBuf), "\x1b[2J") || strings.ContainsRune(string(term.outBuf), '\u202e') {
		t.Fatal("prompt metadata injected terminal controls")
	}
	for _, width := range []int{80, 12, 6, 2, 40} {
		if err := term.SetSize(width, 20); err != nil {
			t.Fatal(err)
		}
		if term.cursorX != 5%width || term.cursorY != term.statusHeight+5/width {
			t.Fatalf("styled prompt moved cursor at width %d: %d,%d", width, term.cursorX, term.cursorY)
		}
	}
	if err := term.OpenSelection("Pick", []Choice{{Value: "one", Label: "one"}}); err != nil {
		t.Fatal(err)
	}
	wire.Reset()
	if _, err := term.Write([]byte("message\n")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wire.String(), "model") || !strings.Contains(wire.String(), "Pick") {
		t.Fatal("prompt context interfered with selection")
	}
	if err := term.CloseSelection(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wire.String(), "model") {
		t.Fatal("prompt context did not return")
	}
	term.c = brokenWire{}
	if err := term.SetPromptInfo("changed", false); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lost context write failure: %v", err)
	}
}
