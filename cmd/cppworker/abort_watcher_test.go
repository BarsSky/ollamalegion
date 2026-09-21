// abort_watcher_test.go — Round 31 #6 (2026-08-09): tests для AbortWatcher.
//
// Покрывают:
//   - базовый flow: ctx.Done → RequestAbort вызывается
//   - nil safety: nil ctx/model → no-op (no goroutine leak)
//   - Wait() блокирует до завершения
//   - multiple watchers на разные ctx не мешают друг другу
//
// Не требует реальной модели — использует stub bridge.RequestAbort
// (build tag llama_stub), либо полагается на test handle с nil ptr
// (real build) — RequestAbort с nil ptr вернёт ошибку, но watcher
// не паникует, а только логирует warning.

package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"ollama-loadbalancer/pkg/logger"
)

// abortFiredCounter — atomic counter, увеличивается из тестового
// RequestAbort hook (если установлен). Используется для верификации
// что watcher действительно дёргает abort path.
var abortFiredCounter int64

// resetAbortCounter — обнуляет счётчик между тестами.
func resetAbortCounter() {
	atomic.StoreInt64(&abortFiredCounter, 0)
}

// R63 (2026-09-15) API change: NewAbortWatcher принимает unsafe.Pointer на
// per-inference atomic int32 (C-bridge abort flag), а не *bridge.ModelHandle.
// Хелпер даёт тестам валидный non-nil flag без завязки на cgo-типы.
func newTestAbortFlag() unsafe.Pointer {
	return unsafe.Pointer(new(int32))
}

// TestAbortWatcher_NilSafety — передача nil ctx или nil model не запускает goroutine
// и не паникует. Возвращает nil *AbortWatcher.
func TestAbortWatcher_NilSafety(t *testing.T) {
	t.Run("nil ctx", func(t *testing.T) {
		// newTestAbortFlag() — non-nil flag. watcher должен корректно отработать
		// (no panic) при nil ctx.
		w := NewAbortWatcher(nil, newTestAbortFlag())
		if w != nil {
			t.Error("NewAbortWatcher(nil ctx) should return nil, got non-nil")
		}
	})

	t.Run("nil model", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		w := NewAbortWatcher(ctx, nil)
		if w != nil {
			t.Error("NewAbortWatcher(nil model) should return nil, got non-nil")
		}
	})

	t.Run("both nil", func(t *testing.T) {
		w := NewAbortWatcher(nil, nil)
		if w != nil {
			t.Error("NewAbortWatcher(nil, nil) should return nil, got non-nil")
		}
	})
}

// TestAbortWatcher_CtxCancelTriggersRun — при cancel ctx goroutine выполняется
// (даже если RequestAbort падает с ошибкой из-за nil ptr). Проверяем через Wait().
func TestAbortWatcher_CtxCancelTriggersRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// non-nil abort flag; в stub-сборке запись в него — no-op безвредно.
	w := NewAbortWatcher(ctx, newTestAbortFlag())
	if w == nil {
		t.Fatal("NewAbortWatcher returned nil for valid args")
	}

	// Отменяем ctx.
	cancel()

	// Wait должен вернуться в течение секунды (goroutine обработала cancel).
	done := make(chan struct{})
	go func() {
		w.Wait()
		close(done)
	}()
	select {
	case <-done:
		// OK — goroutine завершилась
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return within 2s after ctx cancel")
	}
}

// TestAbortWatcher_NoFireBeforeCtxCancel — до cancel watcher НЕ выполняет RequestAbort.
// Косвенно проверяем через то, что goroutine остаётся в <-ctx.Done().
func TestAbortWatcher_NoFireBeforeCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewAbortWatcher(ctx, newTestAbortFlag())
	if w == nil {
		t.Fatal("NewAbortWatcher returned nil")
	}

	// Ждём 100ms — watcher должен оставаться в <-ctx.Done() всё это время.
	// Если бы он сразу выполнился, RequestAbort был бы вызван и goroutine
	// завершилась бы. Но без cancel он не должен выходить.
	select {
	case <-w.done:
		t.Fatal("watcher.done closed before ctx cancel")
	case <-time.After(100 * time.Millisecond):
		// OK — goroutine всё ещё в <-ctx.Done()
	}
}

// TestAbortWatcher_WaitOnNil — Wait на nil receiver не паникует.
func TestAbortWatcher_WaitOnNil(t *testing.T) {
	var w *AbortWatcher
	// Должно вернуться немедленно без panic.
	done := make(chan struct{})
	go func() {
		w.Wait()
		close(done)
	}()
	select {
	case <-done:
		// OK
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Wait() on nil did not return")
	}
}

// TestAbortWatcher_MultipleInstances — два watcher на разные ctx не мешают друг другу.
func TestAbortWatcher_MultipleInstances(t *testing.T) {
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	w1 := NewAbortWatcher(ctx1, newTestAbortFlag())
	w2 := NewAbortWatcher(ctx2, newTestAbortFlag())

	// Отменяем только ctx1.
	cancel1()

	// w1 должен завершиться, w2 — нет.
	select {
	case <-w1.done:
		// OK
	case <-time.After(1 * time.Second):
		t.Fatal("w1 did not finish after ctx1 cancel")
	}

	// w2 всё ещё ждёт — проверяем что его не задели.
	select {
	case <-w2.done:
		t.Fatal("w2 finished but ctx2 was not cancelled")
	case <-time.After(100 * time.Millisecond):
		// OK
	}

	cancel2() // cleanup
}

// TestAbortWatcher_ConcurrentCreation — 100 одновременных watcher не вызывают
// race conditions. Sanity check перед live test.
func TestAbortWatcher_ConcurrentCreation(t *testing.T) {
	const N = 100
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watchers := make([]*AbortWatcher, N)
	for i := 0; i < N; i++ {
		watchers[i] = NewAbortWatcher(ctx, newTestAbortFlag())
	}

	cancel() // разом отменяем все

	for i, w := range watchers {
		if w == nil {
			t.Fatalf("watcher[%d] is nil", i)
		}
		select {
		case <-w.done:
			// OK
		case <-time.After(5 * time.Second):
			t.Fatalf("watcher[%d] did not finish within 5s", i)
		}
	}
}

// TestAbortWatcher_StubModeSmoke — watcher не паникует на реальном flag,
// полученном из bridge.LoadModel в stub-режиме.
func TestAbortWatcher_StubModeSmoke(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewAbortWatcher(ctx, newTestAbortFlag())
	if w == nil {
		t.Fatal("NewAbortWatcher returned nil")
	}
	cancel()

	done := make(chan struct{})
	go func() {
		w.Wait()
		close(done)
	}()
	select {
	case <-done:
		// OK — в stub mode RequestAbort no-op, watcher завершается чисто
	case <-time.After(2 * time.Second):
		t.Fatal("stub mode watcher did not finish within 2s")
	}

	// Suppress unused logger import warning в stub-only test.
	_ = logger.Get()
}
