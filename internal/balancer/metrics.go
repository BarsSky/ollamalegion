package balancer

import (
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

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

	return map[string]interface{}{
		"timestamp":                   time.Now().UTC().Format(time.RFC3339),
		"total_backends":              totalBackends,
		"healthy_backends":            healthyCount,
		"request_queue_depth":         queueDepth,
		"request_queue_max":           queueMax,
		"request_queue_fill_percent":  queueFillPct,
		"prewarm_in_progress":         prewarmCount,
		"model_instance_count":        modelInstanceCounts,
		"total_proxy_requests":        bm.proxy.totalRequests,
	}
}

// RecordModelLoadTime — запись времени загрузки модели (вызывается из warmupModel)
func (p *Proxy) RecordModelLoadTime(backendID, model string, duration time.Duration) {
	logger.Get().Infow("model load completed",
		"backend", backendID,
		"model", model,
		"load_time_sec", duration.Seconds(),
	)
	// TODO: register in Prometheus counter
	// modelLoadTimeHistogram.WithLabelValues(backendID, model).Observe(duration.Seconds())
}

// RecordQueueWaitTime — запись времени ожидания в очереди (вызывается из QueueManager.worker)
func (qm *QueueManager) RecordQueueWaitTime(model string, waitTimeMs int64) {
	logger.Get().Debugw("queue wait time recorded",
		"model", model,
		"wait_time_ms", waitTimeMs,
	)
	// TODO: register in Prometheus histogram
	// queueWaitTimeHistogram.WithLabelValues(model).Observe(float64(waitTimeMs) / 1000)
}