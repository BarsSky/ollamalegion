package balancer

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ModelInstanceController — контроллер количества экземпляров модели.
// Поддерживает min_instances, max_instances, idle_unload_after.
type ModelInstanceController struct {
	proxy          *Proxy
	config         types.ModelInstanceConfig
	mu             sync.Mutex
	pendingUnloads map[string]time.Time // model:backendID -> when requested
	stopCh         chan struct{}
	wg             sync.WaitGroup
}

// NewModelInstanceController — создание контроллера экземпляров модели
func NewModelInstanceController(proxy *Proxy, config types.ModelInstanceConfig) *ModelInstanceController {
	return &ModelInstanceController{
		proxy:          proxy,
		config:         config,
		pendingUnloads: make(map[string]time.Time),
		stopCh:         make(chan struct{}),
	}
}

// Start — запуск фонового цикла (30 сек)
func (mic *ModelInstanceController) Start() {
	if mic.config.DefaultMinInstances <= 0 && mic.config.DefaultMaxInstances <= 0 {
		logger.Get().Infow("model instance controller disabled (no min/max configured)")
		return
	}
	mic.wg.Add(1)
	go mic.loop(30 * time.Second)
	logger.Get().Infow("model instance controller started",
		"min_instances", mic.config.DefaultMinInstances,
		"max_instances", mic.config.DefaultMaxInstances,
		"idle_unload", mic.config.IdleUnloadAfter,
	)
}

// Stop — остановка контроллера
func (mic *ModelInstanceController) Stop() {
	close(mic.stopCh)
	mic.wg.Wait()
	logger.Get().Infow("model instance controller stopped")
}

// loop — основной цикл
func (mic *ModelInstanceController) loop(interval time.Duration) {
	defer mic.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			mic.reconcile()
		case <-mic.stopCh:
			return
		}
	}
}

// reconcile — сверка фактического количества экземпляров модели с желаемым
func (mic *ModelInstanceController) reconcile() {
	mic.mu.Lock()
	defer mic.mu.Unlock()

	mic.proxy.mu.RLock()
	backends := make(map[string]*BackendState, len(mic.proxy.backends))
	for id, state := range mic.proxy.backends {
		backends[id] = state
	}
	mic.proxy.mu.RUnlock()

	// Собираем информацию: model -> []backendID (где загружена)
	modelInstances := make(map[string][]string)

	for id, state := range backends {
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		mic.proxy.metricsMgr.mu.RLock()
		metrics, ok := mic.proxy.metricsMgr.metrics[id]
		mic.proxy.metricsMgr.mu.RUnlock()
		if !ok {
			continue
		}
		for _, m := range metrics.Ollama.RunningModels {
			modelInstances[m.Name] = append(modelInstances[m.Name], id)
		}
	}

	// Для каждой модели проверяем лимиты
	for model, instanceIDs := range modelInstances {
		count := len(instanceIDs)

		// --- Ниже минимума: загружаем ---
		if count < mic.config.DefaultMinInstances {
			needed := mic.config.DefaultMinInstances - count
			logger.Get().Infow("model below min instances, loading on free backend",
				"model", model,
				"current", count,
				"min", mic.config.DefaultMinInstances,
				"needed", needed,
			)
			mic.ensureInstances(model, needed, instanceIDs, backends)
		}

		// --- Выше максимума: выгружаем ---
		if count > mic.config.DefaultMaxInstances {
			excess := count - mic.config.DefaultMaxInstances
			logger.Get().Infow("model above max instances, unloading",
				"model", model,
				"current", count,
				"max", mic.config.DefaultMaxInstances,
				"excess", excess,
			)
			mic.unloadExcessInstances(model, instanceIDs, excess, backends)
		}

		// --- Idle unload (модель не используется > idle_unload_after) ---
		mic.checkIdleUnload(model, instanceIDs, backends)
	}
}

// ensureInstances — загрузка модели на свободных бэкендах
func (mic *ModelInstanceController) ensureInstances(model string, needed int, alreadyOnIDs []string, backends map[string]*BackendState) {
	exclude := make(map[string]bool)
	for _, id := range alreadyOnIDs {
		exclude[id] = true
	}
	// Исключаем также бэкенды, на которых уже идёт prewarm этой модели
	for id, state := range backends {
		state.mu.Lock()
		if ws, ok := state.WarmingUpModels[model]; ok && time.Now().Before(ws.EstimatedReadyAt) {
			exclude[id] = true
		}
		state.mu.Unlock()
	}

	loaded := 0
	for id, state := range backends {
		if loaded >= needed {
			break
		}
		if exclude[id] {
			continue
		}
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		if maxReqs > 0 && active >= maxReqs {
			continue
		}

		mic.proxy.metricsMgr.mu.RLock()
		metrics, ok := mic.proxy.metricsMgr.metrics[id]
		mic.proxy.metricsMgr.mu.RUnlock()
		if !ok {
			continue
		}

		estimatedVRAM := estimateModelVRAM(model)
		if metrics.GPU.MemoryFree < estimatedVRAM {
			continue
		}

		// Загрузка
		go mic.proxy.warmupModel(id, state.Backend.Host, state.Backend.OllamaPort, model)
		loaded++
		logger.Get().Infow("instance controller: loading model",
			"backend", id,
			"model", model,
		)
	}
}

