// handlers_config_test.go — тесты для runtime config endpoint + auto-reload.
//
// R-7: WebUI на вкладке GGUF Models должен показывать реальные параметры
// загруженных моделей рядом с дефолтами (default: 8192, runtime: 32768).
//
// R-8: при update config через WebUI загруженные модели должны
// auto-reload с новыми defaults, если изменились параметры нагрузки.
//
// Тесты используют stub-сборку (build tag llama_stub) — backend.ListModels
// возвращает пустой массив, поэтому проверяем структуру ответа и поведение
// helpers без реальной загрузки модели.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

// setupConfigForTest инициализирует глобальный currentConfig (он nil в stub-тестах).
func setupConfigForTest(t *testing.T) *cppbackend.Config {
	t.Helper()
	if currentConfig == nil {
		cfg := &cppbackend.Config{
			Host:                 "127.0.0.1",
			Port:                 18091,
			ModelsDir:            t.TempDir(),
			DefaultCtxSize:       8192,
			DefaultBatchSize:     512,
			DefaultGPULayers:     -1,
			DefaultFlashAttnType: -1,
			DefaultUseMmap:       true,
		}
		currentConfig = cfg
	}
	return currentConfig
}

// TestHandleCppWorkerRuntimeConfig_BackendNil — endpoint возвращает 503,
// если backend не инициализирован (nil).
func TestHandleCppWorkerRuntimeConfig_BackendNil(t *testing.T) {
	prevBackend := backend
	backend = nil
	defer func() { backend = prevBackend }()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/config/runtime", nil)
	w := httptest.NewRecorder()
	handleCppWorkerRuntimeConfig(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when backend is nil, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestHandleCppWorkerRuntimeConfig_MethodNotAllowed — POST/PUT → 405.
func TestHandleCppWorkerRuntimeConfig_MethodNotAllowed(t *testing.T) {
	prevBackend := backend
	backend = &cppbackend.Backend{} // non-nil достаточно для проверки метода
	defer func() { backend = prevBackend }()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cppworker/config/runtime", nil)
	w := httptest.NewRecorder()
	handleCppWorkerRuntimeConfig(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for POST, got %d", w.Code)
	}
}

// TestHandleCppWorkerRuntimeConfig_EmptyModels — при пустом списке моделей
// возвращается корректный JSON с count=0.
func TestHandleCppWorkerRuntimeConfig_EmptyModels(t *testing.T) {
	prevBackend := backend
	// Используем реальный backend с минимальной инициализацией — но т.к. stub-build
	// не имеет настоящего llama.cpp, вызываем только ListModels (которая безопасна).
	b := cppbackend.NewBackend(cppbackend.Config{
		Host:      "127.0.0.1",
		Port:      18091,
		ModelsDir: t.TempDir(),
	})
	if err := b.Init(); err != nil {
		t.Skipf("backend init failed (expected for stub-build): %v", err)
	}
	backend = b
	defer func() { backend = prevBackend }()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/config/runtime", nil)
	w := httptest.NewRecorder()
	handleCppWorkerRuntimeConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	count, _ := resp["count"].(float64)
	if count != 0 {
		t.Fatalf("expected count=0 for empty backend, got %v", resp["count"])
	}
	models, ok := resp["loaded_models"].([]interface{})
	if !ok {
		t.Fatalf("loaded_models must be array, got %T", resp["loaded_models"])
	}
	if len(models) != 0 {
		t.Fatalf("expected empty loaded_models, got %d items", len(models))
	}
}

// TestHasReloadedDefaults — true для параметров нагрузки, false для прочих.
func TestHasReloadedDefaults(t *testing.T) {
	cases := []struct {
		applied []string
		want    bool
	}{
		{[]string{"defaultCtxSize"}, true},
		{[]string{"defaultBatchSize"}, true},
		{[]string{"defaultGpuLayers"}, true},
		{[]string{"defaultFlashAttnType"}, true},
		{[]string{"defaultNuma"}, true},
		{[]string{"defaultUseMmap"}, true},
		{[]string{"defaultNThreads"}, true},
		{[]string{}, false},
		{nil, false},
		{[]string{"port", "modelsDir"}, false},
		{[]string{"defaultCtxSize", "port"}, true}, // один из списка — true
	}
	for _, c := range cases {
		got := hasReloadedDefaults(c.applied)
		if got != c.want {
			t.Errorf("hasReloadedDefaults(%v) = %v, want %v", c.applied, got, c.want)
		}
	}
}

// TestResetAllReloadAttempts — сбрасывает счётчик для всех моделей.
func TestResetAllReloadAttempts(t *testing.T) {
	// Засеять
	recordReloadAttempt("test-model-A")
	recordReloadAttempt("test-model-A")
	recordReloadAttempt("test-model-B")

	if count, _ := getReloadAttempts("test-model-A"); count != 2 {
		t.Fatalf("expected count=2 for test-model-A, got %d", count)
	}

	// Reset
	resetAllReloadAttempts()

	if _, isLimit := getReloadAttempts("test-model-A"); isLimit {
		t.Error("test-model-A should be cleared after reset")
	}
	if _, isLimit := getReloadAttempts("test-model-B"); isLimit {
		t.Error("test-model-B should be cleared after reset")
	}
}

// TestHandleCppWorkerUpdateConfig_BadJSON — некорректный JSON → 400.
func TestHandleCppWorkerUpdateConfig_BadJSON(t *testing.T) {
	setupConfigForTest(t)

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		invalidJSONReader())
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestHandleCppWorkerUpdateConfig_WrongMethod — GET → 405.
func TestHandleCppWorkerUpdateConfig_WrongMethod(t *testing.T) {
	setupConfigForTest(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/config/update", nil)
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", w.Code)
	}
}

// TestHandleCppWorkerUpdateConfig_AppliesUseMmap — проверяет, что PUT с
// defaultUseMmap=true/false корректно применяется к currentConfig и попадает
// в applied[]. Регрессионный тест: ранее defaultUseMmap игнорировался,
// хотя WebUI отправлял это поле.
func TestHandleCppWorkerUpdateConfig_AppliesUseMmap(t *testing.T) {
	cfg := setupConfigForTest(t)
	// Сохраняем исходное значение, чтобы восстановить после теста.
	prevMmap := cfg.DefaultUseMmap
	defer func() { cfg.DefaultUseMmap = prevMmap }()

	// Устанавливаем известное начальное значение.
	cfg.DefaultUseMmap = true

	body := `{"defaultUseMmap": false}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	applied, ok := resp["applied"].([]interface{})
	if !ok {
		t.Fatalf("applied must be array, got %T", resp["applied"])
	}
	found := false
	for _, a := range applied {
		if a == "defaultUseMmap" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("defaultUseMmap not in applied: %v", applied)
	}
	if cfg.DefaultUseMmap != false {
		t.Fatalf("expected DefaultUseMmap=false after update, got %v", cfg.DefaultUseMmap)
	}
}

// TestHandleCppWorkerUpdateConfig_AppliesGpuLayers_Negative — регрессионный тест.
// Session 17 P.3 (2026-07-27): WebUI форма для per-backend настроек имела min=-1,
// но llama.cpp поддерживает gpuLayers=-2 как "auto / all layers" (см.
// internal/cppbackend/backend.go:678). Валидатор PUT отклонял -2 с ошибкой
// "must be >= -1, got -2", что блокировало сохранение "auto" режима.
// После фикса: min=-2, -2/-1/0/N принимаются.
func TestHandleCppWorkerUpdateConfig_AppliesGpuLayers_Negative(t *testing.T) {
	cfg := setupConfigForTest(t)
	prev := cfg.DefaultGPULayers
	defer func() { cfg.DefaultGPULayers = prev }()

	cfg.DefaultGPULayers = 20 // initial state from user

	// Test all accepted values: -2 (auto), -1 (all), 0 (CPU), N (specific).
	cases := []int{-2, -1, 0, 20, 100}
	for _, want := range cases {
		body := `{"defaultGpuLayers": ` + strconv.Itoa(want) + `}`
		req := httptest.NewRequest(http.MethodPut,
			"/api/v1/cppworker/config/update",
			strings.NewReader(body))
		w := httptest.NewRecorder()
		handleCppWorkerUpdateConfig(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("gpuLayers=%d: expected 200, got %d (body: %s)",
				want, w.Code, w.Body.String())
			continue
		}
		if cfg.DefaultGPULayers != want {
			t.Errorf("gpuLayers=%d: expected applied, got %d", want, cfg.DefaultGPULayers)
		}
	}
}

// TestHandleCppWorkerUpdateConfig_NullValuesAreSkipped — регрессионный тест.
// Session 17 P.6 (2026-07-27): WebUI шлёт `defaultTensorSplit: null` для пустого
// поля в форме, и `defaultRopeFreqBase: 0` для неинициализированных numeric полей.
// Раньше cppworker трактовал null как validation error ("expected array of numbers"),
// а 0 — как "must be >= 1, got 0" (если min=1).
// После фикса: null и "0 для поля с min>0" — это "skip" (не обновлять), а не ошибка.
func TestHandleCppWorkerUpdateConfig_NullValuesAreSkipped(t *testing.T) {
	cfg := setupConfigForTest(t)
	prevSplit := cfg.DefaultTensorSplit
	prevRope := cfg.DefaultRopeFreqBase
	defer func() {
		cfg.DefaultTensorSplit = prevSplit
		cfg.DefaultRopeFreqBase = prevRope
	}()

	// Initial values (что-то конкретное, чтобы проверить что НЕ перезаписались)
	cfg.DefaultTensorSplit = []float32{0.5, 0.5}
	cfg.DefaultRopeFreqBase = 10000.0

	// WebUI отправляет null для очищенных полей
	body := `{"defaultTensorSplit": null, "defaultCtxSize": 8192}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	// defaultCtxSize должен примениться
	if cfg.DefaultCtxSize != 8192 {
		t.Errorf("defaultCtxSize should be applied: got %d", cfg.DefaultCtxSize)
	}
	// defaultTensorSplit НЕ должен перезаписаться (null = skip)
	if len(cfg.DefaultTensorSplit) != 2 || cfg.DefaultTensorSplit[0] != 0.5 {
		t.Errorf("defaultTensorSplit should be unchanged (null=skip), got %v", cfg.DefaultTensorSplit)
	}
	// defaultRopeFreqBase НЕ должен перезаписаться (не в payload = skip)
	if cfg.DefaultRopeFreqBase != 10000.0 {
		t.Errorf("defaultRopeFreqBase should be unchanged, got %v", cfg.DefaultRopeFreqBase)
	}

	// Дополнительный кейс: явный null для bool/string/float
	body2 := `{"defaultNuma": null, "defaultKvCacheType": null, "defaultRmsNormEps": null}`
	req2 := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body2))
	w2 := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("null bool/string/float: expected 200, got %d (body: %s)", w2.Code, w2.Body.String())
	}
	// Проверяем что в ответе нет validation_errors (иначе fix не сработал)
	var resp2 map[string]interface{}
	_ = json.NewDecoder(w2.Body).Decode(&resp2)
	if ve, ok := resp2["validation_errors"].([]interface{}); ok && len(ve) > 0 {
		t.Errorf("null values should not produce validation_errors, got: %v", ve)
	}
}

