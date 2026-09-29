package webchat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/cmd/internal/chat"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

type catalogFake struct {
	*fakeModel
	lists atomic.Int32
	fail  atomic.Bool
}

func (m *catalogFake) ListModels(context.Context) ([]llm.ModelOption, error) {
	m.lists.Add(1)
	if m.fail.Load() {
		return nil, errors.New("offline")
	}
	return []llm.ModelOption{
		{ID: "new-model", Name: "New model", Efforts: []llm.EffortOption{{Effort: "medium"}, {Effort: "future-effort"}}, DefaultEffort: "medium"},
		{ID: "no-reasoning", Name: "No reasoning", Efforts: []llm.EffortOption{}},
		{ID: "unknown", Name: "Unknown"},
	}, nil
}
func choices(t *testing.T, s *Server, path string) chat.ModelChoices {
	t.Helper()
	w := request(t, s, "GET", path, nil)
	ok(t, w)
	var result chat.ModelChoices
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func TestSessionModelSelectionDiscoveryAndPersistence(t *testing.T) {
	config, model := testConfig(t)
	catalog := &catalogFake{fakeModel: model}
	config.Providers[0].NewClient = func(string, string, int, func(string) string) (agentrunner.Client, error) { return catalog, nil }
	s := newTestServer(t, config)
	workspace := t.TempDir()
	path := projectSession(t, s, workspace, "00000000-0000-4000-8000-000000000091")
	other := projectSession(t, s, workspace, "00000000-0000-4000-8000-000000000092")
	initial := choices(t, s, path+"/models")
	if len(initial.Models) != 3 || initial.Current.Model != "gpt-6-astra" {
		t.Fatalf("choices = %+v", initial)
	}
	choices(t, s, path+"/models")
	if catalog.lists.Load() != 1 {
		t.Fatal("discovery not cached")
	}
	refreshed := choices(t, s, path+"/models?refresh=1")
	if catalog.lists.Load() != 2 {
		t.Fatal("refresh ignored")
	}
	catalog.fail.Store(true)
	stale := choices(t, s, path+"/models?refresh=1")
	if len(stale.Models) != 3 || stale.Warning == "" || !stale.FetchedAt.Equal(refreshed.FetchedAt) {
		t.Fatal("stale catalog lost", stale)
	}
	if cached := choices(t, s, path+"/models"); cached.Warning != stale.Warning {
		t.Fatal("cached failure warning lost")
	}
	catalog.fail.Store(false)
	if fresh := choices(t, s, path+"/models?refresh=1"); fresh.Warning != "" {
		t.Fatal("successful refresh kept warning")
	}
	bad := request(t, s, "POST", path+"/settings", chat.ModelSettings{Model: "new-model", Effort: "xhigh"})
	if bad.Code == 200 {
		t.Fatal("unsupported effort accepted")
	}
	selection := chat.ModelSettings{Model: "new-model", Effort: "future-effort"}
	ok(t, request(t, s, "POST", path+"/settings", selection))
	select {
	case <-model.calls:
		t.Fatal("settings/discovery woke model")
	default:
	}
	if got := choices(t, s, other+"/models").Current; got.Model != "gpt-6-astra" {
		t.Fatal("selection leaked to other session", got)
	}
	ok(t, request(t, s, "POST", path+"/send", map[string]string{"id": "first", "text": "start"}))
	first := nextCall(t, model)
	if first.request.Model.ID != selection.Model || first.request.Model.ReasoningEffort != selection.Effort {
		t.Fatal("first turn ignored saved selection", first.request.Model)
	}
	// While generating, switching is durable but must not cancel or wake work.
	selection = chat.ModelSettings{Model: "no-reasoning"}
	ok(t, request(t, s, "POST", path+"/settings", selection))
	if first.ctx.Err() != nil {
		t.Fatal("switch canceled generation")
	}
	select {
	case <-model.calls:
		t.Fatal("switch started another turn")
	default:
	}
	first.reply <- answer("first completed")
	waitHistory(t, s, path, `"state":"idle"`)
	ok(t, request(t, s, "POST", path+"/send", map[string]string{"id": "second", "text": "continue"}))
	second := nextCall(t, model)
	if second.request.Model.ID != "no-reasoning" || second.request.Model.ReasoningEffort != "" {
		t.Fatal("next turn kept stale effort", second.request.Model)
	}
	second.reply <- answer("second completed")
	waitHistory(t, s, path, `"state":"idle"`)
	// Settings do not wake an idle runtime either.
	selection = chat.ModelSettings{Model: "new-model", Effort: "medium"}
	ok(t, request(t, s, "POST", path+"/settings", selection))
	select {
	case <-model.calls:
		t.Fatal("idle settings woke model")
	default:
	}
	ok(t, request(t, s, "POST", path+"/release", map[string]string{}))
	if got := choices(t, s, path+"/models").Current; got != selection {
		t.Fatal("release lost selection", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = newTestServer(t, config)
	if got := choices(t, s, path+"/models").Current; got != selection {
		t.Fatal("restart lost selection", got)
	}
	ok(t, request(t, s, "POST", path+"/send", map[string]string{"id": "third", "text": "after restart"}))
	third := nextCall(t, model)
	if third.request.Model.ID != selection.Model || third.request.Model.ReasoningEffort != selection.Effort {
		t.Fatal("restart request", third.request.Model)
	}
	// Context/history remain intact on model changes.
	found := false
	for _, item := range third.request.Input {
		if msg, ok := item.Data.(llm.Message); ok && strings.Contains(msg.Text, "second completed") {
			found = true
		}
	}
	if !found {
		t.Fatal("switch lost transcript")
	}
	third.reply <- answer("done")
}

func TestUnavailableCatalogAllowsExplicitManualSelection(t *testing.T) {
	config, model := testConfig(t)
	s := newTestServer(t, config)
	path := projectSession(t, s, t.TempDir(), "00000000-0000-4000-8000-000000000093")
	if c := choices(t, s, path+"/models"); c.Warning == "" || len(c.Models) != 0 {
		t.Fatal("unsupported catalog", c)
	}
	for _, selection := range []chat.ModelSettings{{Model: ""}, {Model: "bad model"}, {Model: "manual", Effort: "bad value"}} {
		if w := request(t, s, "POST", path+"/settings", selection); w.Code == 200 {
			t.Fatal("invalid setting accepted", selection)
		}
	}
	selection := chat.ModelSettings{Model: "manual", Effort: "future-effort"}
	ok(t, request(t, s, "POST", path+"/settings", selection))
	if got := choices(t, s, path+"/models").Current; got != selection {
		t.Fatal(got)
	}
	select {
	case <-model.calls:
		t.Fatal("manual settings woke work")
	default:
	}
}
