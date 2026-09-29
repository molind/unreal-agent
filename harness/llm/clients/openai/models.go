package openai

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/clients"
)

func (client *Client) ListModels(ctx context.Context) ([]llm.ModelOption, error) {
	var response struct {
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := clients.GetCatalog(ctx, client.catalogURL, http.Header{"Authorization": {"Bearer " + client.apiKey}}, &response); err != nil {
		return nil, err
	}
	if response.Data == nil {
		return nil, errors.New("model catalog is missing data")
	}
	models := []llm.ModelOption{}
	seen := map[string]bool{}
	for _, row := range *response.Data {
		if !llm.ValidModelID(row.ID) || seen[row.ID] {
			continue
		}
		seen[row.ID] = true
		// /v1/models does not publish Responses/tool support or reasoning levels.
		// Do not infer capabilities from model names (including new releases).
		models = append(models, llm.ModelOption{ID: row.ID, Name: row.ID})
	}
	slices.SortFunc(models, func(a, b llm.ModelOption) int { return strings.Compare(a.ID, b.ID) })
	return models, nil
}
