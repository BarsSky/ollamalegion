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
// R-Image (2026-09-27): image_cpp — валидный тип; до этой правки он молча
// превращался в Ollama, из-за чего image-бэкенд опрашивался как Ollama
// (/api/tags) и получал OllamaPort.
func normalizeBackendType(bt types.BackendType) types.BackendType {
	if bt == "" {
		return types.BackendTypeOllama
	}
	switch bt {
	case types.BackendTypeOllama, types.BackendTypeLlamaCpp, types.BackendTypeImage:
		return bt
	default:
		return types.BackendTypeOllama
	}
}

// getBackendEngine возвращает движок кластера.
//
// R112 (2026-10-11): РАЗДЕЛЕНИЕ ПО ТИПУ УБРАНО. Раньше здесь был приоритет
// «явный конфиг → operatingMode → автоопределение», и operatingMode жёстко
// назначал llama.cpp для virtual_router/distributed_inference. Следствие: смена
// режима меняла правила инференса для ЧУЖИХ бэкендов, а смешанный кластер
// (llama.cpp + Ollama) вообще не мог существовать — режим оставлял ровно один тип.
//
// Теперь движок выводится из фактического состава кластера:
//   - только llama.cpp → EngineLlamaCPP;
//   - только Ollama    → EngineOllamaAPI;
//   - оба текстовых типа → EngineAuto (правила одного движка не навязываются
//     другому пулу: иначе n_ctx/KV-типы/offload считались бы по чужой модели);
//   - нет бэкендов → EngineAuto.
//
// Явный backendEngine из конфига (осознанный выбор оператора) уважается ТОЛЬКО
// пока бэкенды этого типа реально зарегистрированы. Иначе он считается
// устаревшим — например, был выведен из operatingMode старой версией при
// инициализации и сохранился в config.json; такой «призрак» не должен прятать
// второй тип (ровно так llama.cpp и Ollama не работали вместе).
func (p *Proxy) getBackendEngine() types.BackendEngine {
	counts := p.countBackendsByType()
	hasOllama := counts[types.BackendTypeOllama] > 0
	hasLlama := counts[types.BackendTypeLlamaCpp] > 0

	if configured := p.config.BackendEngine; configured != "" && configured != types.EngineAuto {
		switch configured {
		case types.EngineLlamaCPP:
			if hasLlama {
				return types.EngineLlamaCPP
			}
		case types.EngineOllamaAPI:
			if hasOllama {
				return types.EngineOllamaAPI
			}
		case types.EngineImageCPP:
			return types.EngineImageCPP
		}
		// Предпочтение не подтверждено составом кластера — идём в автоопределение.
	}

	if hasLlama && !hasOllama {
		return types.EngineLlamaCPP
	}
	if hasOllama && !hasLlama {
		return types.EngineOllamaAPI
	}
	return types.EngineAuto
}

// getEffectiveBackendType возвращает доминирующий тип бэкенда в кластере.
// Используется для фильтрации отображения бэкендов в WebUI и мониторе.
//
// R112 (2026-10-11): тип считается ТОЛЬКО по фактическому составу кластера.
// Раньше первым приоритетом шёл явный backendEngine из конфига, поэтому
// сохранённое при инициализации значение (выведенное из operatingMode) прятало
// бэкенды второго типа из /api/v1/cluster и /api/v1/metrics — оператор видел
// «подключилась только половина стенда». Конфиг учитывается лишь как подсказка,
// когда бэкендов нет вообще (кластер ещё не зарегистрировался).
// При смешанном кластере возвращает пустую строку = «показывать все типы».
func (p *Proxy) getEffectiveBackendType() types.BackendType {
	counts := p.countBackendsByType()
	hasOllama := counts[types.BackendTypeOllama] > 0
	hasLlama := counts[types.BackendTypeLlamaCpp] > 0

	if hasLlama && !hasOllama {
		return types.BackendTypeLlamaCpp
	}
	if hasOllama && !hasLlama {
		return types.BackendTypeOllama
	}
	if hasLlama && hasOllama {
		// Смешанный кластер — доминанты нет, показываем оба пула.
		return ""
	}

	// Бэкендов нет (старт/пустой кластер): подсказка из конфига, иначе «все».
	switch p.config.BackendEngine {
	case types.EngineLlamaCPP:
		return types.BackendTypeLlamaCpp
	case types.EngineOllamaAPI:
		return types.BackendTypeOllama
	case types.EngineImageCPP:
		return types.BackendTypeImage
	}
	return ""
}

// filterBackendsByEffectiveType фильтрует срез BackendMetrics по эффективному типу.
// Если effectiveType пуст — возвращает все бэкенды без фильтрации.
//
// R-Image (2026-09-27): image_cpp НИКОГДА не отфильтровывается.
// Зачем: эффективный тип описывает ТЕКСТОВЫЙ движок кластера
// (backendEngine = llama_cpp/ollama). Image-бэкенды ортогональны ему — иначе
// при backendEngine=llama_cpp (bundled-стек) image-ноды исчезали бы из
// /api/v1/metrics и /api/v1/cluster, и Monitor не показывал бы их вообще.
func filterBackendsByEffectiveType(backends []types.BackendMetrics, effectiveType types.BackendType) []types.BackendMetrics {
	if effectiveType == "" {
		return backends
	}
	filtered := make([]types.BackendMetrics, 0, len(backends))
	for _, b := range backends {
		// Image-бэкенды ортогональны текстовому движку — пропускаем всегда.
		if b.BackendType == types.BackendTypeImage {
			filtered = append(filtered, b)
			continue
		}
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
