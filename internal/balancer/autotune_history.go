// autotune_history.go — Round 55.2 (2026-08-24): AutoTune event log (ring buffer).
//
// Цель: оператор должен иметь возможность посмотреть "что AutoTune делал
// последние N часов" — когда сменился KV cache, когда был n_ctx reload,
// когда circuit breaker сработал. EventBus (R54.8) даёт pub/sub для
// live WebSocket, но не replay для новых HTTP clients.
//
// Решение: ring buffer последних N=500 AutoTune событий (per-Proxy, in-memory).
// HTTP endpoint /api/v1/admin/autotune/history возвращает JSON массив.
//
// Thread-safety: sync.RWMutex. Record = write lock. Recent/Since = read lock.
//
// Memory: 500 events × ~500 bytes = ~250KB max. OK для in-memory.

package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// AutoTuneHistoryEntry — одна запись в AutoTune history log.
type AutoTuneHistoryEntry struct {
	Timestamp time.Time              `json:"timestamp"`
	Type      string                 `json:"type"` // triggered | succeeded | failed | circuit_open
	BackendID string                 `json:"backendId"`
	Model     string                 `json:"model"`
	Severity  string                 `json:"severity"`
	Reason    string                 `json:"reason"`
	Message   string                 `json:"message"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// AutoTuneHistory — Round 55.2: ring buffer of AutoTune events.
type AutoTuneHistory struct {
	mu        sync.RWMutex
	entries   []AutoTuneHistoryEntry
	maxSize   int
	droppedCount uint64 // count of events dropped due to overflow (для observability)
}

// NewAutoTuneHistory — Round 55.2: constructor.
func NewAutoTuneHistory(maxSize int) *AutoTuneHistory {
	if maxSize <= 0 {
		maxSize = 500
	}
	return &AutoTuneHistory{
		entries: make([]AutoTuneHistoryEntry, 0, maxSize),
		maxSize: maxSize,
	}
}

// Record — добавляет event в log. Если буфер полон, oldest удаляется
// (drop-oldest policy — newest events важнее для operator visibility).
func (h *AutoTuneHistory) Record(ev types.Event) {
	if h == nil {
		return
	}
	entry := AutoTuneHistoryEntry{
		Timestamp: ev.Timestamp,
		Type:      string(ev.Type),
		BackendID: ev.BackendID,
		Model:     ev.Model,
		Severity:  string(ev.Severity),
		Reason:    ev.Source,
		Message:   ev.Message,
		Data:      ev.Data,
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.entries) >= h.maxSize {
		// Drop oldest (FIFO)
		h.entries = h.entries[1:]
		h.droppedCount++
	}
	h.entries = append(h.entries, entry)
}

// Recent — возвращает последние N entries (newest first).
// limit=0 → все entries.
func (h *AutoTuneHistory) Recent(limit int) []AutoTuneHistoryEntry {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	// Копируем + reverse для newest-first
	all := make([]AutoTuneHistoryEntry, len(h.entries))
	copy(all, h.entries)
	// Reverse in-place
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if limit > 0 && limit < len(all) {
		return all[:limit]
	}
	return all
}

// Since — возвращает entries с Timestamp > since (newest first).
func (h *AutoTuneHistory) Since(since time.Time) []AutoTuneHistoryEntry {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]AutoTuneHistoryEntry, 0)
	// Iterate in reverse (newest first)
	for i := len(h.entries) - 1; i >= 0; i-- {
		if h.entries[i].Timestamp.After(since) {
			result = append(result, h.entries[i])
		} else {
			break // older entries are all before `since`
		}
	}
	return result
}

// Stats — returns current stats (для observability).
type AutoTuneHistoryStats struct {
	Size         int    `json:"size"`
	MaxSize      int    `json:"maxSize"`
	DroppedCount uint64 `json:"droppedCount"`
}

// Stats — current state.
func (h *AutoTuneHistory) Stats() AutoTuneHistoryStats {
	if h == nil {
		return AutoTuneHistoryStats{}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return AutoTuneHistoryStats{
		Size:         len(h.entries),
		MaxSize:      h.maxSize,
		DroppedCount: h.droppedCount,
	}
}

// Reset — очищает log. Используется для тестов или operator action.
func (h *AutoTuneHistory) Reset() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = h.entries[:0]
	h.droppedCount = 0
}
