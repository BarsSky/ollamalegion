// waitforload_r60_56_test.go — tests for R60.56 WaitForLoad timeout bugfix.
//
// Bug: lazy-load goroutine takes TryLockLoad (creates channel), enters C
// code (llama.cpp load_model), gets stuck there. defer UnlockLoad never
// fires. Channel never closes. Every subsequent WaitForLoad blocks <-ch
// forever. HTTP handlers hang. Balancer hangs. User sees "5/5 messages"
// white dot indefinitely.
//
// Fix: WaitForLoad now waits on a select with timer (waitForLoadTimeout
// = 180s). After timeout, returns false → caller returns errModelIsLoading
// → client gets 503 + Retry-After → graceful retry instead of infinite hang.
package cppbackend

import (
	"sync/atomic"
	"testing"
	"time"
)

// setWaitForLoadTimeoutForTest — переопределяет package-level waitForLoadTimeout
// на время теста, возвращает restore-функцию. Используется чтобы тест
// deadlock'а работал за секунды, а не 3 минуты.
func setWaitForLoadTimeoutForTest(t *testing.T, d time.Duration) func() {
	t.Helper()
	orig := waitForLoadTimeout
	waitForLoadTimeout = d
	return func() { waitForLoadTimeout = orig }
}

// TestWaitForLoad_TimeoutWhenChannelNeverCloses — R60.56 deadlock reproduction.
// Создаём канал (как TryLockLoad делает), НЕ закрываем его. Ждём,
// что WaitForLoad вернёт false после timeout (не зависнет).
func TestWaitForLoad_TimeoutWhenChannelNeverCloses(t *testing.T) {
	t.Parallel()
	defer setWaitForLoadTimeoutForTest(t, 2*time.Second)()

	b := NewBackend(Config{ModelsDir: "/tmp/test-models-r60_56"})

	// TryLockLoad создаёт канал. НЕ вызываем UnlockLoad — эмулируем
	// застрявшую в C-коде lazy-load goroutine.
	lockOk, lockErr := b.TryLockLoad("test-model-deadlock")
	if lockErr != nil || !lockOk {
		t.Fatalf("TryLockLoad failed: ok=%v err=%v", lockOk, lockErr)
	}

	// WaitForLoad должен вернуться через ~2s (наш test timeout) с false.
	start := time.Now()
	result := b.WaitForLoad("test-model-deadlock")
	elapsed := time.Since(start)

	if result {
		t.Fatalf("WaitForLoad вернул true для deadlock-сценария — баг в R60.56 (канал не закрыт, модель не в b.models, должно быть false)")
	}
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("WaitForLoad вернулся слишком быстро (%v) — должно быть ≥timeout. Проверьте что timer+select действительно работает", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("WaitForLoad занял %v — слишком долго для timeout=2s", elapsed)
	}

	t.Logf("✅ R60.56 fix работает: WaitForLoad вернулся через %v (timeout был 2s) с false — caller получит 503 + Retry-After", elapsed)
}

// TestWaitForLoad_SuccessWhenChannelCloses — happy path: канал закрывается,
// WaitForLoad возвращается быстро (не дожидаясь timeout).
func TestWaitForLoad_SuccessWhenChannelCloses(t *testing.T) {
	t.Parallel()
	defer setWaitForLoadTimeoutForTest(t, 10*time.Second)() // длинный timeout, чтобы убедиться что возвращается из <-ch, а не из timeout

	b := NewBackend(Config{ModelsDir: "/tmp/test-models-r60_56"})

	lockOk, _ := b.TryLockLoad("test-model-success")
	if !lockOk {
		t.Fatal("TryLockLoad failed")
	}

	closed := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		b.UnlockLoad("test-model-success")
		close(closed)
	}()

	start := time.Now()
	result := b.WaitForLoad("test-model-success")
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("WaitForLoad занял %v — ожидалось <2s (канал закрылся через 50ms)", elapsed)
	}
	// result может быть true/false в зависимости от b.models — главное что вернулся.
	_ = result
	<-closed
}

// TestWaitForLoad_NoLoadingInProgress — если канала нет (никто не грузит),
// WaitForLoad должен вернуться мгновенно.
func TestWaitForLoad_NoLoadingInProgress(t *testing.T) {
	t.Parallel()
	defer setWaitForLoadTimeoutForTest(t, 10*time.Second)()

	b := NewBackend(Config{ModelsDir: "/tmp/test-models-r60_56"})

	start := time.Now()
	result := b.WaitForLoad("nonexistent-model")
	elapsed := time.Since(start)

	if elapsed > 100*time.Millisecond {
		t.Fatalf("WaitForLoad без активной загрузки должен быть мгновенным, занял %v", elapsed)
	}
	if result {
		t.Fatal("WaitForLoad без загрузки вернул true — должен false")
	}
}

// TestWaitForLoad_ConcurrentWaiters — несколько goroutine ждут один канал,
// канал закрывается — все должны разблокироваться (не deadlock).
func TestWaitForLoad_ConcurrentWaiters(t *testing.T) {
	t.Parallel()
	defer setWaitForLoadTimeoutForTest(t, 10*time.Second)()

	b := NewBackend(Config{ModelsDir: "/tmp/test-models-r60_56"})

	lockOk, _ := b.TryLockLoad("test-model-concurrent")
	if !lockOk {
		t.Fatal("TryLockLoad failed")
	}

	const numWaiters = 10
	var unblocked atomic.Int32
	done := make(chan struct{})

	for i := 0; i < numWaiters; i++ {
		go func() {
			b.WaitForLoad("test-model-concurrent")
			if unblocked.Add(1) == numWaiters {
				close(done)
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	b.UnlockLoad("test-model-concurrent")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Только %d/%d waiter'ов разблокировались — deadlock в fan-out", unblocked.Load(), numWaiters)
	}
}
