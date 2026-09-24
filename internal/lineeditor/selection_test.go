package lineeditor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

type selectionWire struct {
	input *strings.Reader
	bytes.Buffer
}

func (w *selectionWire) Read(p []byte) (int, error) { return w.input.Read(p) }

func TestSelectionReadDoesNotSubmitOrRememberKeys(t *testing.T) {
	for _, test := range []struct{ keys, value string }{
		{"ignored\x1b[B\x1b[B\x1b[A\r", "second"},
		{"\x1b[B\x1b\x1b", ""},
	} {
		w := &selectionWire{input: strings.NewReader(test.keys)}
		term := NewTerminal(w, "you> ")
		term.line = []rune("беларускі draft")
		term.pos = 3
		term.writePrompt()
		term.writeLine(term.line)
		term.moveCursorToPos(term.pos)
		term.outBuf = nil
		term.History.Add("old")
		if err := term.OpenSelection("Pick", []Choice{{Value: "first", Label: "same"}, {Value: "second", Label: "same"}, {Value: "third"}}); err != nil {
			t.Fatal(err)
		}
		_, err := term.ReadLine()
		var result *Selection
		if !errors.As(err, &result) || result.Value != test.value || result.Canceled != (test.value == "") {
			t.Fatalf("selection: %v", err)
		}
		if string(term.line) != "беларускі draft" || term.pos != 3 || term.History.Len() != 1 {
			t.Fatal("selection changed draft/history")
		}
		if term.selection != nil {
			t.Fatal("selection retained")
		}
	}
}

func TestSelectionViewportStatusWriteAndResize(t *testing.T) {
	var wire bytes.Buffer
	term := NewTerminal(&wire, "you> ")
	var choices []Choice
	for i := 0; i < 30; i++ {
		choices = append(choices, Choice{Value: fmt.Sprint(i), Label: "тэма паўтараецца", Detail: fmt.Sprintf("ID: exact-%d", i)})
	}
	if err := term.SetSize(40, 8); err != nil {
		t.Fatal(err)
	}
	if err := term.OpenSelection("Pick", choices); err != nil {
		t.Fatal(err)
	}
	for range 23 {
		term.selectionKey(keyDown)
	}
	if term.selection.index != 23 || term.selection.top == 0 {
		t.Fatal("not scrolled")
	}
	for _, size := range [][2]int{{24, 4}, {1, 1}, {80, 16}, {32, 6}} {
		if err := term.SetSize(size[0], size[1]); err != nil {
			t.Fatal(err)
		}
		if term.selection.index != 23 || term.cursorY >= size[1] || term.cursorX >= size[0] {
			t.Fatalf("resize lost selection/bounds: %+v", term.selection)
		}
		if err := term.SetStatus([]string{"* task running", "+ other running", "- third running"}); err != nil {
			t.Fatal(err)
		}
		if _, err := term.Write([]byte("async completion\n")); err != nil {
			t.Fatal(err)
		}
		if term.selection.index != 23 {
			t.Fatal("async write lost selection")
		}
	}
	if result := term.selectionKey(keyEnter); result.Value != "23" {
		t.Fatal("wrong exact choice")
	}
	if len(term.status) != 3 {
		t.Fatal("lost live tasks")
	}
}

func TestSelectionSafeTextAndEOF(t *testing.T) {
	text := string(selectionText("Беларуская\x1b[2J\r\n\t\u009b\u202e界🙂", 200))
	if !strings.Contains(text, "Беларуская") || strings.ContainsAny(text, "\x1b\r\n\t\u009b\u202e界🙂") {
		t.Fatalf("unsafe/unreadable: %q", text)
	}
	for width := 0; width < 30; width++ {
		if len(selectionText(text, width)) > width {
			t.Fatal("unbounded row")
		}
	}
	w := &selectionWire{input: strings.NewReader("\x04")}
	term := NewTerminal(w, "you> ")
	if err := term.OpenSelection("Pick", []Choice{{Value: "one"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := term.ReadLine(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if err := term.CloseSelection(); err != nil {
		t.Fatal(err)
	}
	if term.selection != nil {
		t.Fatal("EOF retained menu")
	}
	term.c = brokenWire{}
	if err := term.OpenSelection("Pick", []Choice{{Value: "one"}}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

// The same primitive can ask a non-session question, including an empty opaque
// value; neither caller titles nor identity are interpreted as chat commands.
func TestSelectionReusableQuestion(t *testing.T) {
	for _, keys := range []string{"\x1b[B\r", "\x1b\x1b"} {
		w := &selectionWire{input: strings.NewReader(keys)}
		term := NewTerminal(w, "answer> ")
		if err := term.OpenSelection("Choose action", []Choice{
			{Value: "inspect", Label: "Inspect files", Detail: "Read only"},
			{Value: "", Label: "Do nothing", Detail: "Leave files unchanged"},
		}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(w.String(), "Choose action 1/2") || strings.Contains(w.String(), "Resume") {
			t.Fatal(w.String())
		}
		_, err := term.ReadLine()
		var result *Selection
		if !errors.As(err, &result) || result.Value != "" || result.Canceled != (keys == "\x1b\x1b") {
			t.Fatalf("question result: %+v %v", result, err)
		}
	}
}
