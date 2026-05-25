package balancer

import (
	"strings"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// getAllowedTypesList преобразует одиночный BackendType в список.
// Если bt пустой — возвращает типы из OperatingMode (обратная совместимость).
func (p *Proxy) getAllowedTypesList(bt types.BackendType) []types.BackendType {
	if bt != "" {
		return []types.BackendType{bt}
	}
	return p.getDefaultAllowedTypes()
}

// getDefaultAllowedTypes возвращает допустимые типы бэкендов на основе OperatingMode.
// Используется когда явный BackendType не передан (обратная совместимость).
func (p *Proxy) getDefaultAllowedTypes() []types.BackendType {
	allowed, ok := types.ModeBackendTypes[p.config.Balancing.OperatingMode]
	if !ok || len(allowed) == 0 {
		// Неизвестный режим — разрешаем оба типа для обратной совместимости
		return []types.BackendType{types.BackendTypeOllama, types.BackendTypeLlamaCpp}
	}
	return allowed
}

// isBackendTypeAllowed проверяет, разрешён ли тип бэкенда в списке allowedTypes.
// Если allowedTypes пустой — разрешены все типы (обратная совместимость).
func isBackendTypeAllowed(bt types.BackendType, allowedTypes []types.BackendType) bool {
	if len(allowedTypes) == 0 {
		return true
	}
	for _, at := range allowedTypes {
		if bt == at {
			return true
		}
	}
	return false
}

// selectBackend - выбор бэкенда для запроса (pre-step + 4 этапа = 5 шагов)
// bt — требуемый тип бэкенда (если пустой — определяется из OperatingMode)
func (p *Proxy) selectBackend(model string, bt types.BackendType) string {
	// RPC Module: Model Replication — если модель в группе репликации, выбираем из реплик
	if p.replicationSelector != nil {
		if selected := p.replicationSelector.Select(model); selected != "" {
			logger.Get().Infow("routed through model replication",
				"model", model, "backend", selected)
			return selected
		}
	}

	allowedTypes := p.getAllowedTypesList(bt)

	candidates := p.expandCandidates(model, allowedTypes)

	// 1. Model Affinity (LOADED) — P1
	// Выбираем лучший бэкенд по score среди loaded-кандидатов с loadRatio < threshold
	for _, group := range candidates {
		if group.Priority != 1 {
			break
		}
		var bestBackendID string
		var bestScore float64 = -1
		for _, backendID := range group.BackendIDs {
			state := p.backends[backendID]
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
			if loadRatio >= threshold {
				continue
			}

			score := p.calculateScore(backendID)
			if score > bestScore {
				bestScore = score
				bestBackendID = backendID
			}
		}
		if bestBackendID != "" {
			atomic.AddInt64(&p.queueMgr.dispatchAffinity, 1)
			return bestBackendID
		}
	}

	// 2. Model Warming (WARMING_UP) — P2
	for _, group := range candidates {
		if group.Priority != 2 {
			continue
		}
		for _, backendID := range group.BackendIDs {
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
				start := time.Now()
				for time.Since(start) < syncTimeout {
					if p.checkModelReadyUnsafe(backendID, model) {
						atomic.AddInt64(&p.queueMgr.dispatchAffinity, 1)
						return backendID
					}
					time.Sleep(500 * time.Millisecond)
				}
				logger.Get().Warnw("model warmup timeout", "backend", backendID, "model", model)
			}
		}
	}

	// 3. Sync Model Load (запуск загрузки) — P3
	if p.config.Balancing.SyncModelLoad.Enabled {
		for _, group := range candidates {
			if group.Priority != 3 {
				continue
			}
			for _, backendID := range group.BackendIDs {
				if !p.canAcceptRequest(backendID) {
					continue
				}
				state := p.backends[backendID]
				if state == nil {
					continue
				}
				p.warmupModel(backendID, state.Backend.Host, state.Backend.OllamaPort, model)
				syncTimeout := p.getModelLoadTimeout()
				start := time.Now()
				for time.Since(start) < syncTimeout {
					if p.checkModelReadyUnsafe(backendID, model) {
						atomic.AddInt64(&p.queueMgr.dispatchAffinity, 1)
						return backendID
					}
					time.Sleep(500 * time.Millisecond)
				}
				logger.Get().Warnw("sync model load timeout", "backend", backendID, "model", model)
			}
		}
	}

	// 4. selectByResources (scoring v2) — P4 (FALLBACK)
	return p.selectByResources(allowedTypes)
}

