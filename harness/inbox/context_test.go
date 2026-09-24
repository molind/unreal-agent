package inbox

import (
	"encoding/json/v2"
	"testing"
)

func TestContextRecoveryControlValidation(t *testing.T) {
	for _, mode := range []ControlMode{ContextOverflow, ApproveCompaction} {
		for _, id := range []string{"request-1", "", "  "} {
			data, _ := json.Marshal(ControlMessage{Mode: mode, Parameters: ContextRecovery{RequestID: id}})
			input := Input{ID: "control", Kind: InputControl, Payload: data}
			err := input.Validate()
			if (err == nil) != (id == "request-1") {
				t.Fatalf("%s id=%q: %v", mode, id, err)
			}
		}
	}
	for _, payload := range []string{
		`{"Mode":"approve_compaction","Parameters":{"RequestID":"a","FailedPrefixItems":1}}`,
		`{"Mode":"context_overflow","Parameters":{"RequestID":"a","FailedPrefixItems":-1}}`,
		`{"Mode":"approve_compaction","Parameters":{"RequestID":"a","AllowForever":true}}`,
	} {
		if err := (Input{ID: "control", Kind: InputControl, Payload: []byte(payload)}).Validate(); err == nil {
			t.Fatal("accepted invalid permission", payload)
		}
	}
}
