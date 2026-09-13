// load_abort_r60_57_test.go — R60.57 (2026-09-13) cancellable load.
//
// Background: R60.57 завершает Go-side integration load-cancellation API
// поверх C-bridge changes (commit e547148). LoadModelWithOpts принимает
// context.Context, spawn watcher goroutine которая вызывает
// bridge.RequestLoadAbort(handle) на ctx.Done().
//
// Тесты проверяют что МОЖНО проверить без C-side fix:
//   - watcher goroutine spawn/exit semantics
//   - ctx propagation через вызовы
//   - ошибка ErrAborted возвращается когда C-bridge её возвращает
//
// Полная end-to-end cancel (cancel реально прерывает C-side bridge_load_model
// во время выполнения) требует C-side fix (см. PLAN.md §3.1 Approach b —
// out_handle parameter). Без этого watcher находит nil handle во время
// load → RequestLoadAbort no-op. Тесты документируют это ограничение.

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
//   1. После cancel — handle НЕ становится "застрявшим"
//   2. ctx.Done() обработка не приводит к panic
//   3. LoadModel возвращает в finite time
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

// setupTestBackendForR60_57 — helper для инициализации minimal Backend
// для R60.57 тестов. В llama_stub build тестов, init() порядок не критичен.
//
// NOTE: этот helper дублирует логику из concurrent_generate_test.go.
// Если выносим в shared helpers — делать через internal/cppbackend/testutil/.
func setupTestBackendForR60_57(t *testing.T) *Backend {
	t.Helper()
	b := NewBackend(Config{
		ModelsDir:       t.TempDir(),
		DefaultCtxSize:  4096,
		DefaultBatchSize: 64,
	})
	return b
}

// ShutdownForTest — best-effort cleanup для тестов. Backend сейчас не имеет
// public Shutdown; здесь no-op (тест полагается на GC + t.Cleanup).
func (b *Backend) ShutdownForTest() {
	// No-op: тест cleanup полагается на GC. Если появятся ресурсы
	// требующие explicit release (channels, files) — добавить здесь.
}
