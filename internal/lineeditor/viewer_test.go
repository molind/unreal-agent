package lineeditor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestViewerEscPreservesDraftCursorAndHistory(t *testing.T) {
	wire := &selectionWire{input: strings.NewReader("ignored\x1b[B\x1b[6~\x1b[H\x1b\x1bX\r")}
	term := NewTerminal(wire, "you> ")
	term.line, term.pos = []rune("чарнавік"), 3
	term.History.Add("previous")
	term.repaint(0)
	term.outBuf = nil
	if opened, err := term.OpenViewer("Status", strings.Repeat("long report\n", 100)); !opened || err != nil {
		t.Fatal(opened, err)
	}
	if _, err := term.ReadLine(); err == nil {
		t.Fatal("viewer keys submitted a message")
	} else {
		var closed *ViewerClosed
		if !errors.As(err, &closed) {
			t.Fatal(err)
		}
	}
	if string(term.line) != "чарнавік" || term.pos != 3 || term.History.Len() != 1 {
		t.Fatal("viewer changed draft/history")
	}
	if term.viewer != nil || !strings.Contains(wire.String(), "\x1b[?1049l") {
		t.Fatal("alternate screen not closed")
	}
	text, err := term.ReadLine()
	if err != nil || text != "чарXнавік" {
		t.Fatal(text, err)
	}
}
func TestViewerQueuesAsyncOutputAndDoesNotLoseStateOnResize(t *testing.T) {
	var wire bytes.Buffer
	term := NewTerminal(&wire, "you> ")
	term.line, term.pos = []rune("first\nsecond"), 8
	var text strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&text, "report line %03d long enough to wrap around\n", i)
	}
	_ = term.SetSize(30, 8)
	if _, err := term.OpenViewer("Status", text.String()); err != nil {
		t.Fatal(err)
	}
	if err := term.viewerKey(keyEnd); err != nil {
		t.Fatal(err)
	}
	for _, size := range [][2]int{{16, 4}, {1, 1}, {80, 20}, {30, 8}} {
		if err := term.SetSize(size[0], size[1]); err != nil {
			t.Fatal(err)
		}
		if term.viewer == nil || term.viewer.top < 0 || term.viewer.top >= len(term.viewer.rows) {
			t.Fatal("lost viewport")
		}
		if err := term.SetStatus([]string{"task running"}); err != nil {
			t.Fatal(err)
		}
	}
	wire.Reset()
	if n, err := term.Write([]byte("ASYNC-ONE\n")); err != nil || n != 10 {
		t.Fatal(n, err)
	}
	if _, err := term.Write([]byte("ASYNC-TWO\n")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wire.String(), "ASYNC-") {
		t.Fatal("async output overwrote viewer")
	}
	if _, err := term.CloseViewer(); err != nil {
		t.Fatal(err)
	}
	output := wire.String()
	if strings.Count(output, "ASYNC-ONE") != 1 || strings.Count(output, "ASYNC-TWO") != 1 || strings.Index(output, "ASYNC-ONE") > strings.Index(output, "ASYNC-TWO") {
		t.Fatal("lost/reordered queued output")
	}
	if string(term.line) != "first\nsecond" || term.pos != 8 || len(term.status) != 1 {
		t.Fatal("draft/status lost")
	}
}
func TestViewerBufferOverflowReturnsAllOutput(t *testing.T) {
	var wire bytes.Buffer
	term := NewTerminal(&wire, "you> ")
	if _, err := term.OpenViewer("Status", "body"); err != nil {
		t.Fatal(err)
	}
	first := strings.Repeat("x", ViewerLimit)
	if _, err := term.Write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := term.Write([]byte("END\n")); err != nil {
		t.Fatal(err)
	}
	if term.viewer != nil || term.viewerClosed == nil || !term.viewerClosed.Overflow {
		t.Fatal("queue not bounded")
	}
	if !strings.Contains(wire.String(), first) || !strings.Contains(wire.String(), "END") {
		t.Fatal("overflow lost output")
	}
}
func TestViewerEOFAndFailedCloseCanRestoreTerminal(t *testing.T) {
	wire := &selectionWire{input: strings.NewReader("\x04")}
	term := NewTerminal(wire, "you> ")
	if _, err := term.OpenViewer("Help", "body"); err != nil {
		t.Fatal(err)
	}
	if _, err := term.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if _, err := term.Write([]byte("pending\n")); err != nil {
		t.Fatal(err)
	}
	term.c = brokenWire{}
	if _, err := term.CloseViewer(); !errors.Is(err, io.ErrClosedPipe) || term.viewer == nil {
		t.Fatal("failed close lost restore state", err)
	}
	term.c = wire
	if _, err := term.CloseViewer(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wire.String(), "pending\r\n") || !strings.Contains(wire.String(), "\x1b[?1049l") {
		t.Fatal("cleanup did not restore/deliver output")
	}
}

func TestViewerMenusSafetyAndFailures(t *testing.T) {
	var wire bytes.Buffer
	term := NewTerminal(&wire, "you> ")
	if _, err := term.OpenViewer("Status", "Беларуская\x1b[2J\u202e界🙂\n"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(term.viewer.text), "\x1b") || !strings.Contains(string(term.viewer.text), "Беларуская") {
		t.Fatal("unsafe/unreadable report")
	}
	if opened, err := term.TryOpenSelection("approval", "Question", []Choice{{Value: "no"}}); opened || err != nil {
		t.Fatal("menu replaced viewer")
	}
	if err := term.OpenSelection("Resume", []Choice{{Value: "session"}}); err == nil {
		t.Fatal("selector replaced viewer")
	}
	if _, err := term.CloseViewer(); err != nil {
		t.Fatal(err)
	}
	if err := term.OpenSelection("Resume", []Choice{{Value: "session"}}); err != nil {
		t.Fatal(err)
	}
	if opened, err := term.OpenViewer("Status", "body"); opened || err != nil {
		t.Fatal("viewer replaced menu")
	}
	_ = term.CloseSelection()
	if _, err := term.OpenViewer("Status", strings.Repeat("x", ViewerLimit+1)); !errors.Is(err, ErrViewerTooLarge) {
		t.Fatal(err)
	}
	term.c = brokenWire{}
	if _, err := term.OpenViewer("Status", "body"); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("lost output failure", err)
	}
}
