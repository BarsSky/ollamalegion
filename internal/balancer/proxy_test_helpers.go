package balancer

import (
	"net/http"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// --- Exported test helpers ---

// ExpandCandidates — экспортируемая обёртка над expandCandidates для тестов (без фильтрации)
func (p *Proxy) ExpandCandidates(modelName string) CandidateGroups {
	return p.expandCandidates(modelName, nil)
}

// ExpandCandidatesWithType — экспортируемая обёртка с фильтрацией по OperatingMode
func (p *Proxy) ExpandCandidatesWithType(modelName string, allowedTypes []types.BackendType) CandidateGroups {
	return p.expandCandidates(modelName, allowedTypes)
}

// DispatchWithModelLoad — экспортируемая обёртка над dispatchWithModelLoad для тестов
func (p *Proxy) DispatchWithModelLoad(model string) (string, time.Time) {
	return p.dispatchWithModelLoad(model)
}

// SetBackendMetrics — установка метрик в MetricsManager для тестов
func (p *Proxy) SetBackendMetrics(backendID string, metrics *types.BackendMetrics) {
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics[backendID] = metrics
	p.metricsMgr.mu.Unlock()
}

// SelectBackend — экспортируемая обёртка для тестов (без фильтрации по типу)
func (p *Proxy) SelectBackend(model string) string {
	return p.selectBackend(model, "")
}

// SelectBackendWithType — экспортируемая обёртка с явным BackendType для тестов
func (p *Proxy) SelectBackendWithType(model string, bt types.BackendType) string {
	return p.selectBackend(model, bt)
}

// GetBackendState — экспортируемый доступ к BackendState для тестов
func (p *Proxy) GetBackendState(backendID string) *BackendState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.backends[backendID]
}

// GetWarmingUpModels — получение WarmingUpModels для тестов (thread-safe)
func (p *Proxy) GetWarmingUpModels(backendID string) map[string]*types.WarmupState {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	// Return a copy
	result := make(map[string]*types.WarmupState, len(state.WarmingUpModels))
	for k, v := range state.WarmingUpModels {
		result[k] = v
	}
	return result
}

// GetConfig — получение конфигурации для тестов
func (p *Proxy) GetConfig() *types.LoadBalancerConfig {
	return p.config
}

// SetWarmingUpModel — установка warming-модели для тестов
func (p *Proxy) SetWarmingUpModel(backendID, modelName string, readyAt time.Time) {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return
	}
	state.mu.Lock()
	if state.WarmingUpModels == nil {
		state.WarmingUpModels = make(map[string]*types.WarmupState)
	}
	state.WarmingUpModels[modelName] = &types.WarmupState{
		StartedAt:        time.Now(),
		EstimatedReadyAt: readyAt,
		TriggerReason:    "test",
	}
	state.mu.Unlock()
}

// StopQueue — остановка QueueManager и всех workers
func (p *Proxy) StopQueue() {
	if p.queueMgr != nil {
		p.queueMgr.Stop()
	}
}

// StopSessionManager — остановка SessionManager (cleanup loop)
func (p *Proxy) StopSessionManager() {
	if p.sessionMgr != nil {
		p.sessionMgr.Stop()
	}
}

// AcquireSlotForTest — захват слота на бэкенде для тестов (увеличивает ActiveReqs)
func (p *Proxy) AcquireSlotForTest(backendID string) {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return
	}
	state.mu.Lock()
	state.ActiveReqs++
	state.mu.Unlock()
}

// GetBackendIDs — возвращает список всех ID бэкендов (для тестов)
func (p *Proxy) GetBackendIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	ids := make([]string, 0, len(p.backends))
	for id := range p.backends {
		ids = append(ids, id)
	}
	return ids
}

// GetBackendStates — возвращает map всех BackendState (для тестов)
func (p *Proxy) GetBackendStates() map[string]*BackendState {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make(map[string]*BackendState, len(p.backends))
	for k, v := range p.backends {
		result[k] = v
	}
	return result
}

// DetermineRequestBackendTypeForTest — экспортируемая обёртка determineRequestBackendType для тестов
func (p *Proxy) DetermineRequestBackendTypeForTest(path string) types.BackendType {
	req, _ := http.NewRequest("GET", path, nil)
	return p.determineRequestBackendType(req)
}
