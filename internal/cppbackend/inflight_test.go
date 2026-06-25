// Package cppbackend — unit-тесты для InFlightCounter (per-model счётчик активных
// inference-запросов, см. inflight.go).
//
// Покрывает:
//   - Базовый Inc/Dec/Get;
//   - Многомодельную изоляцию (Inc одной модели не влияет на счётчик другой);
//   - WaitZero: мгновенный success при counter=0, ожидание с таймаутом, поведение
//     при Inc между проверками (best-effort polling каждые 100ms);
//   - Snapshot: возвращает только ненулевые счётчики;
//   - Reset: обнуляет счётчик указанной модели;
//   - nil-safety: вызовы на nil-получателе / пустом modelName не паникуют.
//
// Все тесты параллельные (нет общего состояния).
package cppbackend

import (
	"sync"
	"testing"
	"time"
)

// TestInFlight_BasicIncDecGet — Inc увеличивает, Dec уменьшает, Get возвращает текущее значение.
func TestInFlight_BasicIncDecGet(t *testing.T) {
	t.Parallel()
	c := NewInFlightCounter()
	if got := c.Get("model-a"); got != 0 {
		t.Fatalf("Get на свежем счётчике = %d, want 0", got)
	}
	c.Inc("model-a")
	c.Inc("model-a")
	if got := c.Get("model-a"); got != 2 {
		t.Fatalf("после двух Inc Get = %d, want 2", got)
	}
	c.Dec("model-a")
	if got := c.Get("model-a"); got != 1 {
		t.Fatalf("после Dec Get = %d, want 1", got)
	}
}

// TestInFlight_EmptyModelNameNoop — пустое имя модели не трогает счётчик (nil-safe).
func TestInFlight_EmptyModelNameNoop(t *testing.T) {
	t.Parallel()
	c := NewInFlightCounter()
	c.Inc("")
	c.Dec("")
	if got := c.Get(""); got != 0 {
		t.Fatalf("Get(\"\") = %d, want 0", got)
	}
}

// TestInFlight_NilReceiverNoPanic — все методы безопасны на nil-получателе.
func TestInFlight_NilReceiverNoPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil-получатель вызвал panic: %v", r)
		}
	}()
	var c *InFlightCounter // nil
	c.Inc("any-model")     // должен быть no-op
	c.Dec("any-model")
	if got := c.Get("any-model"); got != 0 {
		t.Errorf("nil c.Get(\"any-model\") = %d, want 0", got)
	}
	if got := c.WaitZero("any-model", 10*time.Millisecond); !got {
		t.Errorf("nil c.WaitZero должен возвращать true (no-op)")
	}
	if snap := c.Snapshot(); len(snap) != 0 {
		t.Errorf("nil c.Snapshot() len = %d, want 0", len(snap))
	}
	c.Reset("any-model") // no-op
}

// TestInFlight_MultiModelIsolation — Inc/Dec для разных моделей не интерферируют.
func TestInFlight_MultiModelIsolation(t *testing.T) {
	t.Parallel()
	c := NewInFlightCounter()
	c.Inc("alpha")
	c.Inc("alpha")
	c.Inc("beta")
	if got := c.Get("alpha"); got != 2 {
		t.Errorf("alpha = %d, want 2", got)
	}
	if got := c.Get("beta"); got != 1 {
		t.Errorf("beta = %d, want 1", got)
	}
	if got := c.Get("gamma"); got != 0 {
		t.Errorf("gamma (не трогали) = %d, want 0", got)
	}
	c.Dec("alpha")
	if got := c.Get("alpha"); got != 1 {
		t.Errorf("после Dec(alpha) alpha = %d, want 1", got)
	}
	if got := c.Get("beta"); got != 1 {
		t.Errorf("beta после Dec(alpha) = %d, want 1", got)
	}
}

// TestInFlight_WaitZero_Immediate — если counter уже 0, WaitZero возвращает true мгновенно.
func TestInFlight_WaitZero_Immediate(t *testing.T) {
	t.Parallel()
	c := NewInFlightCounter()
	// Модель не отслеживается → counter == 0 → моментальный success.
	start := time.Now()
	if got := c.WaitZero("never-incremented", 30*time.Second); !got {
		t.Fatal("WaitZero для неизвестной модели должен возвращать true")
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("WaitZero занял %v, ожидался моментальный return", elapsed)
	}
}

