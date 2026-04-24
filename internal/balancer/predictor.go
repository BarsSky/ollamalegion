package balancer

import (
	"math"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// Predictor - модуль прогнозирования критических состояний бэкендов
type Predictor struct {
	mu sync.RWMutex

	// Конфигурация
	historyLimit   int           // Максимальное количество точек истории
	minHistorySize int           // Минимум точек для расчёта тренда
	trendWindow    time.Duration // Окно для расчёта тренда
}

// NewPredictor - создание предиктора
func NewPredictor() *Predictor {
	return &Predictor{
		historyLimit:   120,                // 10 минут при сборе раз в 5 сек
		minHistorySize: 6,                  // Минимум 30 секунд истории
		trendWindow:    2 * time.Minute,    // Окно тренда
	}
}

// UpdateHistory - добавление новой точки метрик в историю бэкенда
func (pr *Predictor) UpdateHistory(state *BackendState, metrics *types.BackendMetrics) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	now := time.Now()

	// Вычисляем проценты использования
	var vramPercent, ramPercent float64
	if metrics.GPU.MemoryTotal > 0 {
		vramPercent = float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
	}
	if metrics.System.MemoryTotal > 0 {
		ramPercent = float64(metrics.System.MemoryUsed) * 100 / float64(metrics.System.MemoryTotal)
	}

	// Вычисляем свободные слоты
	freeSlots := pr.calculateFreeSlots(state, metrics)

	snapshot := types.MetricsSnapshot{
		Timestamp:         now,
		GPUUsagePercent:   metrics.GPU.UsagePercent,
		VRAMUsagePercent:  vramPercent,
		RAMUsagePercent:   ramPercent,
		ActiveRequests:    metrics.Ollama.ActiveRequests,
		RunningModels:     len(metrics.Ollama.RunningModels),
		FreeSlots:         freeSlots,
		RequestsPerSecond: metrics.Ollama.RequestsPerSecond,
	}

	// Добавляем в историю
	state.MetricsHistory = append(state.MetricsHistory, snapshot)

	// Ограничиваем размер истории
	if len(state.MetricsHistory) > pr.historyLimit {
		state.MetricsHistory = state.MetricsHistory[len(state.MetricsHistory)-pr.historyLimit:]
	}

	// Пересчитываем прогноз
	state.Prediction = pr.calculatePrediction(state, metrics, freeSlots)
}

// calculateFreeSlots - вычисление свободных слотов для запросов
func (pr *Predictor) calculateFreeSlots(state *BackendState, metrics *types.BackendMetrics) int {
	// 1. Если задан MaxConcurrentRequests — используем его
	if metrics.Ollama.MaxConcurrentRequests > 0 {
		// ActiveRequests теперь считается балансировщиком (proxy-счётчик)
		// Но если агент прислал ненулевое значение — используем его
		active := metrics.Ollama.ActiveRequests
		if active == 0 {
			// Fallback: используем proxy-счётчик активных запросов
			active = state.ActiveReqs
		}
		free := metrics.Ollama.MaxConcurrentRequests - active
		if free < 0 {
			free = 0
		}
		return free
	}

	// 2. Оценка по VRAM (fallback для GPU)
	if metrics.GPU.MemoryTotal > 0 && metrics.GPU.MemoryTotal > metrics.GPU.MemoryUsed {
		freeVRAM := metrics.GPU.MemoryTotal - metrics.GPU.MemoryUsed
		// Средняя модель ~4GB, запрос на модель требует ~512MB-2GB дополнительно
		// Консервативная оценка: 1 слот = 2GB
		slotVRAM := uint64(2048) // MB
		free := int(freeVRAM / slotVRAM)
		if free < 0 {
			free = 0
		}
		return free
	}

	// 3. Оценка по RAM (fallback для CPU)
	if metrics.System.MemoryTotal > 0 {
		freeRAM := metrics.System.MemoryFree
		slotRAM := uint64(4096) // MB на CPU
		free := int(freeRAM / slotRAM)
		if free < 0 {
			free = 0
		}
		return free
	}

	return 0
}

