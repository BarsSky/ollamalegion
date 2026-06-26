// Package rpccoordinator — B7: Prometheus-style metrics aggregation.
//
// Простой in-memory aggregator, который накапливает метрики coordinator'а
// и экспортирует их в Prometheus exposition format (text/plain; version=0.0.4).
//
// Реализованы минимально необходимые типы:
//
//   - Counter   — монотонный счётчик (rpc_inference_total, rpc_inference_errors_total).
//   - Gauge     — текущее значение (rpc_active_jobs, rpc_worker_health, rpc_kv_cache_size_bytes).
//   - Histogram — распределение latency с фиксированными buckets.
//
// Не используем github.com/prometheus/client_golang (его нет в go.mod);
// экспортируем в нативном Prometheus format вручную.

package rpccoordinator

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =====================================================================
// Counter
// =====================================================================

// Counter — атомарный счётчик с монотонным инкрементом.
type Counter struct {
	v atomic.Int64
}

// Inc увеличивает на 1.
func (c *Counter) Inc() {
	c.v.Add(1)
}

// Add прибавляет delta.
func (c *Counter) Add(delta int64) {
	c.v.Add(delta)
}

// Value возвращает текущее значение.
func (c *Counter) Value() int64 {
	return c.v.Load()
}

// =====================================================================
// Gauge
// =====================================================================

// Gauge — текущее значение (может увеличиваться/уменьшаться).
type Gauge struct {
	v atomic.Int64
}

// Set устанавливает значение.
func (g *Gauge) Set(v int64) {
	g.v.Store(v)
}

// Inc увеличивает на 1.
func (g *Gauge) Inc() {
	g.v.Add(1)
}

// Dec уменьшает на 1.
func (g *Gauge) Dec() {
	g.v.Add(-1)
}

// Value возвращает текущее значение.
func (g *Gauge) Value() int64 {
	return g.v.Load()
}

// =====================================================================
// Histogram
// =====================================================================

// DefaultLatencyBuckets — стандартные buckets для latency в миллисекундах.
// Совпадают с prometheus.DefBuckets (в ms): 5, 10, 25, ..., 10000.
var DefaultLatencyBuckets = []float64{
	5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000,
}

// Histogram — распределение значений по фиксированным buckets.
type Histogram struct {
	mu       sync.Mutex
	buckets  []float64           // верхние границы buckets (sorted ascending)
	counts   []int64              // cumulative counts per bucket
	count    int64                // total observations
	sum      float64              // sum of all observed values
	infCount int64                // count of observations above largest bucket
}

// NewHistogram создаёт histogram с заданными buckets.
func NewHistogram(buckets []float64) *Histogram {
	cp := append([]float64(nil), buckets...)
	sort.Float64s(cp)
	return &Histogram{
		buckets: cp,
		counts:  make([]int64, len(cp)),
	}
}

// Observe записывает наблюдение.
func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	h.sum += v
	for i, ub := range h.buckets {
		if v <= ub {
			h.counts[i]++
		}
	}
	if v > h.buckets[len(h.buckets)-1] {
		h.infCount++
	}
}

// Snapshot возвращает копию buckets + counts (lock-free read).
type HistogramSnapshot struct {
	Buckets  []float64
	Counts   []int64 // cumulative per bucket
	Count    int64
	Sum      float64
	InfCount int64
}

// Snapshot возвращает копию для экспорта.
func (h *Histogram) Snapshot() HistogramSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return HistogramSnapshot{
		Buckets:  append([]float64(nil), h.buckets...),
		Counts:   append([]int64(nil), h.counts...),
		Count:    h.count,
		Sum:      h.sum,
		InfCount: h.infCount,
	}
}

// =====================================================================
// MetricsAggregator — корневой объект метрик coordinator.
// =====================================================================

// MetricsAggregator — namespace для всех метрик coordinator'а.
type MetricsAggregator struct {
	// Счётчики.
	InferenceTotal       *Counter // всего inference (включая failed)
	InferenceErrorsTotal *Counter // inference, завершившиеся с ошибкой
	SliceInferTotal      *Counter // всего slice infer вызовов
	SliceInferErrors     *Counter // slice infer с ошибкой
	LoadSliceTotal       *Counter
	UnloadSliceTotal     *Counter

	// Gauges.
	ActiveJobs         *Gauge
	RegisteredWorkers  *Gauge
	RegisteredModels   *Gauge
	KVCacheSizeBytes   *Gauge

	// Per-worker gauges (workerID → healthy gauge 0/1).
	workerHealth sync.Map // map[string]*Gauge

	// Histograms.
	InferenceDurationMs *Histogram // end-to-end latency в мс
	SliceLatencyMs      *Histogram // latency одного среза в мс

	startedAt time.Time
}

