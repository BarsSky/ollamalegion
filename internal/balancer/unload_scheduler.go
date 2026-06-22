package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// UnloadScheduler — LRU выгрузка неиспользуемых моделей для освобождения VRAM
type UnloadScheduler struct {
	proxy          *Proxy
	mu             sync.RWMutex
	lastUsed       map[string]time.Time // key: "modelName@backendID" → lastRequest
	checkInterval  time.Duration
	idleTimeout    time.Duration
	stopCh         chan struct{}
	enabled        bool
}

// NewUnloadScheduler — создание планировщика выгрузки.
//
// ВАЖНО (2026-06-22): по умолчанию планировщик ВЫКЛЮЧЕН. Раньше здесь всегда
// стоял enabled=true с idleTimeout=30m, и балансировщик раз в 5 минут помечал
// модели как «unloading» через WarmingUpModels (без реальной выгрузки с бэкенда).
// Это приводило к:
//   - пометке моделей как «выгружаемых» через 30 мин idle, даже если пользователь
//     активно с ними работает (LRU-эвристика работала по lastUse на бэкенде,
//     а не по реальной активности пользователя);
//   - путанице в мониторе и WebUI (модель «выгружена» по мнению балансера, но
//     реально живёт в VRAM на cppworker).
//
// Теперь по умолчанию:
//   - idleTimeout = 0 → фоновый цикл не запускается,
//   - enabled = false,
//   - для включения нужно явно задать balancing.modelInstances.idleUnloadAfter,
//     например "30m" или "1h".
func NewUnloadScheduler(proxy *Proxy) *UnloadScheduler {
	idleTimeout := time.Duration(0)
	if proxy.config.Balancing.ModelInstances.IdleUnloadAfter != "" {
		if d, err := time.ParseDuration(proxy.config.Balancing.ModelInstances.IdleUnloadAfter); err == nil {
			idleTimeout = d
		}
	}

	enabled := idleTimeout > 0
	if !enabled {
		logger.Get().Infow("unloadScheduler: disabled (idleUnloadAfter not set or 0). "+
			"Models will remain in VRAM until explicit /api/models/unload or container restart.",
			"idleTimeout", idleTimeout.String())
	}

	us := &UnloadScheduler{
		proxy:         proxy,
		lastUsed:      make(map[string]time.Time),
		checkInterval: 5 * time.Minute,
		idleTimeout:   idleTimeout,
		stopCh:        make(chan struct{}),
		enabled:       enabled,
	}

	return us
}

// RecordModelUse — запись использования модели на бэкенде
func (us *UnloadScheduler) RecordModelUse(modelName, backendID string) {
	if !us.enabled || modelName == "" || backendID == "" {
		return
	}
	key := modelName + "@" + backendID
	us.mu.Lock()
	us.lastUsed[key] = time.Now()
	us.mu.Unlock()
}

// Start — запуск фонового цикла проверки
func (us *UnloadScheduler) Start() {
	if !us.enabled {
		return
	}
	go us.loop()
}

// Stop — остановка планировщика
func (us *UnloadScheduler) Stop() {
	us.mu.Lock()
	defer us.mu.Unlock()

	if us.stopCh != nil {
		select {
		case <-us.stopCh:
		default:
			close(us.stopCh)
		}
		us.stopCh = nil
	}
}

// SetEnabled — включение/отключение планировщика
func (us *UnloadScheduler) SetEnabled(enabled bool) {
	us.mu.Lock()
	defer us.mu.Unlock()

	if enabled && !us.enabled {
		us.enabled = true
		us.stopCh = make(chan struct{})
		go us.loop()
	} else if !enabled && us.enabled {
		us.enabled = false
		if us.stopCh != nil {
			select {
			case <-us.stopCh:
			default:
				close(us.stopCh)
			}
			us.stopCh = nil
		}
	}
}

// loop — основной цикл проверки idle моделей.
// stopCh копируется под RLock чтобы избежать data race с Stop/SetEnabled.
func (us *UnloadScheduler) loop() {
	ticker := time.NewTicker(us.checkInterval)
	defer ticker.Stop()

	us.mu.RLock()
	stopCh := us.stopCh
	us.mu.RUnlock()

	for {
		select {
		case <-ticker.C:
			us.checkAndUnload()
		case <-stopCh:
			return
		}
	}
}

// checkAndUnload — проверка моделей на простой и выгрузка
func (us *UnloadScheduler) checkAndUnload() {
	candidates := us.getUnloadCandidates()
	if len(candidates) == 0 {
		return
	}

	for _, c := range candidates {
		us.unloadModel(c.Model, c.BackendID)
	}
}

