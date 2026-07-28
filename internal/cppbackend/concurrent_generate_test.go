// concurrent_generate_test.go — Round 8 (2026-07-28) regression test.
//
// BUG (до фикса): Backend.Generate / GenerateStream НЕ удерживали
// inst.mu при вызове inst.handle.Infer(). Два параллельных HTTP-запроса
// к одной модели вызывали llama_decode() на одном и том же контексте —
// race на KV-cache приводил к GGML_ASSERT(ggml_are_same_shape) и
// крашу cppworker (SIGABRT).
//
// Симптом у пользователя: qwen3.6 35B A3B на A10, второй OpenWebUI-
// клиент получал 503 "connection closed" / "model is loading" через
// ~0.5 сек после первого, а cppworker падал в логах.
//
// ФИКС: inst.mu удерживается на всём inst.handle.Infer() (см. backend.go).
//
// Эти тесты требуют build tag llama_stub (используют bridge.SetStubInferDelay).
//go:build llama_stub

package cppbackend

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// TestGenerate_ConcurrentSameModel_NotPanic — два параллельных вызова
// Generate на одну и ту же модель не должны паниковать и оба должны
// вернуть результат (а не крашить cppworker).
//
// До фикса: inst.handle.Infer() вызывался без inst.mu → race → SIGABRT.
// После фикса: второй вызов ЖДЁТ завершения первого.
func TestGenerate_ConcurrentSameModel_NotPanic(t *testing.T) {
	// Устанавливаем задержку в stub.Infer — имитирует долгий inference.
	prevDelay := bridge.SetStubInferDelay(100 * time.Millisecond)
	defer bridge.SetStubInferDelay(prevDelay)

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

	// Создаём фейковый GGUF-файл (stub не читает содержимое, но LoadModel
	// может stat'ить для отображения размера).
	fakePath := cfg.ModelsDir + "/test_model.gguf"
	if err := writeMinimalFile(fakePath, 1024); err != nil {
		t.Fatalf("writeMinimalFile: %v", err)
	}

	loadOpts := LoadModelOpts{
		ContextSize: 512,
		BatchSize:   64,
		GPULayers:   0,
		UseMmap:     false,
	}
	if err := backend.LoadModelWithOpts("test_model", fakePath, loadOpts); err != nil {
		t.Fatalf("LoadModelWithOpts: %v", err)
	}

	// Запускаем 2 параллельных Generate. Должны оба вернуть результат.
	const n = 2
	var wg sync.WaitGroup
	var errCount atomic.Int32
	results := make([]*bridge.InferenceResult, n)
	errors := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			params := bridge.GenerationParams{
				NPredict:    10,
				Temperature: 0.5,
			}
			res, err := backend.Generate("test_model", "hello", params)
			results[idx] = res
			errors[idx] = err
			if err != nil {
				errCount.Add(1)
				t.Logf("Generate[%d] error: %v", idx, err)
			}
		}(i)
	}

	wg.Wait()

	if errCount.Load() > 0 {
		t.Errorf("got %d errors from concurrent Generate, want 0", errCount.Load())
	}

	// Проверяем, что оба результата непустые.
	for i, r := range results {
		if r == nil {
			t.Errorf("results[%d] is nil", i)
			continue
		}
		if r.Output == "" {
			t.Errorf("results[%d].Output is empty", i)
		}
	}
}

// TestGenerate_ConcurrentSameModel_Serialized — проверяет, что параллельные
// вызовы Generate на одну модель СЕРИАЛИЗУЮТСЯ (не выполняются одновременно).
// Это критично, потому что llama.cpp context не thread-safe.
//
// Используем счётчик «активных вызовов Infer»: он должен быть <= 1
// в любой момент времени. До фикса счётчик мог быть = 2 одновременно.
func TestGenerate_ConcurrentSameModel_Serialized(t *testing.T) {
	prevDelay := bridge.SetStubInferDelay(50 * time.Millisecond)
	defer bridge.SetStubInferDelay(prevDelay)

	cfg := Config{
		ModelsDir:        t.TempDir(),
		DefaultCtxSize:   512,
		DefaultBatchSize: 64,
		DefaultGPULayers: 0,
	}
	backend := NewBackend(cfg)
	fakePath := cfg.ModelsDir + "/test_model.gguf"
	if err := writeMinimalFile(fakePath, 1024); err != nil {
		t.Fatalf("writeMinimalFile: %v", err)
	}
	if err := backend.LoadModelWithOpts("test_model", fakePath, LoadModelOpts{
		ContextSize: 512, BatchSize: 64, GPULayers: 0,
	}); err != nil {
		t.Fatalf("LoadModelWithOpts: %v", err)
	}

	// Счётчик активных вызовов через stub.InferDelay.
	// Когда stub.Infer спит, можем проверить, что только 1 goroutine
	// находится внутри. Используем сам факт задержки как «окно» —
	// если 2 Infer'а идут одновременно, оба задержки завершатся
	// одновременно, и проверка ниже увидит активные вызовы через
	// modelInstance.info.ActiveQueries.

	const n = 5
	var startBarrier sync.WaitGroup
	startBarrier.Add(1)
	var done sync.WaitGroup
	maxActive := atomic.Int32{}

	done.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer done.Done()
			startBarrier.Wait() // запускаем все горутины одновременно
			_, _ = backend.Generate("test_model", "hi", bridge.GenerationParams{NPredict: 5})

			// После завершения Generate — ActiveQueries должен быть 0
			// (потому что Generate удерживает mu всё время).
			// Snapshot info.ActiveQueries:
			backend.mu.RLock()
			inst := backend.models["test_model"]
			backend.mu.RUnlock()
			if inst == nil {
				return
			}
			inst.mu.Lock()
			curActive := inst.info.ActiveQueries
			inst.mu.Unlock()
			if curActive > 1 {
				maxActive.Store(int32(curActive))
			}
		}()
	}

	// Даём всем горутинам время войти в Generate, потом отпускаем barrier.
	time.Sleep(20 * time.Millisecond)
	startBarrier.Done()
	done.Wait()

	if maxActive.Load() > 1 {
		t.Errorf("ActiveQueries reached %d — Generate calls were NOT serialized (Round 8 bug regression!)",
			maxActive.Load())
	}
}

// writeMinimalFile создаёт файл заданного размера, заполненный нулями.
// Нужен для LoadModelWithOpts (backend stat'ит файл).
func writeMinimalFile(path string, sizeBytes int) error {
	f, err := openFileCreate(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if sizeBytes <= 0 {
		return nil
	}
	// Пишем блоками по 4KB, последний блок усечённый.
	buf := make([]byte, 4096)
	written := 0
	for written < sizeBytes {
		toWrite := sizeBytes - written
		if toWrite > len(buf) {
			toWrite = len(buf)
		}
		if _, err := f.Write(buf[:toWrite]); err != nil {
			return err
		}
		written += toWrite
	}
	return nil
}
