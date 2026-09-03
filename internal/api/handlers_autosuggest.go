// handlers_autosuggest.go — R59 + R59.1 (2026-09-03): Cluster AutoDistribute.
//
// GET  /api/v1/admin/cluster/autosuggest        — R59 read-only suggestions.
// POST /api/v1/admin/cluster/autosuggest/apply  — R59.1 apply (operator-confirmed).
//
// Reference: docs/superpowers/specs/2026-09-03-cluster-autodistribute.md
//            docs/superpowers/specs/2026-09-03-cluster-autosuggest-apply.md
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// handleAdminAutosuggest — GET /api/v1/admin/cluster/autosuggest (R59)
//
// Returns cluster state + suggestions for redistributing loaded models.
// Suggestions computed by balancer.ComputeSuggestions (TDD'd in autosuggest_test.go).
// Read-only — apply requires explicit operator confirmation via /apply.
func (s *Server) handleAdminAutosuggest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	proxy := s.proxy
	if proxy == nil {
		http.Error(w, "balancer proxy not initialized", http.StatusInternalServerError)
		return
	}

	// Collect backends + their loaded models
	backends := proxy.GetAllBackends()
	metrics := proxy.GetMetricsManager()

	backendList := make([]map[string]interface{}, 0, len(backends))
	loadedByBackend := make(map[string][]string, len(backends))
	for _, b := range backends {
		loaded := []string{}
		if metrics != nil {
			lm := metrics.GetLlamaCppMetrics(b.ID)
			if lm != nil {
				for _, m := range lm.LoadedModels {
					loaded = append(loaded, m.Name)
				}
			}
		}
		loadedByBackend[b.ID] = loaded
		backendList = append(backendList, map[string]interface{}{
			"id":                b.ID,
			"name":              b.Name,
			"status":            string(b.Status),
			"type":              string(b.Type),
			"activeRequests":    b.ActiveRequests,
			"maxConcurrent":     b.MaxConcurrentReqs,
			"maxModels":          b.MaxModels,
			"loadedModels":       loaded,
			"loadedModelCount":   len(loaded),
		})
	}

	// Compute suggestions (pure function, TDD'd)
	suggestions := proxy.ComputeAutosuggestions(loadedByBackend)

	// Compute summary
	overloadedCount := 0
	underloadedCount := 0
	for _, b := range backends {
		busy := 0.0
		if b.MaxConcurrentReqs > 0 {
			busy = float64(b.ActiveRequests) / float64(b.MaxConcurrentReqs)
		}
		models := len(loadedByBackend[b.ID])
		modelsScore := 0.0
		if b.MaxModels > 0 {
			modelsScore = float64(models) / float64(b.MaxModels)
		}
		if b.Status == "healthy" {
			if busy > 0.8 || modelsScore > 0.9 {
				overloadedCount++
			} else if busy < 0.3 && modelsScore < 0.5 {
				underloadedCount++
			}
		}
	}

	resp := map[string]interface{}{
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"backends":    backendList,
		"suggestions": suggestions,
		"summary": map[string]interface{}{
			"totalBackends":    len(backends),
			"overloadedCount":  overloadedCount,
			"underloadedCount": underloadedCount,
			"suggestionCount":  len(suggestions),
		},
		"hint": "POST /api/v1/admin/cluster/autosuggest/apply with {suggestion_ids: [...]} to apply.",
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleAdminAutosuggestApply — POST /api/v1/admin/cluster/autosuggest/apply (R59.1)
//
// Body: {"suggestion_ids": [...]}
// Returns ApplyResult with per-suggestion status (applied/failed).
//
// Behavior:
//   - Re-validates ALL suggestions from current state (don't trust client).
//   - Filters to requested IDs; unknown IDs reported as failed.
//   - Validates cross-type + status + loaded model existence (TDD'd).
//   - Calls injected applyFn for each accepted suggestion (currently stub).
//   - Best-effort: partial success returned (applied=N, failed=M).
//   - NO atomic apply — if suggestion #2 fails, suggestion #1 stays applied.
func (s *Server) handleAdminAutosuggestApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	proxy := s.proxy
	if proxy != nil {
		// proxy is required
	} else {
		http.Error(w, "balancer proxy not initialized", http.StatusInternalServerError)
		return
	}

	var body struct {
		SuggestionIDs []string `json:"suggestion_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	if len(body.SuggestionIDs) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "suggestion_ids required"})
		return
	}

	// Collect backends + loaded models
	backends := proxy.GetAllBackends()
	metrics := proxy.GetMetricsManager()
	loadedByBackend := make(map[string][]string, len(backends))
	for _, b := range backends {
		loaded := []string{}
		if metrics != nil {
			lm := metrics.GetLlamaCppMetrics(b.ID)
			if lm != nil {
				for _, m := range lm.LoadedModels {
					loaded = append(loaded, m.Name)
				}
			}
		}
		loadedByBackend[b.ID] = loaded
	}

	// Re-compute suggestions (don't trust client — always derive from current state)
	allSuggestions := proxy.ComputeAutosuggestions(loadedByBackend)

	// Apply via injected function. R59.1: stubbed (just logs).
	// R59.2 will wire to GgufApi.loadModel / unloadModel with proper HTTP calls.
	applyFn := func(from, to, model string) error {
		logger.Get().Infow("autosuggest.apply: would move model",
			"from", from, "to", to, "model", model)
		return nil
	}

	requested := body.SuggestionIDs
	// ApplySuggestions expects []*types.Backend; GetAllBackends returns []types.Backend.
	// Convert value-slice to pointer-slice.
	ptrBackends := make([]*types.Backend, len(backends))
	for i := range backends {
		b := backends[i]
		ptrBackends[i] = &b
	}
	result := balancer.ApplySuggestions(
		ptrBackends, allSuggestions, loadedByBackend, &requested, applyFn,
	)
	s.writeJSON(w, http.StatusOK, result)
}
