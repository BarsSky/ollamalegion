// Package cppbackend — SlotManager для multi-slot batched inference.
//
// Round 13 (2026-07-28): defaultNParallel > 1 требует slot management.
// SlotManager выдаёт seq_id (0..maxSlots-1) каждому concurrent inference
// call, чтобы llama.cpp KV-cache регионы не пересекались.
//
// Дизайн (PRAGMATIC, не true batched parallel):
//   - maxSlots=1 → 1 concurrent call, поведение идентично Round 8 (inst.mu
//     сериализует llama_decode, slot acquisition не блокирует).
//   - maxSlots>1 → до N concurrent calls, КАЖДЫЙ держит свой seq_id.
//     llama_decode всё равно сериализуется через inst.mu (Round 8 invariant),
//     но state isolation между calls: каждый slot имеет свой регион в
//     KV-cache и не нужно делать reset_inference_state между calls.
//
// Acquire/Release порядок (важно!):
//   1. slot = sm.Acquire(ctx)        // BLOCKS if all busy (с FIFO ordering)
//   2. inst.mu.Lock()                // serialize llama_decode
//   3. handle.Infer(params{SeqId: slot, ...})
//   4. inst.mu.Unlock()
//   5. sm.Release(slot)              // освобождает слот, будит следующего waiter'а
//
// Deadlock prevention: defer Release() сразу после успешного Acquire(),
// ДО lock inst.mu — это гарантирует что слот освобождается даже при panic.
package cppbackend

import (
	"context"
	"errors"
	"sync"
)

// ErrAllSlotsBusy — все слоты заняты и ctx отменён до освобождения.
var ErrAllSlotsBusy = errors.New("all slots busy and context cancelled")

// SlotManager — thread-safe пул слотов для multi-slot inference.
//
// Изначально пуст (maxSlots=0). Вызвать Init(maxSlots) перед первым Acquire.
// maxSlots=1 эквивалентно Round 8 сериализации (slot acquire не блокирует).
type SlotManager struct {
	mu       sync.Mutex
	cond     *sync.Cond // для блокировки Acquire если все слоты заняты
	maxSlots int        // 0 = not initialized, 1+ = initialized
	inUse    []bool     // [false, false, ...] длина maxSlots
	waiters  []chan int // FIFO очередь ожидающих (round-robin: pop front)
}

// NewSlotManager — конструктор.
// maxSlots: 1 = single-slot legacy (Round 8 behavior), 2..8 = multi-slot.
func NewSlotManager(maxSlots int) *SlotManager {
	if maxSlots < 1 {
		maxSlots = 1
	}
	sm := &SlotManager{
		maxSlots: maxSlots,
		inUse:    make([]bool, maxSlots),
	}
	sm.cond = sync.NewCond(&sm.mu)
	return sm
}

// MaxSlots — возвращает максимальное количество слотов.
func (sm *SlotManager) MaxSlots() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.maxSlots
}

// ActiveCount — текущее количество занятых слотов.
// Используется в метриках и тестах.
func (sm *SlotManager) ActiveCount() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	count := 0
	for _, b := range sm.inUse {
		if b {
			count++
		}
	}
	return count
}

// Acquire — занимает свободный слот, возвращает его ID (0..maxSlots-1).
// Если все слоты заняты — блокирует (FIFO порядок) до освобождения любого
// слота или до отмены ctx.
//
// Возвращает:
//   - slot int — ID слота (0..maxSlots-1), использовать в params.SeqId
//   - release func() — ОБЯЗАТЕЛЬНО вызвать (defer!) для освобождения слота
//   - err error — ErrAllSlotsBusy если ctx отменён до получения слота
//
// Вызывающий код ОБЯЗАН вызвать release() даже при panic:
//
//	slot, release, err := sm.Acquire(ctx)
//	if err != nil { return err }
//	defer release()  // освобождает слот даже при panic в handle.Infer
func (sm *SlotManager) Acquire(ctx context.Context) (int, func(), error) {
	sm.mu.Lock()

	// Быстрый путь: есть свободный слот.
	if slot := sm.findFreeSlot(); slot >= 0 {
		sm.inUse[slot] = true
		sm.mu.Unlock()
		return slot, sm.makeRelease(slot), nil
	}

	// Медленный путь: все слоты заняты. Создаём waiter-канал и ждём.
	waiter := make(chan int, 1)
	sm.waiters = append(sm.waiters, waiter)
	sm.mu.Unlock()

	// Мониторим ctx параллельно с ожиданием waiter-канала.
	select {
	case slot := <-waiter:
		// Слот получен. Release уже привязан к slot ID.
		return slot, sm.makeRelease(slot), nil
	case <-ctx.Done():
		// ctx отменён — убираем waiter из очереди (если ещё там) и возвращаем ошибку.
		sm.mu.Lock()
		for i, w := range sm.waiters {
			if w == waiter {
				sm.waiters = append(sm.waiters[:i], sm.waiters[i+1:]...)
				break
			}
		}
		sm.mu.Unlock()
		return -1, nil, ErrAllSlotsBusy
	}
}

// Release (internal) — освобождает слот и будит одного waiter'а (FIFO).
// Вызывается через release() функцию, возвращённую из Acquire.
//
// ВАЖНО: Release НЕ пытается передать слот конкретному waiter'у в порядке
// FIFO через select. Вместо этого он находит ПЕРВОГО свободного слота и
// отдаёт его СЛЕДУЮЩЕМУ waiter'у в очереди. Это упрощает код и работает
// корректно для нашего use case (slot IDs взаимозаменяемы).
func (sm *SlotManager) releaseInternal(slot int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Помечаем слот свободным.
	sm.inUse[slot] = false

	// Если есть waiter'ы — передаём слот первому.
	if len(sm.waiters) > 0 {
		waiter := sm.waiters[0]
		sm.waiters = sm.waiters[1:]
		// Отдаём слот этому waiter'у.
		sm.inUse[slot] = true
		// Отправляем slot ID в канал (non-blocking, capacity 1).
		waiter <- slot
		// signal cond на случай если кто-то ждёт через cond.Wait (legacy).
		sm.cond.Signal()
	} else {
		// Нет waiter'ов — просто будим cond (для совместимости с будущим cond.Wait).
		sm.cond.Broadcast()
	}
}

// makeRelease — возвращает closure для безопасного release.
func (sm *SlotManager) makeRelease(slot int) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			sm.releaseInternal(slot)
		})
	}
}

// findFreeSlot — находит первый свободный слот (internal, lock held).
// Возвращает -1 если все заняты.
func (sm *SlotManager) findFreeSlot() int {
	for i, b := range sm.inUse {
		if !b {
			return i
		}
	}
	return -1
}
