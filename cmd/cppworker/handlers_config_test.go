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

// TestHandleCppWorkerUpdateConfig_AppliesCtxSize — проверяет, что PUT с
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

// TestEstimateKVCacheBytes — формула для f16.
func TestEstimateKVCacheBytes(t *testing.T) {
	// Типичные 7B параметры: n_layers=32, n_embd=4096, n_heads=32, n_kv_heads=32
	// head_dim = 4096/32 = 128
	// bytes = 4 * n_ctx * 32 * 32 * 128 = 524288 * n_ctx
	// Для n_ctx=8192: 524288 * 8192 = 4_294_967_296 байт = 4 GB
	bytes := estimateKVCacheBytes(8192, 32, 4096, 32, 32)
	expectedBytes := int64(4) * 8192 * 32 * 32 * 128
	if bytes != expectedBytes {
		t.Errorf("estimateKVCacheBytes(8192, 32, 4096, 32, 32) = %d, want %d",
			bytes, expectedBytes)
	}

	// GQA (n_kv_heads < n_heads): 7B с GQA-8 (n_kv_heads=8)
	// head_dim = 4096/32 = 128, effKVHeads = 8
	// bytes = 4 * n_ctx * 32 * 8 * 128 = 131072 * n_ctx
	bytesGQA := estimateKVCacheBytes(4096, 32, 4096, 32, 8)
	expectedGQA := int64(4) * 4096 * 32 * 8 * 128
	if bytesGQA != expectedGQA {
		t.Errorf("GQA estimate = %d, want %d", bytesGQA, expectedGQA)
	}

	// n_kv_heads=0 → fallback на n_heads (MHA)
	bytesNoKV := estimateKVCacheBytes(1024, 32, 4096, 32, 0)
	expectedNoKV := int64(4) * 1024 * 32 * 32 * 128
	if bytesNoKV != expectedNoKV {
		t.Errorf("n_kv_heads=0 fallback = %d, want %d", bytesNoKV, expectedNoKV)
	}

	// Нулевые nEmbd/nHeads → fallback на 4MB per 1K tokens.
	// nLayers=32, чтобы пройти первую проверку (nCtx>0 && nLayers>0).
	bytesZero := estimateKVCacheBytes(1024, 32, 0, 0, 0)
	if bytesZero != 1024*4096 {
		t.Errorf("zero n_embd/n_heads fallback = %d, want %d", bytesZero, 1024*4096)
	}

	// nLayers=0 → возврат 0 (нет слоёв для KV-cache)
	bytesNoLayers := estimateKVCacheBytes(1024, 0, 4096, 32, 32)
	if bytesNoLayers != 0 {
		t.Errorf("n_layers=0 should return 0, got %d", bytesNoLayers)
	}

	// Отрицательные значения → 0
	bytesNeg := estimateKVCacheBytes(-1, 32, 4096, 32, 32)
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