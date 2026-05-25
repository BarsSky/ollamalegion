package balancer

import (
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// CandidateGroup — группа бэкендов-кандидатов одного приоритета
type CandidateGroup struct {
	Priority   int      // 1=LOADED, 2=WARMING, 3=FREE, 4=FALLBACK
	BackendIDs []string
}

// CandidateGroups — упорядоченный список групп кандидатов
type CandidateGroups []CandidateGroup

// isBackendAvailableForRequests — проверяет, можно ли направлять запросы на бэкенд.
// Бэкенд доступен если: healthy, или ollama_unavailable (но только если у него есть агент,
// тогда мы пробуем — агент может восстановить ollama). Обычно ollama_unavailable
// исключается из expandCandidates, но позволяет retry в proxyRequest.
func isBackendAvailableForRequests(status types.BackendStatus) bool {
	return status == types.StatusHealthy || status == types.StatusOllamaUnavailable
}

// expandCandidates — группирует бэкенды по приоритетам для заданной модели
// P1 (LOADED): healthy бэкенды с моделью в памяти, loadRatio < 80%
// P2 (WARMING): healthy бэкенды где модель в WarmingUpModels
// P3 (FREE): healthy бэкенды со свободными слотами, без модели
// P4 (FALLBACK): все healthy бэкенды для resource-based scoring
// Исключены: offline, unhealthy, draining, ollama_unavailable (агент жив, но ollama не отвечает)
// allowedTypes — допустимые типы бэкендов (если nil/пустой — без фильтрации для обратной совместимости)
func (p *Proxy) expandCandidates(modelName string, allowedTypes []types.BackendType) CandidateGroups {
	p.mu.RLock()
	defer p.mu.RUnlock()

	threshold := p.config.Balancing.Prewarm.TriggerLoadThreshold
	if threshold <= 0 {
		threshold = 0.80
	}

	var loaded, warming, free, fallback []string

	for id, state := range p.backends {
		// Фильтрация по типу бэкенда
		if len(allowedTypes) > 0 {
			bt := normalizeBackendType(state.Backend.Type)
			allowed := false
			for _, at := range allowedTypes {
				if bt == at {
					allowed = true
					break
				}
			}
			if !allowed {
				continue
			}
		}

		// Бэкенд должен быть healthy для routing через expandCandidates.
		// StatusOllamaUnavailable исключается — агент жив, но ollama не отвечает.
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if !p.checkResourceLimits(id) {
			continue
		}

		pred := state.Prediction
		if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 300 {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		_, isWarming := state.WarmingUpModels[modelName]
		state.mu.Unlock()

		loadRatio := 0.0
		if maxReqs > 0 {
			loadRatio = float64(active) / float64(maxReqs)
		}

		p.metricsMgr.mu.RLock()
		metrics, hasMetrics := p.metricsMgr.metrics[id]
		p.metricsMgr.mu.RUnlock()
		if !hasMetrics {
			fallback = append(fallback, id)
			continue
		}

		hasModel := false
		for _, m := range metrics.Ollama.RunningModels {
			if m.Name == modelName || strings.Contains(m.Name, modelName) {
				hasModel = true
				break
			}
		}

		if hasModel && loadRatio < threshold {
			loaded = append(loaded, id)
		} else if isWarming {
			warming = append(warming, id)
		} else if loadRatio < 1.0 {
			free = append(free, id)
		}

		// Все healthy с метриками идут в fallback
		fallback = append(fallback, id)
	}

	groups := make(CandidateGroups, 0, 4)
	if len(loaded) > 0 {
		groups = append(groups, CandidateGroup{Priority: 1, BackendIDs: loaded})
	}
	if len(warming) > 0 {
		groups = append(groups, CandidateGroup{Priority: 2, BackendIDs: warming})
	}
	if len(free) > 0 {
		groups = append(groups, CandidateGroup{Priority: 3, BackendIDs: free})
	}
	if len(fallback) > 0 {
		groups = append(groups, CandidateGroup{Priority: 4, BackendIDs: fallback})
	}

	return groups
}

// dispatchWithModelLoad — инициирует загрузку модели на free бэкенде
// Возвращает backendID и deadline для ожидания загрузки. Если модель уже загружена — возвращает "".
func (p *Proxy) dispatchWithModelLoad(model string) (string, time.Time) {
	candidates := p.expandCandidates(model, nil)

	// Ищем лучший free backend (P3) с максимальным свободным VRAM
	var bestBackend string
	var maxFreeVRAM uint64
	for _, group := range candidates {
		if group.Priority != 3 {
			continue
		}
		for _, backendID := range group.BackendIDs {
			_, ok := p.backends[backendID]
			if !ok {
				continue
			}
			p.metricsMgr.mu.RLock()
			metrics, hasMetrics := p.metricsMgr.metrics[backendID]
			p.metricsMgr.mu.RUnlock()
			if !hasMetrics {
				continue
			}
			freeVRAM := metrics.GPU.MemoryFree
			if freeVRAM > maxFreeVRAM {
				maxFreeVRAM = freeVRAM
				bestBackend = backendID
			}
			if bestBackend == "" {
				bestBackend = backendID
			}
		}
	}

	if bestBackend == "" {
		return "", time.Time{}
	}

	// Инициируем загрузку модели
	backendState := p.backends[bestBackend]
	p.warmupModel(bestBackend, backendState.Backend.Host, backendState.Backend.OllamaPort, model)

	// Добавляем в WarmingUpModels
	backendState.mu.Lock()
	if backendState.WarmingUpModels == nil {
		backendState.WarmingUpModels = make(map[string]*types.WarmupState)
	}
	loadTimeout := 120
	if p.config.Balancing.ModelLoadTimeout > 0 {
		loadTimeout = p.config.Balancing.ModelLoadTimeout
	}
	backendState.WarmingUpModels[model] = &types.WarmupState{
		StartedAt:        time.Now(),
		EstimatedReadyAt: time.Now().Add(time.Duration(loadTimeout) * time.Second),
		TriggerReason:    "dispatch",
	}
	backendState.mu.Unlock()

	deadline := time.Now().Add(time.Duration(loadTimeout) * time.Second)
	logger.Get().Infow("dispatchWithModelLoad: initiated model load",
		"backend", bestBackend, "model", model, "deadline", deadline)

	return bestBackend, deadline
}