// R66d (2026-09-22): метрики токенов в cppbackend.
//
// ДО ФИКСА: RecordRequest получал tokens=0 на КАЖДЫЙ запрос, потому что
// значение считалось как approximateTokens(approxResultLen(...)), а
// approxResultLen() была заглушкой, всегда возвращавшей "" (её написали, когда
// результата инференса под рукой ещё не было). В стриминговых путях
// (GenerateStream, batchedInferStream) в метрику писали жёсткий 0.
// Итог: /api/v1/metrics в WebUI показывал нулевой расход токенов, а
// success всегда был true (даже на отказах инференса).
//
// Эти тесты требуют build tag llama_stub (используют stub-мост и его хуки).
//go:build llama_stub

package cppbackend

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// newStubBackendForMetrics — backend со загруженной stub-моделью "test_model"
// (тот же паттерн, что в concurrent_generate_test.go).
//
// R66d: обязателен t.Cleanup с WaitForPendingWrites — RecordModelLoad запускает
// фоновую запись .name_history.json в ModelsDir, и без ожидания t.TempDir()
// cleanup на Windows падает с "directory is not empty" (тот же флейк, что
// ловили в R65d, см. setupTestBackendForR60_57).
func newStubBackendForMetrics(t *testing.T) *Backend {
	t.Helper()

	cfg := Config{
		ModelsDir:        t.TempDir(),
		DefaultCtxSize:   512,
		DefaultBatchSize: 64,
		DefaultGPULayers: 0,
	}
	backend := NewBackend(cfg)
	if backend == nil {
		t.Fatal("NewBackend returned nil")
	}
	t.Cleanup(func() {
		if mm := backend.ModelManager(); mm != nil {
			if !mm.WaitForPendingWrites(2 * time.Second) {
				t.Logf("warning: nameHistory persist did not finish within 2s")
			}
		}
	})

	fakePath := filepath.Join(cfg.ModelsDir, "test_model.gguf")
	if err := writeMinimalFile(fakePath, 1024); err != nil {
		t.Fatalf("writeMinimalFile: %v", err)
	}
	if err := backend.LoadModelWithOpts(context.Background(), "test_model", fakePath, LoadModelOpts{
		ContextSize: 512,
		BatchSize:   64,
		GPULayers:   0,
		UseMmap:     false,
	}); err != nil {
		t.Fatalf("LoadModelWithOpts: %v", err)
	}
	return backend
}

// TestCompletionTokens_R66d — юнит-проверка счётчика токенов ответа.
func TestCompletionTokens_R66d(t *testing.T) {
	t.Parallel()

	if got := completionTokens(nil, "", true); got != 0 {
		t.Errorf("пустой текст: got %d, want 0", got)
	}
	// handle == nil → оценка pkg/tokencount (точный tokenizer недоступен).
	if got := completionTokens(nil, "hello world, this is a test", true); got <= 0 {
		t.Errorf("оценка без handle: got %d, want > 0", got)
	}
	// exact=false → оценка даже при наличии handle.
	if got := completionTokens(nil, "hello", false); got <= 0 {
		t.Errorf("exact=false: got %d, want > 0", got)
	}
}

// TestIsBackendFailure_R66d — cancel не считается отказом бэкенда.
func TestIsBackendFailure_R66d(t *testing.T) {
	t.Parallel()

	if isBackendFailure(nil) {
		t.Error("nil: want false (успех)")
	}
	if !isBackendFailure(errors.New("boom")) {
		t.Error("обычная ошибка: want true (отказ бэкенда)")
	}
	wrapped := fmt.Errorf("stream inference cancelled: %w", bridge.ErrAborted)
	if isBackendFailure(wrapped) {
		t.Error("bridge.ErrAborted (cancel): want false — отмена не отказ")
	}
}

// TestGenerate_RecordsCompletionTokens_R66d — раньше здесь всегда был 0.
func TestGenerate_RecordsCompletionTokens_R66d(t *testing.T) {
	backend := newStubBackendForMetrics(t)

	res, err := backend.Generate("test_model", "посчитай токены в этом ответе", bridge.GenerationParams{
		NPredict: 16,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res == nil || res.Output == "" {
		t.Fatal("Generate вернул пустой результат — тест не сможет проверить токены")
	}

	snap, ok := backend.metrics.GetModelMetricsSnapshot()["test_model"]
	if !ok {
		t.Fatal("в метриках нет модели test_model")
	}
	if snap.Tokens <= 0 {
		t.Errorf("metrics tokens = %d, want > 0 (R66d: раньше всегда 0)", snap.Tokens)
	}
	if total := backend.metrics.TotalTokens.Load(); total <= 0 {
		t.Errorf("TotalTokens = %d, want > 0", total)
	}
	if snap.Errors != 0 {
		t.Errorf("errors = %d, want 0 (успешный инференс)", snap.Errors)
	}
}

// TestGenerateStream_RecordsEmittedTokenCount_R66d — в стриме считаем реально
// отданные токены (stub эмитит ровно len(tokens) кусков).
func TestGenerateStream_RecordsEmittedTokenCount_R66d(t *testing.T) {
	emitted := []string{"раз", " два", " три"}
	prev := bridge.SetStubEmitTokens(emitted)
	defer bridge.SetStubEmitTokens(prev)

	backend := newStubBackendForMetrics(t)

	got := 0
	_, err := backend.GenerateStream("test_model", "prompt", bridge.GenerationParams{NPredict: 8}, func(piece string) bool {
		got++
		return true
	})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if got != len(emitted) {
		t.Fatalf("callback вызван %d раз, want %d", got, len(emitted))
	}

	snap, ok := backend.metrics.GetModelMetricsSnapshot()["test_model"]
	if !ok {
		t.Fatal("в метриках нет модели test_model")
	}
	if snap.Tokens != int64(got) {
		t.Errorf("metrics tokens = %d, want %d (R66d: раньше всегда 0)", snap.Tokens, got)
	}
}

// TestGenerateStream_EmptyOutput_ZeroTokens_R66d — если модель не выдала ни
// одного токена, метрика остаётся 0 (не выдумываем значения).
func TestGenerateStream_EmptyOutput_ZeroTokens_R66d(t *testing.T) {
	prevEmpty := bridge.SetStubEmptyOutput(true)
	defer bridge.SetStubEmptyOutput(prevEmpty)

	backend := newStubBackendForMetrics(t)

	if _, err := backend.GenerateStream("test_model", "prompt", bridge.GenerationParams{NPredict: 8}, func(string) bool {
		t.Error("callback не должен вызываться при пустом output")
		return true
	}); err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}

	snap := backend.metrics.GetModelMetricsSnapshot()["test_model"]
	if snap.Tokens != 0 {
		t.Errorf("metrics tokens = %d, want 0", snap.Tokens)
	}
}
