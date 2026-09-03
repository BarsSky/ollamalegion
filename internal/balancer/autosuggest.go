// autosuggest.go — Cluster AutoDistribute algorithm (R59, 2026-09-03).
//
// Reference: docs/superpowers/specs/2026-09-03-cluster-autodistribute.md
//
// This is the MINIMUM useful version. It analyzes cluster state and produces
// suggestions WITHOUT applying them. Operator reviews + applies via separate
// endpoint (R59.1).
//
// Algorithm:
//  1. Compute busyScore and modelCountScore per backend.
//  2. Identify overloaded (busy > 0.8 OR models > 0.9) and underloaded
//     (busy < 0.3 AND models < 0.5) backends.
//  3. For each overloaded backend, suggest moving models to underloaded
//     backends of the SAME Type (cross-type moves break load format).
//  4. Cap at 3 suggestions per source backend (don't flood operator).
//  5. Skip unhealthy backends as destinations.
//
// R59 scope: GET /api/v1/admin/cluster/autosuggest endpoint. R59.1+
// adds POST /apply.
package balancer

import (
	"fmt"
	"sort"

	"ollama-loadbalancer/pkg/types"
)

// Suggestion is a single autosuggest recommendation.
type Suggestion struct {
	ID          string `json:"id"`
	Type        string `json:"type"` // "move" | "load" | "weight"
	FromBackend string `json:"fromBackend,omitempty"`
	ToBackend   string `json:"toBackend"`
	Model       string `json:"model,omitempty"`
	Reason      string `json:"reason"`
	Priority    int    `json:"priority"` // 1=high, 2=medium, 3=low
}

// BackendUtilization — utilization metrics for a single backend.
type BackendUtilization struct {
	BackendID        string
	Status           types.BackendStatus
	ActiveRequests   int
	MaxConcurrent    int
	BusyScore        float64 // activeRequests / maxConcurrent
	LoadedModels     []string
	ModelCount       int
	MaxModels        int
	ModelCountScore  float64 // modelCount / maxModels
	IsOverloaded     bool
	IsUnderloaded    bool
	Unhealthy        bool
}

// ComputeBackendUtilization — pure function, computes util metrics for one backend.
func ComputeBackendUtilization(b *types.Backend, loadedModels []string) BackendUtilization {
	u := BackendUtilization{
		BackendID:      b.ID,
		Status:         b.Status,
		ActiveRequests: b.ActiveRequests,
		MaxConcurrent:  b.MaxConcurrentReqs,
		LoadedModels:   loadedModels,
		ModelCount:     len(loadedModels),
		MaxModels:      b.MaxModels,
	}
	if b.MaxConcurrentReqs > 0 {
		u.BusyScore = float64(b.ActiveRequests) / float64(b.MaxConcurrentReqs)
	}
	if b.MaxModels > 0 {
		u.ModelCountScore = float64(len(loadedModels)) / float64(b.MaxModels)
	}
	u.Unhealthy = (b.Status != types.StatusHealthy)
	u.IsOverloaded = !u.Unhealthy && (u.BusyScore > 0.8 || u.ModelCountScore > 0.9)
	u.IsUnderloaded = !u.Unhealthy && (u.BusyScore < 0.3 && u.ModelCountScore < 0.5)
	return u
}

// ComputeSuggestions — main entry point. Returns ordered suggestions
// (highest priority first). Input: list of backends + map of backendId → loaded model names.
//
// Output: array of Suggestion. Empty if cluster is balanced.
func ComputeSuggestions(backends []*types.Backend, loadedByBackend map[string][]string) []Suggestion {
	if len(backends) < 2 {
		return nil
	}

	// Step 1: compute util for all backends
	utils := make(map[string]BackendUtilization)
	for _, b := range backends {
		utils[b.ID] = ComputeBackendUtilization(b, loadedByBackend[b.ID])
	}

	// Step 2: partition into overloaded + underloaded
	var overloaded []string
	var underloaded []string
	for id, u := range utils {
		if u.IsOverloaded {
			overloaded = append(overloaded, id)
		} else if u.IsUnderloaded {
			underloaded = append(underloaded, id)
		}
	}
	// Stable order: deterministic output
	sort.Strings(overloaded)
	sort.Strings(underloaded)

	if len(overloaded) == 0 || len(underloaded) == 0 {
		return nil
	}

	// Step 3: for each overloaded backend, suggest moves to underloaded
	var suggestions []Suggestion
	sugCounter := 0
	const maxSuggestionsPerSource = 3
	const minBusyToSuggest = 0.8 // only suggest if source is really stressed

	for _, srcID := range overloaded {
		src := utils[srcID]
		if src.BusyScore < minBusyToSuggest && src.ModelCountScore <= 0.9 {
			continue
		}
		srcBackend := findBackend(backends, srcID)
		if srcBackend == nil {
			continue
		}
		perSourceCount := 0
		for _, model := range src.LoadedModels {
			if perSourceCount >= maxSuggestionsPerSource {
				break
			}
			// Find best destination: underloaded + same type
			bestDestID := ""
			bestScore := -1.0
			for _, dstID := range underloaded {
				dst := utils[dstID]
				if dst.BackendID == srcID {
					continue
				}
				// Cross-type check (defensive — algorithm already excludes, but Type check is explicit)
				dstBackend := findBackend(backends, dstID)
				if dstBackend == nil || dstBackend.Type != srcBackend.Type {
					continue
				}
				// Prefer destination with most free capacity
				freeCap := 1.0 - dst.BusyScore
				if dst.ModelCountScore < 0.5 {
					freeCap += 0.5
				}
				if freeCap > bestScore {
					bestScore = freeCap
					bestDestID = dstID
				}
			}
			if bestDestID == "" {
				continue
			}
			sugCounter++
			priority := 1
			if src.BusyScore < 0.9 && src.ModelCountScore < 0.95 {
				priority = 2
			}
			suggestions = append(suggestions, Suggestion{
				ID:          fmt.Sprintf("sug-%d", sugCounter),
				Type:        "move",
				FromBackend: srcID,
				ToBackend:   bestDestID,
				Model:       model,
				Reason: fmt.Sprintf("%s busy=%.2f models=%d/%d; %s busy=%.2f models=%d/%d",
					srcID, src.BusyScore, src.ModelCount, src.MaxModels,
					bestDestID, utils[bestDestID].BusyScore, utils[bestDestID].ModelCount, utils[bestDestID].MaxModels),
				Priority: priority,
			})
			perSourceCount++
		}
	}
	return suggestions
}

// findBackend — small helper. Returns nil if not found.
func findBackend(backends []*types.Backend, id string) *types.Backend {
	for _, b := range backends {
		if b.ID == id {
			return b
		}
	}
	return nil
}
