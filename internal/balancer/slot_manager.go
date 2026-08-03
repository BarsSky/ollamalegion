package balancer

import (
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// tryAcquireSlot — атомарная проверка и захват слота на бэкенде.
// Возвращает true если слот успешно захвачен (ActiveReqs < MaxConcurrentReqs).
// Если все слоты заняты, проверяет наличие зомби-сессий на бэкенде
// и принудительно освобождает один слот (preemption).
// Вызывающий код обязан вызвать releaseSlot после завершения запроса.
func (p *Proxy) tryAcquireSlot(backendID string) bool {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return false
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.Backend.Status != types.StatusHealthy {
		return false
	}

	maxReqs := state.Backend.MaxConcurrentReqs
	// Используем RuntimeMaxConcurrentRequests если задан
	if state.Backend.RuntimeMaxConcurrentRequests > 0 {
		maxReqs = state.Backend.RuntimeMaxConcurrentRequests
	}

	if maxReqs > 0 && state.ActiveReqs >= maxReqs {
		// Все слоты заняты — проверяем, не заняты ли они зомби-сессиями.
		// Если на бэкенде есть зомби-сессия (HasActiveStream=true, но LastRequestAt > 60s),
		// принудительно освобождаем слот для нового запроса.
		zombies := p.sessionMgr.DetectZombieSessions(60 * time.Second)
		for _, zombie := range zombies {
			if zombie.BackendID == backendID {
				logger.Get().Warnw("preempting zombie session slot for new request",
					"backend", backendID,
					"zombie_session_id", zombie.ID,
					"zombie_last_request", zombie.LastRequestAt,
				)
				// Удаляем зомби-сессию и её слот будет переиспользован
				p.sessionMgr.ForceRemove(zombie.ID)
				// Уменьшаем ActiveReqs чтобы освободить место
				if state.ActiveReqs > 0 {
					state.ActiveReqs--
				}
				// Захватываем слот для нового запроса
				state.ActiveReqs++
				state.LastUsed = time.Now()
				return true
			}
		}
		// Нет зомби для preemption — действительно нет свободных слотов
		return false
	}

	state.ActiveReqs++
	state.LastUsed = time.Now()
	return true
}

// releaseSlot — освобождение слота на бэкенде.
func (p *Proxy) releaseSlot(backendID string) {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return
	}

	state.mu.Lock()
	if state.ActiveReqs > 0 {
		state.ActiveReqs--
	}
	state.mu.Unlock()
}

// checkResourceLimits - проверка лимитов ресурсов (SOFT-режим)
func (p *Proxy) checkResourceLimits(backendID string) bool {
	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()

	if !ok {
		p.mu.RLock()
		state, exists := p.backends[backendID]
		p.mu.RUnlock()
		if !exists {
			return false
		}
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		if maxReqs > 0 && active >= maxReqs {
			return false
		}
		return true
	}

	limits := p.config.Resources

	// Headroom reservation: учитываем GPU headroom из конфигурации balancing
	headroom := p.config.Balancing.ResourceReservation.GPUHeadroomPercent
	if headroom > 0 && metrics.GPU.MemoryTotal > 0 {
		vramPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
		if vramPercent > (100 - headroom) {
			logger.Get().Warnw("backend VRAM exceeds headroom reservation, blocking",
				"backend", backendID, "vram_percent", vramPercent, "headroom_pct", headroom)
			return false
		}
	}

	if metrics.GPU.UsagePercent > 90 {
		logger.Get().Warnw("backend GPU usage critical, blocking", "backend", backendID, "gpu_usage", metrics.GPU.UsagePercent)
		return false
	}

	if limits.GPU.MaxVRAMUsagePercent > 0 && metrics.GPU.MemoryTotal > 0 {
		vramPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
		if vramPercent > limits.GPU.MaxVRAMUsagePercent {
			logger.Get().Warnw("backend VRAM usage above threshold (degraded, not blocked)",
				"backend", backendID, "vram_percent", vramPercent, "threshold", limits.GPU.MaxVRAMUsagePercent)
		}
	}

	if metrics.System.CPUUsagePercent > 95 {
		logger.Get().Warnw("backend CPU usage critical, blocking", "backend", backendID, "cpu_usage", metrics.System.CPUUsagePercent)
		return false
	}

	if limits.Memory.MaxUsagePercent > 0 && metrics.System.MemoryTotal > 0 {
		memPercent := float64(metrics.System.MemoryUsed) * 100 / float64(metrics.System.MemoryTotal)
		if memPercent > 98 {
			logger.Get().Warnw("backend RAM usage critical, blocking", "backend", backendID, "ram_percent", memPercent)
			return false
		}
	}

	if metrics.System.DiskTotal > 0 && metrics.System.DiskFree < limits.Disk.MinFreeMB {
		logger.Get().Warnw("backend disk space critical, blocking", "backend", backendID,
			"disk_free_mb", metrics.System.DiskFree, "min_required_mb", limits.Disk.MinFreeMB)
		return false
	}

	if metrics.Ollama.MaxModels > 0 {
		if len(metrics.Ollama.RunningModels) >= metrics.Ollama.MaxModels {
			logger.Get().Debugw("backend model slots full (degraded, not blocked)",
				"backend", backendID, "loaded_models", len(metrics.Ollama.RunningModels), "max_models", metrics.Ollama.MaxModels)
		}
	}

	// Проверка слотов запросов вынесена в tryAcquireSlot для атомарности.
	// checkResourceLimits отвечает только за аппаратные/ресурсные лимиты.

	return true
}

// findModelOnAnyBackendNoVRAMCheck — ищет backend с загруженной моделью,
// игнорируя VRAM headroom check. Для read/mgmt endpoints (Round 22).
//
// Read operations не нагружают GPU/VRAM (нет inference), поэтому блокировка
// при 95% VRAM некорректна — нужно дать им пройти.
func (p *Proxy) findModelOnAnyBackendNoVRAMCheck(model string, bt types.BackendType) string {
	if model == "" {
		return ""
	}
	allowedTypes := p.getAllowedTypesList(bt)

	p.mu.RLock()
	defer p.mu.RUnlock()

	for id, state := range p.backends {
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		p.metricsMgr.mu.RLock()
		// Проверяем оба cache: Ollama-agent и cppworker-poller.
		hasModel := false
		if metrics, ok := p.metricsMgr.metrics[id]; ok {
			if p.backendHasModel(metrics, model) {
				hasModel = true
			}
		}
		if !hasModel {
			if lm, ok := p.metricsMgr.llamaMetrics[id]; ok && lm != nil {
				for _, m := range lm.LoadedModels {
					if m.Name == model {
						hasModel = true
						break
					}
				}
			}
		}
		p.metricsMgr.mu.RUnlock()
		if hasModel {
			return id
		}
	}
	return ""
}

// selectAnyHealthy — выбирает любой healthy бэкенд подходящего типа.
// Для read/mgmt endpoints (Round 22) — fallback когда модель нигде не загружена.
func (p *Proxy) selectAnyHealthy(bt types.BackendType) string {
	allowedTypes := p.getAllowedTypesList(bt)
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id, state := range p.backends {
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status == types.StatusHealthy {
			return id
		}
	}
	return ""
}