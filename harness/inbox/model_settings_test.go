package inbox_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestAtomicModelSettingsRoundTrip(t *testing.T) {
	for _, effort := range []llm.ReasoningEffort{"", "none", "minimal", "ultra", "persistent", "future-effort"} {
		want := inbox.ControlMessage{Mode: inbox.UpdateSettings, Parameters: inbox.Settings{Model: "brand-new-model", ReasoningEffort: effort}}
		payload, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		input := inbox.Input{ID: "model-settings", Kind: inbox.InputControl, Payload: payload}
		got, err := submitAndReceive(t, newInbox(t), input).DecodeControlMessage()
		if err != nil || got != want {
			t.Fatalf("round trip = %+v, %v", got, err)
		}
	}
}
