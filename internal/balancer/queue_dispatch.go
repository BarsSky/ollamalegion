package balancer

import (
	"context"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// DispatchResult - результат dispatch-операции
type DispatchResult struct {
	BackendID string
	DispatchType string // "affinity", "warming", "sync_load", "fallback"
	Error     error
}

// dispatchRequest - централизованная логика выбора бэкенда с загрузкой модели.
// 3-ступенчатая стратегия:
//   1. Group A (loaded) - модель уже загружена, проверяем loadRatio
//   2. Group B (config, not loaded) - инициируем загрузку, ждём готовности
//   3. Fallback (P4) - выбор по ресурсам без учёта модели
func (p *Proxy) dispatchRequest(req *QueuedRequest) DispatchResult {
	model := req.Model
	queueWaitMs := time.Since(req.Enqueued).Milliseconds()
	logger.Get().Debugw("dispatchRequest: starting dispatch",
		"model", model, "queue_wait_ms", queueWaitMs)


	// Получаем кандидатов с 4 приоритетами
	candidates := p.expandCandidates(model, nil)

	// === Stage 1: Model Affinity (LOADED) - P1 ===
	for _, group := range candidates {
		if group.Priority != 1 {
			break
		}
		for _, backendID := range group.BackendIDs {
			if !p.canAcceptRequest(backendID) {
				continue
			}
			if p.tryAcquireSlot(backendID) {
				atomic.AddInt64(&p.queueMgr.dispatchAffinity, 1)
				logger.Get().Debugw("dispatch: affinity", "backend", backendID, "model", model)
				return DispatchResult{BackendID: backendID, DispatchType: "affinity"}
			}
		}
	}

	// === Stage 2: Model Warming (WARMING_UP) - P2 ===
	for _, group := range candidates {
		if group.Priority != 2 {
			continue
		}
		for _, backendID := range group.BackendIDs {
			if !p.canAcceptRequest(backendID) {
				continue
			}
			state := p.backends[backendID]
			state.mu.Lock()
			ws, exists := state.WarmingUpModels[model]
			state.mu.Unlock()

			if !exists {
				continue
			}
			eta := time.Until(ws.EstimatedReadyAt)
			syncTimeout := p.getModelLoadTimeout()
			if eta > 0 && eta < syncTimeout {
				if p.waitForModelReady(backendID, model, syncTimeout) {
					if p.tryAcquireSlot(backendID) {
						atomic.AddInt64(&p.queueMgr.dispatchAffinity, 1)
						logger.Get().Debugw("dispatch: warming ready", "backend", backendID, "model", model)
						return DispatchResult{BackendID: backendID, DispatchType: "warming"}
					}
				}
				logger.Get().Warnw("dispatch: warming timeout", "backend", backendID, "model", model)
			}
		}
	}

	// === Stage 3: Sync Model Load (запуск загрузки) - P3 ===
	if p.config.Balancing.SyncModelLoad.Enabled {
		for _, group := range candidates {
			if group.Priority != 3 {
				continue
			}
			for _, backendID := range group.BackendIDs {
				if !p.canAcceptRequest(backendID) {
					continue
				}
				// Проверяем VRAM перед загрузкой
				if !p.checkResourceLimits(backendID) {
					continue
				}

				state := p.backends[backendID]
				if state == nil {
					continue
				}

				logger.Get().Infow("dispatch: initiating model load",
					"backend", backendID, "model", model)

				// Инициируем загрузку модели на конкретном бэкенде
				p.warmupModel(backendID, state.Backend.Host, state.Backend.OllamaPort, model)
				syncTimeout := p.getModelLoadTimeout()

				if p.waitForModelReady(backendID, model, syncTimeout) {
					if p.tryAcquireSlot(backendID) {
						atomic.AddInt64(&p.queueMgr.dispatchAffinity, 1)
						logger.Get().Infow("dispatch: sync load success",
							"backend", backendID, "model", model)
						return DispatchResult{BackendID: backendID, DispatchType: "sync_load"}
					}
				}
				logger.Get().Warnw("dispatch: sync load failed",
					"backend", backendID, "model", model)
			}
		}
	}

	// === Stage 4: Fallback по ресурсам (P4) ===
	backendID := p.selectByResources(nil)
	if backendID != "" {
		if p.tryAcquireSlot(backendID) {
			dispatchType := "fallback_load"
			if p.config.Balancing.UseEnhancedScoring {
				dispatchType = "fallback_enhanced"
				atomic.AddInt64(&p.queueMgr.dispatchLoad, 1)
			} else {
				atomic.AddInt64(&p.queueMgr.dispatchConfig, 1)
			}
			logger.Get().Debugw("dispatch: fallback", "backend", backendID, "model", model, "type", dispatchType)
			return DispatchResult{BackendID: backendID, DispatchType: dispatchType}
		}
	}

	return DispatchResult{Error: ErrNoBackendAvailable}
}

// canAcceptRequest - проверяет, может ли бэкенд принять запрос
// (healthy + loadRatio ниже порога + resource limits)
func (p *Proxy) canAcceptRequest(backendID string) bool {
	p.mu.RLock()
	state, exists := p.backends[backendID]
	p.mu.RUnlock()

	if !exists {
		return false
	}

	if state.Backend.Status != types.StatusHealthy {
		return false
	}

	if !p.checkResourceLimits(backendID) {
		return false
	}

	state.mu.Lock()
	active := state.ActiveReqs
	maxReqs := state.Backend.MaxConcurrentReqs
	state.mu.Unlock()

	loadRatio := 0.0
	if maxReqs > 0 {
		loadRatio = float64(active) / float64(maxReqs)
	}
	threshold := p.config.Balancing.Prewarm.TriggerLoadThreshold
	if threshold <= 0 {
		threshold = 0.80
	}

	return loadRatio < threshold
}

// waitForModelReady - ожидание загрузки модели с таймаутом и контекстом.
// Использует backoff polling: 100ms → 250ms → 500ms → max 1s.
func (p *Proxy) waitForModelReady(backendID, model string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	intervals := []time.Duration{100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond}
	maxInterval := 1 * time.Second

	// Мгновенная проверка
	if p.checkModelReadyUnsafe(backendID, model) {
		return true
	}

	attempt := 0
	for {
		interval := maxInterval
		if attempt < len(intervals) {
			interval = intervals[attempt]
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
			if p.checkModelReadyUnsafe(backendID, model) {
				return true
			}
			attempt++
		}
	}
}

// getModelLoadTimeout - возвращает таймаут загрузки модели из конфигурации.
// Default: 120s (если не задан или <= 0).
func (p *Proxy) getModelLoadTimeout() time.Duration {
	timeoutSec := p.config.Balancing.ModelLoadTimeout
	if timeoutSec <= 0 {
		timeoutSec = 120
	}
	return time.Duration(timeoutSec) * time.Second
}

// ErrNoBackendAvailable - ошибка: нет доступных бэкендов
var ErrNoBackendAvailable = &dispatchError{msg: "no backend available"}

type dispatchError struct {
	msg string
}

func (e *dispatchError) Error() string {
	return e.msg
}

// getSyncModelLoadTimeout - таймаут из SyncModelLoad.Timeout (для обратной совместимости)
func (p *Proxy) getSyncModelLoadTimeout() time.Duration {
	if p.config.Balancing.SyncModelLoad.Timeout != "" {
		if d, err := time.ParseDuration(p.config.Balancing.SyncModelLoad.Timeout); err == nil {
			return d
		}
	}
	return p.getModelLoadTimeout()
}
