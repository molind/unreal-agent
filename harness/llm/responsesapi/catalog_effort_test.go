package responsesapi

import (
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestCatalogEffortsAreForwardCompatibleOnWire(t *testing.T) {
	for _, effort := range []llm.ReasoningEffort{"none", "minimal", "ultra", "persistent", "future-effort", ""} {
		body, err := requestBody(llm.Request{Model: llm.Model{ID: "brand-new", ReasoningEffort: effort}}, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Model     string `json:"model"`
			Reasoning *struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got.Model != "brand-new" {
			t.Fatal(got)
		}
		if effort == "" {
			if got.Reasoning != nil {
				t.Fatal("default must omit reasoning")
			}
		} else if got.Reasoning == nil || got.Reasoning.Effort != string(effort) {
			t.Fatal("effort lost", string(body))
		}
	}
}