// TestHandleCppWorkerUpdateConfig_ZeroIsSkipForFloat — регрессионный тест.
// Session 17 P.6 (2026-07-27): 0 для опциональных float полей (defaultRopeFreqBase,
// defaultRmsNormEps и т.д.) трактуется как "use llama.cpp default" (skip), а не
// как "must be >= 1, got 0" (validation error). До фикса WebUI форма с пустым
// полем шлёт 0 → пользователь не мог сохранить.
func TestHandleCppWorkerUpdateConfig_ZeroIsSkipForFloat(t *testing.T) {
	cfg := setupConfigForTest(t)
	prevRope := cfg.DefaultRopeFreqBase
	prevEps := cfg.DefaultRMSNormEps
	defer func() {
		cfg.DefaultRopeFreqBase = prevRope
		cfg.DefaultRMSNormEps = prevEps
	}()

	// Initial non-zero values
	cfg.DefaultRopeFreqBase = 10000.0
	cfg.DefaultRMSNormEps = 0.00001

	// 0 для обоих полей → должны skip'аться
	body := `{"defaultRopeFreqBase": 0, "defaultRmsNormEps": 0, "defaultCtxSize": 8192}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	// defaultCtxSize применился
	if cfg.DefaultCtxSize != 8192 {
		t.Errorf("defaultCtxSize should be applied: got %d", cfg.DefaultCtxSize)
	}
	// defaultRopeFreqBase НЕ изменился (0 = skip)
	if cfg.DefaultRopeFreqBase != 10000.0 {
		t.Errorf("defaultRopeFreqBase should be unchanged (0=skip), got %v", cfg.DefaultRopeFreqBase)
	}
	// defaultRmsNormEps НЕ изменился (0 = skip)
	if cfg.DefaultRMSNormEps != 0.00001 {
		t.Errorf("defaultRmsNormEps should be unchanged (0=skip), got %v", cfg.DefaultRMSNormEps)
	}

	// Проверяем что в ответе НЕТ validation_errors
	var resp map[string]interface{}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if ve, ok := resp["validation_errors"].([]interface{}); ok && len(ve) > 0 {
		t.Errorf("0 should not produce validation_errors, got: %v", ve)
	}

	// Дополнительно: явное ненулевое значение должно примениться
	body2 := `{"defaultRopeFreqBase": 500000.0}`
	req2 := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body2))
	w2 := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("explicit value: expected 200, got %d (body: %s)", w2.Code, w2.Body.String())
	}
	if cfg.DefaultRopeFreqBase != 500000.0 {
		t.Errorf("explicit value should be applied: got %v", cfg.DefaultRopeFreqBase)
	}
}

// TestHandleCppWorkerUpdateConfig_RejectsGpuLayers_BelowMinus2 — нижняя граница.
// После фикса min=-2; -3 и ниже должны отвергаться.
func TestHandleCppWorkerUpdateConfig_RejectsGpuLayers_BelowMinus2(t *testing.T) {
	cfg := setupConfigForTest(t)
	prev := cfg.DefaultGPULayers
	defer func() { cfg.DefaultGPULayers = prev }()

	cfg.DefaultGPULayers = 20

	for _, bad := range []int{-3, -10, -100} {
		body := `{"defaultGpuLayers": ` + strconv.Itoa(bad) + `}`
		req := httptest.NewRequest(http.MethodPut,
			"/api/v1/cppworker/config/update",
			strings.NewReader(body))
		w := httptest.NewRecorder()
		handleCppWorkerUpdateConfig(w, req)

		if w.Code == http.StatusOK {
			// Если 200, проверим что значение НЕ применилось и есть validation error.
			var resp map[string]interface{}
			_ = json.NewDecoder(w.Body).Decode(&resp)
			if ve, ok := resp["validation_errors"].([]interface{}); ok {
				if len(ve) == 0 {
					t.Errorf("gpuLayers=%d: got 200 but no validation error reported", bad)
				}
			} else {
				t.Errorf("gpuLayers=%d: expected 400 or 200+validation_errors, got 200", bad)
			}
			if cfg.DefaultGPULayers == bad {
				t.Errorf("gpuLayers=%d: validation should have rejected, but value was applied", bad)
			}
		}
	}
}
// defaultCtxSize корректно применяется и триггерит reload.
func TestHandleCppWorkerUpdateConfig_AppliesCtxSize(t *testing.T) {
	cfg := setupConfigForTest(t)
	prevCtx := cfg.DefaultCtxSize
	defer func() { cfg.DefaultCtxSize = prevCtx }()

	cfg.DefaultCtxSize = 8192

	body := `{"defaultCtxSize": 32768}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if cfg.DefaultCtxSize != 32768 {
		t.Fatalf("expected DefaultCtxSize=32768 after update, got %d", cfg.DefaultCtxSize)
	}
}

