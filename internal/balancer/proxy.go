package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/internal/distinference"
	"ollama-loadbalancer/internal/modelreplication"
	"ollama-loadbalancer/internal/rpccoordinator"
	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// Proxy - HTTP прокси для балансировки запросов
type Proxy struct {
	config          *types.LoadBalancerConfig
	backends        map[string]*BackendState
	mu              sync.RWMutex
	roundRobin      int
	sessionMgr      *SessionManager
	metricsMgr      *MetricsManager
	queueMgr        *QueueManager
	predictor       *Predictor
	client          *http.Client      // Клиент для обычных запросов
	streamingClient *http.Client      // Клиент для streaming/SSE запросов (без таймаута)
	statePath       string            // Путь к state файлу
	saveTimer       *time.Timer       // Таймер для debounced autosave
	saveMu          sync.Mutex        // Мьютекс для защиты saveTimer
	totalRequests   int64             // Atomic: всего запросов через прокси
	ollamaRouter    *OllamaRouter     // Маршрутизатор Ollama API endpoint'ов

	// EventBus (вынесен в eventbus.go)
	eventBus *EventBus

	// Background controllers
		AutoPull        *AutoPullManager       // Менеджер автоматической загрузки моделей (Pull-on-Demand)
	unloadScheduler *UnloadScheduler       // Планировщик выгрузки моделей
	weightTuner     *AdaptiveWeightTuner   // Адаптивный тюнер весов
	agentChecker    *agentTimeoutChecker   // Проверка таймаута агентов

	// RPC model distribution modules
	modelReplication    *modelreplication.ModelGroupManager
	replicationSelector *modelreplication.GroupAwareSelector
	replicationCtrl     *modelreplication.GroupController
	rpcCoordinator      *rpccoordinator.Coordinator
	virtualModels       *virtualmodel.Registry
	virtualModelRouter  *virtualmodel.Router
	distInference       *distinference.Engine
}


// NewProxy - создание нового прокси
func NewProxy(config *types.LoadBalancerConfig) *Proxy {
	// Транспорт для обычных запросов
	regularTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	// Транспорт для streaming запросов - без сжатия и с увеличенными буферами
	streamingTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // Важно для SSE
	}

	// Определяем TTL сессий из конфигурации (дефолт 15 минут)
	sessionTTL := time.Duration(config.Balancing.SessionTTL) * time.Second
	if sessionTTL <= 0 {
		sessionTTL = 15 * time.Minute
	}

	p := &Proxy{
		config:     config,
		backends:   make(map[string]*BackendState),
		sessionMgr: NewSessionManagerWithTTL(sessionTTL),
		metricsMgr: NewMetricsManager(),
		predictor:  NewPredictor(),
		statePath:  config.LoadBalancer.StatePath,
		eventBus:   NewEventBus(),
		client: &http.Client{
			Timeout:   time.Duration(config.Balancing.RequestTimeout) * time.Second,
			Transport: regularTransport,
		},
		// Streaming клиент без общего таймаута для long-running запросов
		streamingClient: &http.Client{
			Timeout:   0, // Нет таймаута для streaming
			Transport: streamingTransport,
		},
	}

	// QueueManager создаём после инициализации p, чтобы передать корректный proxy
	p.queueMgr = NewQueueManager(p, config.Balancing.QueueMaxSize, config.Balancing.QueueWorkers, time.Duration(config.Balancing.QueueTimeout)*time.Second)

	// Инициализация бэкендов из config
	for i := range config.Backends {
		backend := config.Backends[i]
		p.backends[backend.ID] = &BackendState{
			Backend:         &backend,
			ActiveReqs:      0,
			LastUsed:        time.Time{},
			WarmingUpModels: make(map[string]*types.WarmupState),
		}
	}

	// Загрузка сохранённого state (поверх config — runtime-данные приоритетнее)
	if err := p.LoadState(); err != nil {
		logger.Get().Infow("state file not loaded, using config only", "error", err)
	}

	// Запуск фоновой проверки таймаута агентов (60 секунд — увеличено для стабильности)
	go p.StartAgentTimeoutChecker(60 * time.Second)

	// Инициализация OllamaRouter для агрегации и целевой маршрутизации API
	p.ollamaRouter = NewOllamaRouter(p)

	// Инициализация AutoPullManager (Pull-on-Demand)
	if config.Balancing.AutoPull.Enabled {
		p.AutoPull = NewAutoPullManager(p, config.Balancing.AutoPull)
		logger.Get().Infow("auto-pull manager initialized",
			"enabled", config.Balancing.AutoPull.Enabled,
			"max_concurrent", config.Balancing.AutoPull.MaxConcurrent,
			"timeout", config.Balancing.AutoPull.PullTimeout)
	}

	// Инициализация background controllers
	p.unloadScheduler = NewUnloadScheduler(p)
	p.unloadScheduler.Start()
	p.weightTuner = NewAdaptiveWeightTuner(p)
	p.weightTuner.Start()

	// Инициализация RPC Model Distribution модулей
	p.initRpcModules()

	return p

}

