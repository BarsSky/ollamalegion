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

// EvacuateBackend — плавная эвакуация бэкенда: переключает все активные сессии
// на другие healthy бэкенды без обрыва соединений. Возвращает количество
// перемещённых сессий.
func (p *Proxy) EvacuateBackend(backendID string) (int, error) {
	p.mu.RLock()
	state, exists := p.backends[backendID]
	p.mu.RUnlock()
	if !exists {
		return 0, fmt.Errorf("backend with ID %s not found", backendID)
	}

	// Помечаем как Draining — новые запросы не будут направляться
	oldStatus := state.Backend.Status
	state.Backend.Status = types.StatusDraining

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

	// Находим все сессии, привязанные к этому бэкенду
	allSessions := p.sessionMgr.GetAll()
	var evacuatedCount int

	for _, session := range allSessions {
		if session.BackendID != backendID {
			continue
		}

		// Ищем альтернативный healthy бэкенд для модели сессии
		altBackend := p.findHealthyBackendForModel(session.Model, backendID)
		if altBackend == "" {
			logger.Get().Warnw("evacuation: no alternative backend for session",
				"session_id", session.ID,
				"model", session.Model,
				"backend", backendID,
			)
			continue
		}

		// Обновляем привязку сессии
		p.sessionMgr.Set(session.ID, altBackend, session.Model, session.ClientName, session.ClientIP, session.UserAgent)
		evacuatedCount++

		logger.Get().Infow("evacuation: session reassigned",
			"session_id", session.ID,
			"from", backendID,
			"to", altBackend,
			"model", session.Model,
		)
	}

	// Принудительно сохраняем state
	if err := p.FlushState(); err != nil {
		logger.Get().Errorw("failed to flush state after evacuation", "backend", backendID, "error", err)
	}

	logger.Get().Infow("backend evacuation completed",
		"backend", backendID,
		"evacuated_sessions", evacuatedCount,
		"total_sessions", len(allSessions),
	)
	return evacuatedCount, nil
}

// findHealthyBackendForModel — находит healthy бэкенд с моделью, исключая указанный.
func (p *Proxy) findHealthyBackendForModel(model, excludeID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var best string
	var bestLoad float64 = 2.0 // >1.0 значит не найден

	for id, state := range p.backends {
		if id == excludeID || state.Backend.Status != types.StatusHealthy {
			continue
		}

		// Проверяем, есть ли модель на бэкенде
		metrics, hasMetrics := p.metricsMgr.metrics[id]
		if hasMetrics {
			found := false
			for _, rm := range metrics.Ollama.RunningModels {
				if rm.Name == model {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		maxReqs := state.Backend.MaxConcurrentReqs
		if state.Backend.RuntimeMaxConcurrentRequests > 0 {
			maxReqs = state.Backend.RuntimeMaxConcurrentRequests
		}
		if maxReqs <= 0 {
			continue
		}

		load := float64(state.ActiveReqs) / float64(maxReqs)
		if load < bestLoad {
			bestLoad = load
			best = id
		}
	}

	return best
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

	// Ждём завершения активных запросов с таймаутом.
	// Базовый таймаут 30 сек, но если StreamTimeout больше — используем его
	// чтобы не обрывать длительные streaming-соединения.
	drainTimeoutSec := 30
	if p.config.Balancing.StreamTimeout > drainTimeoutSec {
		drainTimeoutSec = p.config.Balancing.StreamTimeout
	}
	drainTimeout := time.After(time.Duration(drainTimeoutSec) * time.Second)
	drainTicker := time.NewTicker(200 * time.Millisecond)
	defer drainTicker.Stop()

drainLoop:
	for {
		select {
		case <-drainTimeout:
			state.mu.Lock()
			remaining := state.ActiveReqs
			state.mu.Unlock()
			logger.Get().Warnw("drain timeout reached, force-removing backend",
				"backend", backendID,
				"timeout_sec", drainTimeoutSec,
				"remaining_active_reqs", remaining,
			)
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

// Agent V2 support methods

// FindBackendByHostPort — ищет ID бэкенда по (host, cppWorkerPort).
func (p *Proxy) FindBackendByHostPort(host string, cppWorkerPort int) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id, state := range p.backends {
		b := state.Backend
		if b.Host == host && b.CppWorkerPort == cppWorkerPort && b.Type == types.BackendTypeLlamaCpp {
			return id
		}
	}
	return ""
}

// AttachAgentToBackend — прикрепляет агента v2 к существующему бэкенду.
func (p *Proxy) AttachAgentToBackend(backendID, agentID string, agentPort int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.backends[backendID]
	if !ok {
		return
	}
	state.Backend.HasAgent = true
	state.Backend.AgentPort = agentPort
	state.Backend.AgentID = agentID
	state.AgentID = agentID
	state.Backend.LastAgentContact = time.Now()
}

// FindBackendByAgentID — ищет ID бэкенда по agentID (v2).
func (p *Proxy) FindBackendByAgentID(agentID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id, state := range p.backends {
		if state.AgentID == agentID || state.Backend.AgentID == agentID {
			return id
		}
	}
	return ""
}

// TouchAgentContact — обновляет LastAgentContact для бэкенда.
func (p *Proxy) TouchAgentContact(backendID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.backends[backendID]
	if !ok {
		return
	}
	state.Backend.LastAgentContact = time.Now()
}