// TestHandleCppWorkerUpdateConfig_AppliesKVCacheType — проверяет, что
// defaultKvCacheType принимается и применяется к currentConfig.
// Раньше это поле было в Config, но не принималось в PUT — регрессионный тест.
func TestHandleCppWorkerUpdateConfig_AppliesKVCacheType(t *testing.T) {
	cfg := setupConfigForTest(t)
	prev := cfg.DefaultKVCacheType
	defer func() { cfg.DefaultKVCacheType = prev }()

	cfg.DefaultKVCacheType = "f16"

	for _, kvType := range []string{"f16", "f32", "q8_0", "q4_0", ""} {
		body := `{"defaultKvCacheType": "` + kvType + `"}`
		req := httptest.NewRequest(http.MethodPut,
			"/api/v1/cppworker/config/update",
			strings.NewReader(body))
		w := httptest.NewRecorder()
		handleCppWorkerUpdateConfig(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("kvCacheType=%q: expected 200, got %d (%s)",
				kvType, w.Code, w.Body.String())
		}
		if cfg.DefaultKVCacheType != kvType {
			t.Errorf("kvCacheType=%q: expected applied, got %q",
				kvType, cfg.DefaultKVCacheType)
		}
	}
}

// TestHandleCppWorkerUpdateConfig_RejectsInvalidKVCacheType — невалидный
// тип KV-cache → не применяется, в validation_errors есть запись.
func TestHandleCppWorkerUpdateConfig_RejectsInvalidKVCacheType(t *testing.T) {
	cfg := setupConfigForTest(t)
	prev := cfg.DefaultKVCacheType
	defer func() { cfg.DefaultKVCacheType = prev }()

	// Явно ставим известное начальное значение (setupConfigForTest не сбрасывает
	// DefaultKVCacheType — он приходит из DefaultConfig() = "f16", но если
	// предыдущий тест уже изменил — здесь мы форсируем известное).
	cfg.DefaultKVCacheType = "f16"

	body := `{"defaultKvCacheType": "q2_k"}` // не входит в [f16, f32, q8_0, q4_0, ""]
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (validation errors in body), got %d", w.Code)
	}
	// Конфиг не должен быть изменён.
	if cfg.DefaultKVCacheType != "f16" {
		t.Errorf("expected DefaultKVCacheType to remain f16, got %q", cfg.DefaultKVCacheType)
	}
	// В ответе должны быть validation_errors.
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	errs, ok := resp["validation_errors"].([]interface{})
	if !ok || len(errs) == 0 {
		t.Fatalf("expected validation_errors, got %v", resp)
	}
	found := false
	for _, e := range errs {
		if s, ok := e.(string); ok && strings.Contains(s, "defaultKvCacheType") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected defaultKvCacheType in validation_errors, got %v", errs)
	}
}

