// Package cppbackend — метрики CppBackend
package cppbackend

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics — метрики работы CppBackend
type Metrics struct {
	// Счётчики запросов
	TotalRequests      atomic.Int64 `json:"totalRequests"`
	TotalTokens        atomic.Int64 `json:"totalTokens"`
	TotalErrors        atomic.Int64 `json:"totalErrors"`
	ActiveRequests     atomic.Int64 `json:"activeRequests"`

	// Счётчики по моделям
	ModelRequests      sync.Map `json:"-"` // map[string]*ModelMetrics

	// Временные метрики
	RequestDurations   []time.Duration `json:"-"` // rolling window
	TokenLatencyMs     atomic.Int64    `json:"tokenLatencyMs"` // средняя задержка токена

	// Загрузка
	TotalModelsLoaded  atomic.Int64 `json:"totalModelsLoaded"`
	TotalModelsUnloaded atomic.Int64 `json:"totalModelsUnloaded"`
	ActiveModels       atomic.Int64 `json:"activeModels"`
	// Round 16 code-review fix (2026-07-30): UnloadModel timeout counter.
	// Инкрементится когда BatchedScheduler.Run() не возвращается в течение
	// 10s timeout при UnloadModel — указывает на баг в scheduler (deadlock,
	// infinite loop, blocked channel). Если этот счётчик растёт в production —
	// нужна postmortem.
	TotalUnloadTimeouts atomic.Int64 `json:"totalUnloadTimeouts"`

	// Round 17 Layer 3 (2026-07-31): reasoning auto-detect counter.
	// Инкрементится когда парсер видит <think> в output модели с
	// reasoningEnabled=false и auto-включает routing. Рост счётчика
	// указывает на (а) новую reasoning-модель вне whitelist, или
	// (б) систематическую проблему с пользовательским workflow
	// (забывают ставить enableReasoning=true при load).
	ReasoningAutoEnables atomic.Int64 `json:"reasoningAutoEnables"`

	// GPU
	GPUMemoryUsedMB    atomic.Int64 `json:"gpuMemoryUsedMb"`
	GPUMemoryTotalMB   atomic.Int64 `json:"gpuMemoryTotalMb"`

	startTime            time.Time
	mu                   sync.Mutex
	maxDurationSamples   int
}

// ModelMetrics — метрики по конкретной модели
type ModelMetrics struct {
	Name          string       `json:"name"`
	Requests      atomic.Int64 `json:"requests"`
	Tokens        atomic.Int64 `json:"tokens"`
	Errors        atomic.Int64 `json:"errors"`
	TotalDuration atomic.Int64 `json:"totalDurationMs"`
	FirstLoadedAt time.Time    `json:"firstLoadedAt"`
	LastUsedAt    atomic.Value `json:"-"` // time.Time

	// Round 18 P1.4 (2026-08-04): per-model ring buffer последних 256 duration_ms.
	// Нужен для p50/p95/p99 latency percentiles (нужно в /api/infer/metrics).
	// Сэмплов сортируются под lock при ComputePercentiles — O(N log N) на сэмпл
	// обычно < 1ms (256 элементов). Если N=256 не хватит для p99 precision —
	// увеличить (constant). Сейчас размер — константа для lock-free инициализации.
	sampleMu     sync.Mutex
	durations   [256]int64 // ms
	durationsIdx int      // next write position
	durationsLen int      // valid count (0..256)
}

// NewMetrics создаёт новый Metrics
func NewMetrics() *Metrics {
	return &Metrics{
		startTime:          time.Now(),
		maxDurationSamples: 1000,
	}
}

