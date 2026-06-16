package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// histogramCollector — простой in-process сборщик гистограммных метрик
// для экспорта через /api/metrics. Используется вместо внешнего Prometheus SDK.
type histogramCollector struct {
	mu      sync.Mutex
	buckets []float64
	values  []float64
	count   int64
	sum     float64
}

func newHistogramCollector(buckets ...float64) *histogramCollector {
	if len(buckets) == 0 {
		buckets = []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}
	}
	return &histogramCollector{
		buckets: buckets,
		values:  make([]float64, len(buckets)),
	}
}

func (h *histogramCollector) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	h.sum += v
	for i, b := range h.buckets {
		if v <= b {
			h.values[i]++
		}
	}
}

func (h *histogramCollector) Snapshot() map[string]interface{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return map[string]interface{}{
		"count":   h.count,
		"sum_sec": h.sum,
		"buckets": h.buckets,
		"values":  h.values,
	}
}

// BalancerMetrics — бизнес-метрики балансера (для последующей регистрации в Prometheus)
// Используется для экспозиции через /api/metrics

type BalancerMetrics struct {
	proxy *Proxy
}

// NewBalancerMetrics — создание сборщика метрик
func NewBalancerMetrics(proxy *Proxy) *BalancerMetrics {
	return &BalancerMetrics{proxy: proxy}
}

// GetMetrics — сбор всех метрик в map для JSON-ответа
func (bm *BalancerMetrics) GetMetrics() map[string]interface{} {
	bm.proxy.mu.RLock()
	totalBackends := len(bm.proxy.backends)
	healthyCount := 0
	for _, state := range bm.proxy.backends {
		if state.Backend.Status == types.StatusHealthy {
			healthyCount++
		}
	}
	bm.proxy.mu.RUnlock()

	queueDepth := len(bm.proxy.queueMgr.queue)
	queueMax := bm.proxy.queueMgr.maxSize
	queueFillPct := 0.0
	if queueMax > 0 {
		queueFillPct = float64(queueDepth) / float64(queueMax) * 100
	}

	// Подсчёт prewarm trigger count
	prewarmCount := 0
	bm.proxy.mu.RLock()
	for _, state := range bm.proxy.backends {
		state.mu.Lock()
		prewarmCount += len(state.WarmingUpModels)
		state.mu.Unlock()
	}
	bm.proxy.mu.RUnlock()

	// Подсчёт model instances
	modelInstanceCounts := make(map[string]int)
	bm.proxy.mu.RLock()
	for _, state := range bm.proxy.backends {
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		bm.proxy.metricsMgr.mu.RLock()
		metrics, ok := bm.proxy.metricsMgr.metrics[state.Backend.ID]
		bm.proxy.metricsMgr.mu.RUnlock()
		if !ok {
			continue
		}
		for _, m := range metrics.Ollama.RunningModels {
			modelInstanceCounts[m.Name]++
		}
	}
	bm.proxy.mu.RUnlock()

	result := map[string]interface{}{
		"timestamp":                  time.Now().UTC().Format(time.RFC3339),
		"total_backends":             totalBackends,
		"healthy_backends":           healthyCount,
		"request_queue_depth":        queueDepth,
		"request_queue_max":          queueMax,
		"request_queue_fill_percent": queueFillPct,
		"prewarm_in_progress":        prewarmCount,
		"model_instance_count":       modelInstanceCounts,
		"total_proxy_requests":       bm.proxy.totalRequests,
		"model_load_time_histogram":  modelLoadTimeHistogram.Snapshot(),
		"queue_wait_time_histogram":  queueWaitTimeHistogram.Snapshot(),
	}

	// ==== n_ctx auto-reload метрики (Stage 5) ====
	// nctxReload.Snapshot() возвращает:
	//   nctx_reloads_total, nctx_rejects_total, nctx_errors_total,
	//   nctx_reload_duration_ms_avg/sum/count, nctx_per_backend
	// Если координатор не инициализирован (p.nctxReload == nil) — Snapshot()
	// возвращает map с дефолтными нулями (см. nctx_reload.go).
	if bm.proxy.nctxReload != nil {
		nctxSnap := bm.proxy.nctxReload.Snapshot()
		// Копируем поля в плоский top-level (для совместимости с Prometheus exporters
		// которые сканият map через рефлексию; вложенные map тоже работают но плоский
		// формат удобнее для отладки).
		for _, key := range []string{
			"nctx_reloads_total",
			"nctx_rejects_total",
			"nctx_errors_total",
			"nctx_reload_duration_ms_avg",
			"nctx_reload_duration_ms_sum",
			"nctx_reload_duration_count",
		} {
			if v, ok := nctxSnap[key]; ok {
				result[key] = v
			}
		}
		// per-backend оставляем вложенным (может содержать много бэкендов)
		if pb, ok := nctxSnap["nctx_per_backend"]; ok {
			result["nctx_per_backend"] = pb
		}
	}

	return result
}

// modelLoadTimeHistogram — гистограмма времени загрузки модели (секунды)
var modelLoadTimeHistogram = newHistogramCollector(0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600)

// queueWaitTimeHistogram — гистограмма времени ожидания в очереди (секунды)
var queueWaitTimeHistogram = newHistogramCollector(0.001, 0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60)

// RecordModelLoadTime — запись времени загрузки модели (вызывается из warmupModel)
func (p *Proxy) RecordModelLoadTime(backendID, model string, duration time.Duration) {
	logger.Get().Infow("model load completed",
		"backend", backendID,
		"model", model,
		"load_time_sec", duration.Seconds(),
	)
	modelLoadTimeHistogram.Observe(duration.Seconds())
}

// RecordQueueWaitTime — запись времени ожидания в очереди (вызывается из QueueManager.worker)
func (qm *QueueManager) RecordQueueWaitTime(model string, waitTimeMs int64) {
	logger.Get().Debugw("queue wait time recorded",
		"model", model,
		"wait_time_ms", waitTimeMs,
	)
	queueWaitTimeHistogram.Observe(float64(waitTimeMs) / 1000)
}

// GetHistogramMetrics возвращает снапшоты гистограмм для /api/metrics
func GetHistogramMetrics() map[string]interface{} {
	return map[string]interface{}{
		"model_load_time_histogram": modelLoadTimeHistogram.Snapshot(),
		"queue_wait_time_histogram": queueWaitTimeHistogram.Snapshot(),
	}
}