// NewMetricsAggregator создаёт новый aggregator с дефолтными buckets.
func NewMetricsAggregator() *MetricsAggregator {
	return &MetricsAggregator{
		InferenceTotal:       &Counter{},
		InferenceErrorsTotal: &Counter{},
		SliceInferTotal:      &Counter{},
		SliceInferErrors:     &Counter{},
		LoadSliceTotal:       &Counter{},
		UnloadSliceTotal:     &Counter{},

		ActiveJobs:        &Gauge{},
		RegisteredWorkers: &Gauge{},
		RegisteredModels:  &Gauge{},
		KVCacheSizeBytes:  &Gauge{},

		InferenceDurationMs: NewHistogram(DefaultLatencyBuckets),
		SliceLatencyMs:      NewHistogram(DefaultLatencyBuckets),

		startedAt: time.Now(),
	}
}

// WorkerHealthGauge возвращает (и создаёт при необходимости) gauge для worker'а.
func (m *MetricsAggregator) WorkerHealthGauge(workerID string) *Gauge {
	if g, ok := m.workerHealth.Load(workerID); ok {
		return g.(*Gauge)
	}
	g := &Gauge{}
	actual, _ := m.workerHealth.LoadOrStore(workerID, g)
	return actual.(*Gauge)
}

// SetWorkerHealth устанавливает значение health для worker'а (0/1).
func (m *MetricsAggregator) SetWorkerHealth(workerID string, healthy bool) {
	g := m.WorkerHealthGauge(workerID)
	if healthy {
		g.Set(1)
	} else {
		g.Set(0)
	}
}

// SetRegisteredWorkers устанавливает значение RegisteredWorkers gauge.
func (m *MetricsAggregator) SetRegisteredWorkers(n int64) {
	m.RegisteredWorkers.Set(n)
}

// SetRegisteredModels устанавливает значение RegisteredModels gauge.
func (m *MetricsAggregator) SetRegisteredModels(n int64) {
	m.RegisteredModels.Set(n)
}

// SetActiveJobs устанавливает значение ActiveJobs gauge.
func (m *MetricsAggregator) SetActiveJobs(n int64) {
	m.ActiveJobs.Set(n)
}

// IncInferenceTotal увеличивает счётчик успешных inference.
func (m *MetricsAggregator) IncInferenceTotal() { m.InferenceTotal.Inc() }

// IncInferenceErrors увеличивает счётчик failed inference.
func (m *MetricsAggregator) IncInferenceErrors() { m.InferenceErrorsTotal.Inc() }

// IncSliceInfer увеличивает счётчик slice infer.
func (m *MetricsAggregator) IncSliceInfer(errored bool) {
	m.SliceInferTotal.Inc()
	if errored {
		m.SliceInferErrors.Inc()
	}
}

// ObserveInferenceDuration записывает latency в гистограмму.
func (m *MetricsAggregator) ObserveInferenceDuration(ms float64) {
	m.InferenceDurationMs.Observe(ms)
}

// ObserveSliceLatency записывает latency одного среза в гистограмму.
func (m *MetricsAggregator) ObserveSliceLatency(ms float64) {
	m.SliceLatencyMs.Observe(ms)
}

// AllWorkerHealth возвращает список (workerID, healthy) для всех worker'ов.
func (m *MetricsAggregator) AllWorkerHealth() []WorkerHealth {
	out := []WorkerHealth{}
	m.workerHealth.Range(func(k, v interface{}) bool {
		g := v.(*Gauge)
		health := WorkerHealth{WorkerID: k.(string), Healthy: g.Value() == 1}
		out = append(out, health)
		return true
	})
	return out
}

// WorkerHealth — пара (workerID, healthy) для экспорта.
type WorkerHealth struct {
	WorkerID string
	Healthy  bool
}

// =====================================================================
// Prometheus exposition
// =====================================================================

