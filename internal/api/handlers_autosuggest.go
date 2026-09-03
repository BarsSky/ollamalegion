// handlers_autosuggest.go — R59 (2026-09-03): Cluster AutoDistribute endpoint.
//
// GET /api/v1/admin/cluster/autosuggest — returns cluster state + suggestions
// for redistributing loaded models. Suggestions are computed by
// balancer.ComputeSuggestions (TDD'd in autosuggest_test.go).
//
// IMPORTANT: this endpoint is read-only. It does NOT apply suggestions.
// Apply requires explicit operator confirmation (R59.1, separate endpoint).
//
// Reference: docs/superpowers/specs/2026-09-03-cluster-autodistribute.md
package api

import (
	"net/http"
	"time"
)

// handleAdminAutosuggest — GET /api/v1/admin/cluster/autosuggest
//
// Returns:
//   - timestamp
//   - backends[] — full state with utilization scores
//   - suggestions[] — ordered list (highest priority first)
//   - summary — counts
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

	// Collect backends + their loaded models from balancer state
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
			"id":                  b.ID,
			"name":                b.Name,
			"status":              string(b.Status),
			"type":                string(b.Type),
			"activeRequests":      b.ActiveRequests,
			"maxConcurrent":       b.MaxConcurrentReqs,
			"maxModels":            b.MaxModels,
			"loadedModels":          loaded,
			"loadedModelCount":      len(loaded),
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
			"totalBackends":     len(backends),
			"overloadedCount":   overloadedCount,
			"underloadedCount":  underloadedCount,
			"suggestionCount":   len(suggestions),
		},
		"hint": "POST /api/v1/admin/cluster/autosuggest/apply with {suggestion_ids: [...]} to apply (R59.1, not implemented yet).",
	}
	s.writeJSON(w, http.StatusOK, resp)
}