// TestHandleCppWorkerUpdateConfig_AppliesRoPE — проверяет rope_freq_base /
// rope_freq_scale / rope_scaling_type / rope_scaling_factor.
func TestHandleCppWorkerUpdateConfig_AppliesRoPE(t *testing.T) {
	cfg := setupConfigForTest(t)
	prevBase, prevScale, prevType, prevFactor :=
		cfg.DefaultRopeFreqBase, cfg.DefaultRopeFreqScale,
		cfg.DefaultRopeScalingType, cfg.DefaultRopeScalingFactor
	defer func() {
		cfg.DefaultRopeFreqBase = prevBase
		cfg.DefaultRopeFreqScale = prevScale
		cfg.DefaultRopeScalingType = prevType
		cfg.DefaultRopeScalingFactor = prevFactor
	}()

	body := `{
		"defaultRopeFreqBase": 500000.0,
		"defaultRopeFreqScale": 0.5,
		"defaultRopeScalingType": "yarn",
		"defaultRopeScalingFactor": 4.0
	}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if cfg.DefaultRopeFreqBase != 500000.0 {
		t.Errorf("expected base=500000, got %v", cfg.DefaultRopeFreqBase)
	}
	if cfg.DefaultRopeFreqScale != 0.5 {
		t.Errorf("expected scale=0.5, got %v", cfg.DefaultRopeFreqScale)
	}
	if cfg.DefaultRopeScalingType != "yarn" {
		t.Errorf("expected type=yarn, got %q", cfg.DefaultRopeScalingType)
	}
	if cfg.DefaultRopeScalingFactor != 4.0 {
		t.Errorf("expected factor=4.0, got %v", cfg.DefaultRopeScalingFactor)
	}
}

// TestHandleCppWorkerUpdateConfig_RejectsBadRopeType — невалидный scaling type
// (не из [none, linear, yarn]) → не применяется.
func TestHandleCppWorkerUpdateConfig_RejectsBadRopeType(t *testing.T) {
	cfg := setupConfigForTest(t)
	prev := cfg.DefaultRopeScalingType
	defer func() { cfg.DefaultRopeScalingType = prev }()

	cfg.DefaultRopeScalingType = "none"
	body := `{"defaultRopeScalingType": "lolwut"}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if cfg.DefaultRopeScalingType != "none" {
		t.Errorf("expected type to remain none, got %q", cfg.DefaultRopeScalingType)
	}
}