// SetQueueManagerProxy - установка proxy для QueueManager (вызывается после создания)
func (p *Proxy) SetQueueManagerProxy() {
	p.queueMgr.proxy = p
}

// ServeHTTP - обработка HTTP запросов
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{\"status\":\"healthy\"}"))
		return
	}

	path := r.URL.Path
	isChatOrGenerate := (path == "/api/generate" || path == "/api/chat")
	clientName := p.getClientName(r)

	model, isStream := p.extractModel(r)
	ctx := context.WithValue(r.Context(), modelContextKey, model)
	ctx = context.WithValue(ctx, streamContextKey, isStream)
	r = r.WithContext(ctx)

	sessionID := p.getSessionIDWithModel(r, clientName, model)
	if !isChatOrGenerate && p.ollamaRouter != nil && p.ollamaRouter.Route(w, r) {
		return
	}

	var targetBackend string

	isEmbed := path == "/api/embeddings"
	if sessionID != "" && p.config.Balancing.SessionStickiness && !isEmbed {
		if session := p.sessionMgr.Get(sessionID); session != nil {
			targetBackend = session.BackendID

			p.mu.RLock()
			backendState, exists := p.backends[targetBackend]
			p.mu.RUnlock()

			if !exists || backendState.Backend.Status != types.StatusHealthy {
				logger.Get().Warnw("session backend unavailable, selecting new backend",
					"session_backend", targetBackend,
					"exists", exists,
				)
				targetBackend = ""
			} else {
				forceRebalance := false
				backendState.mu.Lock()
				active := backendState.ActiveReqs
				maxReqs := backendState.Backend.MaxConcurrentReqs
				backendState.mu.Unlock()

				if maxReqs > 0 && active > 0 {
					loadRatio := float64(active) / float64(maxReqs)
					if loadRatio >= 1.0 {
						forceRebalance = true
					}
					if loadRatio > 0.50 || forceRebalance {
						altBackend := p.findLessLoadedBackendWithModel(model, targetBackend)
						if altBackend != "" {
							logger.Get().Infow("rebalancing session to less loaded backend (same model)",
								"session", sessionID, "from", targetBackend, "to", altBackend,
								"load_ratio", loadRatio, "force", forceRebalance)
							targetBackend = altBackend
						} else {
							altBackend = p.findLessLoadedBackendAny(model, targetBackend)
							if altBackend != "" {
								logger.Get().Infow("rebalancing session to less loaded backend (any)",
									"session", sessionID, "from", targetBackend, "to", altBackend,
									"load_ratio", loadRatio, "force", forceRebalance)
								targetBackend = altBackend
							} else if forceRebalance {
								logger.Get().Warnw("force rebalance failed, queueing request",
									"session", sessionID, "backend", targetBackend, "load_ratio", loadRatio)
								targetBackend = ""
							}
						}
					}
				}

				// Always update session binding — new backend or re-sticky to current
				if targetBackend != "" && p.config.Balancing.SessionStickiness {
					p.sessionMgr.Set(sessionID, targetBackend, model, clientName, p.getClientRealIP(r), r.UserAgent())
				}
			}
		}
	}

	if targetBackend == "" {
		targetBackend = p.selectBackend(model)
		if targetBackend != "" && p.config.Balancing.SessionStickiness {
			sessionID = p.getSessionIDWithModel(r, clientName, model)
			p.sessionMgr.Set(sessionID, targetBackend, model, clientName, p.getClientRealIP(r), r.UserAgent())
		}
	}

	// Атомарный захват слота с retry при неудаче
	acquired := false
	if targetBackend != "" {
		acquired = p.tryAcquireSlot(targetBackend)
	}

	// Если не удалось захватить слот на выбранном бэкенде — пробуем другие
	if targetBackend != "" && !acquired {
		attemptedBackends := map[string]bool{targetBackend: true}
		const maxRetries = 10
		for retry := 0; retry < maxRetries; retry++ {
			altBackend := p.selectBackendExcluding(model, attemptedBackends)
			if altBackend == "" {
				break
			}
			if p.tryAcquireSlot(altBackend) {
				targetBackend = altBackend
				acquired = true
				if sessionID != "" && p.config.Balancing.SessionStickiness {
					p.sessionMgr.Set(sessionID, targetBackend, model, clientName, p.getClientRealIP(r), r.UserAgent())
				}
				logger.Get().Infow("acquired slot on alternate backend",
					"backend", targetBackend, "original", attemptedBackends)
				break
			}
			attemptedBackends[altBackend] = true
		}
	}

	if !acquired {
		if !p.queueRequest(w, r, model) {
			http.Error(w, "Service unavailable - all backends busy", http.StatusServiceUnavailable)
		}
		return
	}

	// Слот захвачен — гарантируем освобождение после ответа
	defer p.releaseSlot(targetBackend)

	// Выполняем запрос
	err := p.proxyRequest(w, r, targetBackend)
	if err == nil {
		return
	}

	// Проверяем, не является ли ошибка "model not found" — запускаем auto-pull
	var modelNotFound *ModelNotFoundError
	if errors.As(err, &modelNotFound) && p.AutoPull != nil && model != "" {
		logger.Get().Warnw("model not found on backend, triggering auto-pull",
			"backend", targetBackend, "model", model,
			"ollama_error", string(modelNotFound.Body))

		// AutoPull.EnsureModel уже проверяет: есть ли модель на каком-то бэкенде,
		// не загружается ли она где-то ещё (dedup), свободны ли слоты для pull'а
		newBackendID, pullErr := p.AutoPull.EnsureModel(model)
		if pullErr == nil {
			logger.Get().Infow("auto-pull successful, retrying request on new backend",
				"model", model, "new_backend", newBackendID)

			// Обновляем привязку сессии к новому бэкенду
			if sessionID != "" && p.config.Balancing.SessionStickiness {
				p.sessionMgr.Set(sessionID, newBackendID, model, clientName, p.getClientRealIP(r), r.UserAgent())
			}

			// Захватываем слот на новом бэкенде и выполняем повторный запрос
			if p.tryAcquireSlot(newBackendID) {
				defer p.releaseSlot(newBackendID)
				err = p.proxyRequest(w, r, newBackendID)
				if err == nil {
					return
				}
				logger.Get().Errorw("auto-pull: request retry failed",
					"model", model, "backend", newBackendID, "error", err)
				http.Error(w, fmt.Sprintf("Model '%s' pulled but request failed: %v", model, err), http.StatusInternalServerError)
			} else {
				logger.Get().Warnw("auto-pull: no free slot on new backend",
					"model", model, "backend", newBackendID)
				http.Error(w, fmt.Sprintf("Model '%s' pulled but no free slot on backend '%s'", model, newBackendID), http.StatusServiceUnavailable)
			}
		} else {
			// Auto-pull не удался — сообщаем клиенту понятную ошибку
			logger.Get().Errorw("auto-pull failed for model",
				"model", model, "error", pullErr)
			http.Error(w, fmt.Sprintf("Model '%s' not found on any backend and auto-pull failed: %v", model, pullErr), http.StatusNotFound)
		}
		return
	}

	// Обычная обработка ошибки (не model-not-found или auto-pull выключен/недоступен)
	logger.Get().Errorw("backend request failed",
		"backend", targetBackend, "error", err, "model", model)
	p.UpdateBackendStatus(targetBackend, types.StatusUnhealthy)
	p.scheduleRecoveryCheck(targetBackend)

	// При ошибке пробуем другие бэкенды
	attemptedBackends := map[string]bool{targetBackend: true}
	for attempt := 1; attempt < 3; attempt++ {
		altBackend := p.selectBackendExcluding(model, attemptedBackends)
		if altBackend == "" {
			break
		}
		if !p.tryAcquireSlot(altBackend) {
			attemptedBackends[altBackend] = true
			continue
		}
		if sessionID != "" && p.config.Balancing.SessionStickiness {
			p.sessionMgr.Set(sessionID, altBackend, model, clientName, p.getClientRealIP(r), r.UserAgent())
		}

		attemptedBackends[altBackend] = true
		// Слот altBackend захвачен — гарантируем освобождение
		defer p.releaseSlot(altBackend)

		err := p.proxyRequest(w, r, altBackend)
		if err == nil {
			return
		}
		logger.Get().Errorw("backend request failed",
			"backend", altBackend, "attempt", attempt, "error", err, "model", model)
	}

	http.Error(w, "Service unavailable - all backends failed", http.StatusServiceUnavailable)
}