// RecordRequest фиксирует запрос
func (m *Metrics) RecordRequest(modelName string, tokens int, duration time.Duration, success bool) {
	m.TotalRequests.Add(1)
	m.TotalTokens.Add(int64(tokens))
	if !success {
		m.TotalErrors.Add(1)
	}

	// Обновляем метрики модели
	modelMetrics := m.getOrCreateModelMetrics(modelName)
	modelMetrics.Requests.Add(1)
	modelMetrics.Tokens.Add(int64(tokens))
	modelMetrics.TotalDuration.Add(duration.Milliseconds())
	if !success {
		modelMetrics.Errors.Add(1)
	}
	modelMetrics.LastUsedAt.Store(time.Now())

	// Round 18 P1.4: ring buffer для percentiles. 0-duration (errors) — пропускаем,
	// чтобы не skew p50/p95 вниз (иначе 0 latency у 50% запросов).
	if duration > 0 {
		modelMetrics.recordDuration(duration.Milliseconds())
	}

	// Rolling window длительности
	m.mu.Lock()
	m.RequestDurations = append(m.RequestDurations, duration)
	if len(m.RequestDurations) > m.maxDurationSamples {
		m.RequestDurations = m.RequestDurations[len(m.RequestDurations)-m.maxDurationSamples:]
	}
	m.mu.Unlock()

	// Средняя задержка токена
	if tokens > 0 {
		latencyMs := duration.Milliseconds() / int64(tokens)
		m.TokenLatencyMs.Store(latencyMs)
	}
}

// RecordLoad фиксирует загрузку модели
func (m *Metrics) RecordLoad(modelName string) {
	m.TotalModelsLoaded.Add(1)
	m.ActiveModels.Add(1)
	m.getOrCreateModelMetrics(modelName)
}

// RecordUnload фиксирует выгрузку модели
func (m *Metrics) RecordUnload(modelName string) {
	m.TotalModelsUnloaded.Add(1)
	m.ActiveModels.Add(-1)
}

// RecordUnloadTimeout — Round 16 code-review fix: фиксирует случай, когда
// BatchedScheduler.Run() не вернулся в течение timeout при UnloadModel.
// reason — категория причины ("batched_scheduler_stuck", "inference_loop",
// и т.п.) для диагностики.
func (m *Metrics) RecordUnloadTimeout(modelName, reason string) {
	m.TotalUnloadTimeouts.Add(1)
}

// RecordReasoningAutoEnable — Round 17 Layer 3: фиксирует случай, когда
// парсер lazy-auto-detect reasoning в output модели. modelName — для
// логирования, счётчик в ReasoningAutoEnables — глобальный (по всем моделям).
// Если счётчик быстро растёт — оператор должен либо добавить новую модель
// в IsReasoningModel whitelist, либо рекомендовать пользователю
// выставить enableReasoning=true при load-with-params.
func (m *Metrics) RecordReasoningAutoEnable(modelName string) {
	m.ReasoningAutoEnables.Add(1)
}

// getOrCreateModelMetrics возвращает или создаёт метрики для модели
func (m *Metrics) getOrCreateModelMetrics(name string) *ModelMetrics {
	val, _ := m.ModelRequests.LoadOrStore(name, &ModelMetrics{
		Name:          name,
		FirstLoadedAt: time.Now(),
	})
	mm := val.(*ModelMetrics)
	mm.LastUsedAt.Store(time.Now())
	return mm
}

// GetModelMetricsList возвращает список метрик всех моделей
func (m *Metrics) GetModelMetricsList() []*ModelMetrics {
	var result []*ModelMetrics
	m.ModelRequests.Range(func(key, value interface{}) bool {
		result = append(result, value.(*ModelMetrics))
		return true
	})
	return result
}

// recordDuration добавляет sample в per-model ring buffer (Round 18 P1.4).
//
// Семантика: ring buffer последних 256 samples. Lock-free write per slot.
// Сортировка при ComputePercentiles. Сэмплы с duration=0 не пишутся
// (вызывающий код RecordRequest уже фильтрует).
func (mm *ModelMetrics) recordDuration(ms int64) {
	mm.sampleMu.Lock()
	mm.durations[mm.durationsIdx] = ms
	mm.durationsIdx = (mm.durationsIdx + 1) % len(mm.durations)
	if mm.durationsLen < len(mm.durations) {
		mm.durationsLen++
	}
	mm.sampleMu.Unlock()
}

