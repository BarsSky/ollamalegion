// Package cppbackend — InFlightCounter: per-model счётчик активных
// inference-запросов.
//
// Зачем: handleReloadModel вызывает UnloadModel → LoadModelWithOpts.
// Без ожидания активных запросов cppworker разрывает их HTTP-соединения
// (EOF клиенту), что приводит к:
//
//   - OpenWebUI / Cline получают EOF посередине ответа и не повторяют;
//   - балансировщик, polling'нувший /api/models в этот момент, ловит RST
//     и зависает в `concurrent load already in progress, waiting`.
//
// Решение: каждый inference-handler вызывает Inc(modelName) на входе и
// Dec(modelName) в defer. handleReloadModel перед UnloadModel зовёт
// WaitZero(modelName, 0) (без лимита — лимит ставится на весь reload через
// context.WithTimeout). Snapshot() используется для heartbeat'а в
// /api/metrics → reload_pending (читается балансировщиком).
package cppbackend

import (
	"sync"
	"sync/atomic"
	"time"
)

// InFlightCounter — потокобезопасный per-model счётчик активных запросов.
//
// Безопасен для конкурентного использования из многих горутин.
// Нулевое значение НЕ готово к работе — используйте NewInFlightCounter.
type InFlightCounter struct {
	mu       sync.RWMutex
	counters map[string]*int64
	// R303: когда счётчик модели стал ненулевым. Нужен, чтобы отличить
	// «долгая генерация» от «запрос, который никто не снял»: в отчёте по стенду
	// (D3) in_flight_requests={"gemma":3} висел минутами без единого признака
	// возраста, и состояние лечилось только перезапуском контейнера.
	ages map[string]time.Time
}

// NewInFlightCounter создаёт новый счётчик.
func NewInFlightCounter() *InFlightCounter {
	return &InFlightCounter{
		counters: make(map[string]*int64),
		ages:     make(map[string]time.Time),
	}
}

// counterFor возвращает (или создаёт) atomic counter для модели.
// Держим mu только для создания/чтения map; сам counter атомарный.
func (c *InFlightCounter) counterFor(modelName string) *int64 {
	c.mu.RLock()
	ctr, ok := c.counters[modelName]
	c.mu.RUnlock()
	if ok {
		return ctr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctr, ok = c.counters[modelName]; ok {
		return ctr
	}
	var v int64
	ctr = &v
	c.counters[modelName] = ctr
	return ctr
}

// Inc увеличивает счётчик активных запросов для модели.
// Используется на входе в handleGenerate/Chat/etc.
func (c *InFlightCounter) Inc(modelName string) {
	if c == nil || modelName == "" {
		return
	}
	if atomic.AddInt64(c.counterFor(modelName), 1) == 1 {
		// R303: первый активный запрос по модели — запоминаем начало.
		c.mu.Lock()
		if c.ages == nil {
			c.ages = make(map[string]time.Time)
		}
		c.ages[modelName] = time.Now()
		c.mu.Unlock()
	}
}

// Dec уменьшает счётчик активных запросов для модели.
// Используется в defer на выходе из inference-handler'а.
func (c *InFlightCounter) Dec(modelName string) {
	if c == nil || modelName == "" {
		return
	}
	ctr := c.counterFor(modelName)
	if atomic.AddInt64(ctr, -1) <= 0 {
		// R303: все запросы по модели закончились — снимаем отметку времени,
		// чтобы возраст не переезжал на следующую генерацию.
		c.mu.Lock()
		delete(c.ages, modelName)
		c.mu.Unlock()
	}
}

// AgesR303 — возраст (в секундах) по каждой модели с ненулевым счётчиком.
// Ключ — имя модели. Пустая мапа, если активных запросов нет.
func (c *InFlightCounter) AgesR303() map[string]float64 {
	if c == nil {
		return map[string]float64{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]float64, len(c.ages))
	now := time.Now()
	for name, at := range c.ages {
		out[name] = now.Sub(at).Seconds()
	}
	return out
}

// OldestAgeR303 — (модель, возраст в секундах) самого старого активного запроса.
func (c *InFlightCounter) OldestAgeR303() (string, float64) {
	if c == nil {
		return "", 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := time.Now()
	bestModel := ""
	bestAge := 0.0
	for name, at := range c.ages {
		age := now.Sub(at).Seconds()
		if age > bestAge {
			bestModel, bestAge = name, age
		}
	}
	return bestModel, bestAge
}

// Get возвращает текущее значение счётчика для модели (0 если модель не отслеживается).
func (c *InFlightCounter) Get(modelName string) int64 {
	if c == nil || modelName == "" {
		return 0
	}
	c.mu.RLock()
	ctr, ok := c.counters[modelName]
	c.mu.RUnlock()
	if !ok {
		return 0
	}
	return atomic.LoadInt64(ctr)
}

// WaitZero блокируется до тех пор, пока счётчик для модели не достигнет нуля.
//
// timeout == 0 → без лимита (используется в handleReloadModel — лимит ставится
// на уровне всего reload через context).
//
// Возвращает true, если дождались нуля, false при таймауте.
//
// Best-effort: проверяет счётчик каждые 100ms; если Inc прилетел между
// проверками — продолжает ждать. Маловероятная гонка (Dec между двумя
// проверками покажет 0, Inc тут же поднимет 1) обрабатывается следующей
// итерацией — WaitZero вернёт true только когда счётчик реально = 0
// на момент финальной проверки.
func (c *InFlightCounter) WaitZero(modelName string, timeout time.Duration) bool {
	if c == nil || modelName == "" {
		return true
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		if c.Get(modelName) == 0 {
			return true
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return c.Get(modelName) == 0
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Snapshot возвращает копию всех счётчиков. Используется для heartbeat'а
// /api/metrics → reload_pending. Ключ — имя модели, значение — текущий счётчик.
//
// nil-safe: возвращает пустую мапу для nil-получателя.
func (c *InFlightCounter) Snapshot() map[string]int64 {
	if c == nil {
		return map[string]int64{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]int64, len(c.counters))
	for name, ctr := range c.counters {
		v := atomic.LoadInt64(ctr)
		if v != 0 {
			out[name] = v
		}
	}
	return out
}

// Reset обнуляет счётчик для указанной модели. Используется в тестах
// и для аварийного сброса через management API.
func (c *InFlightCounter) Reset(modelName string) {
	if c == nil || modelName == "" {
		return
	}
	c.mu.RLock()
	ctr, ok := c.counters[modelName]
	c.mu.RUnlock()
	if !ok {
		return
	}
	atomic.StoreInt64(ctr, 0)
	// R303: аварийный сброс снимает и отметку возраста.
	c.mu.Lock()
	delete(c.ages, modelName)
	c.mu.Unlock()
}