// TestHandleCppWorkerUpdateConfig_AppliesYaRN — yarn_ext_factor / yarn_attn_factor
// / yarn_beta_fast / yarn_beta_slow.
func TestHandleCppWorkerUpdateConfig_AppliesYaRN(t *testing.T) {
	cfg := setupConfigForTest(t)
	prevExt, prevAttn, prevFast, prevSlow :=
		cfg.DefaultYarnExtFactor, cfg.DefaultYarnAttnFactor,
		cfg.DefaultYarnBetaFast, cfg.DefaultYarnBetaSlow
	defer func() {
		cfg.DefaultYarnExtFactor = prevExt
		cfg.DefaultYarnAttnFactor = prevAttn
		cfg.DefaultYarnBetaFast = prevFast
		cfg.DefaultYarnBetaSlow = prevSlow
	}()

	body := `{
		"defaultYarnExtFactor": 2.0,
		"defaultYarnAttnFactor": 1.5,
		"defaultYarnBetaFast": 64.0,
		"defaultYarnBetaSlow": 2.0
	}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if cfg.DefaultYarnExtFactor != 2.0 {
		t.Errorf("ext_factor: got %v, want 2.0", cfg.DefaultYarnExtFactor)
	}
	if cfg.DefaultYarnAttnFactor != 1.5 {
		t.Errorf("attn_factor: got %v, want 1.5", cfg.DefaultYarnAttnFactor)
	}
	if cfg.DefaultYarnBetaFast != 64.0 {
		t.Errorf("beta_fast: got %v, want 64.0", cfg.DefaultYarnBetaFast)
	}
	if cfg.DefaultYarnBetaSlow != 2.0 {
		t.Errorf("beta_slow: got %v, want 2.0", cfg.DefaultYarnBetaSlow)
	}
}

// TestHandleCppWorkerUpdateConfig_AppliesTensorSplit — массив []float для
// tensor split. Multi-GPU use-case.
func TestHandleCppWorkerUpdateConfig_AppliesTensorSplit(t *testing.T) {
	cfg := setupConfigForTest(t)
	prev := cfg.DefaultTensorSplit
	defer func() { cfg.DefaultTensorSplit = prev }()

	body := `{
		"defaultTensorSplit": [0.5, 0.5],
		"defaultSplitMode": 0,
		"defaultMainGpu": 0,
		"autoGpuDistribution": false
	}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if len(cfg.DefaultTensorSplit) != 2 {
		t.Fatalf("expected 2-element split, got %d", len(cfg.DefaultTensorSplit))
	}
	if cfg.DefaultTensorSplit[0] != 0.5 || cfg.DefaultTensorSplit[1] != 0.5 {
		t.Errorf("split mismatch: %v", cfg.DefaultTensorSplit)
	}
	if cfg.DefaultSplitMode != 0 {
		t.Errorf("split_mode: got %d, want 0", cfg.DefaultSplitMode)
	}
	if cfg.DefaultMainGPU != 0 {
		t.Errorf("main_gpu: got %d, want 0", cfg.DefaultMainGPU)
	}
	if cfg.AutoGPUDistribution {
		t.Errorf("autoGpuDistribution: got true, want false")
	}
}

