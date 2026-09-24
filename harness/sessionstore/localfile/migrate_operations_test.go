package localfile

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestAutomaticMigrationRestoresUnfinishedShellWithoutOldDirectory(t *testing.T) {
	for _, layout := range []string{"XDG", "in-place", "aliased-in-place"} {
		inPlace := layout != "XDG"
		for _, phase := range []operation.ShellPhase{"", operation.ShellPhaseReadOut, operation.ShellPhaseProcess} {
			t.Run(strings.Join([]string{layout, string(phase)}, "/"), func(t *testing.T) {
				source, old, dest := legacyFixture(t)
				if layout == "aliased-in-place" {
					alias := filepath.Join(t.TempDir(), "workspace-alias")
					if err := os.Symlink(filepath.Dir(filepath.Dir(source)), alias); err != nil {
						t.Fatal(err)
					}
					dest = sqliteStore(t, filepath.Join(alias, ".harness", "sessions"))
				} else if inPlace {
					dest = sqliteStore(t, source)
				}
				if err := old.AppendTurn(t.Context(), "old", session.Turn{ID: "turn"}); err != nil {
					t.Fatal(err)
				}
				workspace := t.TempDir()
				base := filepath.Join(source, "operations", "old")
				spec, err := operation.NewShellSpec(operation.ShellInput{Command: "printf executed > marker; printf fresh-output", Shell: "/bin/sh", Directory: workspace}, base, 100)
				if err != nil {
					t.Fatal(err)
				}
				op := operation.Operation{ID: "op", Type: spec.Type, Version: spec.Version, State: spec.State, Status: operation.StatusReady, MaxOutputLength: 100}
				if err = old.AppendToolCallStatus(t.Context(), "old", sessionstore.ToolCallStatus{TurnID: "turn", CallID: "call", Status: tool.CallStatus{WaitingFor: []operation.ID{"op"}}, Operations: []operation.Operation{op}}); err != nil {
					t.Fatal(err)
				}
				content := ""
				if phase != "" {
					state, err := operation.DecodeShellState(op)
					if err != nil {
						t.Fatal(err)
					}
					state.Phase = phase
					state.OutPath = filepath.Join(base, "op", "out")
					state.ErrPath = filepath.Join(base, "op", "err")
					if phase == operation.ShellPhaseReadOut {
						exit := 0
						state.PendingExitCode = &exit
					}
					op.Status = operation.StatusAwaiting
					op.State, _ = json.Marshal(state)
					if err = old.SaveOperation(t.Context(), "old", op); err != nil {
						t.Fatal(err)
					}
					content = "already-executed-output"
				}
				sourceFile(t, source, "operations/old/op/out", content)
				sourceFile(t, source, "operations/old/op/err", "")
				if err = dest.MigrateLegacy(t.Context(), source); err != nil {
					t.Fatal(err)
				}
				resumed, err := dest.Resume(t.Context(), "old")
				if err != nil || len(resumed.Operations) != 1 {
					t.Fatal(resumed, err)
				}
				state, err := operation.DecodeShellState(resumed.Operations[0])
				if err != nil {
					t.Fatal(err)
				}
				if state.BaseDirectory == base {
					t.Fatal("unfinished checkpoint still uses old spool")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				manager := operation.NewLocalOperationManagerWithStorage(ctx, dest.database)
				if err = manager.Add(resumed.Operations[0]); err != nil {
					t.Fatal(err)
				}
				var terminal operation.Operation
				for terminal.ID == "" {
					select {
					case update, ok := <-manager.Updates():
						if !ok {
							t.Fatal("manager ended early")
						}
						if err = dest.SaveOperation(ctx, "old", update); err != nil {
							t.Fatal(err)
						}
						if terminalOperationStatus(update.Status) {
							terminal = update
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				cancel()
				for range manager.Updates() {
				}
				state, err = operation.DecodeShellState(terminal)
				if err != nil {
					t.Fatal(err)
				}
				if phase == operation.ShellPhaseProcess {
					if terminal.Status != operation.StatusFailed || !strings.Contains(state.TerminalError, "unknown") {
						t.Fatal(terminal.Status, state.TerminalError)
					}
				} else {
					want := "fresh-output"
					if phase == operation.ShellPhaseReadOut {
						want = content
					}
					if terminal.Status != operation.StatusCompleted || state.Result == nil || state.Result.Out != want {
						t.Fatal(terminal.Status, state)
					}
				}
				_, markerErr := os.Stat(filepath.Join(workspace, "marker"))
				if phase == "" && markerErr != nil {
					t.Fatal("ready command did not execute", markerErr)
				}
				if phase != "" && !os.IsNotExist(markerErr) {
					t.Fatal("interrupted command was repeated", markerErr)
				}
				if !inPlace {
					if _, err = os.Stat(filepath.Dir(source)); !os.IsNotExist(err) {
						t.Fatal("old .harness recreated", err)
					}
				}
				if err = dest.MigrateLegacy(t.Context(), source); err != nil {
					t.Fatal("repeat cleanup", err)
				}
			})
		}
	}
}
