package rpcworker

import (
	"net/http"
	"sync/atomic"
	"time"
)

// Metrics — счётчики worker'а для эндпоинта /rpc/metrics.
type Metrics struct {
	startedAt time.Time

	// Общие счётчики
	totalInfer   atomic.Int64
	totalErrors  atomic.Int64
	totalLoadOK  atomic.Int64
	totalLoadErr atomic.Int64

	// Текущее состояние
	activeRequests atomic.Int64
	loadedSlices   atomic.Int64

	// Per-model
	perModelInfer  *CounterMap
	perModelErrors *CounterMap

	// Latency
	totalLatencyMs atomic.Int64
}

// NewMetrics создаёт новый metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		startedAt:      time.Now(),
		perModelInfer:  NewCounterMap(),
		perModelErrors: NewCounterMap(),
	}
}

// OnInferStart увеличивает activeRequests. Возвращает функцию OnInferEnd.
func (m *Metrics) OnInferStart(model string) func(tokens int, latencyMs int64, err error) {
	m.activeRequests.Add(1)
	m.totalInfer.Add(1)
	m.perModelInfer.Inc(model)
	start := time.Now()
	return func(tokens int, latencyMs int64, err error) {
		elapsed := latencyMs
		if elapsed == 0 {
			elapsed = time.Since(start).Milliseconds()
		}
		m.activeRequests.Add(-1)
		m.totalLatencyMs.Add(elapsed)
		if err != nil {
			m.totalErrors.Add(1)
			m.perModelErrors.Inc(model)
		}
	}
}

// OnLoadOk увеличивает счётчик успешных загрузок.
func (m *Metrics) OnLoadOk() {
	m.totalLoadOK.Add(1)
	m.loadedSlices.Add(1)
}

// OnLoadErr увеличивает счётчик ошибок загрузки.
func (m *Metrics) OnLoadErr() {
	m.totalLoadErr.Add(1)
}

// OnUnload уменьшает loadedSlices.
func (m *Metrics) OnUnload() {
	if m.loadedSlices.Load() > 0 {
		m.loadedSlices.Add(-1)
	}
}

// Snapshot возвращает текущий снимок метрик.
func (m *Metrics) Snapshot() map[string]interface{} {
	total := m.totalInfer.Load()
	avgLatency := int64(0)
	if total > 0 {
		avgLatency = m.totalLatencyMs.Load() / total
	}
	return map[string]interface{}{
		"active_requests":  m.activeRequests.Load(),
		"loaded_slices":    m.loadedSlices.Load(),
		"total_infer":      total,
		"total_errors":     m.totalErrors.Load(),
		"total_load_ok":    m.totalLoadOK.Load(),
		"total_load_err":   m.totalLoadErr.Load(),
		"avg_latency_ms":   avgLatency,
		"uptime_s":         int64(time.Since(m.startedAt).Seconds()),
		"per_model_infer":  m.perModelInfer.Snapshot(),
		"per_model_errors": m.perModelErrors.Snapshot(),
	}
}

// handleMetrics — GET /rpc/metrics.
//
// WorkerClient.GetMetrics ожидает JSON с произвольной структурой.
func (s *WorkerServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	snap := s.metrics.Snapshot()
	snap["worker_id"] = s.cfg.WorkerID
	snap["stub_mode"] = s.cfg.StubMode
	snap["gpu_usage"] = 0.0 // B1 stub
	snap["vram_usage"] = 0 // B1 stub
	snap["active_jobs"] = s.metrics.activeRequests.Load()
	writeJSON(w, http.StatusOK, snap)
}
