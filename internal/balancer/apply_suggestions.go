// apply_suggestions.go — R59.1 (2026-09-03): Apply cluster autosuggestions.
//
// Pure function (no HTTP, no real backends). Validates suggestions, applies
// them via injected `applyFn` callback, and returns a summary.
//
// Reference: docs/superpowers/specs/2026-09-03-cluster-autosuggest-apply.md
package balancer

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// ApplyFn — injected callback for actually moving a model between backends.
// Returns nil on success, error string on failure.
// In production, this calls GgufApi.loadModel / unloadModel.
type ApplyFn func(fromBackendID, toBackendID, modelName string) error

// ApplyResult — summary of apply operation.
type ApplyResult struct {
	Applied  int                  `json:"applied"`
	Failed   int                  `json:"failed"`
	Details  []ApplyDetailResult  `json:"details"`
	Errors   []string             `json:"errors,omitempty"`
}

// ApplyDetailResult — per-suggestion apply result.
type ApplyDetailResult struct {
	SuggestionID string   `json:"suggestionId"`
	FromBackend  string   `json:"fromBackend"`
	ToBackend    string   `json:"toBackend"`
	Model        string   `json:"model"`
	Status       string   `json:"status"` // "applied" | "failed" | "rolled_back"
	Error        string   `json:"error,omitempty"`
	Actions      []string `json:"actions,omitempty"`
}

// ApplySuggestions — main entry point. Returns ApplyResult with applied/failed counts.
//
// requestedIDs — if nil, applies ALL suggestions. If non-nil, only applies
// suggestions whose ID is in the list (unknown IDs are reported as failed).
func ApplySuggestions(
	backends []*types.Backend,
	allSuggestions []Suggestion,
	loadedByBackend map[string][]string,
	requestedIDs *[]string,
	applyFn ApplyFn,
) ApplyResult {
	result := ApplyResult{Details: []ApplyDetailResult{}}

	// Build requested set
	wantedAll := requestedIDs == nil
	wanted := make(map[string]bool)
	if !wantedAll {
		for _, id := range *requestedIDs {
			wanted[id] = true
		}
	}

	// Build available set (для детекта unknown IDs)
	available := make(map[string]bool, len(allSuggestions))
	for _, s := range allSuggestions {
		available[s.ID] = true
	}

	// Unknown IDs (requested but not available) — fail immediately
	if !wantedAll {
		for _, id := range *requestedIDs {
			if !available[id] {
				result.Errors = append(result.Errors, id+": unknown suggestion id")
				result.Details = append(result.Details, ApplyDetailResult{
					SuggestionID: id,
					Status:       "failed",
					Error:        "unknown suggestion id",
				})
				result.Failed++
			}
		}
	}

	// Build backend lookup
	beMap := make(map[string]*types.Backend, len(backends))
	for _, b := range backends {
		beMap[b.ID] = b
	}

	for _, sug := range allSuggestions {
		if !wantedAll && !wanted[sug.ID] {
			continue
		}
		detail := ApplyDetailResult{
			SuggestionID: sug.ID,
			FromBackend:  sug.FromBackend,
			ToBackend:    sug.ToBackend,
			Model:        sug.Model,
		}

		// Validation
		if err := validateSuggestion(sug, beMap, loadedByBackend); err != nil {
			detail.Status = "failed"
			detail.Error = err.Error()
			result.Details = append(result.Details, detail)
			result.Errors = append(result.Errors, sug.ID+": "+err.Error())
			result.Failed++
			continue
		}

		// Apply (if applyFn provided)
		if applyFn != nil {
			if err := applyFn(sug.FromBackend, sug.ToBackend, sug.Model); err != nil {
				detail.Status = "failed"
				detail.Error = err.Error()
				detail.Actions = []string{"apply failed"}
				result.Details = append(result.Details, detail)
				result.Errors = append(result.Errors, sug.ID+": "+err.Error())
				result.Failed++
				continue
			}
		}

		// Update local loadedByBackend (для consistency при multi-step apply)
		// R59.1: упрощённо — реальное состояние обновит следующий poll.
		// Просто отражаем что apply прошёл.
		detail.Status = "applied"
		detail.Actions = []string{
			fmt.Sprintf("unloaded from %s", sug.FromBackend),
			fmt.Sprintf("loaded on %s", sug.ToBackend),
		}
		result.Details = append(result.Details, detail)
		result.Applied++
	}
	return result
}

// validateSuggestion — pure validation. Returns error if suggestion can't be applied.
func validateSuggestion(sug Suggestion, backends map[string]*types.Backend, loadedByBackend map[string][]string) error {
	from, ok := backends[sug.FromBackend]
	if !ok {
		return fmt.Errorf("source backend %q not found", sug.FromBackend)
	}
	to, ok := backends[sug.ToBackend]
	if !ok {
		return fmt.Errorf("destination backend %q not found", sug.ToBackend)
	}
	// Cross-type: same Type required
	if from.Type != to.Type {
		return fmt.Errorf("cross-type move not allowed (%s vs %s)", from.Type, to.Type)
	}
	// Source must be healthy
	if from.Status != types.StatusHealthy {
		return fmt.Errorf("source backend %q is not healthy (%s)", sug.FromBackend, from.Status)
	}
	// Destination must be healthy
	if to.Status != types.StatusHealthy {
		return fmt.Errorf("destination backend %q is not healthy (%s)", sug.ToBackend, to.Status)
	}
	// Model must be loaded on source
	loaded := loadedByBackend[sug.FromBackend]
	found := false
	for _, m := range loaded {
		if m == sug.Model || strings.HasPrefix(m, sug.Model+":") {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("model %q not loaded on %q (loaded: %v)", sug.Model, sug.FromBackend, loaded)
	}
	return nil
}
