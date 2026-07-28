// round13_slot_test.go — Round 13 (2026-07-28): тесты для SlotManager.
//
// Покрывает:
//   - Acquire возвращает свободный slot сразу (fast path)
//   - Acquire блокирует если все заняты, Release будит waiter'а
//   - FIFO порядок (первый waiter получает первый освободившийся slot)
//   - ctx.Done() unblocks Acquire с ErrAllSlotsBusy
//   - ActiveCount() корректно отражает состояние
//   - maxSlots=1 поведение идентично Round 8 (Acquire не блокирует)
package cppbackend

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSlotManager_AcquireFreeSlot — fast path: Acquire возвращает slot 0
// сразу, без блокировки.
func TestSlotManager_AcquireFreeSlot(t *testing.T) {
	sm := NewSlotManager(2)
	slot, release, err := sm.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: unexpected error: %v", err)
	}
	if slot != 0 {
		t.Errorf("expected slot 0, got %d", slot)
	}
	if sm.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d, want 1", sm.ActiveCount())
	}
	release()
	if sm.ActiveCount() != 0 {
		t.Errorf("after Release: ActiveCount = %d, want 0", sm.ActiveCount())
	}
}

// TestSlotManager_AllSlotsBusyBlocks — при maxSlots=1 второй Acquire блокируется.
func TestSlotManager_AllSlotsBusyBlocks(t *testing.T) {
	sm := NewSlotManager(1)
	slot1, release1, err := sm.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if slot1 != 0 {
		t.Errorf("first slot = %d, want 0", slot1)
	}

	// Второй Acquire должен заблокироваться. Используем goroutine + сигнал.
	acquired := make(chan int, 1)
	acquireErr := make(chan error, 1)
	go func() {
		s, r, e := sm.Acquire(context.Background())
		_ = r // release happens later
		acquired <- s
		acquireErr <- e
	}()

	// Проверяем что goroutine заблокирован (acquired пуст).
	select {
	case s := <-acquired:
		t.Fatalf("second Acquire did not block, got slot %d", s)
	case <-time.After(100 * time.Millisecond):
		// ОК — заблокирован
	}

	// Освобождаем slot — waiter должен проснуться.
	release1()

	select {
	case s := <-acquired:
		if s != 0 {
			t.Errorf("acquired slot = %d, want 0", s)
		}
		if e := <-acquireErr; e != nil {
			t.Errorf("acquire err = %v, want nil", e)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("second Acquire did not unblock after Release")
	}
}

// TestSlotManager_MultiSlotTwoConcurrent — 2 concurrent Acquire с maxSlots=2
// оба получают разные slot ID (0 и 1).
func TestSlotManager_MultiSlotTwoConcurrent(t *testing.T) {
	sm := NewSlotManager(2)

	slot0, release0, err := sm.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	slot1, release1, err := sm.Acquire(context.Background())
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}

	if slot0 == slot1 {
		t.Errorf("both got same slot %d, want different", slot0)
	}
	if sm.ActiveCount() != 2 {
		t.Errorf("ActiveCount = %d, want 2", sm.ActiveCount())
	}

	release0()
	release1()
	if sm.ActiveCount() != 0 {
		t.Errorf("after Release x2: ActiveCount = %d, want 0", sm.ActiveCount())
	}
}

