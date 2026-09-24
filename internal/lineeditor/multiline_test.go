package lineeditor

import (
	"bytes"
	"strings"
	"testing"
)

func TestMultilineEditingAndHistory(t *testing.T) {
	for _, enter := range []string{"\x1b\r", "\x1b\n", "\x1b[13;3u", "\x1b[13;9u", "\x1b[27;3;13~", "\n"} {
		w := &fragmentedKeyWire{input: "first" + enter + "second\x1b[A!\x1b[B?\r"}
		term := NewTerminal(w, "you> ")
		if err := term.SetStatus([]string{"work running"}); err != nil {
			t.Fatal(err)
		}
		got, err := term.ReadLine()
		if err != nil || got != "first!\nsecond?" {
			t.Fatalf("%q => %q %v", enter, got, err)
		}
		if term.statusHeight != 0 {
			t.Fatal("submission retained a transient origin")
		}
		w.input = "\x10\x01X\x05Y\r"
		got, err = term.ReadLine()
		if err != nil || got != "Xfirst!\nsecond?Y" {
			t.Fatalf("multiline history: %q %v", got, err)
		}
		w.input = "a" + enter + "b\x1b[D\x7f\r"
		got, err = term.ReadLine()
		if err != nil || got != "ab" {
			t.Fatalf("newline backspace: %q %v", got, err)
		}
	}
}

func TestMultilineViewportAndAsyncWrite(t *testing.T) {
	var wire bytes.Buffer
	term := NewTerminal(&wire, "")
	if err := term.SetSize(20, 6); err != nil {
		t.Fatal(err)
	}
	term.line = []rune(strings.Repeat("row\n", 20) + "tail")
	term.pos = len(term.line)
	term.writePrompt()
	if term.draftTop == 0 || term.cursorY >= 5 {
		t.Fatal("draft not viewport bounded")
	}
	term.outBuf = nil
	if err := term.SetStatus([]string{"work running"}); err != nil {
		t.Fatal(err)
	}
	if term.statusHeight != 0 {
		t.Fatal("status displaced a tall draft")
	}
	term.lock.Lock()
	term.pos = len([]rune("row\n")) * 5
	term.moveCursorToPos(term.pos)
	term.lock.Unlock()
	if term.cursorX != 0 || term.cursorY != 0 {
		t.Fatal("did not scroll to row-start cursor")
	}
	if _, err := term.Write([]byte("async\n")); err != nil {
		t.Fatal(err)
	}
	if term.cursorX != 0 || term.cursorY != 0 || !strings.Contains(wire.String(), "async") {
		t.Fatal("async output lost multiline cursor")
	}
	if err := term.SetSize(14, 5); err != nil {
		t.Fatal(err)
	}
	if term.cursorY >= 4 {
		t.Fatal("resize lost viewport bounds")
	}
	if cleared, err := term.ClearDraft(); err != nil || !cleared || term.draftTop != 0 {
		t.Fatal("clear retained multiline viewport", err)
	}
}

func TestMultilineContinuationStartsAtColumnZero(t *testing.T) {
	var wire bytes.Buffer
	term := NewTerminal(&wire, "you> ")
	term.line = []rune("першы\n")
	term.pos = len(term.line)
	term.writePrompt()
	if term.cursorX != 0 || term.cursorY != 1 {
		t.Fatalf("newline cursor = %d,%d, want column zero on row two", term.cursorX, term.cursorY)
	}
	if got := string(term.layoutDraft().rows[1]); got != "" {
		t.Fatalf("synthetic continuation padding: %q", got)
	}
	if string(term.line) != "першы\n" {
		t.Fatal("layout changed source")
	}
}
