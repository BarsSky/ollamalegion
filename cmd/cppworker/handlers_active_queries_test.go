package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// newTestBackend — создаёт минимальный backend для тестов active-queries.
// Используем текущий паттерн из handlers_config_test.go.
func newTestBackend() *cppbackend.Backend {
	b := cppbackend.NewBackend(cppbackend.Config{
		Host:      "127.0.0.1",
		Port:      18091,
		ModelsDir: "",
	})
	if err := b.Init(); err != nil {
		// stub-build не имеет настоящего llama.cpp — Init может вернуть
		// ошибку, но InFlight counter всё равно должен быть рабочим.
		_ = err
	}
	return b
}

// TestHandleGetActiveQueries_PerModel — GET /api/models/active-queries?model=X
// возвращает {"model": X, "activeQueries": N} для конкретной модели.
func TestHandleGetActiveQueries_PerModel(t *testing.T) {
	// Setup: inflight counter
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()
	backend.InFlight().Reset("test-model")

	req := httptest.NewRequest(http.MethodGet, "/api/models/active-queries?model=test-model", nil)
	rr := httptest.NewRecorder()

	handleGetActiveQueries(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", rr.Code, rr.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if resp["model"] != "test-model" {
		t.Errorf("expected model=test-model, got %v", resp["model"])
	}
	// 0 активных запросов
	if n, ok := resp["activeQueries"].(float64); !ok || n != 0 {
		t.Errorf("expected activeQueries=0, got %v", resp["activeQueries"])
	}
}

// TestHandleGetActiveQueries_PerModel_NonZero — проверяет, что
// Inc/Dec правильно отражается в ответе endpoint'а.
func TestHandleGetActiveQueries_PerModel_NonZero(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	// Simulate 3 active generations
	backend.InFlight().Inc("busy-model")
	backend.InFlight().Inc("busy-model")
	backend.InFlight().Inc("busy-model")
	defer backend.InFlight().Reset("busy-model")

	req := httptest.NewRequest(http.MethodGet, "/api/models/active-queries?model=busy-model", nil)
	rr := httptest.NewRecorder()

	handleGetActiveQueries(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&resp)
	if n := resp["activeQueries"].(float64); n != 3 {
		t.Errorf("expected activeQueries=3, got %v", n)
	}
}

// TestHandleGetActiveQueries_All — без ?model возвращает snapshot всех
// моделей с ненулевыми счётчиками + total.
func TestHandleGetActiveQueries_All(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	backend.InFlight().Inc("modelA")
	backend.InFlight().Inc("modelA")
	backend.InFlight().Inc("modelB")
	defer func() {
		backend.InFlight().Reset("modelA")
		backend.InFlight().Reset("modelB")
	}()

	req := httptest.NewRequest(http.MethodGet, "/api/models/active-queries", nil)
	rr := httptest.NewRecorder()

	handleGetActiveQueries(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&resp)

	queries, ok := resp["queries"].(map[string]interface{})
	if !ok {
		t.Fatalf("queries field missing or wrong type: %v", resp)
	}
	if len(queries) != 2 {
		t.Errorf("expected 2 models in queries, got %d", len(queries))
	}
	if queries["modelA"].(float64) != 2 {
		t.Errorf("modelA expected 2, got %v", queries["modelA"])
	}
	if queries["modelB"].(float64) != 1 {
		t.Errorf("modelB expected 1, got %v", queries["modelB"])
	}
	if total := resp["total"].(float64); total != 3 {
		t.Errorf("total expected 3, got %v", total)
	}
	if count := resp["count"].(float64); count != 2 {
		t.Errorf("count expected 2, got %v", count)
	}
}

// TestHandleGetActiveQueries_All_Empty — без активных запросов возвращает
// пустую карту.
func TestHandleGetActiveQueries_All_Empty(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	req := httptest.NewRequest(http.MethodGet, "/api/models/active-queries", nil)
	rr := httptest.NewRecorder()

	handleGetActiveQueries(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&resp)
	queries := resp["queries"].(map[string]interface{})
	if len(queries) != 0 {
		t.Errorf("expected empty queries, got %d entries", len(queries))
	}
	if total := resp["total"].(float64); total != 0 {
		t.Errorf("expected total=0, got %v", total)
	}
}

// TestHandleGetActiveQueries_MethodNotAllowed — только GET.
func TestHandleGetActiveQueries_MethodNotAllowed(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	req := httptest.NewRequest(http.MethodPost, "/api/models/active-queries", nil)
	rr := httptest.NewRecorder()

	handleGetActiveQueries(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rr.Code)
	}
}

// TestHandleGetActiveQueries_UnknownModel — для незагруженной модели N=0,
// а не 404. WebUI polling удобнее когда всегда 200.
func TestHandleGetActiveQueries_UnknownModel(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	req := httptest.NewRequest(http.MethodGet, "/api/models/active-queries?model=never-loaded", nil)
	rr := httptest.NewRecorder()

	handleGetActiveQueries(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for unknown model, got %d", rr.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&resp)
	if n := resp["activeQueries"].(float64); n != 0 {
		t.Errorf("expected activeQueries=0 for unknown model, got %v", n)
	}
}

// TestHandleGetActiveQueries_TrimmedModel — model name с пробелами trim'ится.
func TestHandleGetActiveQueries_TrimmedModel(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()
	backend.InFlight().Inc("trimmed-model")
	defer backend.InFlight().Reset("trimmed-model")

	req := httptest.NewRequest(http.MethodGet, "/api/models/active-queries?model=%20trimmed-model%20", nil)
	rr := httptest.NewRecorder()

	handleGetActiveQueries(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&resp)
	if n := resp["activeQueries"].(float64); n != 1 {
		t.Errorf("expected activeQueries=1 after trim, got %v", n)
	}
	if resp["model"] != "trimmed-model" {
		t.Errorf("expected trimmed name in response, got %v", resp["model"])
	}
}

// TestSumInt64Values — unit-тест для helper'а.
func TestSumInt64Values(t *testing.T) {
	if got := sumInt64Values(map[string]int64{}); got != 0 {
		t.Errorf("empty map should sum to 0, got %d", got)
	}
	if got := sumInt64Values(map[string]int64{"a": 5, "b": 3, "c": 2}); got != 10 {
		t.Errorf("expected 10, got %d", got)
	}
}
