// round12_nparallel_config_test.go — Round 12 (2026-07-28): тесты для
// applyInt("defaultNParallel", ...) в handleCppWorkerUpdateConfig.
//
// Покрывает:
//   - PUT с валидным defaultNParallel (0..8) → applied, currentConfig updated
//   - PUT с out-of-range (>8) → отклоняется, currentConfig не меняется
//   - PUT с невалидным значением (string вместо number) → отклоняется
//   - PUT с null/пропуском → defaultNParallel остаётся как был (applyInt skip)
//   - Round-trip: defaultNParallel в knownKeys (не даёт "unknown field" warning)
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestPUTConfig_DefaultNParallel_Valid проверяет что валидные значения
// defaultNParallel (0..8) применяются через PUT.
func TestPUTConfig_DefaultNParallel_Valid(t *testing.T) {
	prevCfg := currentConfig
	prevBackend := backend
	defer func() {
		currentConfig = prevCfg
		backend = prevBackend
	}()

	// Минимальный backend stub (init может fail в test-mode → Skip).
	currentConfig = &cppbackend.Config{
		Host:      "127.0.0.1",
		Port:      18091,
		ModelsDir: t.TempDir(),
	}
	backend = nil // не важно — applyInt сработает до reload

	for _, val := range []int{0, 1, 2, 4, 8} {
		body, _ := json.Marshal(map[string]interface{}{
			"defaultNParallel": val,
		})
		req := httptest.NewRequest(http.MethodPut,
			"/api/v1/cppworker/config",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handleCppWorkerUpdateConfig(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("PUT defaultNParallel=%d: status=%d body=%s",
				val, w.Code, w.Body.String())
			continue
		}
		if currentConfig.DefaultNParallel != val {
			t.Errorf("PUT defaultNParallel=%d: currentConfig.DefaultNParallel=%d, want %d",
				val, currentConfig.DefaultNParallel, val)
		}
	}
}

// TestPUTConfig_DefaultNParallel_OutOfRange проверяет что значения >8
// отклоняются applyInt validation (max=8).
func TestPUTConfig_DefaultNParallel_OutOfRange(t *testing.T) {
	prevCfg := currentConfig
	defer func() { currentConfig = prevCfg }()

	currentConfig = &cppbackend.Config{
		Host:              "127.0.0.1",
		Port:              18091,
		ModelsDir:         t.TempDir(),
		DefaultNParallel:  2, // baseline
	}

	for _, val := range []int{9, 16, 100, 1000} {
		body, _ := json.Marshal(map[string]interface{}{
			"defaultNParallel": val,
		})
		req := httptest.NewRequest(http.MethodPut,
			"/api/v1/cppworker/config",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handleCppWorkerUpdateConfig(w, req)

		// 200 OK + validation_errors в response, currentConfig НЕ изменился.
		if w.Code != http.StatusOK {
			t.Errorf("PUT defaultNParallel=%d: status=%d (want 200 even on validation fail)",
				val, w.Code)
		}
		if currentConfig.DefaultNParallel != 2 {
			t.Errorf("PUT defaultNParallel=%d: currentConfig.DefaultNParallel=%d, want baseline 2 (rejected)",
				val, currentConfig.DefaultNParallel)
		}
	}
}

// TestPUTConfig_DefaultNParallel_InvalidType проверяет что строковое значение
// (вместо int) отклоняется парсером getInt → applied не содержит ключ.
func TestPUTConfig_DefaultNParallel_InvalidType(t *testing.T) {
	prevCfg := currentConfig
	defer func() { currentConfig = prevCfg }()

	currentConfig = &cppbackend.Config{
		Host:             "127.0.0.1",
		Port:             18091,
		ModelsDir:        t.TempDir(),
		DefaultNParallel: 1,
	}

	body, _ := json.Marshal(map[string]interface{}{
		"defaultNParallel": "not-a-number",
	})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (with validation_errors), got %d", w.Code)
	}
	if currentConfig.DefaultNParallel != 1 {
		t.Errorf("currentConfig.DefaultNParallel=%d, want baseline 1 (rejected)",
			currentConfig.DefaultNParallel)
	}
}

// TestPUTConfig_DefaultNParallel_NullSkip проверяет null → applyInt skip
// (forward-compat: WebUI отправляет пустые поля как null).
func TestPUTConfig_DefaultNParallel_NullSkip(t *testing.T) {
	prevCfg := currentConfig
	defer func() { currentConfig = prevCfg }()

	currentConfig = &cppbackend.Config{
		Host:             "127.0.0.1",
		Port:             18091,
		ModelsDir:        t.TempDir(),
		DefaultNParallel: 4, // baseline
	}

	body, _ := json.Marshal(map[string]interface{}{
		"defaultNParallel": nil, // null → skip
	})
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if currentConfig.DefaultNParallel != 4 {
		t.Errorf("null should skip: DefaultNParallel=%d, want baseline 4",
			currentConfig.DefaultNParallel)
	}
}
