package chat

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/internal/lineeditor"
)

func TestOperationNoticeWaitsForManagerOwnership(t *testing.T) {
	var output bytes.Buffer
	d := newDisplay(&output, func(string) string { return "" })
	d.calls["turn/call"] = "Bash: sleep 60"
	op := operation.Operation{ID: "operation-id", Type: operation.TypeShell, Status: operation.StatusReady}
	item := sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{TurnID: "turn", CallID: "call", Operations: []operation.Operation{op}}}
	if err := d.item(item, false); err != nil {
		t.Fatal(err)
	}
	if err := d.status(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "operation-id") {
		t.Fatal("exposed ID before manager.Add")
	}
	output.Reset()
	op.Status = operation.StatusAwaiting
	if err := d.operation(op, "", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "tool operation-id running — Bash: sleep 60") {
		t.Fatal(output.String())
	}
	op.Status = operation.StatusCanceled
	if err := d.operation(op, "", false); err != nil {
		t.Fatal(err)
	}
	if err := d.operation(op, "", false); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "tool operation-id canceled") != 1 {
		t.Fatal("duplicate terminal notice")
	}
}

func TestLiveOperationLifecycle(t *testing.T) {
	for _, color := range []bool{false, true} {
		t.Run(fmt.Sprint(color), func(t *testing.T) {
			var transcript, screen bytes.Buffer
			d := newDisplay(&transcript, func(string) string { return "" })
			d.color = color
			d.ui = &terminalUI{editor: lineeditor.NewTerminal(&screen, "you> ")}
			if err := d.working(); err != nil {
				t.Fatal(err)
			}
			if err := d.working(); err != nil {
				t.Fatal(err)
			}
			ops := []operation.Operation{
				{ID: "full-success-id", Status: operation.StatusAwaiting},
				{ID: "full-failure-id", MaxOutputLength: 1000, Type: operation.TypeShell, Version: operation.VersionShell, Status: operation.StatusAwaiting},
				{ID: "full-canceled-id", Status: operation.StatusAwaiting},
				{ID: "full-error-id", Status: operation.StatusAwaiting},
			}
			for _, op := range ops {
				if err := d.operation(op, "Bash: echo hello\n\tworld; echo API_KEY=private", false); err != nil {
					t.Fatal(err)
				}
			}
			ops[2].Status = operation.StatusCanceling
			if err := d.operation(ops[2], "", false); err != nil {
				t.Fatal(err)
			}
			if transcript.Len() != 0 {
				t.Fatalf("transient notice leaked: %s", &transcript)
			}
			ops[0].Status, ops[1].Status, ops[2].Status, ops[3].Status = operation.StatusCompleted, operation.StatusCompleted, operation.StatusCanceled, operation.StatusFailed
			ops[1].State, _ = json.Marshal(operation.ShellState{Result: &operation.ShellResult{ExitCode: 7}})
			for _, i := range []int{1, 2, 0, 3} {
				for range 2 {
					if err := d.operation(ops[i], "", false); err != nil {
						t.Fatal(err)
					}
				}
			}
			text := transcript.String()
			for i, want := range []string{"completed", "failed (exit 7)", "canceled", "failed"} {
				if color && i == 0 {
					matches := regexp.MustCompile(`\x1b\[32mBash: echo hello world; echo \[redacted\] \([0-9]+\.[0-9]s\)\n\x1b\[0m`).FindAllString(text, -1)
					if len(matches) != 1 || strings.Contains(text, string(ops[i].ID)) || strings.Contains(text, "completed") {
						t.Fatalf("compact green success: %q", text)
					}
					continue
				}
				notice := "tool " + string(ops[i].ID) + " " + want + " — Bash: echo hello world; echo [redacted]"
				if strings.Count(text, notice) != 1 {
					t.Fatalf("not exactly one %q: %s", notice, text)
				}
				code := []string{"32", "31", "33", "31"}[i]
				if strings.Contains(text, "\x1b["+code+"m"+notice) != color {
					t.Fatalf("result color: %q", text)
				}
			}
			if color && strings.Count(text, "\x1b[0m") != 4 {
				t.Fatal("missing resets")
			}
			if !color && strings.Contains(text, "\x1b") {
				t.Fatal("NO_COLOR")
			}
			d.generating = false
			if err := d.tick(2); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPlainLifecycleAndReplay(t *testing.T) {
	var output bytes.Buffer
	d := newDisplay(&output, func(string) string { return "" })
	_ = d.working()
	_ = d.working()
	op := operation.Operation{ID: "plain-id", Status: operation.StatusAwaiting}
	_ = d.operation(op, "command", false)
	op.Status = operation.StatusCompleted
	_ = d.operation(op, "", false)
	_ = d.operation(op, "", true)
	for _, want := range []string{"Working —", "tool plain-id running", "tool plain-id completed"} {
		if strings.Count(output.String(), want) != 1 {
			t.Fatal(output.String())
		}
	}
	if strings.Contains(output.String(), "\x1b") {
		t.Fatal("cursor/color in pipe")
	}
}
