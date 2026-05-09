package balancer

import (
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// GetClusterState - получение состояния кластера
func (p *Proxy) GetClusterState() *types.ClusterState {
	p.mu.RLock()

	backendsCopy := make(map[string]*BackendState, len(p.backends))
	for id, state := range p.backends {
		backendsCopy[id] = state
	}

	p.mu.RUnlock()

	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()

	state := &types.ClusterState{
		Timestamp:       time.Now().UTC(),
		TotalBackends:   len(backendsCopy),
		HealthyBackends: 0,
		Backends:        make([]types.BackendMetrics, 0, len(backendsCopy)),
	}

	state.TotalRequests = atomic.LoadInt64(&p.totalRequests)

	for id, backendState := range backendsCopy {
		backendState.mu.Lock()
		status := backendState.Backend.Status
		hasAgent := backendState.Backend.HasAgent
		prediction := backendState.Prediction
		backendState.mu.Unlock()

		if status == types.StatusHealthy {
			state.HealthyBackends++
		}

		backendConfig := *backendState.Backend

		// Приоритет: RuntimeMaxConcurrentRequests > MaxConcurrentReqs
		maxConcurrent := backendConfig.MaxConcurrentReqs
		if backendConfig.RuntimeMaxConcurrentRequests > 0 {
			maxConcurrent = backendConfig.RuntimeMaxConcurrentRequests
		}
		metrics := types.BackendMetrics{
			ID:        id,
			Timestamp: time.Now().UTC(),
			Status:    status,
			HasAgent:  hasAgent,
			Host:      backendConfig.Host,
			OllamaPort: backendConfig.OllamaPort,
			GPU:       types.GPUMetrics{},
			System:    types.SystemMetrics{},
			Ollama:    types.OllamaMetrics{RunningModels: []types.RunningModel{}},
			MaxConcurrentRequests: maxConcurrent,
		}

		if agentMetrics, ok := p.metricsMgr.metrics[id]; ok {
			metrics = *agentMetrics
			metrics.Status = status
			metrics.HasAgent = hasAgent
			metrics.Prediction = prediction
			metrics.MaxConcurrentRequests = maxConcurrent
			metrics.Models = make([]string, 0, len(metrics.Ollama.RunningModels))
			for _, m := range metrics.Ollama.RunningModels {
				metrics.Models = append(metrics.Models, m.Name)
			}
			if metrics.GPU.MemoryTotal > 0 {
				metrics.VRAMUsagePercent = float64(metrics.GPU.MemoryUsed) / float64(metrics.GPU.MemoryTotal) * 100
				metrics.VRAMTotalGB = float64(metrics.GPU.MemoryTotal) / 1024.0
				metrics.VRAMUsedGB = float64(metrics.GPU.MemoryUsed) / 1024.0
			}
			if metrics.System.MemoryTotal > 0 {
				metrics.MemoryUsagePercent = float64(metrics.System.MemoryUsed) / float64(metrics.System.MemoryTotal) * 100
			}
			state.ActiveRequests += agentMetrics.Ollama.ActiveRequests
			backendState.mu.Lock()
			if backendState.CalculatedRPS > 0 {
				state.RPS += backendState.CalculatedRPS
				metrics.Ollama.RequestsPerSecond = backendState.CalculatedRPS
			} else {
				state.RPS += agentMetrics.Ollama.RequestsPerSecond
			}
			metrics.Ollama.TotalRequests = backendState.TotalRequests
			backendState.mu.Unlock()
			state.TotalGPUUsage += agentMetrics.GPU.UsagePercent
		} else {
			metrics.MaxConcurrentRequests = backendConfig.MaxConcurrentReqs
			backendState.mu.Lock()
			metrics.Ollama.TotalRequests = backendState.TotalRequests
			backendState.mu.Unlock()
		}

		state.Backends = append(state.Backends, metrics)
	}

	return state
}

// GetQueueStats - получение статистики очереди
func (p *Proxy) GetQueueStats() QueueStats {
	p.queueMgr.mu.Lock()
	processed := p.queueMgr.processed
	workers := p.queueMgr.numWorkers
	timeout := p.queueMgr.timeout
	p.queueMgr.mu.Unlock()

	// Вычисляем среднее время ожидания из истории завершённых запросов
	var avgWaitMs int64
	p.queueMgr.historyMu.RLock()
	if len(p.queueMgr.completedHistory) > 0 {
		// Берём последние 100 записей для актуального среднего
		history := p.queueMgr.completedHistory
		start := 0
		if len(history) > 100 {
			start = len(history) - 100
		}
		var totalMs int64
		count := 0
		for i := start; i < len(history); i++ {
			totalMs += history[i].WaitTimeMs
			count++
		}
		if count > 0 {
			avgWaitMs = totalMs / int64(count)
		}
	}
	p.queueMgr.historyMu.RUnlock()

	return QueueStats{
		CurrentSize:        len(p.queueMgr.queue),
		MaxSize:            p.queueMgr.maxSize,
		Processed:          processed,
		WaitTimeAvgMs:      avgWaitMs,
		Workers:            workers,
		TimeoutSec:         int(timeout.Seconds()),
		DispatchByAffinity: atomic.LoadInt64(&p.queueMgr.dispatchAffinity),
		DispatchByLoad:     atomic.LoadInt64(&p.queueMgr.dispatchLoad),
		DispatchByConfig:   atomic.LoadInt64(&p.queueMgr.dispatchConfig),
	}
}

// GetQueueHistory - получение истории выполненных запросов
func (p *Proxy) GetQueueHistory() []map[string]interface{} {
	p.queueMgr.historyMu.RLock()
	defer p.queueMgr.historyMu.RUnlock()

	result := make([]map[string]interface{}, 0, len(p.queueMgr.completedHistory))
	for _, req := range p.queueMgr.completedHistory {
		result = append(result, map[string]interface{}{
			"model":        req.Model,
			"target":       req.Target,
			"enqueued":     req.Enqueued.UTC().Format(time.RFC3339),
			"completed_at": req.CompletedAt.UTC().Format(time.RFC3339),
			"wait_time_ms": req.WaitTimeMs,
		})
	}
	return result
}

// GetQueuePendingRequests — получение списка ожидающих запросов в очереди
func (p *Proxy) GetQueuePendingRequests() []map[string]interface{} {
	return p.queueMgr.getPendingDTOs()
}

// GetQueueProcessingRequests — получение списка обрабатываемых запросов из очереди
func (p *Proxy) GetQueueProcessingRequests() []map[string]interface{} {
	return p.queueMgr.getProcessingDTOs()
}

// GetDirectProcessingRequests — получение списка прямых запросов (не через очередь),
// обрабатываемых напрямую на бэкендах. Это позволяет монитору показывать полную картину.
func (p *Proxy) GetDirectProcessingRequests() []map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]map[string]interface{}, 0)
	now := time.Now()

	for id, state := range p.backends {
		state.mu.Lock()
		active := state.ActiveReqs
		state.mu.Unlock()

		if active > 0 {
			// Получаем модель из метрик если доступна
			model := "-"
			p.metricsMgr.mu.RLock()
			if m, ok := p.metricsMgr.metrics[id]; ok && len(m.Ollama.RunningModels) > 0 {
				model = m.Ollama.RunningModels[0].Name
			}
			p.metricsMgr.mu.RUnlock()

			for i := 0; i < active; i++ {
				result = append(result, map[string]interface{}{
					"model":      model,
					"enqueued":   now.UTC().Format(time.RFC3339),
					"waitTimeMs": 0,
					"target":     id,
					"status":     "processing",
					"source":     "direct",
				})
			}
		}
	}
	return result
}