// TestHandleCppWorkerUpdateConfig_PartialUpdate — partial update: только
// одно поле в body, остальные не должны затираться.
func TestHandleCppWorkerUpdateConfig_PartialUpdate(t *testing.T) {
	cfg := setupConfigForTest(t)
	prevBatch := cfg.DefaultBatchSize
	prevKV := cfg.DefaultKVCacheType
	defer func() {
		cfg.DefaultBatchSize = prevBatch
		cfg.DefaultKVCacheType = prevKV
	}()

	cfg.DefaultBatchSize = 256
	cfg.DefaultKVCacheType = "f32"

	// Только defaultBatchSize — KV cache должен остаться f32.
	body := `{"defaultBatchSize": 1024}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if cfg.DefaultBatchSize != 1024 {
		t.Errorf("batch: got %d, want 1024", cfg.DefaultBatchSize)
	}
	if cfg.DefaultKVCacheType != "f32" {
		t.Errorf("kv_cache: got %q, want f32 (unchanged)", cfg.DefaultKVCacheType)
	}
}

// TestHandleCppWorkerUpdateConfig_AppliesIdleUnload — int >= 0, 0 = off.
func TestHandleCppWorkerUpdateConfig_AppliesIdleUnload(t *testing.T) {
	cfg := setupConfigForTest(t)
	prev := cfg.IdleUnloadMinutes
	defer func() { cfg.IdleUnloadMinutes = prev }()

	for _, val := range []int{0, 5, 30, 1440} {
		body := `{"idleUnloadMinutes": ` + strconv.Itoa(val) + `}`
		req := httptest.NewRequest(http.MethodPut,
			"/api/v1/cppworker/config/update",
			strings.NewReader(body))
		w := httptest.NewRecorder()
		handleCppWorkerUpdateConfig(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("idleUnload=%d: expected 200, got %d (%s)",
				val, w.Code, w.Body.String())
		}
		if cfg.IdleUnloadMinutes != val {
			t.Errorf("idleUnload=%d: got %d", val, cfg.IdleUnloadMinutes)
		}
	}
}

// TestHandleCppWorkerUpdateConfig_RejectsNegativeCtxSize — нижняя граница
// defaultCtxSize >= 256.
func TestHandleCppWorkerUpdateConfig_RejectsNegativeCtxSize(t *testing.T) {
	cfg := setupConfigForTest(t)
	prev := cfg.DefaultCtxSize
	defer func() { cfg.DefaultCtxSize = prev }()

	cfg.DefaultCtxSize = 4096
	body := `{"defaultCtxSize": 100}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if cfg.DefaultCtxSize != 4096 {
		t.Errorf("ctx should remain 4096, got %d", cfg.DefaultCtxSize)
	}
}

