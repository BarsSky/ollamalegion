// slot_cancel_race_r413_test.go — R413 (2026-11-03): отмена клиента, стоящего
// в очереди за слотом, не должна терять слот.
//
// ЖИВОЙ ДЕФЕКТ. SlotManager.releaseInternal отдаёт освободившийся слот ПЕРВОМУ
// waiter'у: помечает inUse[slot]=true и шлёт slot в его канал (буфер 1). Если
// этот waiter к тому моменту уже отменён (клиент закрыл соединение), Acquire
// уходил в ветку ctx.Done(), waiter из очереди уже убран releaseInternal'ом —
// и зарезервированный слот не освобождал НИКТО. Дальше все генерации висели в
// Acquire(context.Background()) навсегда: active_queries=1, GPU простаивает,
// «stream response summary» в логах больше не появляется, а балансер держит
// свой слот и пускает в очередь всё новые запросы.
//
// Тест воспроизводит ровно гонку «отмена + освобождение одновременно» и
// требует, чтобы после развязки занятых слотов было НОЛЬ.
package cppbackend

import (
	"context"
	"sync"
	"testing"
	"time"
)

// waitQueuedR413 ждёт, пока в очереди появится n ожидающих.
func waitQueuedR413(t *testing.T, sm *SlotManager, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sm.mu.Lock()
		got := len(sm.waiters)
		sm.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiter не появился в очереди за 2 с (ждали %d)", n)
}

// TestR413_CancelRaceDoesNotLeakSlot — главный регресс: 300 раундов гонки
// «отмена стоящего в очереди клиента» против «освобождение слота».
func TestR413_CancelRaceDoesNotLeakSlot(t *testing.T) {
	const rounds = 300
	for i := 0; i < rounds; i++ {
		sm := NewSlotManager(1)
		slotA, relA, err := sm.Acquire(context.Background())
		if err != nil {
			t.Fatalf("раунд %d: Acquire A: %v", i, err)
		}
		_ = slotA

		ctxB, cancelB := context.WithCancel(context.Background())
		type acquireResult struct {
			slot    int
			release func()
			err     error
		}
		resCh := make(chan acquireResult, 1)
		go func() {
			slot, rel, aerr := sm.Acquire(ctxB)
			resCh <- acquireResult{slot: slot, release: rel, err: aerr}
		}()
		waitQueuedR413(t, sm, 1)

		// Гонка: отмена и освобождение — одновременно, порядок не фиксирован.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancelB() }()
		go func() { defer wg.Done(); relA() }()
		wg.Wait()

		var res acquireResult
		select {
		case res = <-resCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("раунд %d: Acquire после отмены завис (слот потерян)", i)
		}
		// Если слот достался B (select выбрал waiter, ctx ещё не был отменён) —
		// освобождаем его, как это сделал бы вызывающий код.
		if res.err == nil {
			if res.release == nil {
				t.Fatalf("раунд %d: Acquire вернул слот без release", i)
			}
			res.release()
		}
		if n := sm.ActiveCount(); n != 0 {
			t.Fatalf("раунд %d: после отмены и освобождения занято %d слотов, ожидалось 0 "+
				"(слот зарезервирован за отменённым waiter'ом и потерян)", i, n)
		}

		// Новый запрос обязан немедленно получить слот — очередь не заклинена.
		done := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, rel, aerr := sm.Acquire(ctx)
			if aerr == nil {
				rel()
			}
			done <- aerr
		}()
		select {
		case aerr := <-done:
			if aerr != nil {
				t.Fatalf("раунд %d: новый Acquire после развязки: %v", i, aerr)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("раунд %d: новый Acquire завис — слот потерян", i)
		}
	}
}

// TestR413_CanceledBeforeStartKeepsSlotFree — отменённый ДО получения слот не
// должен удерживаться: после освобождения занятого слота счётчик обязан стать 0.
func TestR413_CanceledBeforeStartKeepsSlotFree(t *testing.T) {
	sm := NewSlotManager(1)
	if _, rel, err := sm.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	} else {
		defer rel()
	}

	ctxB, cancelB := context.WithCancel(context.Background())
	resCh := make(chan error, 1)
	go func() {
		_, _, err := sm.Acquire(ctxB)
		resCh <- err
	}()
	waitQueuedR413(t, sm, 1)
	cancelB()
	select {
	case err := <-resCh:
		if err == nil {
			t.Fatalf("отменённый Acquire вернул слот")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("отменённый Acquire не вернулся")
	}
	if n := sm.ActiveCount(); n != 1 {
		t.Fatalf("занято %d слотов, ожидался 1 (держит A; B не должен был занять)", n)
	}
}

// TestR413_CancelAfterReservationReturnsSlot — прямой тест ветки «releaseInternal
// уже зарезервировал слот за waiter'ом, а waiter отменён»: Acquire обязан
// вернуть слот и отдать ошибку.
func TestR413_CancelAfterReservationReturnsSlot(t *testing.T) {
	sm := NewSlotManager(1)
	_, relA, err := sm.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire A: %v", err)
	}

	ctxB, cancelB := context.WithCancel(context.Background())
	resCh := make(chan error, 1)
	go func() {
		_, rel, err := sm.Acquire(ctxB)
		if err == nil {
			rel()
		}
		resCh <- err
	}()
	waitQueuedR413(t, sm, 1)

	// Сначала освобождаем слот: releaseInternal забирает waiter и резервирует
	// слот за ним. Затем отменяем ctx — Acquire обязан увидеть отмену и вернуть
	// слот, а не удерживать его.
	relA()
	cancelB()

	select {
	case err := <-resCh:
		if err == nil {
			t.Logf("Acquire успел получить слот до отмены — слот освобождён самим Acquire")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Acquire не вернулся после отмены")
	}
	if n := sm.ActiveCount(); n != 0 {
		t.Fatalf("занято %d слотов, ожидалось 0: слот потерян на отменённом waiter'е", n)
	}
	if _, rel, err := sm.Acquire(context.Background()); err != nil {
		t.Fatalf("слот не вернулся в оборот: %v", err)
	} else {
		rel()
	}
}