// extractModel - извлечение модели и stream-флага из запроса (с восстановлением тела)
func (p *Proxy) extractModel(r *http.Request) (string, bool) {
	if r.Method == http.MethodPost && r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return "", true
		}
		r.Body = io.NopCloser(bytes.NewBuffer(body))

		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			return "", true
		}

		var model string
		if m, ok := req["model"].(string); ok {
			model = m
		}

		isStream := true
		if stream, ok := req["stream"].(bool); ok {
			isStream = stream
		}

		return model, isStream
	}

	return "", false
}

// UpdateMetrics - обновление метрик бэкенда с прогнозированием
func (p *Proxy) UpdateMetrics(backendID string, metrics *types.BackendMetrics) {
	p.mu.RLock()
	state, exists := p.backends[backendID]
	p.mu.RUnlock()

	if exists {
		state.mu.Lock()
		proxyActiveReqs := state.ActiveReqs
		state.mu.Unlock()

		if metrics.Ollama.ActiveRequests == 0 && proxyActiveReqs > 0 {
			metrics.Ollama.ActiveRequests = proxyActiveReqs
		}

		freeSlots := metrics.Ollama.MaxConcurrentRequests - metrics.Ollama.ActiveRequests
		if freeSlots < 0 {
			freeSlots = 0
		}
		metrics.Ollama.FreeSlots = freeSlots

		p.predictor.UpdateHistory(state, metrics)
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics[backendID] = metrics
	p.metricsMgr.mu.Unlock()
}

// initRpcModules - инициализация RPC Model Distribution модулей по enabled флагам
func (p *Proxy) initRpcModules() {
	cfg := &p.config.Balancing

	// Вариант A: Model Replication Manager
	if cfg.ModelReplication.Enabled {
		p.modelReplication = modelreplication.NewModelGroupManager()
		p.modelReplication.SetEnabled(true)

		// Настраиваем callbacks для интеграции с Balancer
		p.modelReplication.SetBackendLoadFn(func(backendID string) float64 {
			p.mu.RLock()
			state, exists := p.backends[backendID]
			p.mu.RUnlock()
			if !exists {
				return 1.0
			}
			state.mu.Lock()
			active := state.ActiveReqs
			maxReqs := state.Backend.MaxConcurrentReqs
			state.mu.Unlock()
			if maxReqs <= 0 {
				return 0.0
			}
			return float64(active) / float64(maxReqs)
		})

		p.modelReplication.SetFreeBackendFn(func(modelName string, targets []string) []string {
			p.mu.RLock()
			defer p.mu.RUnlock()
			var free []string
			for id, state := range p.backends {
				if state.Backend.Status != types.StatusHealthy {
					continue
				}
				if len(targets) > 0 {
					found := false
					for _, t := range targets {
						if id == t {
							found = true
							break
						}
					}
					if !found {
						continue
					}
				}
				state.mu.Lock()
				active := state.ActiveReqs
				state.mu.Unlock()
				if active == 0 {
					free = append(free, id)
				}
			}
			return free
		})

		p.modelReplication.SetWarmupFn(func(backendID, modelName string) error {
			p.mu.RLock()
			state, exists := p.backends[backendID]
			p.mu.RUnlock()
			if !exists {
				return fmt.Errorf("backend %s not found", backendID)
			}
			p.warmupModel(backendID, state.Backend.Host, state.Backend.OllamaPort, modelName)
			return nil
		})

		// Создаём селектор и контроллер
		p.replicationSelector = modelreplication.NewGroupAwareSelector(p.modelReplication)
		p.replicationCtrl = modelreplication.NewGroupController(p.modelReplication)
		if err := p.replicationCtrl.Start(); err != nil {
			logger.Get().Warnw("failed to start group controller", "error", err)
		}

		// Загружаем группы из конфига
		for _, groupCfg := range cfg.ModelReplication.Groups {
			if err := p.modelReplication.CreateGroup(groupCfg); err != nil {
				logger.Get().Warnw("failed to create model group from config",
					"model", groupCfg.ModelName, "error", err)
			}
		}

		logger.Get().Infow("model replication manager initialized",
			"defaultMinInstances", cfg.ModelReplication.DefaultMinInstances,
			"defaultMaxInstances", cfg.ModelReplication.DefaultMaxInstances,
			"idleUnloadAfter", cfg.ModelReplication.IdleUnloadAfter,
			"groups", len(cfg.ModelReplication.Groups))
	}

	// Вариант B: External RPC Coordinator
	if cfg.RpcCoordinator.Enabled {
		p.rpcCoordinator = rpccoordinator.NewCoordinator()
		p.rpcCoordinator.SetEnabled(true)
		p.rpcCoordinator.SetConfig(
			cfg.RpcCoordinator.CoordinatorURL,
			cfg.RpcCoordinator.WorkerPort,
			cfg.RpcCoordinator.Protocol,
		)
		logger.Get().Infow("rpc coordinator initialized",
			"url", cfg.RpcCoordinator.CoordinatorURL,
			"port", cfg.RpcCoordinator.WorkerPort,
			"protocol", cfg.RpcCoordinator.Protocol)
	}

	// Вариант C: Virtual Model Router
	if cfg.VirtualModels.Enabled {
		p.virtualModels = virtualmodel.NewRegistry()
		p.virtualModels.SetEnabled(true)
		// Регистрируем виртуальные модели из конфига
		for _, vmCfg := range cfg.VirtualModels.Models {
			if err := p.virtualModels.Register(vmCfg); err != nil {
				logger.Get().Warnw("failed to register virtual model", "name", vmCfg.Name, "error", err)
			} else {
				logger.Get().Infow("virtual model registered", "name", vmCfg.Name,
					"slices", len(vmCfg.Slices))
			}
		}
		// Создаём Router для маршрутизации через VirtualModel pipeline
		p.virtualModelRouter = virtualmodel.NewRouter(p.virtualModels)
		logger.Get().Infow("virtual model router initialized",
			"enabled", true, "count", len(cfg.VirtualModels.Models))
	}

	// Вариант D: Distributed Inference (Custom Backend)
	if cfg.DistInference.Enabled {
		p.distInference = distinference.NewEngine()
		p.distInference.SetEnabled(true)
		// Регистрируем worker'ов из конфига
		for _, wCfg := range cfg.DistInference.Workers {
			worker := distinference.NewWorker(
				wCfg.WorkerID,
				wCfg.Host,
				wCfg.GrpcPort,
				wCfg.LayerRange,
				wCfg.GPUMode,
			)
			p.distInference.RegisterWorker(worker)
			logger.Get().Infow("dist-inference worker registered",
				"id", wCfg.WorkerID, "host", wCfg.Host, "port", wCfg.GrpcPort)
		}
		logger.Get().Infow("distributed inference engine initialized",
			"workers", len(cfg.DistInference.Workers))
	}
}

// GetModelReplicationManager возвращает ModelGroupManager (для API).
func (p *Proxy) GetModelReplicationManager() *modelreplication.ModelGroupManager {
	return p.modelReplication
}

// GetReplicationSelector возвращает GroupAwareSelector (для API).
func (p *Proxy) GetReplicationSelector() *modelreplication.GroupAwareSelector {
	return p.replicationSelector
}

// GetReplicationController возвращает GroupController (для API).
func (p *Proxy) GetReplicationController() *modelreplication.GroupController {
	return p.replicationCtrl
}

// GetVirtualModelRouter возвращает VirtualModel Router (для API).
func (p *Proxy) GetVirtualModelRouter() *virtualmodel.Router {
	return p.virtualModelRouter
}

// queueRequest - постановка запроса в очередь
func (p *Proxy) queueRequest(w http.ResponseWriter, r *http.Request, model string) bool {

	// Backpressure: проверяем fill rate очереди
	queueFillPct := float64(len(p.queueMgr.queue)) / float64(p.queueMgr.maxSize)
	if queueFillPct > 0.90 {
		logger.Get().Warnw("queue overflow, rejecting request with 503",
			"model", model, "queue_fill_pct", queueFillPct, "queue_size", len(p.queueMgr.queue))
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Service overloaded", http.StatusServiceUnavailable)
		return false
	}

	done := make(chan bool, 1)
	queuedReq := &QueuedRequest{
		Request:  r,
		Writer:   w,
		Model:    model,
		Enqueued: time.Now(),
		Done:     done,
	}

	p.queueMgr.addPending(queuedReq)

	select {
	case p.queueMgr.queue <- queuedReq:
	case <-p.queueMgr.ctx.Done():
		p.queueMgr.removePending(queuedReq)
		return false
	default:
		p.queueMgr.removePending(queuedReq)
		return false
	}

	select {
	case success := <-done:
		return success
	case <-time.After(p.queueMgr.timeout):
		return false
	case <-p.queueMgr.ctx.Done():
		return false
	}
}
