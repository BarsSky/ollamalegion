// Package cppbackend — UserTracker: per-user parallel request counter.
//
// Round 18 P0.3 (2026-08-04): per-user admission control.
//
// Зачем: InFlightCounter — per-MODEL (для reload-protection), но не защищает
// от ситуации "один клиент занял все слоты бэкенда" (баг в клиенте, retry-loop,
// abuse). UserTracker — per-USER счётчик с atomic check-and-increment для
// admission control.
//
// Использование:
//
//	tracker := NewUserTracker()
//	if !tracker.TryAcquire("alice", 4) { return 429 }  // 5-й запрос от alice
//	defer tracker.Release("alice")
//
// nil-safe: все методы no-op на nil receiver. Default (0) max = unlimited.
//
// Concurrency: sync.Mutex (counters обновляются редко, throughput не критичен).
// Для hot-path (тысячи RPS) можно заменить на atomic.Int64 per-user, но сейчас
// проще и понятнее.
package cppbackend

import "sync"

// UserTracker — потокобезопасный per-user счётчик параллельных запросов.
//
// Сценарий: 1 user = 1 bucket, N parallel = bucket value. Admission
// через TryAcquire (atomic check-and-increment), release через Release.
//
// nil-safe: методы на nil receiver не паникуют. MaxParallel <= 0 → unlimited.
type UserTracker struct {
	mu       sync.Mutex
	counters map[string]int
}

// NewUserTracker создаёт новый tracker.
func NewUserTracker() *UserTracker {
	return &UserTracker{
		counters: make(map[string]int),
	}
}

// TryAcquire атомарно проверяет лимит и инкрементирует счётчик.
//
// Возвращает true если слот получен (current + 1 <= max), false если лимит исчерпан.
//
// Семантика:
//   - max <= 0 → unlimited, всегда true (НЕ инкрементирует)
//   - userID == "" → bucket "anonymous"
//   - nil receiver → false (defensive: admission запрещён если tracker не инициализирован)
func (t *UserTracker) TryAcquire(userID string, max int) bool {
	if t == nil {
		return false
	}
	if max <= 0 {
		// unlimited: admission всегда разрешён, счётчик НЕ трогаем
		// (чтобы Snapshot не показывал аномально большие значения)
		return true
	}
	if userID == "" {
		userID = "anonymous"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counters[userID] >= max {
		return false
	}
	t.counters[userID]++
	return true
}

// Release декрементирует счётчик для userID.
//
// Семантика:
//   - счётчик не опускается ниже 0 (защита от double-release)
//   - userID == "" → bucket "anonymous"
//   - nil receiver → no-op
func (t *UserTracker) Release(userID string) {
	if t == nil {
		return
	}
	if userID == "" {
		userID = "anonymous"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counters[userID] > 0 {
		t.counters[userID]--
	}
	if t.counters[userID] == 0 {
		// Удаляем нулевые записи чтобы Snapshot не возвращал мусор.
		delete(t.counters, userID)
	}
}

// Current возвращает текущее значение счётчика для userID (0 если нет).
func (t *UserTracker) Current(userID string) int {
	if t == nil {
		return 0
	}
	if userID == "" {
		userID = "anonymous"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counters[userID]
}

// Snapshot возвращает копию всех non-zero счётчиков.
//
// Используется для /api/infer/users endpoint. nil-safe.
func (t *UserTracker) Snapshot() map[string]int {
	if t == nil {
		return map[string]int{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int, len(t.counters))
	for u, c := range t.counters {
		if c > 0 {
			out[u] = c
		}
	}
	return out
}

// Reset обнуляет счётчик для userID. Используется в тестах и для admin
// override (debugging в проде).
func (t *UserTracker) Reset(userID string) {
	if t == nil {
		return
	}
	if userID == "" {
		userID = "anonymous"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.counters, userID)
}
