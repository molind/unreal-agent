package llm

import "context"

// ModelCatalog is an optional provider capability. A nil Efforts means the
// provider does not publish reasoning capabilities; an empty list means none.
// Catalog entries are metadata, never instructions for the model.
type ModelCatalog interface {
	ListModels(context.Context) ([]ModelOption, error)
}

type EffortOption struct {
	Effort      ReasoningEffort `json:"effort"`
	Description string          `json:"description,omitempty"`
}

type ModelOption struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Efforts       []EffortOption  `json:"efforts"`
	DefaultEffort ReasoningEffort `json:"default_effort"`
}

// ValidValue validates wire syntax, not a frozen enumeration: provider catalogs
// may introduce new effort values without a harness release.
func (effort ReasoningEffort) ValidValue() bool {
	if len(effort) == 0 || len(effort) > 64 {
		return false
	}
	for _, c := range effort {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func ValidModelID(id string) bool {
	if len(id) == 0 || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}
