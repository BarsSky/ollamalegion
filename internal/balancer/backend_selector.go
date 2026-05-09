package balancer

import (
	"strings"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// selectBackend - выбор бэкенда для запроса (5-этапный алгоритм)
func (p *Proxy) selectBackend(model string) string {
	// RPC Module: Model Replication — если модель в группе репликации, выбираем из реплик
	if p.replicationSelector != nil {
		if selected := p.replicationSelector.Select(model); selected != "" {
			logger.Get().Infow("routed through model replication",
				"model", model, "backend", selected)
			return selected
		}
	}

	// RPC Module: VirtualModel Router — если модель виртуальная, маршрутизируем через неё
	if p.virtualModelRouter != nil {
		if selected := p.virtualModelRouter.GetRegistry().IsVirtualModel(model); selected {
			logger.Get().Infow("routing through virtual model", "model", model)
			// Virtual Model Pipeline выполняется в proxyRequest или queueRequest,
			// здесь возвращаем специальный маркер, чтобы outer code знал,
			// что это виртуальная модель
			return "__virtual__"
		}
	}

	candidates := p.expandCandidates(model)

	// 1. Model Affinity (LOADED) — P1

	for _, group := range candidates {
		if group.Priority != 1 {
			break
		}
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
			if loadRatio < threshold {
				atomic.AddInt64(&p.queueMgr.dispatchAffinity, 1)
				return backendID
			}
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
	return p.selectByResources()
}

// selectBackendExcluding - выбор бэкенда, исключая указанные
func (p *Proxy) selectBackendExcluding(model string, exclude map[string]bool) string {
	p.mu.RLock()

	if p.config.Balancing.ModelAffinity && model != "" {
		if backend := p.findBackendWithModelExcluding(model, exclude); backend != "" {
			p.mu.RUnlock()
			return backend
		}
	}
	p.mu.RUnlock()

	backend := p.selectByResourcesExcluding(exclude)
	if backend == "" {
		logger.Get().Warnw("no available backends with exclusions")
	}
	return backend
}

// findBackendWithModelExcluding - поиск бэкенда с моделью, исключая указанные
func (p *Proxy) findBackendWithModelExcluding(modelName string, exclude map[string]bool) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestScore float64 = -1

	for id, state := range p.backends {
		if exclude[id] {
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

		hasModel := false
		for _, m := range metrics.Ollama.RunningModels {
			if m.Name == modelName || strings.Contains(m.Name, modelName) {
				hasModel = true
				break
			}
		}
		if !hasModel {
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
	for _, m := range metrics.Ollama.RunningModels {
		if m.Name == modelName || strings.Contains(m.Name, modelName) {
			return true
		}
	}
	return false
}

// selectByResourcesExcluding - выбор по ресурсам с исключением бэкендов
func (p *Proxy) selectByResourcesExcluding(exclude map[string]bool) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackend string
	var bestScore float64 = -1

	for id, state := range p.backends {
		if exclude[id] {
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
func (p *Proxy) findLessLoadedBackendWithModel(modelName, excludeBackendID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestLoadRatio float64 = 2.0

	for id, state := range p.backends {
		if id == excludeBackendID {
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

		hasModel := false
		for _, m := range metrics.Ollama.RunningModels {
			if m.Name == modelName || strings.Contains(m.Name, modelName) {
				hasModel = true
				break
			}
		}
		if !hasModel {
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

// findLessLoadedBackendAny — поиск любого менее загруженного healthy бэкенда
// Если модель не загружена на выбранном бэкенде — запускает асинхронный warmup
func (p *Proxy) findLessLoadedBackendAny(modelName, excludeBackendID string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestLoadRatio float64 = 2.0

	for id, state := range p.backends {
		if id == excludeBackendID {
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
		if loadRatio < bestLoadRatio {
			bestLoadRatio = loadRatio
			bestBackendID = id
		}
	}

	// Если нашли бэкенд без модели — запускаем warmup асинхронно
	if bestBackendID != "" && modelName != "" {
		p.metricsMgr.mu.RLock()
		metrics, hasMetrics := p.metricsMgr.metrics[bestBackendID]
		p.metricsMgr.mu.RUnlock()
		if hasMetrics {
			hasModel := false
			for _, m := range metrics.Ollama.RunningModels {
				if m.Name == modelName || strings.Contains(m.Name, modelName) {
					hasModel = true
					break
				}
			}
			if !hasModel {
				state := p.backends[bestBackendID]
				p.warmupModel(bestBackendID, state.Backend.Host, state.Backend.OllamaPort, modelName)
				logger.Get().Infow("rebalance: triggering model warmup on new backend",
					"backend", bestBackendID, "model", modelName)
			}
		}
	}

	return bestBackendID
}

// findBackendWithModel - поиск бэкенда с загруженной моделью
func (p *Proxy) findBackendWithModel(modelName string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackendID string
	var bestScore float64 = -1

	for id, state := range p.backends {
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

		hasModel := false
		for _, m := range metrics.Ollama.RunningModels {
			if m.Name == modelName || strings.Contains(m.Name, modelName) {
				hasModel = true
				break
			}
		}
		if !hasModel {
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
func (p *Proxy) findFreeBackendForModelUnsafe(model string) string {
	var best string
	var maxFree uint64
	for id, state := range p.backends {
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
		hasModel := false
		for _, m := range metrics.Ollama.RunningModels {
			if m.Name == model || strings.Contains(m.Name, model) {
				hasModel = true
				break
			}
		}
		if !hasModel && freeVRAM > maxFree {
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
	for _, m := range metrics.Ollama.RunningModels {
		if m.Name == model || strings.Contains(m.Name, model) {
			return true
		}
	}
	return false
}

// selectFreeBackendAny — выбор любого свободного healthy бэкенда с наименьшей загрузкой
func (p *Proxy) selectFreeBackendAny() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackend string
	var bestLoadRatio float64 = 2.0

	for id, state := range p.backends {
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
func (p *Proxy) selectByResources() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackend string
	var bestScore float64 = -1
	var bestLoadRatio float64 = 2.0
	var bestLastUsed time.Time

	for id, state := range p.backends {
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

		if maxReqs >= 0 && active >= maxReqs {
			continue
		}

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