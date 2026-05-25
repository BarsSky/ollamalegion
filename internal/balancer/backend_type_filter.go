package balancer

import "ollama-loadbalancer/pkg/types"

// filterBackendsByType возвращает только бэкенды указанного типа.
func (p *Proxy) filterBackendsByType(bt types.BackendType) []*BackendState {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var result []*BackendState
	for _, state := range p.backends {
		state.mu.Lock()
		bType := normalizeBackendType(state.Backend.Type)
		status := state.Backend.Status
		state.mu.Unlock()

		if bType == bt && status == types.StatusHealthy {
			result = append(result, state)
		}
	}
	return result
}

// filterHealthyBackendsByType возвращает только здоровые бэкенды указанного типа.
func (p *Proxy) filterHealthyBackendsByType(bt types.BackendType) []*BackendState {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var result []*BackendState
	for _, state := range p.backends {
		state.mu.Lock()
		bType := normalizeBackendType(state.Backend.Type)
		status := state.Backend.Status
		state.mu.Unlock()

		if bType == bt && status == types.StatusHealthy {
			result = append(result, state)
		}
	}
	return result
}

// getAllActiveBackendsByType возвращает активные (не offline) бэкенды указанного типа.
func (p *Proxy) getAllActiveBackendsByType(bt types.BackendType) []*BackendState {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var result []*BackendState
	for _, state := range p.backends {
		state.mu.Lock()
		bType := normalizeBackendType(state.Backend.Type)
		status := state.Backend.Status
		state.mu.Unlock()

		if bType == bt && status != types.StatusOffline {
			result = append(result, state)
		}
	}
	return result
}

// countBackendsByType возвращает количество бэкендов каждого типа.
func (p *Proxy) countBackendsByType() map[types.BackendType]int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	counts := make(map[types.BackendType]int)
	for _, state := range p.backends {
		state.mu.Lock()
		bt := normalizeBackendType(state.Backend.Type)
		state.mu.Unlock()
		counts[bt]++
	}
	return counts
}

// normalizeBackendType приводит тип бэкенда к стандартному значению.
// Пустой тип считается Ollama (обратная совместимость).
func normalizeBackendType(bt types.BackendType) types.BackendType {
	if bt == "" {
		return types.BackendTypeOllama
	}
	switch bt {
	case types.BackendTypeOllama, types.BackendTypeLlamaCpp:
		return bt
	default:
		return types.BackendTypeOllama
	}
}

// getBackendEngine возвращает текущий движок балансировщика.
// Приоритет: явный конфиг → операционный режим → автоопределение по зарегистрированным бэкендам.
func (p *Proxy) getBackendEngine() types.BackendEngine {
	if p.config.BackendEngine != "" && p.config.BackendEngine != types.EngineAuto {
		return p.config.BackendEngine
	}
	if types.IsModeLlamaCpp(p.config.Balancing.OperatingMode) {
		return types.EngineLlamaCPP
	}
	if p.config.Balancing.OperatingMode == "standard" || p.config.Balancing.OperatingMode == "" {
		// Стандартный режим: автоопределение по типам зарегистрированных бэкендов
		counts := p.countBackendsByType()
		hasOllama := counts[types.BackendTypeOllama] > 0
		hasLlama := counts[types.BackendTypeLlamaCpp] > 0
		if hasLlama && !hasOllama {
			return types.EngineLlamaCPP
		}
		if hasOllama && !hasLlama {
			return types.EngineOllamaAPI
		}
		// Смешанный кластер или нет бэкендов — возвращаем auto
		return types.EngineAuto
	}
	return types.EngineOllamaAPI
}

// getEffectiveBackendType возвращает доминирующий тип бэкенда в кластере.
// Приоритет: явный backendEngine из конфигурации → автоопределение по фактическим бэкендам.
// Используется для фильтрации отображения бэкендов в WebUI и мониторе.
// При смешанном кластере и отсутствии явного конфига возвращает пустую строку (показывать все).
func (p *Proxy) getEffectiveBackendType() types.BackendType {
	// Приоритет 1: явно заданный backendEngine в конфигурации (выбран при инициализации)
	if p.config.BackendEngine != "" && p.config.BackendEngine != types.EngineAuto {
		if p.config.BackendEngine == types.EngineLlamaCPP {
			return types.BackendTypeLlamaCpp
		}
		if p.config.BackendEngine == types.EngineOllamaAPI {
			return types.BackendTypeOllama
		}
	}

	// Приоритет 2: автоопределение по фактически зарегистрированным бэкендам
	counts := p.countBackendsByType()
	hasOllama := counts[types.BackendTypeOllama] > 0
	hasLlama := counts[types.BackendTypeLlamaCpp] > 0

	if hasLlama && !hasOllama {
		return types.BackendTypeLlamaCpp
	}
	if hasOllama && !hasLlama {
		return types.BackendTypeOllama
	}
	// Смешанный кластер или нет бэкендов — показываем все
	return ""
}

// filterBackendsByEffectiveType фильтрует срез BackendMetrics по эффективному типу.
// Если effectiveType пуст — возвращает все бэкенды без фильтрации.
func filterBackendsByEffectiveType(backends []types.BackendMetrics, effectiveType types.BackendType) []types.BackendMetrics {
	if effectiveType == "" {
		return backends
	}
	filtered := make([]types.BackendMetrics, 0, len(backends))
	for _, b := range backends {
		if b.BackendType == effectiveType || (b.BackendType == "" && effectiveType == types.BackendTypeOllama) {
			filtered = append(filtered, b)
		}
	}
	return filtered
}

// isLlamaCppMode возвращает true, если текущий режим использует llama.cpp.
func (p *Proxy) isLlamaCppMode() bool {
	return p.getBackendEngine() == types.EngineLlamaCPP
}

// isOllamaMode возвращает true, если текущий режим использует Ollama.
func (p *Proxy) isOllamaMode() bool {
	return p.getBackendEngine() == types.EngineOllamaAPI
}