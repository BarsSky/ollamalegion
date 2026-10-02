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
	"context"
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
	if err := backend.LoadModelWithOpts(context.Background(), "test_model", fakePath, loadOpts); err != nil {
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

// TestGenerate_ConcurrentSameModel_Serialized — проверяет ФАКТ: одновременные
// вызовы Generate к одной модели никогда не выполняют inst.handle.Infer()
// параллельно. Это критично, потому что контекст llama.cpp не thread-safe.
//
// ЧТО ИСПРАВЛЕНО (R-Image follow-up, 2026-10-02). Тест был недетерминированным
// сразу по трём причинам, и ни одна из них не была «таймингом самого факта»:
//
//  1. Факт выводился КОСВЕННО: горутина после возврата из Generate читала
//     inst.info.ActiveQueries. Это поле пишется под тем же inst.mu, который тест
//     и проверяет, поэтому наблюдаемое значение всегда ≤1 — проверить
//     сериализацию такой снимок не может (ни поймать регрессию, ни надёжно её
//     исключить). Теперь факт наблюдается ПРЯМО в стабе:
//     bridge.StubInferMaxOverlap() — счётчик одновременных вызовов Infer,
//     инкремент на входе и декремент на выходе. maxOverlap >= 2 ⟺ два Infer
//     пересекались во времени; никаких окон и опросов.
//  2. Горутины запускались «одновременно» через time.Sleep(20ms) перед
//     снятием барьера — то есть тест надеялся, что планировщик успел запустить
//     все n горутин (под нагрузкой 7 пакетов это не так), а не синхронизировался
//     с ними. Теперь барьер честный: все горутины сигналят готовность и ждут
//     закрытия канала start — ни одного time.Sleep в координации.
//  3. Модель грузилась с одним слотом (LoadModelOpts без Parallel → SlotManager
//     maxSlots=1), и запросы сериализовал САМ SlotManager — то есть inst.mu,
//     ради которого тест и написан, вообще не участвовал: со снятым inst.mu
//     тест всё равно проходил. Теперь модель грузится с Parallel=8 (> n), и
//     сериализовать может только inst.mu; это проверяется явно по MaxSlots().
//
// Ширина окна (25 мс задержки стаба) нужна не для «поймать перекрытие», а чтобы
// РЕГРЕССИЯ (снятый inst.mu) ловилась надёжно, а не по удаче планировщика.
func TestGenerate_ConcurrentSameModel_Serialized(t *testing.T) {
	prevDelay := bridge.SetStubInferDelay(25 * time.Millisecond)
	defer bridge.SetStubInferDelay(prevDelay)
	bridge.ResetStubInferOverlap()
	defer bridge.ResetStubInferOverlap()

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
	if err := backend.LoadModelWithOpts(context.Background(), "test_model", fakePath, LoadModelOpts{
		ContextSize: 512, BatchSize: 64, GPULayers: 0,
		// > числа одновременных вызовов: иначе запросы сериализует SlotManager,
		// и проверка inst.mu становится пустой (см. п.3 в комментарии выше).
		Parallel: 8,
	}); err != nil {
		t.Fatalf("LoadModelWithOpts: %v", err)
	}

	backend.mu.RLock()
	inst := backend.models["test_model"]
	backend.mu.RUnlock()
	if inst == nil || inst.slots == nil {
		t.Fatal("модель/слоты не инициализированы — стенд теста сломан")
	}

	const n = 5
	if got := inst.slots.MaxSlots(); got < n {
		t.Fatalf("SlotManager.MaxSlots() = %d, want >= %d: с меньшим числом слотов "+
			"запросы сериализует сам SlotManager, и тест перестаёт проверять inst.mu", got, n)
	}

	// Честный барьер: все горутины доходят до start и ждут его закрытия — никаких
	// «подождём 20 мс, авось планировщик успел».
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(n)
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		go func() {
			ready.Done()
			<-start
			_, err := backend.Generate("test_model", "hi", bridge.GenerationParams{NPredict: 5})
			errs <- err
		}()
	}
	ready.Wait()
	close(start)

	// Ждём завершения ВСЕХ Generate (каналом, а не таймером), затем проверяем
	// факты. Каждый вызов обязан дойти до Infer и вернуться без ошибки.
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("Generate[%d] error: %v", i, err)
		}
	}

	if calls := bridge.StubSyncInferCalls(); calls != n {
		t.Errorf("Infer вызван %d раз, а Generate было %d — часть запросов не дошла "+
			"до инференса (тест проверял бы не то)", calls, n)
	}
	if got := bridge.StubInferMaxOverlap(); got != 1 {
		t.Errorf("одновременно внутри Infer было %d вызовов — Generate НЕ сериализованы "+
			"(Round 8 bug regression: параллельный llama_decode / гонка на KV-cache)", got)
	}
	// Счётчики запросов не «протекли»: после возврата всех Generate активных нет.
	if got := inst.activeQueriesCount(); got != 0 {
		t.Errorf("activeQueries = %d после завершения всех Generate, want 0", got)
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
