package chat

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

type ModelSettings struct {
	Model  string              `json:"model"`
	Effort llm.ReasoningEffort `json:"effort"`
}

type ModelChoices struct {
	Provider  string            `json:"provider"`
	Current   ModelSettings     `json:"current"`
	Models    []llm.ModelOption `json:"models"`
	Warning   string            `json:"warning,omitempty"`
	FetchedAt time.Time         `json:"fetched_at"`
}

// Only an explicit model selection overrides launch defaults. Legacy startup
// effort-only events keep their original launch-override semantics.
func savedModelSettings(ctx context.Context, store sessionstore.Store, id session.ID) (inbox.Settings, error) {
	var selected inbox.Settings
	var after sessionstore.Sequence
	for {
		page, err := store.Items(ctx, id, after, 128)
		if err != nil {
			return selected, err
		}
		for _, item := range page.Items {
			applyModelSettings(&selected, item)
		}
		if !page.More {
			return selected, nil
		}
		after = page.NextAfter
	}
}

func applyModelSettings(selected *inbox.Settings, item sessionstore.Item) {
	input, ok := item.Data.(inbox.Input)
	if !ok || input.Kind != inbox.InputControl {
		return
	}
	control, err := input.DecodeControlMessage()
	if err != nil || control.Mode != inbox.UpdateSettings {
		return
	}
	settings := control.Parameters.(inbox.Settings)
	if settings.Model != "" {
		selected.Model = settings.Model
	}
	if selected.Model != "" {
		selected.ReasoningEffort = settings.ReasoningEffort
	}
}

// Models does not start/resume generation. Discovery is bounded and cached for
// five minutes; refresh bypasses the cache. Failures keep an explicitly stale list.
func (h *Headless) Models(ctx context.Context, refresh bool) ModelChoices {
	h.actions.Lock()
	defer h.actions.Unlock()
	return h.models(ctx, refresh)
}

func (h *Headless) models(ctx context.Context, refresh bool) ModelChoices {
	result := ModelChoices{Provider: h.config.provider, Current: ModelSettings{h.config.model, llm.ReasoningEffort(h.config.effort)}, Models: h.modelChoices, FetchedAt: h.modelsFetched, Warning: h.modelsWarning}
	if result.Models == nil {
		result.Models = []llm.ModelOption{}
	}
	if !refresh && !h.modelsFetched.IsZero() && time.Since(h.modelsFetched) < 5*time.Minute {
		return result
	}
	if err := h.prepare(); err != nil {
		result.Warning = h.safe(err.Error())
		return result
	}
	catalog, ok := h.client.(llm.ModelCatalog)
	if !ok {
		result.Warning = "Гэты правайдар не падтрымлівае аўтаматычны каталог мадэляў. Можна задаць мадэль уручную."
		return result
	}
	models, err := catalog.ListModels(ctx)
	if err != nil {
		result.Warning = "Не ўдалося абнавіць каталог мадэляў. Спіс можа быць састарэлы. " + h.safe(err.Error())
		h.modelsWarning = result.Warning
		return result
	}
	h.modelsWarning, result.Warning = "", ""
	h.modelChoices, h.modelsFetched = models, time.Now()
	result.Models, result.FetchedAt = models, h.modelsFetched
	if result.Models == nil {
		result.Models = []llm.ModelOption{}
	}
	return result
}

// SetModel persists an atomic model/effort change, without waking an idle session
// or canceling an in-flight request. A running coordinator applies it next turn.
func (h *Headless) SetModel(ctx context.Context, selection ModelSettings) error {
	if !llm.ValidModelID(selection.Model) || (selection.Effort != "" && !selection.Effort.ValidValue()) {
		return errors.New("invalid model settings")
	}
	h.actions.Lock()
	defer h.actions.Unlock()
	if h.closed {
		return errors.New("session is closed")
	}
	// Validate capabilities if known; manual selection remains possible when
	// discovery is unsupported/unavailable, or the model is absent from the list.
	choices := h.models(ctx, false)
	for _, model := range choices.Models {
		if model.ID == selection.Model && model.Efforts != nil && selection.Effort != "" && !slices.ContainsFunc(model.Efforts, func(e llm.EffortOption) bool { return e.Effort == selection.Effort }) {
			return errors.New("reasoning effort is not supported by this model")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	input := newInput(inbox.InputControl, inbox.ControlMessage{Mode: inbox.UpdateSettings, Parameters: inbox.Settings{Model: selection.Model, ReasoningEffort: selection.Effort}})
	if h.run != nil {
		select {
		case <-h.run.done:
			h.run = nil
		default:
		}
	}
	if h.run == nil {
		if err := h.store.AppendInput(ctx, session.ID(h.config.session), input); err != nil {
			return err
		}
	} else {
		// Store observers run after commit. Wait for durability rather than merely
		// acknowledging the inbox; no configuration changes on failed persistence.
		saved := make(chan struct{}, 1)
		h.mu.Lock()
		h.settingsID, h.settingsSaved = input.ID, saved
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			h.settingsID, h.settingsSaved = "", nil
			h.mu.Unlock()
		}()
		if err := h.run.runtime.inputs.Submit(h.ctx, input); err != nil {
			return err
		}
		select {
		case <-saved:
		case <-h.run.done:
			select {
			case <-saved:
			default:
				return errors.New("runtime stopped before saving model settings")
			}
		}
	}
	h.config.model, h.config.effort = selection.Model, string(selection.Effort)
	h.mu.Lock()
	h.status.ModelSettings = selection
	h.mu.Unlock()
	h.changed()
	return nil
}