// WritePrometheus экспортирует метрики в Prometheus text format (v0.0.4).
//
// Формат:
//
//	# HELP <metric> <description>
//	# TYPE <metric> <type>
//	<metric>{labels} <value>
//
// Согласно https://prometheus.io/docs/instrumenting/exposition_formats/#text-format-details.
func (m *MetricsAggregator) WritePrometheus(w io.Writer) error {
	// rpc_inference_total
	if err := writeCounter(w, "rpc_inference_total",
		"Total number of inference requests handled by coordinator.",
		m.InferenceTotal.Value()); err != nil {
		return err
	}
	// rpc_inference_errors_total
	if err := writeCounter(w, "rpc_inference_errors_total",
		"Total number of inference requests that failed.",
		m.InferenceErrorsTotal.Value()); err != nil {
		return err
	}
	// rpc_slice_infer_total
	if err := writeCounter(w, "rpc_slice_infer_total",
		"Total slice infer calls (across all workers).",
		m.SliceInferTotal.Value()); err != nil {
		return err
	}
	// rpc_slice_infer_errors_total
	if err := writeCounter(w, "rpc_slice_infer_errors_total",
		"Total slice infer calls that failed.",
		m.SliceInferErrors.Value()); err != nil {
		return err
	}
	// rpc_load_slice_total / rpc_unload_slice_total
	if err := writeCounter(w, "rpc_load_slice_total",
		"Total /rpc/load calls.",
		m.LoadSliceTotal.Value()); err != nil {
		return err
	}
	if err := writeCounter(w, "rpc_unload_slice_total",
		"Total /rpc/unload calls.",
		m.UnloadSliceTotal.Value()); err != nil {
		return err
	}

	// Gauges.
	if err := writeGauge(w, "rpc_active_jobs",
		"Number of in-flight inference jobs.",
		m.ActiveJobs.Value()); err != nil {
		return err
	}
	if err := writeGauge(w, "rpc_registered_workers",
		"Number of registered workers.",
		m.RegisteredWorkers.Value()); err != nil {
		return err
	}
	if err := writeGauge(w, "rpc_registered_models",
		"Number of registered distributed models.",
		m.RegisteredModels.Value()); err != nil {
		return err
	}
	if err := writeGauge(w, "rpc_kv_cache_size_bytes",
		"Estimated size of distributed KV cache (bytes, stub).",
		m.KVCacheSizeBytes.Value()); err != nil {
		return err
	}
	if err := writeGauge(w, "rpc_uptime_seconds",
		"Coordinator uptime in seconds.",
		int64(time.Since(m.startedAt).Seconds())); err != nil {
		return err
	}

	// Per-worker health gauges.
	for _, wh := range m.AllWorkerHealth() {
		v := int64(0)
		if wh.Healthy {
			v = 1
		}
		if err := writeGauge(w, "rpc_worker_health",
			"Worker health (1 = healthy, 0 = unhealthy).",
			v,
			fmt.Sprintf(`worker_id="%s"`, wh.WorkerID),
		); err != nil {
			return err
		}
	}

	// Histograms: rpc_inference_duration_ms + rpc_slice_latency_ms.
	if err := writeHistogram(w, "rpc_inference_duration_ms",
		"End-to-end inference duration in milliseconds.",
		m.InferenceDurationMs.Snapshot(),
	); err != nil {
		return err
	}
	if err := writeHistogram(w, "rpc_slice_latency_ms",
		"Per-slice infer latency in milliseconds.",
		m.SliceLatencyMs.Snapshot(),
	); err != nil {
		return err
	}

	return nil
}

// writeCounter пишет одну counter-метрику в Prometheus format.
func writeCounter(w io.Writer, name, help string, value int64) error {
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n",
		name, help, name, name, value); err != nil {
		return err
	}
	return nil
}

// writeGauge пишет одну gauge-метрику (с optional labels).
func writeGauge(w io.Writer, name, help string, value int64, labels ...string) error {
	labelStr := ""
	if len(labels) > 0 {
		labelStr = "{" + strings.Join(labels, ",") + "}"
	}
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s%s %d\n",
		name, help, name, name, labelStr, value); err != nil {
		return err
	}
	return nil
}

// writeHistogram пишет histogram в Prometheus format.
//
// Согласно спецификации, для каждого bucket выводится отдельная строка:
//
//	<name>_bucket{le="<bucket>"} <cumulative count>
//	<name>_bucket{le="+Inf"} <total count>
//	<name>_sum <sum>
//	<name>_count <count>
func writeHistogram(w io.Writer, name string, help string, snap HistogramSnapshot) error {
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n",
		name, help, name); err != nil {
		return err
	}
	for i, ub := range snap.Buckets {
		if _, err := fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n",
			name, ub, snap.Counts[i]); err != nil {
			return err
		}
	}
	// +Inf bucket — total count (cumulative + new observations beyond last bucket).
	if _, err := fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n",
		name, snap.Count+snap.InfCount); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s_sum %g\n", name, snap.Sum); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s_count %d\n", name, snap.Count); err != nil {
		return err
	}
	return nil
}