// UnloadCandidate — кандидат на выгрузку
type UnloadCandidate struct {
	Model     string
	BackendID string
	IdleTime  time.Duration
}

// getUnloadCandidates — возвращает список моделей, которые не использовались > idleTimeout
func (us *UnloadScheduler) getUnloadCandidates() []UnloadCandidate {
	us.mu.Lock()
	defer us.mu.Unlock()

	now := time.Now()

	// Собираем все запущенные модели по бэкендам
	us.proxy.metricsMgr.mu.RLock()
	defer us.proxy.metricsMgr.mu.RUnlock()

	// Защита: не выгружаем модели с активными запросами
	activeModels := us.activeModelsMap()

	var candidates []UnloadCandidate
	for id, metrics := range us.proxy.metricsMgr.metrics {
		// Пропускаем недоступные бэкенды
		us.proxy.mu.RLock()
		state, ok := us.proxy.backends[id]
		us.proxy.mu.RUnlock()
		if !ok || state.Backend.Status != types.StatusHealthy {
			continue
		}

		// Пропускаем модели в процессе загрузки/прогрева
		state.mu.Lock()
		warmingUp := state.WarmingUpModels
		state.mu.Unlock()

		for _, model := range metrics.Ollama.RunningModels {
			key := model.Name + "@" + id

			// Пропускаем warming модели
			if _, isWarming := warmingUp[model.Name]; isWarming {
				continue
			}

			// Пропускаем если есть активные запросы к этой модели
			if activeModels[key] {
				continue
			}

			lastUse, recorded := us.lastUsed[key]
			if !recorded {
				// Модель загружена но никогда не использовалась — используем время загрузки
				us.lastUsed[key] = now
				continue
			}

			idleTime := now.Sub(lastUse)
			if idleTime >= us.idleTimeout {
				candidates = append(candidates, UnloadCandidate{
					Model:     model.Name,
					BackendID: id,
					IdleTime:  idleTime,
				})
			}
		}
	}

	return candidates
}

// activeModelsMap — карта моделей с активными запросами
func (us *UnloadScheduler) activeModelsMap() map[string]bool {
	result := make(map[string]bool)

	us.proxy.queueMgr.processingMu.RLock()
	defer us.proxy.queueMgr.processingMu.RUnlock()

	for _, req := range us.proxy.queueMgr.processing {
		if req.Model != "" && req.Target != "" {
			key := req.Model + "@" + req.Target
			result[key] = true
		}
	}

	// Также проверяем pending (находятся в очереди)
	us.proxy.queueMgr.pendingMu.RLock()
	defer us.proxy.queueMgr.pendingMu.RUnlock()

	for _, req := range us.proxy.queueMgr.pending {
		if req.Model != "" && req.Target != "" {
			key := req.Model + "@" + req.Target
			result[key] = true
		}
	}

	return result
}

// unloadModel — отправка запроса на выгрузку модели (если поддерживается Ollama)
func (us *UnloadScheduler) unloadModel(modelName, backendID string) {
	us.proxy.mu.RLock()
	state, ok := us.proxy.backends[backendID]
	us.proxy.mu.RUnlock()
	if !ok {
		return
	}

	// Проверяем статус перед выгрузкой
	if state.Backend.Status != types.StatusHealthy {
		return
	}

	// Помечаем модель как выгружаемую чтобы избежать гонки
	state.mu.Lock()
	if state.WarmingUpModels == nil {
		state.WarmingUpModels = make(map[string]*types.WarmupState)
	}
	state.WarmingUpModels[modelName] = &types.WarmupState{
		StartedAt:        time.Now(),
		EstimatedReadyAt: time.Now(),
		TriggerReason:    "lru_unload",
	}
	state.mu.Unlock()

	logger.Get().Infow("unloadScheduler: unloading idle model",
		"backend", backendID, "model", modelName)

	// Очищаем запись lastUsed
	key := modelName + "@" + backendID
	us.mu.Lock()
	delete(us.lastUsed, key)
	us.mu.Unlock()

	// NOTE: Ollama API не имеет прямого endpoint для выгрузки модели.
	// Модель будет выгружена при загрузке другой модели (LRU в Ollama) или
	// автоматически через ollama keep_alive. Мы помечаем модель как выгружаемую
	// чтобы балансер не отправлял на неё новые запросы.
	// В будущем можно добавить выгрузку через ollama API или перезапуск процесса.

	// Убираем из warmingUpModels через keepalive-таймер
	go func() {
		time.Sleep(2 * time.Minute)
		state.mu.Lock()
		delete(state.WarmingUpModels, modelName)
		state.mu.Unlock()
	}()
}