// calculatePrediction - расчёт прогноза критического состояния
func (pr *Predictor) calculatePrediction(state *BackendState, metrics *types.BackendMetrics, currentFreeSlots int) types.Prediction {
	history := state.MetricsHistory
	if len(history) < pr.minHistorySize {
		return types.Prediction{
			SecondsToCritical: -1,
			CriticalReason:      "none",
			RequestCapacity:     pr.calculateRequestCapacity(metrics, currentFreeSlots),
		}
	}

	pred := types.Prediction{
		CriticalReason: "none",
	}

	// --- Расчёт трендов (linear regression) ---
	pred.GPUUsageTrend = pr.linearTrend(history, func(s types.MetricsSnapshot) float64 { return s.GPUUsagePercent })
	pred.VRAMUsageTrend = pr.linearTrend(history, func(s types.MetricsSnapshot) float64 { return s.VRAMUsagePercent })
	pred.RAMUsageTrend = pr.linearTrend(history, func(s types.MetricsSnapshot) float64 { return s.RAMUsagePercent })
	pred.FreeSlotsTrend = pr.linearTrend(history, func(s types.MetricsSnapshot) float64 { return float64(s.FreeSlots) })

	// --- Прогнозирование времени до критического состояния ---
	var minTime float64 = math.Inf(1)
	var reason string

	// GPU Usage → 100%
	if pred.GPUUsageTrend > 0 {
		t := pr.timeToThreshold(metrics.GPU.UsagePercent, 100.0, pred.GPUUsageTrend)
		if t < minTime {
			minTime = t
			reason = "gpu_usage"
		}
	}

	// VRAM → 100%
	if pred.VRAMUsageTrend > 0 {
		t := pr.timeToThreshold(pr.lastValue(history, func(s types.MetricsSnapshot) float64 { return s.VRAMUsagePercent }), 100.0, pred.VRAMUsageTrend)
		if t < minTime {
			minTime = t
			reason = "vram"
		}
	}

	// RAM → 95% (менее критично, но важно)
	if pred.RAMUsageTrend > 0 {
		t := pr.timeToThreshold(pr.lastValue(history, func(s types.MetricsSnapshot) float64 { return s.RAMUsagePercent }), 95.0, pred.RAMUsageTrend)
		if t < minTime {
			minTime = t
			reason = "ram"
		}
	}

	// Free Slots → 0 (система не может принимать новые запросы)
	if pred.FreeSlotsTrend < 0 {
		t := pr.timeToThreshold(float64(currentFreeSlots), 0.0, -pred.FreeSlotsTrend)
		if t < minTime {
			minTime = t
			reason = "concurrent_requests"
		}
	}

	// Модели capacity → если MaxModels задан и заполнен
	if metrics.Ollama.MaxModels > 0 {
		modelsLeft := metrics.Ollama.MaxModels - len(metrics.Ollama.RunningModels)
		modelTrend := pr.linearTrend(history, func(s types.MetricsSnapshot) float64 { return float64(s.RunningModels) })
		if modelTrend > 0 {
			t := pr.timeToThreshold(float64(modelsLeft), 0.0, modelTrend)
			if t < minTime {
				minTime = t
				reason = "models_capacity"
			}
		}
	}

	if reason != "" {
		pred.SecondsToCritical = minTime
		pred.CriticalReason = reason
	} else {
		pred.SecondsToCritical = -1 // Нет угрозы
	}

	// --- Текущая ёмкость запросов (0-100%) ---
	pred.RequestCapacity = pr.calculateRequestCapacity(metrics, currentFreeSlots)

	return pred
}

