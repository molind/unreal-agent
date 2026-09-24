package bash

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestArtifactRenderingIsPersistedNotChangedByMigration(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		original := &translator{config: Config{Shell: "/bin/sh", BaseDirectory: "/spool", Artifacts: recorded}}
		spec, err := original.buildOperation("echo", 10)
		if err != nil {
			t.Fatal(err)
		}
		op := operation.Operation{ID: "op", Type: spec.Type, Version: spec.Version, Status: operation.StatusCompleted, State: spec.State, MaxOutputLength: 10}
		state, err := operation.DecodeShellState(op)
		if err != nil {
			t.Fatal(err)
		}
		if state.Input.ArtifactReferences != recorded {
			t.Fatal("rendering mode not persisted")
		}
		state.OutPath = "/spool/out"
		state.Result = &operation.ShellResult{Out: "start...bytes truncated; complete output in /spool/out...end"}
		op.State, _ = json.Marshal(state)
		// A new host's backend must not change old transcript text or prefix hashes.
		reopened := &translator{config: Config{Artifacts: !recorded}}
		got, err := reopened.TranslateResult("call", tool.CallStatus{}, []operation.Operation{op})
		if err != nil {
			t.Fatal(err)
		}
		text := got.Output[0].Value
		if strings.Contains(text, "capture:") != recorded {
			t.Fatal(recorded, text)
		}
		if !recorded && text != state.Result.Out {
			t.Fatal("rewrote historical output")
		}
	}
}
