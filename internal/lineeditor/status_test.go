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