// unloadExcessInstances — выгрузка избыточных экземпляров модели
func (mic *ModelInstanceController) unloadExcessInstances(model string, instanceIDs []string, excess int, backends map[string]*BackendState) {
	// Сортируем бэкенды по загрузке (выгружаем с наименее загруженных)
	type scoredBackend struct {
		id        string
		loadRatio float64
		active    int
	}
	var scored []scoredBackend
	for _, id := range instanceIDs {
		state, ok := backends[id]
		if !ok {
			continue
		}
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		ratio := 0.0
		if maxReqs > 0 {
			ratio = float64(active) / float64(maxReqs)
		}
		scored = append(scored, scoredBackend{id: id, loadRatio: ratio, active: active})
	}

	// Сортируем: сначала наименее загруженные (их и выгружаем)
	for i := 0; i < len(scored)-1; i++ {
		for j := i + 1; j < len(scored); j++ {
			if scored[i].loadRatio > scored[j].loadRatio || (scored[i].loadRatio == scored[j].loadRatio && scored[i].active > scored[j].active) {
				scored[i], scored[j] = scored[j], scored[i]
			}
		}
	}

	unloaded := 0
	for _, s := range scored {
		if unloaded >= excess {
			break
		}
		state := backends[s.id]
		mic.unloadModel(s.id, state, model)
		unloaded++
	}
}

// checkIdleUnload — проверка idle моделей для выгрузки
func (mic *ModelInstanceController) checkIdleUnload(model string, instanceIDs []string, backends map[string]*BackendState) {
	if mic.config.IdleUnloadAfter == "" {
		return
	}
	idleDuration, err := time.ParseDuration(mic.config.IdleUnloadAfter)
	if err != nil {
		logger.Get().Warnw("invalid idle_unload_after duration", "value", mic.config.IdleUnloadAfter, "error", err)
		return
	}

	// Проверяем сессии: если нет активных сессий с этой моделью — можно выгружать
	allSessions := mic.proxy.sessionMgr.GetAll()
	activeModels := make(map[string]int)
	now := time.Now()
	for _, s := range allSessions {
		if now.Sub(s.LastRequestAt) < idleDuration {
			activeModels[s.Model]++
		}
	}

	// Если есть активные сессии — не выгружаем
	if _, active := activeModels[model]; active {
		return
	}

	// Не выгружаем ВСЕ экземпляры — оставляем минимум один
	if len(instanceIDs) <= mic.config.DefaultMinInstances {
		return
	}

	// Выгружаем один экземпляр (самый незагруженный без активных запросов)
	var bestID string
	var bestRatio float64 = 2.0
	for _, id := range instanceIDs {
		state := backends[id]
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		if active > 0 {
			continue
		}
		ratio := 0.0
		if maxReqs > 0 {
			ratio = float64(active) / float64(maxReqs)
		}
		if ratio < bestRatio {
			bestRatio = ratio
			bestID = id
		}
	}

	if bestID == "" {
		return
	}

	logger.Get().Infow("idle model unload triggered",
		"model", model,
		"backend", bestID,
		"idle_duration", idleDuration,
	)
	mic.unloadModel(bestID, backends[bestID], model)
}

// unloadModel — выгрузка модели с бэкенда через Ollama API
func (mic *ModelInstanceController) unloadModel(backendID string, state *BackendState, model string) {
	key := fmt.Sprintf("%s:%s", model, backendID)
	mic.pendingUnloads[key] = time.Now()

	go func() {
		body := fmt.Sprintf(`{"model":"%s","prompt":"unload","stream":false,"num_predict":1,"keep_alive":0}`, model)
		url := fmt.Sprintf("http://%s:%d/api/generate", state.Backend.Host, state.Backend.OllamaPort)
		client := mic.proxy.client

		for retry := 0; retry < 2; retry++ {
			resp, err := doPost(client, url, body)
			if err != nil {
				logger.Get().Errorw("unload: request failed",
					"backend", backendID, "model", model,
					"retry", retry, "error", err)
				time.Sleep(2 * time.Second)
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == 200 {
				logger.Get().Infow("model unloaded successfully", "backend", backendID, "model", model)
				break
			}
			logger.Get().Warnw("unload: unexpected status", "backend", backendID, "status", resp.StatusCode, "model", model)
			break
		}
		mic.mu.Lock()
		delete(mic.pendingUnloads, key)
		mic.mu.Unlock()
	}()
}

// doPost — helper для POST-запросов с JSON
func doPost(client *http.Client, url, body string) (*http.Response, error) {
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}