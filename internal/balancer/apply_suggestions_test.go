// apply_suggestions_test.go — TDD for R59.1 apply endpoint.
//
// Reference: docs/archive/superpowers/specs/2026-09-03-cluster-autosuggest-apply.md
//
// Tests cover the ApplySuggestions pure function (no HTTP, no real backends).
// HTTP handler tests will be in handlers_autosuggest_test.go (R59.1+).
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// makeSuggestion — helper for tests.
func makeSuggestion(id, from, to, model string, priority int) Suggestion {
	return Suggestion{
		ID:          id,
		Type:        "move",
		FromBackend: from,
		ToBackend:   to,
		Model:       model,
		Reason:      "test",
		Priority:    priority,
	}
}

// TestApplySuggestions_AllValid — все suggestions валидны.
//
// R499 / аудит S-7 (2026-11-19): без applyFn записи НЕ «applied», а «planned».
//
// БЫЛО (этот тест и закреплял): applyFn = nil, но результат считался
// «2 applied» — то есть отчёт выдавал валидацию за переезд моделей, ничего не
// применяя. Отдельный подтест ниже — положительный контроль: с реальной
// функцией применения те же две записи обязаны дать applied=2, иначе «planned»
// можно было бы получить всегда и тест ничего не проверял бы.
func TestApplySuggestions_AllValid(t *testing.T) {
	backends := []*types.Backend{
		makeBackend("b1", statusHealthy, 0, 10, 0, 4, types.BackendTypeOllama),
		makeBackend("b2", statusHealthy, 0, 10, 0, 4, types.BackendTypeOllama),
	}
	suggestions := []Suggestion{
		makeSuggestion("sug-1", "b1", "b2", "m1", 1),
		makeSuggestion("sug-2", "b1", "b2", "m2", 2),
	}
	loadedByBackend := map[string][]string{
		"b1": {"m1", "m2"},
		"b2": {},
	}

	t.Run("dry-run (applyFn=nil) → planned, не applied", func(t *testing.T) {
		result := ApplySuggestions(backends, suggestions, loadedByBackend, nil /* requestedIDs */, nil /* applyFn */)
		if result.Applied != 0 {
			t.Errorf("applied = %d без функции применения: валидация выдана за переезд (аудит S-7)", result.Applied)
		}
		if result.Planned != 2 {
			t.Errorf("planned = %d, ожидалось 2 (проверено, но не применено)", result.Planned)
		}
		if result.Failed != 0 {
			t.Errorf("failed = %d, ожидалось 0", result.Failed)
		}
	})

	t.Run("с applyFn → applied", func(t *testing.T) {
		calls := 0
		applyFn := func(from, to, model string) error {
			calls++
			return nil
		}
		result := ApplySuggestions(backends, suggestions, loadedByBackend, nil, applyFn)
		if result.Applied != 2 {
			t.Errorf("applied = %d с функцией применения, ожидалось 2", result.Applied)
		}
		if result.Planned != 0 {
			t.Errorf("planned = %d при реальном применении, ожидалось 0", result.Planned)
		}
		if calls != 2 {
			t.Errorf("функция применения вызвана %d раз, ожидалось 2", calls)
		}
	})
}

// TestApplySuggestions_InvalidID — suggestion_id не существует в списке.
func TestApplySuggestions_InvalidID(t *testing.T) {
	backends := []*types.Backend{
		makeBackend("b1", statusHealthy, 0, 10, 0, 4, types.BackendTypeOllama),
	}
	suggestions := []Suggestion{
		makeSuggestion("sug-1", "b1", "b1", "m1", 1),
	}
	// Request asks for sug-99, but only sug-1 exists
	requested := []string{"sug-99"}
	result := ApplySuggestions(backends, suggestions, map[string][]string{"b1": {"m1"}},
		&requested, nil)
	if result.Applied != 0 {
		t.Errorf("expected 0 applied (sug-99 doesn't exist), got %d", result.Applied)
	}
	if result.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", result.Failed)
	}
}

// TestApplySuggestions_ModelNotLoaded — sug says "move m1 from b1" but m1 not loaded on b1.
func TestApplySuggestions_ModelNotLoaded(t *testing.T) {
	backends := []*types.Backend{
		makeBackend("b1", statusHealthy, 0, 10, 0, 4, types.BackendTypeOllama),
		makeBackend("b2", statusHealthy, 0, 10, 0, 4, types.BackendTypeOllama),
	}
	suggestions := []Suggestion{
		makeSuggestion("sug-1", "b1", "b2", "m1", 1),
	}
	loadedByBackend := map[string][]string{
		"b1": {"m2"}, // m1 not loaded
		"b2": {},
	}
	requested := []string{"sug-1"}
	result := ApplySuggestions(backends, suggestions, loadedByBackend, &requested, nil)
	if result.Applied != 0 {
		t.Errorf("expected 0 applied (m1 not on b1), got %d", result.Applied)
	}
	if result.Failed != 1 {
		t.Errorf("expected 1 failed, got %d", result.Failed)
	}
	if len(result.Errors) == 0 {
		t.Errorf("expected error message, got none")
	}
}

// TestApplySuggestions_CrossType — different types rejected before load attempt.
func TestApplySuggestions_CrossType(t *testing.T) {
	backends := []*types.Backend{
		makeBackend("b1", statusHealthy, 0, 10, 0, 4, types.BackendTypeOllama),
		makeBackend("b2", statusHealthy, 0, 10, 0, 4, types.BackendTypeLlamaCpp),
	}
	suggestions := []Suggestion{
		makeSuggestion("sug-1", "b1", "b2", "m1", 1),
	}
	loadedByBackend := map[string][]string{
		"b1": {"m1"},
		"b2": {},
	}
	requested := []string{"sug-1"}
	result := ApplySuggestions(backends, suggestions, loadedByBackend, &requested, nil)
	if result.Applied != 0 {
		t.Errorf("cross-type should be rejected, got %d applied", result.Applied)
	}
}

// TestApplySuggestions_UnhealthySource — source backend is unhealthy, rejected.
func TestApplySuggestions_UnhealthySource(t *testing.T) {
	backends := []*types.Backend{
		makeBackend("b1", statusUnhealthy, 0, 10, 0, 4, types.BackendTypeOllama),
		makeBackend("b2", statusHealthy, 0, 10, 0, 4, types.BackendTypeOllama),
	}
	suggestions := []Suggestion{
		makeSuggestion("sug-1", "b1", "b2", "m1", 1),
	}
	loadedByBackend := map[string][]string{
		"b1": {"m1"},
		"b2": {},
	}
	requested := []string{"sug-1"}
	result := ApplySuggestions(backends, suggestions, loadedByBackend, &requested, nil)
	if result.Applied != 0 {
		t.Errorf("unhealthy source should be rejected, got %d applied", result.Applied)
	}
}

// TestApplySuggestions_EmptyList — empty suggestion_ids list.
func TestApplySuggestions_EmptyList(t *testing.T) {
	backends := []*types.Backend{
		makeBackend("b1", statusHealthy, 0, 10, 0, 4, types.BackendTypeOllama),
	}
	suggestions := []Suggestion{
		makeSuggestion("sug-1", "b1", "b1", "m1", 1),
	}
	loadedByBackend := map[string][]string{"b1": {"m1"}}
	emptyList := []string{}
	result := ApplySuggestions(backends, suggestions, loadedByBackend, &emptyList, nil)
	if result.Applied != 0 {
		t.Errorf("empty list should give 0 applied, got %d", result.Applied)
	}
}
