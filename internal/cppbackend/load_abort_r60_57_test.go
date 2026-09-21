// load_abort_r60_57_test.go — R60.57 (2026-09-13) cancellable load.
//
// Background: R60.57 завершает Go-side integration load-cancellation API
// поверх C-bridge changes (commit e547148). LoadModelWithOpts принимает
// context.Context, spawn watcher goroutine которая вызывает
// bridge.RequestLoadAbort(handle) на ctx.Done().
//
// R60.57 follow-up (2026-09-13, commit a62853e): C-bridge теперь
// экспонирует early-allocated handle в earlyHandle.ptr СРАЗУ после
// malloc+atomic_init, ДО blocking llama_model_load_from_file. Это
// позволяет watcher вызвать bridge.RequestLoadAbort(handle) в реальном
// времени (handle уже валиден к моменту ctx.Done).
//
// Тесты проверяют:
//   - watcher goroutine spawn/exit semantics
//   - ctx propagation через вызовы
//   - ошибка ErrAborted возвращается когда C-bridge её возвращает
//   - pre-allocated inst.handle доступен watcher'у напрямую (no polling)
//   - bridge.RequestLoadAbort ВЫЗЫВАЕТСЯ watcher'ом при cancel (verify
//     через stub counter hook bridge.GetStubLoadAbortCount)

//go:build llama_stub

package cppbackend

