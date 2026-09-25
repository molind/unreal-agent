package chat

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/internal/lineeditor"
)

func TestReportsUseViewerAndRedactWithoutChangingLiveDisplay(t *testing.T) {
	var wire bytes.Buffer
	editor := lineeditor.NewTerminal(&wire, "you> ")
	d := newDisplay(editor, func(name string) string {
		if name == "OPENAI_API_KEY" {
			return "private-fixture"
		}
		return ""
	})
	d.ui = &terminalUI{editor: editor}
	d.color = true
	a := &application{display: d}
	if err := a.report("Status", func(view *application) error { return view.display.print("private-fixture\nreport-body\n") }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wire.String(), "\x1b[?1049h") || strings.Contains(wire.String(), "private-fixture") || !strings.Contains(wire.String(), "[redacted]") {
		t.Fatal("viewer/redaction missing")
	}
	if d.ui == nil || !d.color || d.out != editor {
		t.Fatal("report mutated live renderer")
	}
	wire.Reset()
	if err := d.print("live answer\n"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wire.String(), "live answer") {
		t.Fatal("live answer overwrote viewer")
	}
	if _, err := editor.CloseViewer(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wire.String(), "live answer") {
		t.Fatal("live answer lost")
	}
}
func TestViewerTerminalCapabilities(t *testing.T) {
	for _, term := range []string{"xterm", "xterm-256color", "screen", "tmux-256color", "foot", "xterm-kitty"} {
		if !viewerTerminal(term) {
			t.Fatal("viewer unavailable", term)
		}
	}
	for _, term := range []string{"", "dumb", "vt100", "ansi", "linux", "unknown"} {
		if viewerTerminal(term) {
			t.Fatal("unsafe alternate-screen assumption", term)
		}
	}
	var wire bytes.Buffer
	editor := lineeditor.NewTerminal(&wire, "you> ")
	d := newDisplay(editor, func(string) string { return "" })
	d.ui = &terminalUI{editor: editor, viewerDisabled: true}
	a := &application{display: d}
	if err := a.report("Help", func(view *application) error { return view.display.print("fallback report\n") }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wire.String(), "1049") || !strings.Contains(wire.String(), "fallback report") {
		t.Fatal("unknown terminal did not use text fallback")
	}
}

func TestPlainReportsKeepOrdinaryOutput(t *testing.T) {
	var wire bytes.Buffer
	a := &application{display: newDisplay(&wire, func(string) string { return "" })}
	if err := a.report("Help", func(view *application) error { return view.display.print("plain\n") }); err != nil {
		t.Fatal(err)
	}
	if wire.String() != "plain\n" {
		t.Fatal(wire.String())
	}
}
func TestReportSnapshotLimitIsExplicit(t *testing.T) {
	b := &reportBuffer{}
	if _, err := io.WriteString(b, strings.Repeat("x", lineeditor.ViewerLimit+1)); !errors.Is(err, lineeditor.ErrViewerTooLarge) || !b.truncated || b.Len() >= lineeditor.ViewerLimit {
		t.Fatal("unbounded report", b.Len(), err)
	}
	var wire bytes.Buffer
	editor := lineeditor.NewTerminal(&wire, "you> ")
	d := newDisplay(editor, func(string) string { return "" })
	d.ui = &terminalUI{editor: editor}
	a := &application{display: d}
	if err := a.report("Large", func(view *application) error {
		return view.display.print("%s", strings.Repeat("x", lineeditor.ViewerLimit+1))
	}); err != nil {
		t.Fatal(err)
	}
	if closed, err := editor.CloseViewer(); err != nil || !closed {
		t.Fatal(closed, err)
	}
}
func TestViewerTransportDiscardsPasteAndRetainsDraftAttachments(t *testing.T) {
	for _, closeKey := range []string{"\x1b\x1b", "\x03"} {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		u := &terminalUI{input: reader, output: &out, failures: make(chan error, 1)}
		u.initEditor()
		u.attachments[attachmentFirst] = "unsubmitted draft attachment"
		if _, err = u.editor.OpenViewer("Help", "body"); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := io.WriteString(writer, bracketed("/exit\nq\x03")+closeKey)
			_ = writer.Close()
			done <- err
		}()
		line, err := u.readLine()
		if err != nil || line.viewer == nil || line.interrupt || line.text != "" {
			t.Fatal("viewer input became a message/interrupt", err)
		}
		if u.attachments[attachmentFirst] != "unsubmitted draft attachment" || len(u.pasted) != 0 || u.history.Len() != 0 {
			t.Fatal("lost draft attachment or retained viewer paste")
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		_ = reader.Close()
	}
}

func TestViewerCloseIsNotAUserMessage(t *testing.T) {
	var out bytes.Buffer
	a := &application{display: newDisplay(&out, func(string) string { return "" })}
	if exit, err := a.accept(line{viewer: &lineeditor.ViewerClosed{}}); exit || err != nil || a.id != "" || out.Len() != 0 {
		t.Fatal("viewer close submitted content", exit, err)
	}
}
