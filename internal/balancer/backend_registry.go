package balancer

import (
	"fmt"
	"os"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// AddBackend - добавление нового бэкенда
func (p *Proxy) AddBackend(backend types.Backend) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.backends[backend.ID]; exists {
		return fmt.Errorf("backend with ID %s already exists", backend.ID)
	}

	if backend.Weight == 0 {
		backend.Weight = 1
	}
	if backend.MaxConcurrentReqs == 0 {
		backend.MaxConcurrentReqs = 10
	}
	if backend.OllamaPort == 0 {
		backend.OllamaPort = 11434
	}
	if backend.AgentPort == 0 {
		backend.AgentPort = 18032
	}
	if backend.Status == "" {
		backend.Status = types.StatusStarting
	}

	p.backends[backend.ID] = &BackendState{
		Backend:         &backend,
		ActiveReqs:      0,
		LastUsed:        time.Time{},
		WarmingUpModels: make(map[string]*types.WarmupState),
	}

	p.PublishEvent(types.Event{
		Type:      types.EventBackendAdd,
		Timestamp: time.Now().UTC(),
		BackendID: backend.ID,
		Data: map[string]interface{}{
			"name": backend.Name,
			"host": backend.Host,
		},
	})

	p.scheduleSave()

	return nil
}

// RemoveBackend - удаление бэкенда с graceful drain активных запросов.
// Помечает бэкенд как Draining, ждёт завершения активных запросов (с таймаутом 30с),
// затем удаляет из backends, metrics и принудительно сохраняет state.json.
func (p *Proxy) RemoveBackend(backendID string) error {
	p.mu.Lock()
	state, exists := p.backends[backendID]
	if !exists {
		p.mu.Unlock()
		return fmt.Errorf("backend with ID %s not found", backendID)
	}

	// Помечаем как Draining — новые запросы не будут направляться
	oldStatus := state.Backend.Status
	state.Backend.Status = types.StatusDraining
	p.mu.Unlock()

	if oldStatus != types.StatusDraining {
		p.PublishEvent(types.Event{
			Type:      types.EventStatusChange,
			Timestamp: time.Now().UTC(),
			BackendID: backendID,
			Data: map[string]interface{}{
				"oldStatus": string(oldStatus),
				"newStatus": string(types.StatusDraining),
			},
		})
	}

	// Ждём завершения активных запросов с таймаутом 30 секунд
	drainTimeout := time.After(30 * time.Second)
	drainTicker := time.NewTicker(200 * time.Millisecond)
	defer drainTicker.Stop()

drainLoop:
	for {
		select {
		case <-drainTimeout:
			logger.Get().Warnw("drain timeout reached, force-removing backend",
				"backend", backendID)
			break drainLoop
		case <-drainTicker.C:
			state.mu.Lock()
			active := state.ActiveReqs
			state.mu.Unlock()
			if active == 0 {
				logger.Get().Infow("all active requests drained, removing backend",
					"backend", backendID)
				break drainLoop
			}
		}
	}

	// Удаление бэкенда
	p.mu.Lock()
	delete(p.backends, backendID)
	p.mu.Unlock()

	p.metricsMgr.mu.Lock()
	delete(p.metricsMgr.metrics, backendID)
	p.metricsMgr.mu.Unlock()

	p.PublishEvent(types.Event{
		Type:      types.EventBackendRemove,
		Timestamp: time.Now().UTC(),
		BackendID: backendID,
		Data:      map[string]interface{}{},
	})

	// Немедленное сохранение state.json, чтобы удалённый бэкенд не восстановился при перезагрузке
	if err := p.FlushState(); err != nil {
		logger.Get().Errorw("failed to flush state after backend removal", "backend", backendID, "error", err)
	}

	logger.Get().Infow("backend removed and state flushed", "backend", backendID)
	return nil
}

// UpdateBackend - обновление бэкенда
func (p *Proxy) UpdateBackend(backendID string, updated types.Backend) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	state, exists := p.backends[backendID]
	if !exists {
		return fmt.Errorf("backend with ID %s not found", backendID)
	}

	currentStatus := state.Backend.Status
	currentActiveReqs := state.ActiveReqs

	updated.ID = backendID
	updated.Status = currentStatus
	state.Backend = &updated
	state.ActiveReqs = currentActiveReqs

	p.scheduleSave()

	return nil
}

// GetBackend - получение бэкенда по ID
func (p *Proxy) GetBackend(backendID string) *types.Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if state, exists := p.backends[backendID]; exists {
		return state.Backend
	}
	return nil
}

// GetAllBackends - получение всех бэкендов
func (p *Proxy) GetAllBackends() []types.Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()

	backends := make([]types.Backend, 0, len(p.backends))
	for _, state := range p.backends {
		backends = append(backends, *state.Backend)
	}
	return backends
}

// BackendExists - проверка существования бэкенда
func (p *Proxy) BackendExists(backendID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	_, exists := p.backends[backendID]
	return exists
}

// UpdateBackendStatus - обновление статуса бэкенда
func (p *Proxy) UpdateBackendStatus(backendID string, status types.BackendStatus) {
	p.mu.Lock()

	if state, ok := p.backends[backendID]; ok {
		state.mu.Lock()
		oldStatus := state.Backend.Status
		if oldStatus != status {
			state.Backend.Status = status
		}
		state.mu.Unlock()

		if oldStatus != status {
			p.PublishEvent(types.Event{
				Type:      types.EventStatusChange,
				Timestamp: time.Now().UTC(),
				BackendID: backendID,
				Data: map[string]interface{}{
					"oldStatus": string(oldStatus),
					"newStatus": string(status),
				},
			})
		}
	}
	p.mu.Unlock()
}

// UpdateBackendAgentStatus - обновление флага активного агента
func (p *Proxy) UpdateBackendAgentStatus(backendID string, hasAgent bool) {
	p.mu.Lock()

	if state, ok := p.backends[backendID]; ok {
		state.mu.Lock()
		state.Backend.HasAgent = hasAgent
		state.Backend.LastAgentContact = time.Now()
		state.mu.Unlock()
	}
	p.mu.Unlock()
}

// UpdateBackendLimits — обновление runtime-лимитов бэкенда
func (p *Proxy) UpdateBackendLimits(backendID string, maxModels, maxConcurrentRequests int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	state, exists := p.backends[backendID]
	if !exists {
		return fmt.Errorf("backend with ID %s not found", backendID)
	}

	state.Backend.RuntimeMaxModels = maxModels
	state.Backend.RuntimeMaxConcurrentRequests = maxConcurrentRequests

	p.PublishEvent(types.Event{
		Type:      types.EventLimitsChange,
		Timestamp: time.Now().UTC(),
		BackendID: backendID,
		Data: map[string]interface{}{
			"maxModels":             maxModels,
			"maxConcurrentRequests": maxConcurrentRequests,
		},
	})

	p.scheduleSave()

	return nil
}

// scheduleRecoveryCheck - планирование проверки восстановления бэкенда
func (p *Proxy) scheduleRecoveryCheck(backendID string) {
	logger.Get().Infow("scheduled recovery check", "backend", backendID)
}

// Restart - инициирует перезапуск балансера через graceful shutdown
func (p *Proxy) Restart() error {
	logger.Get().Infow("restart triggered, flushing state and exiting")

	if err := p.FlushState(); err != nil {
		logger.Get().Errorw("failed to flush state during restart", "error", err)
	}

	time.Sleep(100 * time.Millisecond)

	os.Exit(0)
	return nil
}