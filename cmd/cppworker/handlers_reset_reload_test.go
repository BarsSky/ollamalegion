// handlers_reset_reload_test.go — тесты для handleResetReloadCounter.
//
// R-6: эндпоинт сброса ramFallbackAttempts без docker restart.
//
// Используется stub-сборка (build tag llama_stub), поскольку endpoint
// работает на уровне in-memory sync.Map и не требует реального llama.cpp.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resetReloadCounterTestSetup — очищает глобальный sync.Map перед каждым тестом.
func resetReloadCounterTestSetup() {
	ramFallbackAttempts.Range(func(key, value interface{}) bool {
		ramFallbackAttempts.Delete(key)
		return true
	})
}

func TestResetReloadCounter_AllModels(t *testing.T) {
	resetReloadCounterTestSetup()
	defer resetReloadCounterTestSetup()

	// Засеять счётчик для двух моделей
	recordReloadAttempt("model-A")
	recordReloadAttempt("model-B")
	recordReloadAttempt("model-B")

	// Проверить, что состояние есть
	if count, _ := getReloadAttempts("model-A"); count != 1 {
		t.Fatalf("expected count=1 for model-A, got %d", count)
	}
	if count, _ := getReloadAttempts("model-B"); count != 2 {
		t.Fatalf("expected count=2 for model-B, got %d", count)
	}

	// Запрос без body → reset all
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cppworker/reset-reload-counter", nil)
	w := httptest.NewRecorder()
	handleResetReloadCounter(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if status, _ := resp["status"].(string); status != "ok" {
		t.Errorf("expected status=ok, got %v", resp["status"])
	}

	// resetCount должен быть >= 2 (минимум две модели в sync.Map)
	if rc, ok := resp["resetCount"].(float64); !ok || rc < 2 {
		t.Errorf("expected resetCount>=2, got %v", resp["resetCount"])
	}

	// Состояние должно быть очищено
	if _, isLimit := getReloadAttempts("model-A"); isLimit {
		t.Error("model-A should be cleared after reset")
	}
	if _, isLimit := getReloadAttempts("model-B"); isLimit {
		t.Error("model-B should be cleared after reset")
	}
}

func TestResetReloadCounter_SingleModel(t *testing.T) {
	resetReloadCounterTestSetup()
	defer resetReloadCounterTestSetup()

	// Засеять
	recordReloadAttempt("target-model")
	recordReloadAttempt("other-model")

	body := bytes.NewBufferString(`{"model": "target-model"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cppworker/reset-reload-counter", body)
	w := httptest.NewRecorder()
	handleResetReloadCounter(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if model, _ := resp["model"].(string); model != "target-model" {
		t.Errorf("expected model=target-model, got %v", resp["model"])
	}

	// resetCount = 1 (только одна модель)
	if rc, _ := resp["resetCount"].(float64); rc != 1 {
		t.Errorf("expected resetCount=1, got %v", resp["resetCount"])
	}

	// target-model очищен
	if _, isLimit := getReloadAttempts("target-model"); isLimit {
		t.Error("target-model should be cleared")
	}

	// other-model НЕ очищен
	if count, _ := getReloadAttempts("other-model"); count != 1 {
		t.Errorf("other-model should still have count=1, got %d", count)
	}
}

func TestResetReloadCounter_MethodNotAllowed(t *testing.T) {
	resetReloadCounterTestSetup()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/reset-reload-counter", nil)
	w := httptest.NewRecorder()
	handleResetReloadCounter(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}

	if !strings.Contains(w.Body.String(), "method not allowed") {
		t.Errorf("expected error message about method, got %s", w.Body.String())
	}
}

func TestResetReloadCounter_InvalidJSON(t *testing.T) {
	resetReloadCounterTestSetup()
	defer resetReloadCounterTestSetup()

	body := bytes.NewBufferString(`{invalid json`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cppworker/reset-reload-counter", body)
	w := httptest.NewRecorder()
	handleResetReloadCounter(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestResetReloadCounter_NoStateNoOp(t *testing.T) {
	resetReloadCounterTestSetup()

	// Нет моделей в sync.Map → reset должен быть no-op, но вернуть 200.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cppworker/reset-reload-counter", nil)
	w := httptest.NewRecorder()
	handleResetReloadCounter(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&resp)

	if rc, _ := resp["resetCount"].(float64); rc != 0 {
		t.Errorf("expected resetCount=0 (no models), got %v", resp["resetCount"])
	}
}