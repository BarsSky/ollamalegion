package balancer

// Package balancer — вспомогательные проверки выбора бэкенда.
//
// R78: из этого файла удалён legacy-путь `dispatchRequest`/`waitForModelReady`,
// который обслуживал канал QueueManager (канал и пул worker'ов удалены, ожидание
// слотов теперь в admission-очереди — см. admission_queue.go, unified_queue_r73.go).
// Остались проверки, которые использует обычный выбор бэкенда
// (backend_selector.go): `canAcceptRequest` и `getModelLoadTimeout`.

import (
	"time"

	"ollama-loadbalancer/pkg/types"
)

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
	maxReqs := state.Backend.EffectiveMaxConcurrentRequests()
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

// getModelLoadTimeout - возвращает таймаут загрузки модели из конфигурации.
// Default: 120s (если не задан или <= 0).
func (p *Proxy) getModelLoadTimeout() time.Duration {
	timeoutSec := p.config.Balancing.ModelLoadTimeout
	if timeoutSec <= 0 {
		timeoutSec = 120
	}
	return time.Duration(timeoutSec) * time.Second
}