// TestInFlight_WaitZero_WaitsForDec — WaitZero блокируется, пока счётчик > 0,
// и возвращает true, когда Dec доводит счётчик до 0.
func TestInFlight_WaitZero_WaitsForDec(t *testing.T) {
	t.Parallel()
	c := NewInFlightCounter()
	c.Inc("model-x")
	c.Inc("model-x")

	// В фоне через 250ms делаем два Dec — WaitZero должен вернуться ~250ms.
	go func() {
		time.Sleep(250 * time.Millisecond)
		c.Dec("model-x")
		time.Sleep(50 * time.Millisecond)
		c.Dec("model-x")
	}()

	start := time.Now()
	if got := c.WaitZero("model-x", 5*time.Second); !got {
		t.Fatal("WaitZero должен дождаться Dec и вернуть true")
	}
	elapsed := time.Since(start)
	if elapsed < 200*time.Millisecond || elapsed > 1*time.Second {
		t.Errorf("WaitZero вернулся через %v, ожидалось ~250-500ms", elapsed)
	}
}

// TestInFlight_WaitZero_Timeout — если счётчик не дойдёт до 0 за timeout,
// возвращает false (но при последней проверке 0 → true).
func TestInFlight_WaitZero_Timeout(t *testing.T) {
	t.Parallel()
	c := NewInFlightCounter()
	c.Inc("never-decremented")

	start := time.Now()
	// timeout 200ms — за это время счётчик не обнулится.
	if got := c.WaitZero("never-decremented", 200*time.Millisecond); got {
		t.Fatal("WaitZero с timeout=200ms при counter=1 должен вернуть false")
	}
	elapsed := time.Since(start)
	if elapsed < 150*time.Millisecond {
		t.Errorf("WaitZero вернулся слишком быстро (%v), polling 100ms", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("WaitZero занял %v, ожидалось ~200ms", elapsed)
	}
}

// TestInFlight_Snapshot_OnlyNonZero — Snapshot отбрасывает счётчики с значением 0.
func TestInFlight_Snapshot_OnlyNonZero(t *testing.T) {
	t.Parallel()
	c := NewInFlightCounter()
	c.Inc("active-model")
	c.Inc("active-model")
	// "zero-model" — добавляли и убавляли, должен быть пропущен.
	c.Inc("zero-model")
	c.Dec("zero-model")

	snap := c.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d, want 1 (только active-model)", len(snap))
	}
	if v, ok := snap["active-model"]; !ok || v != 2 {
		t.Errorf("snap[active-model] = %d, ok=%v, want 2,true", v, ok)
	}
	if _, ok := snap["zero-model"]; ok {
		t.Errorf("snap содержит zero-model со значением 0, должен быть пропущен")
	}
}

// TestInFlight_Snapshot_NilSafe — Snapshot на nil возвращает пустую карту (не nil).
func TestInFlight_Snapshot_NilSafe(t *testing.T) {
	t.Parallel()
	var c *InFlightCounter
	snap := c.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot на nil должен возвращать пустую карту, не nil")
	}
	if len(snap) != 0 {
		t.Errorf("Snapshot на nil имеет %d записей, want 0", len(snap))
	}
}

// TestInFlight_Reset — Reset обнуляет счётчик указанной модели.
func TestInFlight_Reset(t *testing.T) {
	t.Parallel()
	c := NewInFlightCounter()
	c.Inc("model-r")
	c.Inc("model-r")
	c.Inc("model-r")
	if got := c.Get("model-r"); got != 3 {
		t.Fatalf("до Reset: %d, want 3", got)
	}
	c.Reset("model-r")
	if got := c.Get("model-r"); got != 0 {
		t.Errorf("после Reset: %d, want 0", got)
	}
	// Snapshot после Reset не должен включать эту модель.
	if _, ok := c.Snapshot()["model-r"]; ok {
		t.Errorf("Snapshot после Reset содержит model-r, должен быть пуст")
	}
}

// TestInFlight_ConcurrentIncDec — стресс-тест: 100 горутин делают Inc/Dec,
// в конце баланс должен быть 0.
func TestInFlight_ConcurrentIncDec(t *testing.T) {
	t.Parallel()
	c := NewInFlightCounter()
	const goroutines = 100
	const iters = 1000
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				c.Inc("shared")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				c.Dec("shared")
			}
		}()
	}
	wg.Wait()
	if got := c.Get("shared"); got != 0 {
		t.Errorf("после %d Inc и %d Dec счётчик = %d, want 0", goroutines*iters, goroutines*iters, got)
	}
}
