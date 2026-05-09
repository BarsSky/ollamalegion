package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// PrewarmController — контроллер превентивной загрузки моделей.
// Триггеры:
//   - Загрузка бэкенда > TriggerLoadThreshold (70%)
//   - Есть свободный бэкенд без модели M, но совместимый по ресурсам
//   - Количество ожидающих запросов к модели M > 0
type PrewarmController struct {
	proxy       *Proxy
	config      types.PrewarmConfig
	mu          sync.Mutex
	activeWarms map[string]time.Time // model -> start time, предотвращает дублирование
	stopCh      chan struct{}
	wg          sync.WaitGroup
}

// NewPrewarmController — создание контроллера превентивной загрузки
func NewPrewarmController(proxy *Proxy, config types.PrewarmConfig) *PrewarmController {
	return &PrewarmController{
		proxy:       proxy,
		config:      config,
		activeWarms: make(map[string]time.Time),
		stopCh:      make(chan struct{}),
	}
}

// Evaluate — экспортируемая обёртка для тестов
func (pc *PrewarmController) Evaluate() {
	pc.evaluate()
}

// Start — запуск фонового цикла проверки
func (pc *PrewarmController) Start() {
	if !pc.config.Enabled {
		logger.Get().Infow("prewarm controller disabled")
		return
	}
	interval := time.Duration(pc.config.CheckIntervalSec) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	pc.wg.Add(1)
	go pc.loop(interval)
	logger.Get().Infow("prewarm controller started", "interval", interval)
}

// Stop — остановка контроллера
func (pc *PrewarmController) Stop() {
	close(pc.stopCh)
	pc.wg.Wait()
	logger.Get().Infow("prewarm controller stopped")
}

// loop — основной цикл проверки
func (pc *PrewarmController) loop(interval time.Duration) {
	defer pc.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pc.evaluate()
		case <-pc.stopCh:
			return
		}
	}
}

// evaluate — оценка необходимости превентивной загрузки
func (pc *PrewarmController) evaluate() {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	// Очистка устаревших записей (старше 5 минут — загрузка уже должна завершиться)
	now := time.Now()
	for model, startTime := range pc.activeWarms {
		if now.Sub(startTime) > 5*time.Minute {
			delete(pc.activeWarms, model)
		}
	}

	// Не запускаем больше maxPrewarmPerCycle одновременных загрузок
	if len(pc.activeWarms) >= pc.config.MaxPrewarmPerCycle {
		return
	}

	pc.proxy.mu.RLock()
	backends := make(map[string]*BackendState, len(pc.proxy.backends))
	for id, state := range pc.proxy.backends {
		backends[id] = state
	}
	pc.proxy.mu.RUnlock()

	// Собираем статистику: какие модели загружены на каких бэкендах и их загрузка
	modelOnBackends := make(map[string][]string)    // model -> []backendID
	backendLoad := make(map[string]float64)         // backendID -> load ratio
	modelQueueDepth := make(map[string]int)         // model -> pending requests

	// Анализируем очередь
	pc.proxy.queueMgr.pendingMu.RLock()
	for _, req := range pc.proxy.queueMgr.pending {
		if req.Model != "" {
			modelQueueDepth[req.Model]++
		}
	}
	pc.proxy.queueMgr.pendingMu.RUnlock()

	for id, state := range backends {
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		pc.proxy.metricsMgr.mu.RLock()
		metrics, ok := pc.proxy.metricsMgr.metrics[id]
		pc.proxy.metricsMgr.mu.RUnlock()
		if !ok {
			continue
		}

		active := metrics.Ollama.ActiveRequests
		maxReqs := state.Backend.MaxConcurrentReqs
		if maxReqs > 0 {
			backendLoad[id] = float64(active) / float64(maxReqs)
		}

		for _, m := range metrics.Ollama.RunningModels {
			modelOnBackends[m.Name] = append(modelOnBackends[m.Name], id)
		}
	}

	// Проверяем каждую модель: нужен ли prewarm
	for model, backendIDs := range modelOnBackends {
		// Проверяем каждый бэкенд с этой моделью
		maxLoad := 0.0
		for _, bid := range backendIDs {
			if load, ok := backendLoad[bid]; ok && load > maxLoad {
				maxLoad = load
			}
		}

		shouldPrewarm := maxLoad > pc.config.TriggerLoadThreshold

		// Дополнительный триггер: есть ожидающие запросы в очереди
		if depth, ok := modelQueueDepth[model]; ok && depth > 0 {
			shouldPrewarm = true
			logger.Get().Debugw("prewarm triggered by queue depth",
				"model", model,
				"queue_depth", depth,
			)
		}

		if !shouldPrewarm {
			continue
		}

		// Уже в процессе загрузки?
		if _, warming := pc.activeWarms[model]; warming {
			continue
		}

		if len(pc.activeWarms) >= pc.config.MaxPrewarmPerCycle {
			break
		}

		// Ищем свободный бэкенд без этой модели
		freeBackend := pc.findFreeBackendForModel(model, backendIDs, backends)
		if freeBackend == "" {
			continue
		}

		// Запускаем превентивную загрузку
		state := backends[freeBackend]
		pc.triggerPrewarm(freeBackend, state.Backend.Host, state.Backend.OllamaPort, model)
		pc.activeWarms[model] = now
	}
}