// Percentiles возвращает p50/p95/p99 latency (ms) и размер выборки.
//
// Round 18 P1.4. Используется в /api/infer/metrics.
//
// Семантика:
//   - p50/p95/p99 = nearest-rank method (sorted[len * p / 100])
//   - Возвращает 0 для всех если сэмплов < 1
//   - count = текущее количество сэмплов (0..256)
//   - Sorted snapshot под lock — O(N log N), N <= 256 обычно < 1ms
func (mm *ModelMetrics) Percentiles() (p50, p95, p99 int64, count int) {
	mm.sampleMu.Lock()
	if mm.durationsLen == 0 {
		mm.sampleMu.Unlock()
		return 0, 0, 0, 0
	}
	// Copy под lock (чтобы не держать lock во время sort).
	sorted := make([]int64, mm.durationsLen)
	copy(sorted, mm.durations[:mm.durationsLen])
	mm.sampleMu.Unlock()

	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	p50 = sorted[len(sorted)*50/100]
	p95 = sorted[len(sorted)*95/100]
	p99 = sorted[len(sorted)*99/100]
	return p50, p95, p99, len(sorted)
}

// GetModelMetricsSnapshot — Round 18 P1.4. JSON-ready per-model stats для
// /api/infer/metrics endpoint.
//
// Returns map[modelName] → ModelMetricsSnapshotJSON. nil-safe.
func (m *Metrics) GetModelMetricsSnapshot() map[string]ModelMetricsSnapshotJSON {
	if m == nil {
		return map[string]ModelMetricsSnapshotJSON{}
	}
	out := make(map[string]ModelMetricsSnapshotJSON)
	m.ModelRequests.Range(func(key, value interface{}) bool {
		name := key.(string)
		mm := value.(*ModelMetrics)
		requests := mm.Requests.Load()
		errors := mm.Errors.Load()
		var errorRate float64
		if requests > 0 {
			errorRate = float64(errors) / float64(requests)
		}
		var avgMs int64
		if requests > 0 {
			avgMs = mm.TotalDuration.Load() / requests
		}
		p50, p95, p99, _ := mm.Percentiles()
		var lastUsedAt string
		if v := mm.LastUsedAt.Load(); v != nil {
			if t, ok := v.(time.Time); ok {
				lastUsedAt = t.Format(time.RFC3339Nano)
			}
		}
		out[name] = ModelMetricsSnapshotJSON{
			Name:        name,
			Requests:    requests,
			Errors:      errors,
			ErrorRate:   errorRate,
			Tokens:      mm.Tokens.Load(),
			AvgMs:       avgMs,
			P50Ms:       p50,
			P95Ms:       p95,
			P99Ms:       p99,
			LastUsedAt:  lastUsedAt,
		}
		return true
	})
	return out
}

// ModelMetricsSnapshotJSON — Round 18 P1.4. JSON shape для /api/infer/metrics.
type ModelMetricsSnapshotJSON struct {
	Name       string  `json:"name"`
	Requests   int64   `json:"requests"`
	Errors     int64   `json:"errors"`
	ErrorRate  float64 `json:"error_rate"`
	Tokens     int64   `json:"tokens"`
	AvgMs      int64   `json:"avg_ms"`
	P50Ms      int64   `json:"p50_ms"`
	P95Ms      int64   `json:"p95_ms"`
	P99Ms      int64   `json:"p99_ms"`
	LastUsedAt string  `json:"last_used_at,omitempty"`
}

// GetMetricsSnapshot возвращает снимок метрик
func (m *Metrics) GetMetricsSnapshot() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	totalDurationMs := int64(0)
	for _, d := range m.RequestDurations {
		totalDurationMs += d.Milliseconds()
	}
	avgDurationMs := int64(0)
	if len(m.RequestDurations) > 0 {
		avgDurationMs = totalDurationMs / int64(len(m.RequestDurations))
	}

	return map[string]interface{}{
		"uptimeSeconds":    time.Since(m.startTime).Seconds(),
		"totalRequests":    m.TotalRequests.Load(),
		"totalTokens":      m.TotalTokens.Load(),
		"totalErrors":      m.TotalErrors.Load(),
		"activeRequests":   m.ActiveRequests.Load(),
		"activeModels":     m.ActiveModels.Load(),
		"avgDurationMs":    avgDurationMs,
		"tokenLatencyMs":   m.TokenLatencyMs.Load(),
		"modelsLoaded":     m.TotalModelsLoaded.Load(),
		"modelsUnloaded":   m.TotalModelsUnloaded.Load(),
		"gpuMemoryUsedMb":  m.GPUMemoryUsedMB.Load(),
		"gpuMemoryTotalMb": m.GPUMemoryTotalMB.Load(),
	}
}
