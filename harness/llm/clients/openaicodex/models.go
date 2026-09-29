package openaicodex

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients"
)

// Codex's subscription catalog (unlike /v1/models) publishes per-model efforts.
// This is the catalog protocol compatibility version, not our application version.
// Protocol checked against openai/codex rust-v0.159.0.
const catalogClientVersion = "0.159.0"

type catalogModel struct {
	Slug       string              `json:"slug"`
	Name       string              `json:"display_name"`
	Visibility string              `json:"visibility"`
	Priority   int                 `json:"priority"`
	Default    llm.ReasoningEffort `json:"default_reasoning_level"`
	Efforts    []llm.EffortOption  `json:"supported_reasoning_levels"`
}

func (client *Client) ListModels(ctx context.Context) ([]llm.ModelOption, error) {
	var response struct {
		Models *[]catalogModel `json:"models"`
	}
	if err := clients.GetCatalog(ctx, client.catalogURL+"?client_version="+catalogClientVersion, client.catalogHeaders, &response); err != nil {
		return nil, err
	}
	if response.Models == nil {
		return nil, errors.New("model catalog is missing models")
	}
	rows := *response.Models
	slices.SortStableFunc(rows, func(a, b catalogModel) int { return cmp.Compare(a.Priority, b.Priority) })
	models := []llm.ModelOption{}
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Visibility != "list" || !llm.ValidModelID(row.Slug) || seen[row.Slug] {
			continue
		}
		seen[row.Slug] = true
		name := row.Name
		if name == "" {
			name = row.Slug
		}
		model := llm.ModelOption{ID: row.Slug, Name: name}
		if row.Efforts != nil {
			model.Efforts = []llm.EffortOption{}
		}
		for _, effort := range row.Efforts {
			if effort.Effort.ValidValue() && !slices.ContainsFunc(model.Efforts, func(e llm.EffortOption) bool { return e.Effort == effort.Effort }) {
				model.Efforts = append(model.Efforts, effort)
			}
		}
		if slices.ContainsFunc(model.Efforts, func(e llm.EffortOption) bool { return e.Effort == row.Default }) {
			model.DefaultEffort = row.Default
		}
		models = append(models, model)
	}
	return models, nil
}