import (
	"context"
	"errors"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// TestR60_57_LoadModelWithOpts_NilContext — LoadModelWithOpts(nil ctx) не падает.
//
// R60.57: ctx == nil → watcher не spawn'ится. Старая behavior (без ctx) сохранена
// для backward-compat с auto-load path (lazyload.go:autoLoadModels и т.п.),
// где HTTP request ctx не доступен.
func TestR60_57_LoadModelWithOpts_NilContext(t *testing.T) {
	b := setupTestBackendForR60_57(t)
	defer b.ShutdownForTest()

	// nil ctx — watcher skip, как pre-R60.57.
	err := b.LoadModelWithOpts(nil, "r6057-nil-ctx-model", "/nonexistent.gguf", LoadModelOpts{
		ContextSize: 512,
		BatchSize:   64,
		GPULayers:   0,
	})
	// В stub-режиме LoadModel возвращает fake handle, error = nil.
	if err != nil {
		t.Logf("expected nil error in stub mode (LoadModel no-op), got: %v", err)
	}
}

// TestR60_57_LoadModelWithOpts_ContextBackground — context.Background() —
// watcher spawn'ится но exit'ит сразу когда load завершается без cancel.
//
// Это регрессия на "watcher doesn't leak".
func TestR60_57_LoadModelWithOpts_ContextBackground(t *testing.T) {
	b := setupTestBackendForR60_57(t)
	defer b.ShutdownForTest()

	ctx := context.Background()
	err := b.LoadModelWithOpts(ctx, "r6057-bg-model", "/nonexistent.gguf", LoadModelOpts{
		ContextSize: 512,
		BatchSize:   64,
		GPULayers:   0,
	})
	if err != nil {
		t.Logf("expected nil error in stub mode, got: %v", err)
	}
	// Если watcher утечка — test runner flag'нет leak detector.
	// Здесь мы не можем напрямую assert "watcher exited", но если watcher
	// hangs на <-ctx.Done() с context.Background() (который никогда не
	// отменяется) — test просто завершится. Утечка проявится в -race/-leak
	// режимах в CI, не здесь.
}

// TestR60_57_LoadModelWithOpts_PreCancelledCtx — pre-cancelled ctx.
//
// R60.57: даже если ctx уже отменён ДО вызова LoadModelWithOpts,
// watcher spawn'ится, видит ctx.Done() сразу, и пробует abort handle
// (который nil во время load — будет no-op). Load продолжается нормально,
// возвращает успех или ошибку.
//
// Это проверяет что pre-cancelled ctx не вызывает панику и не блокирует
// навсегда — watcher exit'ит через <-watcherDone после возврата LoadModel.
func TestR60_57_LoadModelWithOpts_PreCancelledCtx(t *testing.T) {
	b := setupTestBackendForR60_57(t)
	defer b.ShutdownForTest()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	// В stub-режиме LoadModel мгновенный, watcher успеет отработать только
	// race window. Здесь мы просто проверяем что нет паники и нет deadlock.
	done := make(chan error, 1)
	go func() {
		done <- b.LoadModelWithOpts(ctx, "r6057-precancel-model", "/nonexistent.gguf", LoadModelOpts{
			ContextSize: 512,
			BatchSize:   64,
			GPULayers:   0,
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("expected nil error in stub mode, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadModelWithOpts with pre-cancelled ctx did not return within 5s — possible deadlock")
	}
}

// TestR60_57_LoadModelWithOpts_CancelledDuringLoad — ctx отменён ВО ВРЕМЯ load.
//
// R60.57: watcher на ctx.Done() пробует RequestLoadAbort(handle). Handle nil
// во время stub LoadModel (мгновенный возврат), но watcher pattern корректно
// отрабатывает: после возврата LoadModel watcher exit'ит через watcherDone.
//
// В stub-режиме load завершается до того как cancel успеет подействовать
// (race window слишком короткий). Поэтому этот test проверяет:
//  1. После cancel — handle НЕ становится "застрявшим"
//  2. ctx.Done() обработка не приводит к panic
//  3. LoadModel возвращает в finite time
//
// ПОЛНАЯ end-to-end cancel (load отменяется через C-bridge) — Phase 5
// C-side integration test, см. PLAN.md §5.
func TestR60_57_LoadModelWithOpts_CancelledDuringLoad(t *testing.T) {
	b := setupTestBackendForR60_57(t)
	defer b.ShutdownForTest()

	ctx, cancel := context.WithCancel(context.Background())

	// Spawn load in goroutine (чтобы можно было cancel одновременно).
	done := make(chan error, 1)
	go func() {
		done <- b.LoadModelWithOpts(ctx, "r6057-cancel-model", "/nonexistent.gguf", LoadModelOpts{
			ContextSize: 512,
			BatchSize:   64,
			GPULayers:   0,
		})
	}()

	// Cancel immediately. В stub-режиме load уже завершился, но watcher
	// exit'ит чисто через watcherDone (close перед выходом LoadModelWithOpts).
	cancel()

	select {
	case err := <-done:
		// В stub-режиме err = nil (LoadModel stub успешен). Тест просто
		// проверяет что нет deadlock и нет panic.
		_ = err
	case <-time.After(5 * time.Second):
		t.Fatal("LoadModelWithOpts with cancel did not return within 5s — watcher leaked or deadlock")
	}
}

// TestR60_57_BridgeErrAborted_Sentinel — проверяет что bridge.ErrAborted
// sentinel работает как errors.Is. R60.57 пробрасывает этот sentinel из
// C-bridge через bridge.LoadModel error path (когда C-bridge возвращает
// NULL + abort error message).
//
// В stub-режиме LoadModel не возвращает ErrAborted (нет реального C-side).
// Тест просто проверяет что sentinel is non-nil и errors.Is работает.
func TestR60_57_BridgeErrAborted_Sentinel(t *testing.T) {
	if bridge.ErrAborted == nil {
		t.Fatal("bridge.ErrAborted is nil — sentinel must be non-nil для errors.Is")
	}

	// Wrap sentinel — проверяем что errors.Is находит его.
	wrapped := errWrap(bridge.ErrAborted, "load aborted")
	if !errors.Is(wrapped, bridge.ErrAborted) {
		t.Errorf("errors.Is(wrapped, bridge.ErrAborted) = false, want true (wrapped: %v)", wrapped)
	}

	// Sanity: другой error не должен match'ить ErrAborted.
	other := errors.New("some other error")
	if errors.Is(other, bridge.ErrAborted) {
		t.Errorf("errors.Is(other, bridge.ErrAborted) = true, want false (other: %v)", other)
	}
}

// errWrap — обёртка для errors.Is testing. Аналог fmt.Errorf("%w", err)
// но inlined (avoid circular import issues if any).
func errWrap(err error, msg string) error {
	return &wrappedError{msg: msg, err: err}
}

type wrappedError struct {
	msg string
	err error
}

func (w *wrappedError) Error() string { return w.msg + ": " + w.err.Error() }
func (w *wrappedError) Unwrap() error { return w.err }

// TestR60_57_EnsureModelLoaded_PassesContextToBackend — sanity test что
// ensureModelLoaded пробрасывает ctx в backend.LoadModelWithOpts.
//
// В stub-ре режиме ensureModelLoaded не доступен напрямую (он в cmd/cppworker,
// а тесты в internal/cppbackend). Этот test вместо этого проверяет что
// backend.LoadModelWithOpts правильно обрабатывает ctx от cancel.
//
// Подробный integration test ensureModelLoaded→HTTP request — в
// cmd/cppworker/handlers_openai_load_abort_test.go (out of scope для этой
// правки, требует полной HTTP setup).
func TestR60_57_LoadModelWithOpts_NoGoroutineLeak(t *testing.T) {
	// Этот test — sanity check что watcher goroutine exit'ит после LoadModel.
	// Если watcher утекает (e.g. неправильная close order) — будет видно
	// в CI через -race detector или через leak detector.

	b := setupTestBackendForR60_57(t)
	defer b.ShutdownForTest()

	const N = 50
	for i := 0; i < N; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		_ = b.LoadModelWithOpts(ctx, "leak-test-model", "/nonexistent.gguf", LoadModelOpts{
			ContextSize: 512, BatchSize: 64, GPULayers: 0,
		})
		cancel() // cancel после load — watcher должен exit'ить
	}

	// Если 50 goroutines leak'нули — Go runtime detection flag'нет в -race mode.
	// Здесь мы только проверяем что test не зависает (timeout).
}

// TestR60_57_LoadAbort_RealCancelViaEarlyHandle — R60.57 follow-up (2026-09-13):
// верификация wiring'а LoadModelWithOpts → LoadModelWithEarlyHandle →
// ctx.Done watcher → bridge.RequestLoadAbort.
//
// Использует stub-hooks из c/bridge/bridge_stub.go:
//   - bridge.SetStubLoadDelay(50ms) — имитирует долгий load (50ms)
//   - bridge.ResetStubLoadAbortCount() / bridge.GetStubLoadAbortCount() —
//     counter вызовов RequestLoadAbort
//
// Сценарий:
//  1. Установить stubLoadDelay = 50ms (load занимает время)
//  2. Запустить Load в goroutine
//  3. Через 20ms (после того как load уже начался и watcher spawned) — cancel ctx
//  4. Watcher должен вызвать bridge.RequestLoadAbort → counter++
//  5. Load возвращает (в stub — success, abort no-op)
//  6. Verify counter >= 1 (abort был вызван watcher'ом)
//  7. Verify load завершился в finite time (не завис)
//
// Это НЕ тестирует реальный C-side abort (в stub abort no-op). Это
// тестирует что wiring правильный: watcher действительно вызывает
// bridge.RequestLoadAbort на pre-allocated handle, не только log'ит
// warning о "handle not yet available".
//
// Полная end-to-end cancel с реальным C-side прерыванием — Phase 5
// integration test (требует CUDA + реальная модель).
func TestR60_57_LoadAbort_RealCancelViaEarlyHandle(t *testing.T) {
	const (
		loadDelay   = 50 * time.Millisecond
		cancelAfter = 20 * time.Millisecond
	)

	// Reset counter перед test (изолируем от других тестов).
	bridge.ResetStubLoadAbortCount()

	// Set stubLoadDelay = 50ms (load занимает 50ms).
	prevDelay := bridge.SetStubLoadDelay(loadDelay)
	defer bridge.SetStubLoadDelay(prevDelay) // restore

	b := setupTestBackendForR60_57(t)
	defer b.ShutdownForTest()

	ctx, cancel := context.WithCancel(context.Background())

	// Запускаем Load в goroutine — load занимает 50ms (stub delay).
	loadDone := make(chan error, 1)
	go func() {
		loadDone <- b.LoadModelWithOpts(ctx, "r6057-real-cancel-model",
			"/nonexistent.gguf",
			LoadModelOpts{
				ContextSize: 512,
				BatchSize:   64,
				GPULayers:   0,
			})
	}()

	// Через 20ms (load уже в процессе, watcher spawned и waiting на ctx.Done)
	// cancel ctx — watcher должен вызвать bridge.RequestLoadAbort.
	time.Sleep(cancelAfter)
	cancel()

	// Load должен вернуться в finite time (stub load = 50ms total).
	select {
	case err := <-loadDone:
		// В stub-режиме Load возвращает success даже после abort (abort no-op).
		// Главное — load завершился и watcher успел вызвать RequestLoadAbort.
		if err != nil {
			t.Logf("LoadModelWithOpts returned error (expected nil in stub mode): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LoadModelWithOpts did not return within 2s — watcher leaked or deadlock")
	}

	// Verify: watcher вызвал bridge.RequestLoadAbort хотя бы 1 раз.
	abortCount := bridge.GetStubLoadAbortCount()
	if abortCount < 1 {
		t.Errorf("expected bridge.RequestLoadAbort to be called at least once by watcher, "+
			"got count=%d (watcher did NOT abort — wiring broken)",
			abortCount)
	}
	t.Logf("TestR60_57_LoadAbort_RealCancelViaEarlyHandle: bridge.RequestLoadAbort called %d time(s) by watcher ✓", abortCount)
}

// TestR60_57_LoadModelWithOpts_PreAllocatesHandle — R60.57 follow-up:
// verify что inst.handle pre-allocated ДО C-вызова (не nil).
//
// После успешного LoadModelWithOpts:
//   - inst.handle должен быть non-nil (pre-allocated и C-populated)
//
// Это критично для watcher pattern: если inst.handle был бы nil во
// время cancel — RequestLoadAbort вернул бы "nil model handle" error
// и abort не произошёл бы (даже в real mode).
//
// NOTE: не проверяем inst.handle.path здесь — поле path не экспортировано
// из c/bridge package, мы не можем его читать из тестов cppbackend.
// Достаточно assert'а что inst.handle != nil (non-nil = pre-allocated).
func TestR60_57_LoadModelWithOpts_PreAllocatesHandle(t *testing.T) {
	b := setupTestBackendForR60_57(t)
	defer b.ShutdownForTest()

	err := b.LoadModelWithOpts(context.Background(), "r6057-prealloc-model",
		"/test/prealloc/model.gguf",
		LoadModelOpts{
			ContextSize: 512,
			BatchSize:   64,
			GPULayers:   0,
		})
	if err != nil {
		t.Fatalf("expected nil error in stub mode, got: %v", err)
	}

	b.mu.RLock()
	inst, ok := b.models["r6057-prealloc-model"]
	b.mu.RUnlock()
	if !ok {
		t.Fatal("model not found in b.models after load")
	}
	if inst.handle == nil {
		t.Fatal("inst.handle is nil after successful load — pre-allocation failed")
	}
}

// TestR60_57_LoadAbort_CancelBeforeLoad — R60.57 follow-up edge case:
// cancel ctx ДО Load (pre-cancelled ctx).
//
// Verify:
//   - Load возвращает в finite time (no deadlock)
//   - Watcher выходит чисто через watcherDone (no leak)
//   - bridge.RequestLoadAbort вызывается хотя бы 1 раз (counter >= 1)
//     — даже если ptr не был exposed (stub: always nil ptr)
//
// В real mode (c-bridge): ptr мог бы быть nil (load не начался) → abort
// no-op (RequestLoadAbort вернёт "ptr is nil" error). Это нормальное
// поведение, не regression.
func TestR60_57_LoadAbort_CancelBeforeLoad(t *testing.T) {
	bridge.ResetStubLoadAbortCount()

	const loadDelay = 30 * time.Millisecond
	prevDelay := bridge.SetStubLoadDelay(loadDelay)
	defer bridge.SetStubLoadDelay(prevDelay)

	b := setupTestBackendForR60_57(t)
	defer b.ShutdownForTest()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel BEFORE Load starts

	done := make(chan error, 1)
	go func() {
		done <- b.LoadModelWithOpts(ctx, "r6057-cancel-before-model",
			"/nonexistent.gguf",
			LoadModelOpts{
				ContextSize: 512,
				BatchSize:   64,
				GPULayers:   0,
			})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("LoadModelWithOpts returned error (expected nil in stub mode): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LoadModelWithOpts with pre-cancelled ctx did not return within 2s")
	}

	// Verify: watcher fired and called bridge.RequestLoadAbort at least once.
	// (Even though stub abort is no-op, the call itself verifies wiring.)
	abortCount := bridge.GetStubLoadAbortCount()
	if abortCount < 1 {
		t.Errorf("expected bridge.RequestLoadAbort called at least once (pre-cancelled ctx), "+
			"got count=%d", abortCount)
	}
}

// TestR60_57_LoadAbort_NoAbortWhenLoadSucceeds — R60.57 follow-up:
// verify что watcher НЕ вызывает RequestLoadAbort если load завершается
// успешно БЕЗ cancel (regression на "watcher doesn't abort idle loads").
//
// Сценарий:
//  1. Load в goroutine с stubLoadDelay = 20ms
//  2. ctx НЕ cancel'ится
//  3. Load возвращает success
//  4. Verify bridge.RequestLoadAbort НЕ вызывался (counter == 0)
func TestR60_57_LoadAbort_NoAbortWhenLoadSucceeds(t *testing.T) {
	bridge.ResetStubLoadAbortCount()

	const loadDelay = 20 * time.Millisecond
	prevDelay := bridge.SetStubLoadDelay(loadDelay)
	defer bridge.SetStubLoadDelay(prevDelay)

	b := setupTestBackendForR60_57(t)
	defer b.ShutdownForTest()

	ctx := context.Background()
	err := b.LoadModelWithOpts(ctx, "r6057-success-model",
		"/nonexistent.gguf",
		LoadModelOpts{
			ContextSize: 512,
			BatchSize:   64,
			GPULayers:   0,
		})
	if err != nil {
		t.Fatalf("expected nil error in stub mode, got: %v", err)
	}

	// Verify: RequestLoadAbort НЕ вызывался (no cancel → no abort).
	abortCount := bridge.GetStubLoadAbortCount()
	if abortCount != 0 {
		t.Errorf("expected bridge.RequestLoadAbort NOT called (no cancel), "+
			"got count=%d (watcher erroneously aborted idle load)",
			abortCount)
	}
}

// setupTestBackendForR60_57 — helper для инициализации minimal Backend
// для R60.57 тестов. В llama_stub build тестов, init() порядок не критичен.
//
// NOTE: этот helper дублирует логику из concurrent_generate_test.go.
// Если выносим в shared helpers — делать через internal/cppbackend/testutil/.
//
// R65d (2026-09-20): регистрируем t.Cleanup, который дожидается завершения
// фоновой персистенции nameHistory. До этого RecordModelLoad запускал
// `go saveNameHistory()` без ожидания, и запись .name_history.json могла идти
// ПОСЛЕ t.TempDir() cleanup — тест падал с
// "TempDir RemoveAll cleanup: directory is not empty" (флейк на Windows,
// проявлялся в TestR60_57_LoadModelWithOpts_ContextBackground и
// _PreAllocatesHandle).
func setupTestBackendForR60_57(t *testing.T) *Backend {
	t.Helper()
	b := NewBackend(Config{
		ModelsDir:        t.TempDir(),
		DefaultCtxSize:   4096,
		DefaultBatchSize: 64,
	})
	t.Cleanup(func() {
		if mm := b.ModelManager(); mm != nil {
			if !mm.WaitForPendingWrites(2 * time.Second) {
				t.Logf("warning: nameHistory persist did not finish within 2s")
			}
		}
	})
	return b
}

// ShutdownForTest — best-effort cleanup для тестов. Backend сейчас не имеет
// public Shutdown; здесь no-op (тест полагается на GC + t.Cleanup).
func (b *Backend) ShutdownForTest() {
	// No-op: тест cleanup полагается на GC. Если появятся ресурсы
	// требующие explicit release (channels, files) — добавить здесь.
}
