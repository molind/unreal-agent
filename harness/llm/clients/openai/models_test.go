package openai

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestModelsHaveUnknownCapabilities(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Error("incorrect models request")
		}
		fmt.Fprint(w, `{"data":[{"id":"new-release"},{"id":"new-release"},{"id":"audio-model"}]}`)
	}))
	defer s.Close()
	c, err := NewClient(Config{BaseURL: s.URL + "/v1", APIKey: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	models, err := c.ListModels(t.Context())
	if err != nil || len(models) != 2 {
		t.Fatalf("models = %+v, %v", models, err)
	}
	for _, m := range models {
		if m.Efforts != nil || m.DefaultEffort != "" {
			t.Fatal("API cannot claim known capabilities", m)
		}
	}
}