// selectBackendExcluding - выбор бэкенда, исключая указанные
func (p *Proxy) selectBackendExcluding(model string, exclude map[string]bool, bt types.BackendType) string {
	allowedTypes := p.getAllowedTypesList(bt)

	p.mu.RLock()

	if p.config.Balancing.ModelAffinity && model != "" {
		if backend := p.findBackendWithModelExcluding(model, exclude, allowedTypes); backend != "" {
			p.mu.RUnlock()
			return backend
		}
	}
	p.mu.RUnlock()

	backend := p.selectByResourcesExcluding(exclude, allowedTypes)
	if backend == "" {
		logger.Get().Warnw("no available backends with exclusions")
	}
	return backend
}

// findBackendWithModelExcluding - поиск бэкенда с моделью, исключая указанные
func (p *Proxy) findBackendWithModelExcluding(modelName string, exclude map[string]bool, allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestScore float64 = -1

	for id, state := range p.backends {
		if exclude[id] {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		if !p.checkResourceLimits(id) {
			continue
		}

		p.metricsMgr.mu.RLock()
		metrics, hasMetrics := p.metricsMgr.metrics[id]
		p.metricsMgr.mu.RUnlock()
		if !hasMetrics {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()

		// Согласно документации: model affinity только при loadRatio < threshold
		threshold := p.config.Balancing.Prewarm.TriggerLoadThreshold
		if threshold <= 0 {
			threshold = 0.80
		}
		if maxReqs > 0 && float64(active)/float64(maxReqs) >= threshold {
			continue
		}

		pred := state.Prediction
		if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 300 {
			continue
		}

		if !p.backendHasModel(metrics, modelName) {
			continue
		}

		score := p.calculateScore(id)
		if score > bestScore {
			bestScore = score
			bestBackendID = id
		}
	}

	return bestBackendID
}

// modelIsRunningOnBackendUnsafe — проверяет, запущена ли модель на бэкенде (без блокировки)
func (p *Proxy) modelIsRunningOnBackendUnsafe(backendID, modelName string) bool {
	if modelName == "" {
		return false
	}
	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()
	if !ok {
		return false
	}
	return p.backendHasModel(metrics, modelName)
}

// selectByResourcesExcluding - выбор по ресурсам с исключением бэкендов
func (p *Proxy) selectByResourcesExcluding(exclude map[string]bool, allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackend string
	var bestScore float64 = -1

	for id, state := range p.backends {
		if exclude[id] {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		if !p.checkResourceLimits(id) {
			continue
		}

		state.mu.Lock()
		if state.ActiveReqs >= state.Backend.MaxConcurrentReqs {
			state.mu.Unlock()
			continue
		}
		state.mu.Unlock()

		score := p.calculateScore(id)
		if score > bestScore {
			bestScore = score
			bestBackend = id
		}
	}

	if bestBackend != "" {
		if p.config.Balancing.UseEnhancedScoring {
			atomic.AddInt64(&p.queueMgr.dispatchLoad, 1)
		} else {
			atomic.AddInt64(&p.queueMgr.dispatchConfig, 1)
		}
	}

	return bestBackend
}

// findLessLoadedBackendWithModel — поиск менее загруженного бэкенда с той же моделью
func (p *Proxy) findLessLoadedBackendWithModel(modelName, excludeBackendID string, allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestLoadRatio float64 = 2.0

	for id, state := range p.backends {
		if id == excludeBackendID {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if !p.checkResourceLimits(id) {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()

		if maxReqs <= 0 {
			continue
		}

		p.metricsMgr.mu.RLock()
		metrics, hasMetrics := p.metricsMgr.metrics[id]
		p.metricsMgr.mu.RUnlock()
		if !hasMetrics {
			continue
		}

		if !p.backendHasModel(metrics, modelName) {
			continue
		}

		loadRatio := float64(active) / float64(maxReqs)
		if loadRatio < bestLoadRatio {
			bestLoadRatio = loadRatio
			bestBackendID = id
		}
	}

	return bestBackendID
}

// findLessLoadedBackendAny — поиск любого менее загруженного healthy бэкенда.
// При равном loadRatio выбирает бэкенд с лучшим score (deterministic).
// Если модель не загружена на выбранном бэкенде — запускает асинхронный warmup.
func (p *Proxy) findLessLoadedBackendAny(modelName, excludeBackendID string, allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestLoadRatio float64 = 2.0
	var bestScore float64 = -1

	for id, state := range p.backends {
		if id == excludeBackendID {
			continue
		}
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if !p.checkResourceLimits(id) {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()

		if maxReqs <= 0 {
			continue
		}

		loadRatio := float64(active) / float64(maxReqs)

		// При равном loadRatio используем score для deterministic выбора
		if loadRatio < bestLoadRatio || (loadRatio == bestLoadRatio && bestScore < 0) {
			bestLoadRatio = loadRatio
			bestBackendID = id
			bestScore = p.calculateScore(id)
		} else if loadRatio == bestLoadRatio {
			score := p.calculateScore(id)
			if score > bestScore {
				bestScore = score
				bestBackendID = id
			}
		}
	}

	// Если нашли бэкенд без модели — запускаем warmup асинхронно
	if bestBackendID != "" && modelName != "" {
		state := p.backends[bestBackendID]
		if state != nil {
			p.metricsMgr.mu.RLock()
			metrics, hasMetrics := p.metricsMgr.metrics[bestBackendID]
			p.metricsMgr.mu.RUnlock()
			if hasMetrics && !p.backendHasModel(metrics, modelName) {
				p.warmupModel(bestBackendID, state.Backend.Host, state.Backend.OllamaPort, modelName)
				logger.Get().Infow("rebalance: triggering model warmup on new backend",
					"backend", bestBackendID, "model", modelName)
			}
		}
	}

	return bestBackendID
}

// backendHasModel проверяет, загружена ли модель на бэкенде (Ollama или llama.cpp)
func (p *Proxy) backendHasModel(metrics *types.BackendMetrics, modelName string) bool {
	// Проверяем Ollama модели
	for _, m := range metrics.Ollama.RunningModels {
		if m.Name == modelName || strings.Contains(m.Name, modelName) {
			return true
		}
	}
	// Проверяем llama.cpp модели
	for _, m := range metrics.LlamaCpp.LoadedModels {
		if m.Name == modelName || strings.Contains(m.Name, modelName) {
			return true
		}
	}
	return false
}

// backendHasModelStrict — строгая проверка (без Contains), для prewarm
func (p *Proxy) backendHasModelStrict(metrics *types.BackendMetrics, modelName string) bool {
	for _, m := range metrics.Ollama.RunningModels {
		if m.Name == modelName {
			return true
		}
	}
	for _, m := range metrics.LlamaCpp.LoadedModels {
		if m.Name == modelName {
			return true
		}
	}
	return false
}

// findBackendWithModel - поиск бэкенда с загруженной моделью
func (p *Proxy) findBackendWithModel(modelName string, allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestScore float64 = -1

	for id, state := range p.backends {
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		if !p.checkResourceLimits(id) {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()

		// Согласно документации: model affinity только при loadRatio < threshold
		threshold := p.config.Balancing.Prewarm.TriggerLoadThreshold
		if threshold <= 0 {
			threshold = 0.80
		}
		if maxReqs > 0 && float64(active)/float64(maxReqs) >= threshold {
			continue
		}

		pred := state.Prediction
		if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 300 {
			continue
		}

		p.metricsMgr.mu.RLock()
		metrics, hasMetrics := p.metricsMgr.metrics[id]
		p.metricsMgr.mu.RUnlock()

		if !hasMetrics {
			continue
		}

		if !p.backendHasModel(metrics, modelName) {
			continue
		}

		score := p.calculateScore(id)
		if score > bestScore {
			bestScore = score
			bestBackendID = id
		}
	}

	return bestBackendID
}

// findWarmingBackendForModelUnsafe — поиск WARMING_UP бэкенда (без блокировки, вызывается под p.mu.RLock)
func (p *Proxy) findWarmingBackendForModelUnsafe(model string) string {
	for id, state := range p.backends {
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		state.mu.Lock()
		ws, exists := state.WarmingUpModels[model]
		state.mu.Unlock()
		if exists && ws != nil && time.Now().Before(ws.EstimatedReadyAt) {
			return id
		}
	}
	return ""
}

// findFreeBackendForModelUnsafe — свободный бэкенд с достаточным VRAM (без блокировки)
func (p *Proxy) findFreeBackendForModelUnsafe(model string, allowedTypes []types.BackendType) string {
	var best string
	var maxFree uint64
	for id, state := range p.backends {
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if !p.checkResourceLimits(id) {
			continue
		}
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		if active > 0 || (maxReqs > 0 && active >= maxReqs) {
			continue
		}
		metrics, ok := p.metricsMgr.metrics[id]
		if !ok || metrics.GPU.MemoryTotal == 0 {
			continue
		}
		freeVRAM := metrics.GPU.MemoryTotal - metrics.GPU.MemoryUsed
		needed := estimateModelVRAM(model)
		if freeVRAM <= needed {
			continue
		}
		if !p.backendHasModel(metrics, model) && freeVRAM > maxFree {
			maxFree = freeVRAM
			best = id
		}
	}
	return best
}

// checkModelReadyUnsafe — готова ли модель на бэкенде (без блокировки)
func (p *Proxy) checkModelReadyUnsafe(backendID, model string) bool {
	metrics, ok := p.metricsMgr.metrics[backendID]
	if !ok {
		return false
	}
	return p.backendHasModel(metrics, model)
}

// selectFreeBackendAny — выбор любого свободного healthy бэкенда с наименьшей загрузкой
func (p *Proxy) selectFreeBackendAny(allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackend string
	var bestLoadRatio float64 = 2.0

	for id, state := range p.backends {
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if !p.checkResourceLimits(id) {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()

		if maxReqs >= 0 && active >= maxReqs {
			continue
		}

		loadRatio := 0.0
		if maxReqs > 0 {
			loadRatio = float64(active) / float64(maxReqs)
		}

		if loadRatio < bestLoadRatio {
			bestLoadRatio = loadRatio
			bestBackend = id
		}
	}

	if bestBackend != "" {
		logger.Get().Infow("selectFreeBackendAny: selected", "backend", bestBackend, "load_ratio", bestLoadRatio)
		atomic.AddInt64(&p.queueMgr.dispatchLoad, 1)
	}
	return bestBackend
}

// selectByResources - выбор бэкенда по ресурсам
func (p *Proxy) selectByResources(allowedTypes []types.BackendType) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackend string
	var bestScore float64 = -1
	var bestLoadRatio float64 = 2.0
	var bestLastUsed time.Time

	for id, state := range p.backends {
		if !isBackendTypeAllowed(normalizeBackendType(state.Backend.Type), allowedTypes) {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		if !p.checkResourceLimits(id) {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		lastUsed := state.LastUsed
		state.mu.Unlock()

		// Не отсеиваем бэкенд если слоты заняты — пусть tryAcquireSlot в dispatchRequest
		// решает можно ли захватить. Если слот занят — dispatch вернёт ErrNoBackendAvailable
		// и queue requeue'ит запрос, дожидаясь освобождения.
		pred := state.Prediction
		if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 300 {
			continue
		}

		score := p.calculateScore(id)

		loadRatio := 0.0
		if maxReqs > 0 {
			loadRatio = float64(active) / float64(maxReqs)
		}

		if score > bestScore ||
			(score == bestScore && loadRatio < bestLoadRatio) ||
			(score == bestScore && loadRatio == bestLoadRatio && lastUsed.Before(bestLastUsed)) {
			bestScore = score
			bestBackend = id
			bestLoadRatio = loadRatio
			bestLastUsed = lastUsed
		}
	}

	if bestBackend != "" {
		if state, ok := p.backends[bestBackend]; ok {
			state.mu.Lock()
			state.LastUsed = time.Now()
			state.mu.Unlock()
		}
		if p.config.Balancing.UseEnhancedScoring {
			atomic.AddInt64(&p.queueMgr.dispatchLoad, 1)
		} else {
			atomic.AddInt64(&p.queueMgr.dispatchConfig, 1)
		}
	}

	return bestBackend
}