// GetDispatchStats - получение агрегированных dispatch-метрик
func (p *Proxy) GetDispatchStats() map[string]int64 {
	return map[string]int64{
		"dispatch_by_affinity": atomic.LoadInt64(&p.queueMgr.dispatchAffinity),
		"dispatch_by_load":     atomic.LoadInt64(&p.queueMgr.dispatchLoad),
		"dispatch_by_config":   atomic.LoadInt64(&p.queueMgr.dispatchConfig),
	}
}

// ResetDispatchCounters - сброс dispatch counters (для ротации метрик)
func (p *Proxy) ResetDispatchCounters() {
	atomic.StoreInt64(&p.queueMgr.dispatchAffinity, 0)
	atomic.StoreInt64(&p.queueMgr.dispatchLoad, 0)
	atomic.StoreInt64(&p.queueMgr.dispatchConfig, 0)
}

// GetSessions - получение всех сессий
func (p *Proxy) GetSessions() []*types.Session {
	return p.sessionMgr.GetAll()
}

// DeleteSession - удаление сессии
func (p *Proxy) DeleteSession(id string) bool {
	return p.sessionMgr.Delete(id)
}

// ClearSessions - очистка всех сессий
func (p *Proxy) ClearSessions() {
	p.sessionMgr.Clear()
}

// GetPrediction - получение прогноза для бэкенда
func (p *Proxy) GetPrediction(backendID string) types.Prediction {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if state, ok := p.backends[backendID]; ok {
		return state.Prediction
	}
	return types.Prediction{
		SecondsToCritical: -1,
		CriticalReason:      "none",
	}
}

// SubscribeEvents — подписка на события, возвращает канал и ID подписки
func (p *Proxy) SubscribeEvents() (string, <-chan types.Event) {
	return p.eventBus.Subscribe()
}

// UnsubscribeEvents — отписка от событий
func (p *Proxy) UnsubscribeEvents(id string) {
	p.eventBus.Unsubscribe(id)
}

// PublishEvent — публикация события всем подписчикам (неблокирующая)
func (p *Proxy) PublishEvent(ev types.Event) {
	p.eventBus.Publish(ev)
}

// SetUnloadScheduler — установка планировщика выгрузки моделей
func (p *Proxy) SetUnloadScheduler(us *UnloadScheduler) {
	p.unloadScheduler = us
}

// SetWeightTuner — установка адаптивного тюнера весов
func (p *Proxy) SetWeightTuner(wt *AdaptiveWeightTuner) {
	p.weightTuner = wt
}

// GetUnloadScheduler — получение планировщика выгрузки
func (p *Proxy) GetUnloadScheduler() *UnloadScheduler {
	return p.unloadScheduler
}

// GetWeightTuner — получение адаптивного тюнера весов
func (p *Proxy) GetWeightTuner() *AdaptiveWeightTuner {
	return p.weightTuner
}