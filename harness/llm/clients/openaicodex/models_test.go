package openaicodex

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/models" || r.URL.Query().Get("client_version") != catalogClientVersion || r.Header.Get("Authorization") != "Bearer synthetic" || r.Header.Get("ChatGPT-Account-ID") != "account" {
			t.Errorf("unexpected catalog request")
		}
		fmt.Fprint(w, `{"models":[
		 {"slug":"later","visibility":"list","priority":2,"supported_reasoning_levels":[]},
		 {"slug":"hidden","visibility":"hide","priority":0},
		 {"slug":"brand-new","display_name":"New","visibility":"list","priority":1,"default_reasoning_level":"future-effort","supported_reasoning_levels":[{"effort":"none"},{"effort":"minimal"},{"effort":"future-effort","description":"Future"},{"effort":"future-effort"},{"effort":"bad value"}]},
		 {"slug":"brand-new","visibility":"list","priority":3},
		 {"slug":"unknown","visibility":"list","priority":4}
		]}`)
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, AccessToken: "synthetic", AccountID: "account"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	models, err := client.ListModels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 || models[0].ID != "brand-new" || models[0].DefaultEffort != "future-effort" || len(models[0].Efforts) != 3 || models[1].Efforts == nil || models[2].Efforts != nil {
		t.Fatalf("catalog = %+v", models)
	}
}

func TestModelCatalogErrorsDoNotExposeBodiesOrFollowRedirects(t *testing.T) {
	for _, mode := range []string{"redirect", "unauthorized", "malformed", "missing", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, target.URL, 302)
				case "unauthorized":
					http.Error(w, "secret-body", 401)
				case "malformed":
					fmt.Fprint(w, `{"models": secret-body`)
				case "missing":
					fmt.Fprint(w, `{"secret-body":[]}`)
				case "oversize":
					fmt.Fprint(w, strings.Repeat("x", (8<<20)+1))
				}
			}))
			defer server.Close()
			client, err := NewClient(Config{BaseURL: server.URL, AccessToken: "synthetic", AccountID: "account"})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			_, err = client.ListModels(t.Context())
			if err == nil || strings.Contains(err.Error(), "secret-body") || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
