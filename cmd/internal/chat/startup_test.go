package chat

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/internal/lineeditor"
)

func TestStartupKeepsLogLocationsOnDemand(t *testing.T) {
	for _, format := range []string{"jsonl", "sqlite"} {
		t.Run(format, func(t *testing.T) {
			workspace := t.TempDir()
			directory := filepath.Join(workspace, "state")
			c := launch(t, workspace, false, "-storage-format", format, "-session-directory", directory)
			startup := c.output.snapshot()
			for _, noise := range []string{"Command logs:", "Diagnostic log:", "Context: recover"} {
				if strings.Contains(startup, noise) {
					t.Fatalf("startup contains diagnostic noise %q: %s", noise, startup)
				}
			}
			for _, want := range []string{"Session:", "Workspace:", "Provider:", "Type /help"} {
				if !strings.Contains(startup, want) {
					t.Fatalf("missing useful startup information %q", want)
				}
			}
			c.send("/status")
			status := c.wait("Diagnostic log:")[len(startup):]
			for _, want := range []string{"Command logs:", "Diagnostic log:", directory} {
				if !strings.Contains(status, want) {
					t.Fatalf("missing on-demand diagnostic %q: %s", want, status)
				}
			}
			if format == "sqlite" && !strings.Contains(status, "use unreal-storage logs") {
				t.Fatal("SQLite diagnostics lack inspection instructions")
			}
			c.finish("/exit")
		})
	}
}

func TestTTYStartupAndStatusDiagnosticPaths(t *testing.T) {
	var wire bytes.Buffer
	editor := lineeditor.NewTerminal(&wire, "you> ")
	if err := editor.SetSize(160, 40); err != nil {
		t.Fatal(err)
	}
	d := newDisplay(editor, func(string) string { return "" })
	d.ui, d.color = &terminalUI{editor: editor}, true
	a := &application{display: d, logs: &logs{directory: "/test/logs", diagnostic: "/test/logs/diagnostic.jsonl"}}
	if err := a.announce(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wire.String(), "Diagnostic log:") || strings.Contains(wire.String(), "Command logs:") || strings.Contains(wire.String(), "Context: recover") {
		t.Fatal("TTY startup contains diagnostic noise")
	}
	wire.Reset()
	if _, err := a.command("/status"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\x1b[?1049h", "Diagnostic log: /test/logs/diagnostic.jsonl", "Command logs: /test/logs/commands"} {
		if !strings.Contains(wire.String(), want) {
			t.Fatalf("status viewer missing %q: %q", want, wire.String())
		}
	}
	if _, err := editor.CloseViewer(); err != nil {
		t.Fatal(err)
	}
}
