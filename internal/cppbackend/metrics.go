// Package cppbackend — метрики CppBackend
package cppbackend

import (
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

	// GPU
	GPUMemoryUsedMB    atomic.Int64 `json:"gpuMemoryUsedMb"`
	GPUMemoryTotalMB   atomic.Int64 `json:"gpuMemoryTotalMb"`

	startTime            time.Time
	mu                   sync.Mutex
	maxDurationSamples   int
}

// ModelMetrics — метрики по конкретной модели
type ModelMetrics struct {
	Name           string        `json:"name"`
	Requests       atomic.Int64  `json:"requests"`
	Tokens         atomic.Int64  `json:"tokens"`
	Errors         atomic.Int64  `json:"errors"`
	TotalDuration  atomic.Int64  `json:"totalDurationMs"`
	FirstLoadedAt  time.Time     `json:"firstLoadedAt"`
	LastUsedAt     atomic.Value  `json:"-"` // time.Time
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
