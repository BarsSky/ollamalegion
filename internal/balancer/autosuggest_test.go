// autosuggest_test.go — TDD for R59 cluster autosuggest algorithm.
//
// Reference: docs/superpowers/specs/2026-09-03-cluster-autodistribute.md
//
// These tests are pure unit tests — no HTTP, no real backends. They verify
// the algorithm with mock backend states.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// makeBackend creates a Backend for testing with the given params.
func makeBackend(id string, status types.BackendStatus, active, maxC, modelCount, maxModels int, btype types.BackendType) *types.Backend {
	return &types.Backend{
		ID:                id,
		Name:              id,
		Host:              "localhost",
		OllamaPort:        11434,
		Weight:            1,
		MaxConcurrentReqs: maxC,
		MaxModels:         maxModels,
		Status:            status,
		ActiveRequests:    active,
		Type:              btype,
	}
}

// statusHealthy / statusUnhealthy — alias constants to avoid import-name mismatch.
var (
	statusHealthy   = types.StatusHealthy
	statusUnhealthy = types.StatusUnhealthy
)

// TestComputeSuggestions_NoOverload — 2 healthy backends, 0 loaded models.
// No suggestions expected (nothing to redistribute).
func TestComputeSuggestions_NoOverload(t *testing.T) {
	backends := []*types.Backend{
		makeBackend("b1", statusHealthy, 0, 10, 0, 4, types.BackendTypeOllama),
		makeBackend("b2", statusHealthy, 0, 10, 0, 4, types.BackendTypeOllama),
	}
	// No loaded models anywhere
	loadedByBackend := map[string][]string{}
	sug := ComputeSuggestions(backends, loadedByBackend)
	if len(sug) != 0 {
		t.Errorf("expected 0 suggestions, got %d: %v", len(sug), sug)
	}
}

// TestComputeSuggestions_OneOverloaded — 1 backend overloaded, 1 underloaded.
// Should suggest moving a model from overloaded to underloaded.
func TestComputeSuggestions_OneOverloaded(t *testing.T) {
	backends := []*types.Backend{
		// b1 overloaded: 9/10 active, 4/4 models (100% util)
		makeBackend("b1", statusHealthy, 9, 10, 4, 4, types.BackendTypeOllama),
		// b2 underloaded: 1/10 active, 1/4 models (25% util)
		makeBackend("b2", statusHealthy, 1, 10, 1, 4, types.BackendTypeOllama),
	}
	loadedByBackend := map[string][]string{
		"b1": {"qwen3-8b", "llama-3.1-8b", "gemma-4-9b", "mistral-7b"},
		"b2": {"phi-3-mini"},
	}
	sug := ComputeSuggestions(backends, loadedByBackend)
	if len(sug) == 0 {
		t.Fatal("expected at least 1 suggestion, got 0")
	}
	// First suggestion should be a move from b1 to b2
	if sug[0].FromBackend != "b1" {
		t.Errorf("expected from=b1, got %q", sug[0].FromBackend)
	}
	if sug[0].ToBackend != "b2" {
		t.Errorf("expected to=b2, got %q", sug[0].ToBackend)
	}
	if sug[0].Type != "move" {
		t.Errorf("expected type=move, got %q", sug[0].Type)
	}
	if sug[0].Priority != 1 {
		t.Errorf("expected priority=1, got %d", sug[0].Priority)
	}
}

// TestComputeSuggestions_DifferentType — cross-type move is forbidden.
// b1 = ollama (overloaded), b2 = llama_cpp (underloaded). NO suggestion
// should be made (cannot move ollama model to cppworker backend).
func TestComputeSuggestions_DifferentType(t *testing.T) {
	backends := []*types.Backend{
		makeBackend("b1", statusHealthy, 9, 10, 4, 4, types.BackendTypeOllama),
		makeBackend("b2", statusHealthy, 1, 10, 1, 4, types.BackendTypeLlamaCpp),
	}
	loadedByBackend := map[string][]string{
		"b1": {"qwen3-8b", "llama-3.1-8b", "gemma-4-9b", "mistral-7b"},
		"b2": {},
	}
	sug := ComputeSuggestions(backends, loadedByBackend)
	for _, s := range sug {
		if s.Type == "move" && s.FromBackend == "b1" && s.ToBackend == "b2" {
			t.Errorf("cross-type move suggested (b1=ollama → b2=llama_cpp); should be skipped")
		}
	}
}

// TestComputeSuggestions_UnhealthySkipped — unhealthy backends are NOT
// destinations for new models.
func TestComputeSuggestions_UnhealthySkipped(t *testing.T) {
	backends := []*types.Backend{
		makeBackend("b1", statusHealthy, 9, 10, 4, 4, types.BackendTypeOllama),
		makeBackend("b2", statusUnhealthy, 0, 10, 0, 4, types.BackendTypeOllama),
	}
	loadedByBackend := map[string][]string{
		"b1": {"qwen3-8b", "llama-3.1-8b", "gemma-4-9b", "mistral-7b"},
		"b2": {},
	}
	sug := ComputeSuggestions(backends, loadedByBackend)
	for _, s := range sug {
		if s.ToBackend == "b2" {
			t.Errorf("suggested moving to unhealthy b2; should be skipped")
		}
	}
}

// TestComputeSuggestions_BackendUtilization — verify the busy_score and
// model_count_score are computed correctly.
func TestComputeSuggestions_BackendUtilization(t *testing.T) {
	backends := []*types.Backend{
		makeBackend("b1", statusHealthy, 5, 10, 2, 4, types.BackendTypeOllama),
	}
	loadedByBackend := map[string][]string{"b1": {"m1", "m2"}}
	state := ComputeBackendUtilization(backends[0], loadedByBackend["b1"])
	if state.BusyScore < 0.49 || state.BusyScore > 0.51 {
		t.Errorf("busy_score for 5/10 should be ~0.5, got %f", state.BusyScore)
	}
	if state.ModelCountScore < 0.49 || state.ModelCountScore > 0.51 {
		t.Errorf("model_count_score for 2/4 should be ~0.5, got %f", state.ModelCountScore)
	}
	if state.IsUnderloaded || state.IsOverloaded {
		t.Errorf("b1 (5/10 active, 2/4 models) should not be overloaded or underloaded: %+v", state)
	}
}

// TestComputeSuggestions_LimitCount — at most 3 suggestions per overloaded
// backend (don't flood operator with too many).
func TestComputeSuggestions_LimitCount(t *testing.T) {
	backends := []*types.Backend{
		makeBackend("b1", statusHealthy, 9, 10, 4, 4, types.BackendTypeOllama),
		makeBackend("b2", statusHealthy, 0, 10, 0, 4, types.BackendTypeOllama),
	}
	loadedByBackend := map[string][]string{
		"b1": {"m1", "m2", "m3", "m4"},
		"b2": {},
	}
	sug := ComputeSuggestions(backends, loadedByBackend)
	movesToB2 := 0
	for _, s := range sug {
		if s.Type == "move" && s.ToBackend == "b2" {
			movesToB2++
		}
	}
	if movesToB2 > 3 {
		t.Errorf("expected at most 3 moves to b2, got %d (algorithm floods operator)", movesToB2)
	}
}