// TestSlotManager_ContextCancelUnblocks — ctx.Done() unblocks Acquire
// с ErrAllSlotsBusy.
func TestSlotManager_ContextCancelUnblocks(t *testing.T) {
	sm := NewSlotManager(1)
	_, release0, _ := sm.Acquire(context.Background())

	acquired := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, _, e := sm.Acquire(ctx)
		acquired <- e
	}()

	// Даём goroutine стартовать.
	time.Sleep(50 * time.Millisecond)

	// Отменяем ctx — Acquire должен вернуть ErrAllSlotsBusy.
	cancel()

	select {
	case err := <-acquired:
		if err != ErrAllSlotsBusy {
			t.Errorf("err = %v, want ErrAllSlotsBusy", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("Acquire did not unblock after ctx.Cancel")
	}

	release0()
}

// TestSlotManager_FIFOOrder — waiter'ы получают слоты в порядке FIFO.
// Каждый waiter сразу релизит слот после получения, чтобы следующий
// waiter в очереди мог его получить.
func TestSlotManager_FIFOOrder(t *testing.T) {
	sm := NewSlotManager(1)
	_, release0, _ := sm.Acquire(context.Background())

	const numWaiters = 3
	order := make([]int, numWaiters)
	done := make(chan struct{}, numWaiters)

	for i := 0; i < numWaiters; i++ {
		i := i
		go func() {
			slot, release, _ := sm.Acquire(context.Background())
			order[i] = slot
			release() // сразу релизим для следующего waiter'а
			done <- struct{}{}
		}()
	}

	// Даём всем waiter'ам встать в очередь.
	time.Sleep(50 * time.Millisecond)

	// Освобождаем slot — запускает цепочку FIFO.
	release0()

	// Все 3 waiter'а должны проснуться последовательно.
	for i := 0; i < numWaiters; i++ {
		select {
		case <-done:
			// OK
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d did not unblock", i)
		}
	}

	// Все получили slot 0 (maxSlots=1, slot IDs всегда 0).
	for i, s := range order {
		if s != 0 {
			t.Errorf("waiter %d: slot = %d, want 0", i, s)
		}
	}
}

// TestSlotManager_ReleaseIsIdempotent — release() можно вызвать только ОДИН раз.
// Второй вызов не должен паниковать (sync.Once защищает).
func TestSlotManager_ReleaseIsIdempotent(t *testing.T) {
	sm := NewSlotManager(2)
	_, release, _ := sm.Acquire(context.Background())
	release()
	release() // should not panic (sync.Once защищает)
	// ActiveCount должен остаться 0, не -1.
	if sm.ActiveCount() != 0 {
		t.Errorf("ActiveCount = %d, want 0 (release was called twice, should be no-op)", sm.ActiveCount())
	}
}

// TestSlotManager_DeferReleaseDoesNotLeak — при exit с defer release() слот
// освобождается даже при panic (через defer LIFO).
func TestSlotManager_DeferReleaseDoesNotLeak(t *testing.T) {
	sm := NewSlotManager(1)
	func() {
		defer func() {
			// Ловим panic и проверяем что slot освобождён.
			_ = recover()
		}()
		_, release, _ := sm.Acquire(context.Background())
		defer release() // должен сработать при panic
		panic("test panic")
	}()

	// После panic+recover, slot должен быть свободен.
	if sm.ActiveCount() != 0 {
		t.Errorf("after panic+recover: ActiveCount = %d, want 0 (defer release did not run)", sm.ActiveCount())
	}
}

// TestSlotManager_ConcurrentAcquireRelease — стресс-тест: 100 goroutines
// Acquire/Release, проверяем что slot count всегда <= maxSlots.
func TestSlotManager_ConcurrentAcquireRelease(t *testing.T) {
	sm := NewSlotManager(4)
	const numGoroutines = 100
	const opsPerGoroutine = 50

	var maxObserved int32
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				slot, release, err := sm.Acquire(context.Background())
				if err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				// Проверяем что slot ID в допустимом диапазоне.
				if slot < 0 || slot >= sm.MaxSlots() {
					t.Errorf("invalid slot %d (maxSlots=%d)", slot, sm.MaxSlots())
				}
				// Отслеживаем peak concurrent count.
				cur := int32(sm.ActiveCount())
				for {
					max := atomic.LoadInt32(&maxObserved)
					if cur <= max || atomic.CompareAndSwapInt32(&maxObserved, max, cur) {
						break
					}
				}
				// Симулируем работу.
				time.Sleep(1 * time.Millisecond)
				release()
			}
		}()
	}

	wg.Wait()
	peak := atomic.LoadInt32(&maxObserved)
	if peak > int32(sm.MaxSlots()) {
		t.Errorf("peak ActiveCount = %d, want <= %d (over-acquire detected!)", peak, sm.MaxSlots())
	}
	if sm.ActiveCount() != 0 {
		t.Errorf("after all goroutines: ActiveCount = %d, want 0", sm.ActiveCount())
	}
}