// TestHandleCppWorkerUpdateConfig_LogsUnknownFields — неизвестные поля
// принимаются без ошибки (для forward-compat), но логируются как warning.
// Здесь мы только проверяем, что не валим запрос.
func TestHandleCppWorkerUpdateConfig_UnknownFieldIsOK(t *testing.T) {
	setupConfigForTest(t)

	body := `{"unknownField": 42, "defaultCtxSize": 4096}`
	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/cppworker/config/update",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestHasReloadedDefaults_Extended — все новые load-affecting поля триггерят reload.
func TestHasReloadedDefaults_Extended(t *testing.T) {
	loadFields := []string{
		// Базовые
		"defaultCtxSize", "defaultBatchSize", "defaultGpuLayers",
		"defaultFlashAttnType", "defaultNuma", "defaultUseMmap",
		"defaultUseMlock", "defaultNThreads", "defaultRmsNormEps",
		// Multi-GPU
		"autoGpuDistribution", "tensorSplitStrategy", "defaultMainGpu",
		"defaultNoMemoryMap", "defaultTensorSplit", "defaultSplitMode",
		// KV cache
		"defaultKvCacheType", "defaultNoKvOffload",
		// RoPE/YaRN
		"defaultRopeFreqBase", "defaultRopeFreqScale",
		"defaultRopeScalingType", "defaultRopeScalingFactor",
		"defaultYarnExtFactor", "defaultYarnAttnFactor",
		"defaultYarnBetaFast", "defaultYarnBetaSlow",
	}
	for _, f := range loadFields {
		if !hasReloadedDefaults([]string{f}) {
			t.Errorf("hasReloadedDefaults([%q]) = false, want true (load-affecting)", f)
		}
	}

	// Метрики / lifecycle НЕ должны триггерить reload.
	nonLoadFields := []string{
		"enableMetrics", "metricsRetentionSeconds", "idleUnloadMinutes",
	}
	for _, f := range nonLoadFields {
		if hasReloadedDefaults([]string{f}) {
			t.Errorf("hasReloadedDefaults([%q]) = true, want false (non-load)", f)
		}
	}
}

// TestEstimateKVCacheBytes — формула для f16.
func TestEstimateKVCacheBytes(t *testing.T) {
	// Типичные 7B параметры: n_layers=32, n_embd=4096, n_heads=32, n_kv_heads=32
	// head_dim = 4096/32 = 128
	// bytes = 4 * n_ctx * 32 * 32 * 128 = 524288 * n_ctx
	// Для n_ctx=8192: 524288 * 8192 = 4_294_967_296 байт = 4 GB
	bytes := estimateKVCacheBytes(8192, 32, 4096, 32, 32, "f16")
	expectedBytes := int64(4) * 8192 * 32 * 32 * 128
	if bytes != expectedBytes {
		t.Errorf("estimateKVCacheBytes(8192, 32, 4096, 32, 32, f16) = %d, want %d",
			bytes, expectedBytes)
	}

	// GQA (n_kv_heads < n_heads): 7B с GQA-8 (n_kv_heads=8)
	// head_dim = 4096/32 = 128, effKVHeads = 8
	// bytes = 4 * n_ctx * 32 * 8 * 128 = 131072 * n_ctx
	bytesGQA := estimateKVCacheBytes(4096, 32, 4096, 32, 8, "f16")
	expectedGQA := int64(4) * 4096 * 32 * 8 * 128
	if bytesGQA != expectedGQA {
		t.Errorf("GQA estimate = %d, want %d", bytesGQA, expectedGQA)
	}

	// n_kv_heads=0 → fallback на n_heads (MHA)
	bytesNoKV := estimateKVCacheBytes(1024, 32, 4096, 32, 0, "f16")
	expectedNoKV := int64(4) * 1024 * 32 * 32 * 128
	if bytesNoKV != expectedNoKV {
		t.Errorf("n_kv_heads=0 fallback = %d, want %d", bytesNoKV, expectedNoKV)
	}

	// Нулевые nEmbd/nHeads → fallback на 4MB per 1K tokens.
	// nLayers=32, чтобы пройти первую проверку (nCtx>0 && nLayers>0).
	bytesZero := estimateKVCacheBytes(1024, 32, 0, 0, 0, "f16")
	if bytesZero != 1024*4096 {
		t.Errorf("zero n_embd/n_heads fallback = %d, want %d", bytesZero, 1024*4096)
	}

	// nLayers=0 → возврат 0 (нет слоёв для KV-cache)
	bytesNoLayers := estimateKVCacheBytes(1024, 0, 4096, 32, 32, "f16")
	if bytesNoLayers != 0 {
		t.Errorf("n_layers=0 should return 0, got %d", bytesNoLayers)
	}

	// Отрицательные значения → 0
	bytesNeg := estimateKVCacheBytes(-1, 32, 4096, 32, 32, "f16")
	if bytesNeg != 0 {
		t.Errorf("negative n_ctx should return 0, got %d", bytesNeg)
	}
}

// TestContextWithTimeout — создаёт контекст с таймаутом и проверяет что
// ctx.Done() срабатывает по истечении timeout. Используем 100ms, чтобы
// тест был стабильным на любых машинах.
func TestContextWithTimeout(t *testing.T) {
	timeout := 100 * time.Millisecond
	ctx, cancel := contextWithTimeout(timeout)
	defer cancel()

	if ctx == nil {
		t.Fatal("context is nil")
	}
	if cancel == nil {
		t.Fatal("cancel is nil")
	}

	// Сразу после создания контекст не должен быть отменён
	select {
	case <-ctx.Done():
		t.Fatal("context should not be Done immediately after creation")
	default:
	}

	// Ждём истечения timeout
	time.Sleep(timeout + 50*time.Millisecond)

	// После timeout контекст должен быть отменён
	select {
	case <-ctx.Done():
		// ОК — контекст отменён по таймауту
	default:
		t.Errorf("context should be Done after %v", timeout)
	}

	// Err должен быть context.DeadlineExceeded
	if err := ctx.Err(); err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

// TestContextWithTimeout_ManualCancel — cancel() сразу отменяет контекст,
// даже если timeout не истёк.
func TestContextWithTimeout_ManualCancel(t *testing.T) {
	timeout := 10 * time.Second
	ctx, cancel := contextWithTimeout(timeout)

	// Отменяем сразу
	cancel()

	// Контекст должен быть отменён немедленно
	select {
	case <-ctx.Done():
		// ОК
	default:
		t.Error("context should be Done immediately after manual cancel()")
	}

	// Err должен быть context.Canceled
	if err := ctx.Err(); err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// invalidJSONReader возвращает io.Reader с невалидным JSON для тестов 400.
func invalidJSONReader() *brokenReader {
	return &brokenReader{}
}

type brokenReader struct{}

func (b *brokenReader) Read(p []byte) (int, error) {
	return 0, errBrokenJSON
}

var errBrokenJSON = &jsonError{"intentionally broken"}

type jsonError struct{ msg string }

func (e *jsonError) Error() string { return e.msg }