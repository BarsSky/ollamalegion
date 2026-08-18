// handlers_model_round37_test.go — Round 37 (2026-08-18) tests for /api/models
// exposing feasible_max_context and gguf_max_context.
//
// БЕЗ этих полей balancer НЕ ЗНАЕТ feasible vs profile → 413 preflight
// (это и был production bug 2026-08-18).
//go:build llama_stub

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleListModels_ExposesFeasibleAndGGUFMaxContext — главный тест
// для /api/models после Round 37 fix. Проверяет что:
//  1. Top-level: feasible_max_context и gguf_max_context присутствуют
//  2. Per-model: каждый элемент массива models имеет gguf_max_context и
//     feasible_max_context
//  3. Если модель не загружена — top-level поля = 0, но структура ответа валидна
func TestHandleListModels_ExposesFeasibleAndGGUFMaxContext(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	// Без загруженных моделей — top-level feasible/gguf = 0,
	// но поля ДОЛЖНЫ быть в JSON (с нулями).
	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	w := httptest.NewRecorder()
	handleListModels(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse JSON: %v\nbody=%s", err, w.Body.String())
	}

	// === Top-level fields ===
	if _, ok := resp["feasible_max_context"]; !ok {
		t.Error("response MISSING top-level field: feasible_max_context (Round 37 required)")
	}
	if _, ok := resp["gguf_max_context"]; !ok {
		t.Error("response MISSING top-level field: gguf_max_context (Round 37 required)")
	}

	// Verify types
	if _, ok := resp["feasible_max_context"].(float64); !ok {
		t.Errorf("feasible_max_context type = %T, want number", resp["feasible_max_context"])
	}
	if _, ok := resp["gguf_max_context"].(float64); !ok {
		t.Errorf("gguf_max_context type = %T, want number", resp["gguf_max_context"])
	}

	// === Backward compat: existing fields still present ===
	for _, key := range []string{
		"models", "count", "max_vram_n_ctx", "max_ram_n_ctx",
		"available_vram_mb", "total_vram_mb", "available_ram_mb", "total_ram_mb",
		"model_max_context", "gpu_count",
	} {
		if _, ok := resp[key]; !ok {
			t.Errorf("response MISSING backward-compat field: %s", key)
		}
	}

	// === Models array ===
	modelsRaw, ok := resp["models"].([]interface{})
	if !ok {
		t.Fatal("models not an array")
	}
	// Backend без загруженных моделей — массив пустой, но валидный
	if len(modelsRaw) != 0 {
		// Если есть загруженные модели — каждая должна иметь round37 поля
		for i, m := range modelsRaw {
			entry, ok := m.(map[string]interface{})
			if !ok {
				t.Errorf("models[%d] not an object", i)
				continue
			}
			if _, ok := entry["gguf_max_context"]; !ok {
				t.Errorf("models[%d] MISSING gguf_max_context", i)
			}
			if _, ok := entry["feasible_max_context"]; !ok {
				t.Errorf("models[%d] MISSING feasible_max_context", i)
			}
		}
	}
}

// TestHandleListModels_TopLevelDefaultsToZeroWhenNoModels — defensive test:
// when no models are loaded, top-level fields are 0 (not missing).
func TestHandleListModels_TopLevelDefaultsToZeroWhenNoModels(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	w := httptest.NewRecorder()
	handleListModels(w, req)

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse JSON: %v", err)
	}

	if v, _ := resp["feasible_max_context"].(float64); v != 0 {
		t.Errorf("feasible_max_context = %v, want 0 (no models loaded)", v)
	}
	if v, _ := resp["gguf_max_context"].(float64); v != 0 {
		t.Errorf("gguf_max_context = %v, want 0 (no models loaded)", v)
	}
}
