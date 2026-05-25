package balancer

import (
	"math"
	"sort"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// Константы для адаптивного расчёта таймаута
const (
	// maxLatencyRecords — максимум записей в истории задержек на бэкенд
	maxLatencyRecords = 100

	// minLatencySamples — минимальное количество успешных замеров для адаптивного расчёта
	minLatencySamples = 3

	// latencyPercentile — перцентиль для расчёта базового таймаута (p95)
	latencyPercentile = 0.95

	// timeoutMultiplier — множитель к p95 для получения таймаута
	timeoutMultiplier = 2.0

	// minTimeoutFraction — минимальный таймаут как доля от глобального (0.5 = 50%)
	minTimeoutFraction = 0.5

	// maxTimeoutFraction — максимальный таймаут как доля от глобального (3.0 = 300%)
	maxTimeoutFraction = 3.0

	// errorPenalty — на сколько продлевать таймаут при ошибке (сек)
	errorPenalty = 30
)

// computeAdaptiveTimeout вычисляет адаптивный таймаут на основе истории задержек.
// Возвращает таймаут в секундах. 0 = недостаточно данных.
//
// Алгоритм:
//  1. Берём последние N записей
//  2. Фильтруем успешные запросы (Success=true)
//  3. Если успешных < minLatencySamples — возвращаем baseTimeout (недостаточно данных)
//  4. Вычисляем p95 latency среди успешных
//  5. Адаптивный таймаут = max(p95 * timeoutMultiplier, baseTimeout)
//  6. Если в истории есть недавние ошибки — добавляем errorPenalty
//  7. Ограничиваем: [baseTimeout * minTimeoutFraction, baseTimeout * maxTimeoutFraction]
func computeAdaptiveTimeout(history []LatencyRecord, baseTimeout int) int {
	if baseTimeout <= 0 {
		baseTimeout = 120 // fallback
	}
	if len(history) == 0 {
		return 0 // недостаточно данных
	}

	// Берём последние minLatencyRecords записей
	sampleSize := len(history)
	if sampleSize > maxLatencyRecords {
		sampleSize = maxLatencyRecords
	}
	samples := history[len(history)-sampleSize:]

	// Собираем успешные latency
	var successfulLatencies []float64
	hasRecentError := false
	recentCutoff := int64(120) // последние 120 секунд

	for _, r := range samples {
		if r.Success {
			successfulLatencies = append(successfulLatencies, float64(r.LatencyMs))
		} else if timeSince(r.Timestamp) < recentCutoff {
			hasRecentError = true
		}
	}

	if len(successfulLatencies) < minLatencySamples {
		// Недостаточно успешных замеров — используем baseTimeout
		if hasRecentError {
			return baseTimeout + errorPenalty
		}
		return 0
	}

	// Вычисляем p95
	sort.Float64s(successfulLatencies)
	p95Idx := int(math.Ceil(float64(len(successfulLatencies))*latencyPercentile) - 1)
	if p95Idx < 0 {
		p95Idx = 0
	}
	if p95Idx >= len(successfulLatencies) {
		p95Idx = len(successfulLatencies) - 1
	}
	p95LatencyMs := successfulLatencies[p95Idx]

	// Таймаут = p95 * множитель (в секундах)
	adaptiveSec := int(math.Ceil(p95LatencyMs * timeoutMultiplier / 1000.0))

	// Не меньше baseTimeout (если данных всё ещё мало — используем консервативное значение)
	if adaptiveSec < baseTimeout {
		adaptiveSec = baseTimeout
	}

	// Добавляем штраф за ошибки
	if hasRecentError {
		adaptiveSec += errorPenalty
	}

	// Ограничиваем диапазоном относительно baseTimeout
	minTimeout := int(math.Ceil(float64(baseTimeout) * minTimeoutFraction))
	maxTimeout := int(math.Floor(float64(baseTimeout) * maxTimeoutFraction))

	if adaptiveSec < minTimeout {
		adaptiveSec = minTimeout
	}
	if adaptiveSec > maxTimeout {
		adaptiveSec = maxTimeout
	}

	return adaptiveSec
}

// recordLatency добавляет запись о задержке в историю бэкенда,
// обрезает историю до maxLatencyRecords и пересчитывает adaptiveTimeout.
// Потокобезопасна (использует state.mu).
func recordLatency(state *BackendState, latencyMs int64, model string, success bool) {
	state.mu.Lock()
	defer state.mu.Unlock()

	record := LatencyRecord{
		Timestamp: timeNow(),
		LatencyMs: latencyMs,
		Model:     model,
		Success:   success,
	}
	state.LatencyHistory = append(state.LatencyHistory, record)

	// Обрезаем до maxLatencyRecords
	if len(state.LatencyHistory) > maxLatencyRecords {
		state.LatencyHistory = state.LatencyHistory[len(state.LatencyHistory)-maxLatencyRecords:]
	}

	// Пересчитываем adaptiveTimeout
	baseTimeout := state.Backend.RequestTimeout
	if baseTimeout <= 0 {
		baseTimeout = 120 // fallback — будет заменён глобальным в getEffectiveTimeout
	}
	newTimeout := computeAdaptiveTimeout(state.LatencyHistory, baseTimeout)
	if newTimeout > 0 {
		state.AdaptiveTimeout = newTimeout
		state.Backend.RuntimeRequestTimeout = newTimeout
	}

	logger.Get().Debugw("recordLatency: updated adaptive timeout",
		"backend", state.Backend.ID,
		"latency_ms", latencyMs,
		"model", model,
		"success", success,
		"new_adaptive_timeout", newTimeout,
		"history_size", len(state.LatencyHistory))
}

// getEffectiveTimeout возвращает эффективный таймаут для бэкенда:
//   - Если есть adaptiveTimeout (>0) — используем его
//   - Если есть per-backend RequestTimeout (>0) — используем его
//   - Иначе — возвращаем globalTimeout
//
// Потокобезопасна (читает state под mu).
func getEffectiveTimeout(state *BackendState, globalTimeout int) int {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.AdaptiveTimeout > 0 {
		return state.AdaptiveTimeout
	}
	if state.Backend.RequestTimeout > 0 {
		return state.Backend.RequestTimeout
	}
	return globalTimeout
}

// getRuntimeRequestTimeout возвращает сохраняемое значение таймаута (RuntimeRequestTimeout).
// Потокобезопасна.
func getRuntimeRequestTimeout(state *BackendState) int {
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.AdaptiveTimeout > 0 {
		return state.AdaptiveTimeout
	}
	return state.Backend.RuntimeRequestTimeout
}

// timeNow — функция для получения текущего времени (переопределяется в тестах).
var timeNow = func() time.Time {
	return time.Now()
}

// ResetTimeNowForTest — сбрасывает timeNow на реальное время (для тестов).
func ResetTimeNowForTest() {
	timeNow = func() time.Time {
		return time.Now()
	}
}

// timeSince — возвращает количество секунд, прошедших с указанного времени.
func timeSince(t time.Time) int64 {
	return int64(timeNow().Sub(t).Seconds())
}