// findFreeBackendForModel — поиск здорового бэкенда без модели M с достаточным VRAM
func (pc *PrewarmController) findFreeBackendForModel(model string, excludeIDs []string, backends map[string]*BackendState) string {
	excludeMap := make(map[string]bool)
	for _, id := range excludeIDs {
		excludeMap[id] = true
	}

	var bestID string
	var bestFreeVRAM uint64

	for id, state := range backends {
		if excludeMap[id] {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		// Не загружаем если бэкенд уже перегружен
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		if maxReqs > 0 && active >= maxReqs {
			continue
		}

		pc.proxy.metricsMgr.mu.RLock()
		metrics, ok := pc.proxy.metricsMgr.metrics[id]
		pc.proxy.metricsMgr.mu.RUnlock()
		if !ok {
			continue
		}

		// Проверяем, нет ли уже этой модели
		alreadyHas := false
		for _, m := range metrics.Ollama.RunningModels {
			if m.Name == model {
				alreadyHas = true
				break
			}
		}
		if alreadyHas {
			continue
		}

		// Проверяем VRAM
		estimatedVRAM := estimateModelVRAM(model)
		freeVRAM := metrics.GPU.MemoryFree
		if freeVRAM < estimatedVRAM {
			continue
		}

		if freeVRAM > bestFreeVRAM {
			bestFreeVRAM = freeVRAM
			bestID = id
		}
	}

	return bestID
}

// triggerPrewarm — запуск превентивной загрузки модели с пометкой WARMING_UP
func (pc *PrewarmController) triggerPrewarm(backendID, host string, port int, model string) {
	pc.proxy.mu.RLock()
	state, ok := pc.proxy.backends[backendID]
	pc.proxy.mu.RUnlock()
	if !ok {
		return
	}

	now := time.Now()
	estimatedReadyAt := now.Add(30 * time.Second) // оценка: 30 секунд на загрузку

	// Помечаем бэкенд как WARMING_UP для этой модели
	state.mu.Lock()
	if state.WarmingUpModels == nil {
		state.WarmingUpModels = make(map[string]*types.WarmupState)
	}
	state.WarmingUpModels[model] = &types.WarmupState{
		StartedAt:        now,
		EstimatedReadyAt: estimatedReadyAt,
		TriggerReason:    "prewarm_controller",
	}
	state.mu.Unlock()

	logger.Get().Infow("prewarm triggered",
		"backend", backendID,
		"model", model,
		"host", host,
		"estimated_ready", estimatedReadyAt,
	)

	// Запускаем асинхронную загрузку
	pc.proxy.warmupModel(backendID, host, port, model)

	// После загрузки убираем из WarmingUpModels
	time.AfterFunc(60*time.Second, func() {
		state.mu.Lock()
		delete(state.WarmingUpModels, model)
		state.mu.Unlock()
		pc.mu.Lock()
		delete(pc.activeWarms, model)
		pc.mu.Unlock()
		logger.Get().Infow("prewarm completed (cleanup)", "backend", backendID, "model", model)
	})
}