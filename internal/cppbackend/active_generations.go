package cppbackend

import (
	"context"
	"strings"
	"sync"

	"ollama-loadbalancer/pkg/logger"
)

// ActiveGenerations — трекает активные inference-запросы для cancel API.
// Ключ: requestID (из X-Request-Id или сгенерированный).
// Значение: context.CancelFunc + userID (для фильтрации по user).
//
// Round 18 P0.2 (2026-08-03). Дополняет InFlightCounter (per-model) —
// ActiveGenerations даёт per-request granularity для cancel API.
//
// Thread-safe. nil-safe (Add/Remove/Cancel — no-op для nil получателя).
type ActiveGenerations struct {
	mu      sync.RWMutex
	entries map[string]*generationEntry
}

type generationEntry struct {
	cancel  context.CancelFunc
	userID  string
	model   string
	backend string
}

// NewActiveGenerations создаёт новый tracker.
func NewActiveGenerations() *ActiveGenerations {
	return &ActiveGenerations{
		entries: make(map[string]*generationEntry),
	}
}

// Add регистрирует активную генерацию.
// Возвращает true если успешно, false если requestID уже занят (collision).
func (a *ActiveGenerations) Add(requestID, userID, model, backend string, cancel context.CancelFunc) bool {
	if a == nil || requestID == "" || cancel == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.entries[requestID]; exists {
		return false
	}
	a.entries[requestID] = &generationEntry{
		cancel:  cancel,
		userID:  userID,
		model:   model,
		backend: backend,
	}
	return true
}

// Remove снимает регистрацию (вызывается при завершении запроса).
// Возвращает true если запрос был зарегистрирован.
func (a *ActiveGenerations) Remove(requestID string) bool {
	if a == nil || requestID == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.entries[requestID]
	if ok {
		delete(a.entries, requestID)
	}
	return ok
}

// CancelByID отменяет генерацию по requestID.
// Возвращает true если нашли и отменили.
func (a *ActiveGenerations) CancelByID(requestID string) bool {
	if a == nil || requestID == "" {
		return false
	}
	a.mu.RLock()
	entry, ok := a.entries[requestID]
	a.mu.RUnlock()
	if !ok {
		return false
	}
	entry.cancel()
	logger.Get().Infow("ActiveGenerations: cancelled by ID",
		"request_id", requestID, "user", entry.userID, "model", entry.model)
	return true
}

// CancelByUser отменяет ВСЕ активные генерации для userID.
// Возвращает количество отменённых.
func (a *ActiveGenerations) CancelByUser(userID string) int {
	if a == nil || userID == "" {
		return 0
	}
	a.mu.RLock()
	var matches []*generationEntry
	for _, entry := range a.entries {
		if entry.userID == userID {
			matches = append(matches, entry)
		}
	}
	a.mu.RUnlock()

	cancelled := 0
	for _, entry := range matches {
		entry.cancel()
		cancelled++
	}
	if cancelled > 0 {
		logger.Get().Infow("ActiveGenerations: cancelled by user",
			"user", userID, "count", cancelled)
	}
	return cancelled
}

// normalizeCancelModelName — форма имени модели для СРАВНЕНИЯ при отмене (R353).
//
// ЗАЧЕМ. Балансер выбирает адресата отмены «мягко» (регистр, путь, расширение
// .gguf, тег Ollama, частичное совпадение), а здесь сравнение было ТОЧНЫМ. Живой
// дефект: клиент жмёт Stop с "Qwen3-Instruct-2507-q4km.gguf", адресат выбран верно,
// приказ доставлен, а воркер отвечает cancelled:0 — стрим продолжается. Снаружи
// «кнопка Stop не работает».
//
// Нормализация сознательно МИНИМАЛЬНАЯ: basename (у пути своя пунктуация), нижний
// регистр и срезание .gguf. Тег Ollama здесь НЕ срезается: если на узле крутятся две
// генерации разных вариантов одной семьи, тег — единственное, что их различает, и
// срезав его можно погасить чужую работу. Балансер компенсирует тег тем, что
// подставляет загруженное имя (cancelModelNameForBackend).
func normalizeCancelModelName(name string) string {
	n := strings.TrimSpace(name)
	if i := strings.LastIndexAny(n, `/\`); i >= 0 {
		n = n[i+1:]
	}
	n = strings.ToLower(n)
	for _, ext := range []string{".gguf", ".GGUF"} {
		n = strings.TrimSuffix(n, strings.ToLower(ext))
	}
	return n
}

// CancelByModel отменяет ВСЕ активные генерации для model+userID.
// Возвращает количество отменённых.
//
// R353: имена сравниваются в нормализованной форме (см. normalizeCancelModelName) —
// точное сравнение оставляло отмену «доставленной, но не сработавшей», если клиент
// назвал модель с расширением/в другом регистре.
func (a *ActiveGenerations) CancelByModel(userID, model string) int {
	if a == nil || model == "" {
		return 0
	}
	want := normalizeCancelModelName(model)
	a.mu.RLock()
	var matches []*generationEntry
	for _, entry := range a.entries {
		if normalizeCancelModelName(entry.model) == want && (userID == "" || entry.userID == userID) {
			matches = append(matches, entry)
		}
	}
	a.mu.RUnlock()

	cancelled := 0
	for _, entry := range matches {
		entry.cancel()
		cancelled++
	}
	if cancelled > 0 {
		logger.Get().Infow("ActiveGenerations: cancelled by model",
			"user", userID, "model", model, "count", cancelled)
	}
	return cancelled
}

// Snapshot возвращает список активных генераций (для /api/infer/active).
// Безопасный для concurrent use — возвращает копию данных.
func (a *ActiveGenerations) Snapshot() []GenerationInfo {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	result := make([]GenerationInfo, 0, len(a.entries))
	for id, entry := range a.entries {
		result = append(result, GenerationInfo{
			RequestID: id,
			UserID:    entry.userID,
			Model:     entry.model,
			Backend:   entry.backend,
		})
	}
	return result
}

// GenerationInfo — описание активной генерации (для /api/infer/active).
// JSON-теги в snake_case (request_id, user_id) — консистентно с X-Request-Id / X-User-Id.
type GenerationInfo struct {
	RequestID string `json:"request_id"`
	UserID    string `json:"user_id"`
	Model     string `json:"model"`
	Backend   string `json:"backend"`
}