// calculateRequestCapacity - текущая загрузка бэкенда (0% = свободен, 100% = полностью загружен)
func (pr *Predictor) calculateRequestCapacity(metrics *types.BackendMetrics, freeSlots int) float64 {
	var scores []float64

	// GPU загрузка (вес 0.35)
	if metrics.GPU.UsagePercent > 0 || metrics.GPU.MemoryTotal > 0 {
		scores = append(scores, metrics.GPU.UsagePercent*0.35)
		if metrics.GPU.MemoryTotal > 0 {
			vramPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
			scores = append(scores, vramPercent*0.25)
		}
	}

	// CPU/RAM загрузка (вес 0.25)
	if metrics.System.CPUUsagePercent > 0 {
		scores = append(scores, metrics.System.CPUUsagePercent*0.15)
	}
	if metrics.System.MemoryTotal > 0 {
		ramPercent := float64(metrics.System.MemoryUsed) * 100 / float64(metrics.System.MemoryTotal)
		scores = append(scores, ramPercent*0.10)
	}

	// Запросы (вес 0.15)
	if metrics.Ollama.MaxConcurrentRequests > 0 {
		reqRatio := float64(metrics.Ollama.ActiveRequests) / float64(metrics.Ollama.MaxConcurrentRequests)
		scores = append(scores, reqRatio*100*0.15)
	} else if freeSlots >= 0 {
		// Fallback: если свободных слотов мало — высокая загрузка
		if freeSlots == 0 {
			scores = append(scores, 100.0*0.15)
		} else if freeSlots <= 2 {
			scores = append(scores, 80.0*0.15)
		} else if freeSlots <= 5 {
			scores = append(scores, 50.0*0.15)
		}
	}

	if len(scores) == 0 {
		return 0
	}

	var total float64
	for _, s := range scores {
		total += s
	}
	capacity := total
	if capacity > 100 {
		capacity = 100
	}
	if capacity < 0 {
		capacity = 0
	}
	return capacity
}

// linearTrend - линейный тренд (процентов в минуту)
func (pr *Predictor) linearTrend(history []types.MetricsSnapshot, getter func(types.MetricsSnapshot) float64) float64 {
	if len(history) < 2 {
		return 0
	}

	// Используем точки за trendWindow
	cutoff := time.Now().Add(-pr.trendWindow)
	var xs []float64
	var ys []float64

	for _, snap := range history {
		if snap.Timestamp.Before(cutoff) {
			continue
		}
		age := time.Since(snap.Timestamp).Minutes()
		xs = append(xs, -age) // Отрицательное время: 0 = сейчас, -2 = 2 минуты назад
		ys = append(ys, getter(snap))
	}

	if len(xs) < 2 {
		// Fallback: используем все точки
		for i, snap := range history {
			age := time.Since(snap.Timestamp).Minutes()
			xs = append(xs, -age)
			ys = append(ys, getter(snap))
			_ = i
		}
	}

	if len(xs) < 2 {
		return 0
	}

	// Simple linear regression: y = a + b*x
	// Возвращаем b (наклон) — изменение в минуту
	n := float64(len(xs))
	var sumX, sumY, sumXY, sumX2 float64
	for i := range xs {
		sumX += xs[i]
		sumY += ys[i]
		sumXY += xs[i] * ys[i]
		sumX2 += xs[i] * xs[i]
	}

	denominator := n*sumX2 - sumX*sumX
	if denominator == 0 {
		return 0
	}

	slope := (n*sumXY - sumX*sumY) / denominator

	// slope — это изменение за минуту (т.к. x в минутах)
	return slope
}

// timeToThreshold - время до достижения порога (в секундах)
func (pr *Predictor) timeToThreshold(current, threshold, trendPerMinute float64) float64 {
	if trendPerMinute <= 0 {
		return math.Inf(1)
	}
	diff := threshold - current
	if diff <= 0 {
		return 0
	}
	minutes := diff / trendPerMinute
	seconds := minutes * 60
	if seconds < 0 {
		return 0
	}
	return seconds
}

// lastValue - последнее значение из истории
func (pr *Predictor) lastValue(history []types.MetricsSnapshot, getter func(types.MetricsSnapshot) float64) float64 {
	if len(history) == 0 {
		return 0
	}
	return getter(history[len(history)-1])
}

// FormatDuration - форматирование секунд в человекочитаемый вид
func FormatDuration(seconds float64) string {
	if seconds < 0 {
		return "∞"
	}
	if math.IsInf(seconds, 1) {
		return "∞"
	}

	d := time.Duration(seconds) * time.Second
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	if d < time.Hour {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}