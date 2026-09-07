package balancer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/internal/modelreplication"
	"ollama-loadbalancer/internal/rpccoordinator"
	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// Proxy - HTTP прокси для балансировки запросов
type Proxy struct {
	config     *types.LoadBalancerConfig
	backends   map[string]*BackendState
	mu         sync.RWMutex
	roundRobin int
	sessionMgr *SessionManager
	metricsMgr *MetricsManager
	queueMgr   *QueueManager
	predictor  *Predictor
	// healthChecker — опциональная зависимость. Выставляется через SetHealthChecker
	// после конструктора (избегаем circular dep). Используется для MarkUnhealthy
	// при persistent connection failures (Round 8 fix).
	healthChecker          *HealthChecker
	client                 *http.Client    // Клиент для обычных запросов
	streamingClient        *http.Client    // Клиент для streaming/SSE запросов (без таймаута)
	streamingTransportBase *http.Transport // Базовый Transport для per-request клонов с ResponseHeaderTimeout
	statePath              string          // Путь к state файлу
	saveTimer              *time.Timer     // Таймер для debounced autosave
	saveMu                 sync.Mutex      // Мьютекс для защиты saveTimer
	totalRequests          int64           // Atomic: всего запросов через прокси
	ollamaRouter           *OllamaRouter   // Маршрутизатор Ollama API endpoint'ов
	llamaCppRouter         *LlamaCppRouter // Маршрутизатор llama.cpp API endpoint'ов

	// Round 31 #7 (2026-08-09): token usage tracking.
	// Atomic counters для агрегации prompt/completion tokens по моделям.
	// Используется для мониторинга через /api/v1/stats/tokens endpoint.
	// Ключ = model name, value = ModelTokenUsage.
	tokensByModelMu sync.RWMutex
	tokensByModel   map[string]*ModelTokenUsage

	// llamaCppMetricsPoller синхронизирует metricsMgr.llamaMetrics[].LoadedModels
	// с фактическим состоянием cppworker через периодический poll /api/models.
	// Нужен для отображения загруженных моделей в WebUI (страница GGUF) и
	// API /api/v1/gguf/backends, когда модели загружены в обход балансера.
	llamaCppMetricsPoller *llamaCppMetricsPoller

	// nctxReload — координатор adaptive n_ctx auto-reload (Stage 3, 5).
	// Решает, можно ли перезагрузить модель с большим n_ctx (если VRAM
	// позволяет), или вернуть клиенту 413. Вызывается из llamacpp_transport.go
	// когда upstream (cppworker) возвращает ErrNCtxNeedsReload.
	nctxReload *NCtxReloadCoordinator

	// R54.4 (2026-08-24): AutoTune tracker — circuit breakers per (backend, model).
	// Защищает от reload storm когда AutoTune fix не удаётся.
	// Инициализируется lazily в triggerAutoTuneReload (нулевый указатель = default cfg).
	autoTuneTracker *AutoTuneTracker

	// R55.2 (2026-08-24): AutoTune history log - ring buffer последних N=500 events.
	// Используется для operator visibility: "что AutoTune делал за последние 24h?"
	// Инициализируется lazily в AutoTuneHistory() getter.
	autoTuneHistory *AutoTuneHistory

	// R54.9 (2026-08-24): Workload tracker — per-(backend, model) sliding window
	// num_ctx samples. Используется AutoTune для workload-aware KV cache
	// selection (light workload → f16, heavy → q4_0).
	// Инициализируется lazily в WorkloadTracker() getter.
	workloadTracker *WorkloadTracker

	// R54.6 (2026-08-24): ModelManager reference для AutoTune apply (load+unload).
	// Использует existing p.modelManager (set в NewProxy). SetModelManager() — alias
	// для совместимости с R54.6 handler'ами.

	// EventBus (вынесен в eventbus.go)
	eventBus *EventBus

	// Round 40 #3 (2026-08-18): apiReverseProxy пересылает /api/v1/*
	// запросы с proxy-порта (18080) на локальный API-сервер (18081).
	// Без этого balancer не знает про /api/v1/* пути и они падают
	// в основной flow proxyRequest → queueRequest → зависают на 30s
	// timeout (Round 22 BUG #7). Теперь balancer сразу forward'ит
	// на API сервер, который знает про эти endpoint'ы.
	apiReverseProxy *httputil.ReverseProxy

	// Background controllers
	AutoPull        *AutoPullManager     // Менеджер автоматической загрузки моделей (Pull-on-Demand)
	unloadScheduler *UnloadScheduler     // Планировщик выгрузки моделей
	weightTuner     *AdaptiveWeightTuner // Адаптивный тюнер весов
	agentChecker    *agentTimeoutChecker // Проверка таймаута агентов

	// RPC model distribution modules
	modelReplication    *modelreplication.ModelGroupManager
	replicationSelector *modelreplication.GroupAwareSelector
	replicationCtrl     *modelreplication.GroupController
	virtualModels       *virtualmodel.Registry
	virtualModelRouter  *virtualmodel.Router
	rpcCoordinator      *rpccoordinator.ModelCoordinator

	// Phase 8 (2026-07-10): P.1 — rpc_coordinator production mode.
	// RpcCoordinatorDispatcher routes inference через coordinator при
	// OperatingMode=rpc_coordinator. Phase 8.5: добавлено поле + scaffold в
	// ServeHTTP. Phase 9: полная интеграция (streaming + circuit breakers).
	rpcDispatcher *RpcCoordinatorDispatcher

	// VirtualRouter routes virtual models (alias-on-pool) при
	// OperatingMode=virtual_router. Phase 8 P.2: перехватывает requests
	// с model=virtual:xxx и выбирает backend через Selector.
	virtualRouter *VirtualRouter

	// Прокси-логгер для отслеживания запросов
	proxyLogger *ProxyLogger

	// Model management
	modelManager *ModelManager

	// Recent clients tracking (for monitor discovery particles)
	recentClients   map[string]*types.RecentClient
	recentClientsMu sync.RWMutex

	// Кеширование доверенных прокси-сетей (парсятся один раз при инициализации)
	trustedNets []*net.IPNet

	// Graceful shutdown
	shuttingDown  atomic.Bool
	activeStreams sync.WaitGroup // счётчик активных SSE-сессий

	// Семафор для ограничения одновременных warmup (загрузка моделей в VRAM).
	// Каждая загрузка делает POST /api/generate, который блокирует слот Ollama.
	// Ограничение предотвращает исчерпание всех свободных слотов загрузками.
	warmupSem chan struct{}

	// modelLatencyTracker собирает per-model метрики генерации (tokens/sec,
	// inter-token gap, first-byte latency) для адаптивного расчёта таймаутов.
	// Используется в proxyRequestLlamaCpp, handleStreamingResponse и proxyRequest
	// для замены глобальных таймаутов на per-model.
	modelLatencyTracker *ModelLatencyTracker

	// metricsHTTPDoer — HTTP-клиент для heartbeat /api/info polling
	// в preflightNCtxReloadIfNeededSync (см. queryBackendReloadPending).
	// По умолчанию nil → используется реальный *http.Client{Timeout: 2s}.
	// Тесты могут подменить на in-memory stub, чтобы не зависеть от сети
	// (см. internal/balancer/nctx_reload_sync_test.go).
	metricsHTTPDoer interface {
		Do(*http.Request) (*http.Response, error)
	}
}

// EventBus — геттер для EventBus (используется API-сервером для F.α SSE endpoint).
// Не nil после NewProxy, всегда возвращает валидный EventBus.
func (p *Proxy) EventBus() *EventBus {
	return p.eventBus
}

// GetLlamaCppRouter — R60.11 (2026-09-07): expose LlamaCppRouter
// для admin endpoint /api/v1/balancer/load-backoff (нужен доступ к loadBackoff).
func (p *Proxy) GetLlamaCppRouter() *LlamaCppRouter {
	if p == nil {
		return nil
	}
	return p.llamaCppRouter
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

	// Транспорт для streaming запросов - без сжатия и с увеличенными буферами.
	// ResponseHeaderTimeout=0 (disabled) — таймаут первого байта теперь управляется
	// per-request контекстом (context.WithTimeout) на основе 3-tier адаптивных
	// таймаутов (Profile > ModelLatencyTracker > config). Это критично для
	// тяжёлых моделей (CPU/partial offload), где первый токен может задерживаться
	// на 60+ секунд (загрузка в VRAM + prompt processing).
	// Транспортный ResponseHeaderTimeout не может быть per-model динамическим,
	// поэтому выносим FirstByteTimeout на уровень контекста в proxyRequest /
	// proxyRequestLlamaCpp. Старый ResponseHeaderTimeout обрывал соединение ДО
	// того, как адаптивный контекст вступал в силу, вызывая
	// TransferEncodingError в OpenWebUI.
	//
	// Контекстный таймаут (FirstByteTimeout + StreamTimeout) покрывает весь
	// lifecycle запроса: ожидание заголовков + стриминг + idle между чанками.
	streamingTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true, // Важно для SSE
		ResponseHeaderTimeout: 0,    // Отключено — управляется per-request контекстом
	}

	// Определяем TTL сессий из конфигурации
	sessionTTL := time.Duration(config.Balancing.SessionTTL) * time.Second
	if sessionTTL <= 0 {
		sessionTTL = 15 * time.Minute
	}
	sessionIdleTTL := time.Duration(config.Balancing.SessionIdleTTL) * time.Second
	if sessionIdleTTL <= 0 {
		sessionIdleTTL = 5 * time.Minute
	}

	p := &Proxy{
		config:        config,
		backends:      make(map[string]*BackendState),
		tokensByModel: make(map[string]*ModelTokenUsage),
		sessionMgr:    NewSessionManagerWithTTL(sessionTTL, sessionIdleTTL),
		metricsMgr:    NewMetricsManager(),
		predictor:     NewPredictor(),
		statePath:     config.LoadBalancer.StatePath,
		eventBus:      NewEventBus(),
		recentClients: make(map[string]*types.RecentClient),
		trustedNets:   parseTrustedProxyCIDRs(config.LoadBalancer.TrustedProxies),
		warmupSem:     make(chan struct{}, maxConcurrentWarmups(config)),

		// Клиент для обычных запросов: Timeout=0 (без глобального), т.к. таймаут
		// задаётся per-request через context.WithTimeout в proxyRequest
		// на основе getEffectiveTimeout().
		client: &http.Client{
			Timeout:   0, // Таймаут управляется через контекст запроса
			Transport: regularTransport,
		},
		// Streaming клиент без общего таймаута для long-running запросов
		streamingClient: &http.Client{
			Timeout:   0, // Нет таймаута для streaming
			Transport: streamingTransport,
		},
		// Базовый Transport для per-request клонов с ResponseHeaderTimeout
		// (используется в newStreamingClientWithResponseHeaderTimeout).
		// Применяется для streaming + явный FirstByteTimeout>0 (PF-5 fix).
		streamingTransportBase: streamingTransport,
	}

	// QueueManager создаём после инициализации p, чтобы передать корректный proxy
	p.queueMgr = NewQueueManager(p, config.Balancing.QueueMaxSize, config.Balancing.QueueWorkers, time.Duration(config.Balancing.QueueTimeout)*time.Second)

	// Прокси-логгер использует тот же EventBus для WebSocket-трансляции
	p.proxyLogger = NewProxyLogger(1000, p.eventBus)

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

	// Инициализация LlamaCppRouter для llama.cpp бэкендов
	p.llamaCppRouter = NewLlamaCppRouter(p)

	// Round 40 #3 (2026-08-18): инициализация reverse proxy для /api/v1/*.
	// Endpoint'ы под /api/v1/ обслуживаются API-сервером (cmd/balancer/api,
	// порт 18081), а не proxy flow. Сюда входят: /api/v1/cluster/*,
	// /api/v1/cppworker/*, /api/v1/events (SSE), /api/v1/rpc/tp/* и др.
	// Без этого клиент, отправивший POST /api/v1/cppworker/config на proxy
	// 18080, получает 30s timeout (Round 22 BUG #7) вместо ответа.
	apiHost := config.LoadBalancer.Host
	if apiHost == "" || apiHost == "0.0.0.0" {
		apiHost = "127.0.0.1"
	}
	apiPort := config.LoadBalancer.APIPort
	if apiPort == 0 {
		apiPort = 18081
	}
	apiURL := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s:%d", apiHost, apiPort),
	}
	p.apiReverseProxy = httputil.NewSingleHostReverseProxy(apiURL)
	// Director (default): keep original Host header (don't override to 127.0.0.1).
	// API server uses Host header for SNI/reverse-proxy detection in WebUI.
	// Round 40 #3 (2026-08-18): flush interval 100ms — same as default,
	// but explicit для forward-compat с streaming endpoints.
	p.apiReverseProxy.FlushInterval = 100 * time.Millisecond

	// Запуск фонового poll llama.cpp бэкендов для синхронизации LoadedModels
	// с фактическим состоянием cppworker. Нужен для корректного отображения
	// загруженных моделей в WebUI (страница GGUF) и API /api/v1/gguf/backends.
	p.llamaCppMetricsPoller = newLlamaCppMetricsPoller(p)
	p.llamaCppMetricsPoller.Start()

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

	// Инициализация nctxReload координатора (Stage 3, 5).
	// Загружает конфиг из BalancingSettings.NCtxReload (см. internal/config).
	// Если конфиг не задан — использует безопасные defaults (AutoReloadNCtx=false).
	p.nctxReload = NewNCtxReloadCoordinator(loadNCtxReloadConfig(p.config))
	logger.Get().Infow("nctx reload coordinator initialized",
		"auto_reload", p.nctxReload.Config().AutoReloadNCtx)

	// Инициализация RPC Model Distribution модулей
	p.initRpcModules()

	// Инициализация ModelManager для управления моделями на бэкендах
	p.modelManager = NewModelManager(p)
	logger.Get().Infow("model manager initialized")

	// Инициализация ModelLatencyTracker для per-model адаптивных таймаутов.
	// Собирает метрики генерации (tokens/sec, inter-token gap, first-byte latency)
	// и автоматически вычисляет рекомендуемые таймауты для каждой модели.
	p.modelLatencyTracker = NewModelLatencyTracker()
	logger.Get().Infow("model latency tracker initialized")

	return p

}

// determineRequestBackendType определяет тип бэкенда на основе пути входящего запроса.
//   - Ollama API endpoints (/api/generate, /api/chat, /api/tags, ...) →
//     если OperatingMode разрешает оба типа (standard/replication/rpc_coordinator) — возвращает "",
//     позволяя selectBackend выбрать бэкенд любого типа где загружена модель.
//     Если mode жёстко привязан к одному типу — возвращает этот тип.
//   - llama.cpp / OpenAI-compatible endpoints (/v1/chat/completions, /v1/completions, ...) → BackendTypeLlamaCpp
//   - Прочие пути → "" (будет определено из OperatingMode через getDefaultAllowedTypes)
func (p *Proxy) determineRequestBackendType(r *http.Request) types.BackendType {
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/") {
		// Проверяем, какие типы бэкендов разрешены текущим OperatingMode
		allowed := p.getDefaultAllowedTypes()
		if len(allowed) > 1 {
			// Режим разрешает оба типа (standard/replication/rpc_coordinator).
			// Если явно задан BackendEngine (не auto), используем его для
			// обратной совместимости с legacy-конфигурациями, где operatingMode
			// не выставлен, а backendEngine = llama_cpp.
			if p.config.BackendEngine != "" && p.config.BackendEngine != types.EngineAuto {
				switch p.config.BackendEngine {
				case types.EngineLlamaCPP:
					return types.BackendTypeLlamaCpp
				case types.EngineOllamaAPI:
					return types.BackendTypeOllama
				}
			}
			// Возвращаем "", чтобы selectBackend мог выбрать бэкенд любого типа
			// на основе того, где модель фактически загружена.
			return ""
		}
		if len(allowed) == 1 {
			return allowed[0]
		}
		// Фолбэк: неизвестный режим — только Ollama для обратной совместимости
		return types.BackendTypeOllama
	}
	if strings.HasPrefix(path, "/v1/") {
		return types.BackendTypeLlamaCpp
	}
	return ""
}

// SetQueueManagerProxy - установка proxy для QueueManager (вызывается после создания)
func (p *Proxy) SetQueueManagerProxy() {
	p.queueMgr.proxy = p
}

// SetHealthChecker — устанавливает HealthChecker после создания Proxy
// (избегаем circular dep Proxy ↔ HealthChecker).
// Round 8 (2026-07-10): нужен для MarkUnhealthy при persistent connection failures.
func (p *Proxy) SetHealthChecker(hc *HealthChecker) {
	p.healthChecker = hc
}

// Shutdown — graceful shutdown с завершением активных SSE-сессий и сохранением состояния.
//
// Порядок остановки (R52.5, 2026-08-24: расширен per-component Stop):
//  1. Запрет новых запросов (возврат 503) — p.shuttingDown
//  2. Остановка приёма очереди — QueueManager.Stop
//  3. Остановка background controllers:
//     - UnloadScheduler (auto-unload idle models)
//     - AdaptiveWeightTuner (per-backend weights)
//     - AgentTimeoutChecker (agent health)
//     - SessionManager (session stickiness)
//     - LlamaCppMetricsPoller (cppworker metrics poll)
//     - GroupController (replication group) — R52.5 NEW
//     - NCtxReloadCoordinator (n_ctx async reload) — R52.5 NEW
//     - EventBus (subscriber channels) — R52.5 NEW
//  4. Завершение активных SSE-сессий с таймаутом
//  5. Сохранение состояния
func (p *Proxy) Shutdown(ctx context.Context) error {
	logger.Get().Infow("shutdown initiated, stopping new requests")

	// 1. Запрещаем приём новых запросов
	p.shuttingDown.Store(true)

	// 2. Останавливаем QueueManager (workers завершат текущие задачи, новые не принимаются)
	if p.queueMgr != nil {
		logger.Get().Infow("stopping queue manager...")
		p.queueMgr.Stop()
	}

	// 3. Останавливаем background controllers
	if p.unloadScheduler != nil {
		logger.Get().Infow("stopping unload scheduler...")
		p.unloadScheduler.Stop()
	}
	if p.weightTuner != nil {
		logger.Get().Infow("stopping weight tuner...")
		p.weightTuner.Stop()
	}
	if p.agentChecker != nil {
		logger.Get().Infow("stopping agent timeout checker...")
		p.StopAgentTimeoutChecker()
	}
	if p.sessionMgr != nil {
		logger.Get().Infow("stopping session manager...")
		p.sessionMgr.Stop()
	}
	if p.llamaCppMetricsPoller != nil {
		logger.Get().Infow("stopping llama.cpp metrics poller...")
		p.llamaCppMetricsPoller.Stop()
	}
	// R52.5 (2026-08-24): per-component Stop for graceful shutdown.
	// Pre-R52.5 эти контроллеры НЕ останавливались → goroutine-leak при shutdown.
	// Покрытие: replicationCtrl (modelreplication.GroupController),
	// nctxReload (n_ctx async reload), eventBus (subscriber channels).
	if p.replicationCtrl != nil {
		logger.Get().Infow("stopping replication controller...")
		if err := p.replicationCtrl.Stop(); err != nil {
			logger.Get().Warnw("replication controller stop error", "error", err)
		}
	}
	if p.nctxReload != nil {
		logger.Get().Infow("stopping nctx reload coordinator...")
		p.nctxReload.Shutdown()
	}
	if p.eventBus != nil {
		logger.Get().Infow("stopping event bus...")
		p.eventBus.Stop()
	}

	// 4. Ожидаем завершения активных SSE-сессий с таймаутом
	logger.Get().Infow("waiting for active streams to finish...")
	streamsDone := make(chan struct{})
	go func() {
		p.activeStreams.Wait()
		close(streamsDone)
	}()

	select {
	case <-streamsDone:
		logger.Get().Infow("all active streams completed")
	case <-ctx.Done():
		logger.Get().Warnw("shutdown deadline exceeded, forcing exit",
			"active_streams_remaining", "unknown")
		// Принудительно не обрываем — Go закроет соединения при выходе процесса
	}

	// 5. Сохраняем состояние
	if err := p.FlushState(); err != nil {
		logger.Get().Errorw("failed to save state during shutdown", "error", err)
		return fmt.Errorf("failed to save state: %w", err)
	}

	logger.Get().Infow("shutdown completed successfully")
	return nil
}

// ServeHTTP - обработка HTTP запросов
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// ==== Correlation ID ====
	// Генерируем (или берём из заголовка X-Request-ID) уникальный id для этого
	// запроса и кладём его в контекст. Все downstream-логи (handleOpenAIChat
	// Completions, ensureModelLoadedOnBackend, queryCppWorkerModels, ModelManager
	// .ExecuteOperation, etc.) подхватывают его через RequestIDFromContext()
	// и автоматически добавляют в log-запись как поле request_id. Это позволяет
	// проследить всю цепочку Cline → balancer → cppworker по одному grep'у.
	rid, ctxWithRID := getOrGenerateRequestID(r)
	r = r.WithContext(ctxWithRID)
	w.Header().Set(requestIDHeader, rid) // echo в response для удобства curl-проверки
	logger.Get().Debugw("ServeHTTP: request received",
		"request_id", rid,
		"method", r.Method,
		"path", r.URL.Path,
		"remote", r.RemoteAddr,
	)

	// CORS headers для всех ответов (нужно для OpenWebUI и других браузерных клиентов)
	origin := r.Header.Get("Origin")
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, Accept, Origin")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
	}

	// Обработка CORS preflight (OPTIONS) запросов
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Phase 8 (2026-07-10): P.1 — rpc_coordinator production mode scaffold.
	// Если balancer в OperatingMode=rpc_coordinator + dispatcher инициализирован
	// + path is intercepted inference endpoint → маршрутизируем через
	// ModelCoordinator. Phase 8.5: добавлен scaffold (полная версия в Phase 9).
	//
	// ВАЖНО: scaffold безопасен — если rpcDispatcher == nil (default bundled
	// config) или mode != rpc_coordinator, выполнение идёт дальше как обычно
	// (existing behavior preserved). Phase 9 заменит ServeHTTP stub на полную
	// реализацию (parse body, call ShouldRoute, dispatch Infer/Stream).
	if IsRpcCoordinatorMode(p.config.Balancing.OperatingMode) &&
		p.rpcDispatcher != nil &&
		p.rpcDispatcher.IsRpcPath(r.URL.Path) {
		logger.Get().Debugw("rpc_coordinator: intercepting request",
			"path", r.URL.Path, "method", r.Method)
		p.rpcDispatcher.ServeHTTP(w, r)
		return
	}

	// Phase 8 (2026-07-11): P.2 — virtual_router production mode interceptor.
	// Если balancer в OperatingMode=virtual_router + VirtualRouter инициализирован
	// + path is intercepted inference endpoint → проверяем body на virtual model.
	//
	// ВАЖНО: VirtualRouter парсит body сам (нужно для extract model name).
	// Если model не в registry — falls through к стандартному flow без изменений.
	// Default bundled config (OperatingMode="standard" или "") → не срабатывает.
	if IsVirtualRouterMode(p.config.Balancing.OperatingMode) &&
		p.virtualRouter != nil &&
		p.virtualRouter.IsActive() &&
		p.virtualRouter.IsVirtualPathRequest(r) {
		// Quick check: read body to extract model. Если model in registry +
		// alias-on-pool mode → forward to VirtualRouter. Иначе — fall through.
		if p.virtualRouter.MatchesVirtualRequest(r) {
			logger.Get().Debugw("virtual_router: intercepting request",
				"path", r.URL.Path, "method", r.Method)
			p.virtualRouter.ServeHTTP(w, r)
			return
		}
	}

	// Единый разбор тела запроса (избегаем тройного чтения в recordRecentClient + extractModel + proxyRequest)
	parsed := p.parseRequestBody(r)
	model := parsed.Model
	isStream := parsed.Stream

	// Определяем требуемый тип бэкенда из URL запроса
	bt := p.determineRequestBackendType(r)
	allowedTypes := p.getAllowedTypesList(bt)

	// Трекинг всех клиентов для монитора (использует уже распарсенные данные)
	p.recordRecentClientParsed(r, parsed)

	// Round 27 follow-up (v0.5.15): disabled-profile check на самом раннем
	// этапе ServeHTTP — до routeRequest, до selectBackend, до любого proxy.
	//
	// Почему здесь, а не в ensureModelLoadedOnBackend (как было в v0.5.15.1):
	//   - В mixed mode (bt == "") /api/chat и /api/generate идут через main
	//     proxy flow, который НЕ вызывает ensureModelLoadedOnBackend для
	//     уже-загруженных моделей (они идут сразу в proxyRequest).
	//   - Только llamacpp_router.Route → handleChat вызывает ensureModelLoadedOnBackend,
	//     но это срабатывает только при bt == BackendTypeLlamaCpp.
	//   - В default bundled-full config (single llama.cpp backend) bt == "" → fall
	//     through → proxyRequest → cppworker → SIGABRT.
	//
	// Поэтому ранняя проверка здесь — единственное место, которое ВСЕГДА
	// срабатывает для disabled-модели, независимо от bt и routing path.
	if model != "" {
		if prof, ok := p.GetModelProfile(model); ok && prof.Disabled {
			msg := fmt.Sprintf("model %q is marked as disabled in profile (broken: see profile.notes). "+
				"Request refused. Use a different model or remove the disabled flag from the profile.",
				model)
			ridLog(ctxWithRID).Warnw("ServeHTTP: model disabled in profile, refusing request",
				"path", r.URL.Path, "model", model, "profileNotes", prof.Notes)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
			return
		}
	}

	// Очистка зомби-сессий перед обработкой запроса.
	// Предотвращает ситуацию "все бэкенды заняты" из-за старых оборванных сессий.
	p.cleanupZombieSessions()

	path := r.URL.Path
	clientName := p.getClientName(r)

	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{\"status\":\"healthy\"}"))
		return
	}

	if r.URL.Path == "/metrics" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "# OllamaLegion balancer metrics\n")
		fmt.Fprintf(w, "healthy 1\n")
		return
	}

	// Оборачиваем ResponseWriter для захвата статус-кода
	sr := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
	w = sr

	isChatOrGenerate := (path == "/api/generate" || path == "/api/chat")
	_ = isChatOrGenerate // используется для логирования/defer ниже

	// Объявляем переменные до defer, чтобы замыкание захватило их финальные значения
	var targetBackend string
	var sessionID string

	// Defer-логирование результата запроса
	defer func() {
		if p.proxyLogger == nil {
			return
		}
		durationMs := time.Since(startTime).Milliseconds()
		entry := ProxyLogEntry{
			Timestamp:  startTime,
			Method:     r.Method,
			Path:       path,
			Model:      model,
			ClientIP:   p.getClientRealIP(r),
			UserAgent:  r.UserAgent(),
			ClientName: clientName,
			BackendID:  targetBackend,
			SessionID:  sessionID,
			StatusCode: sr.statusCode,
			DurationMs: durationMs,
			Stream:     isStream,
		}
		// Round 16 (2026-07-10): для 4xx/5xx — заполняем Error field чтобы в WebUI
		// логах (Logs page) видно что произошло. Используем sr.errBody
		// (накоплен в statusRecorder через Write/WriteHeader).
		if sr.statusCode >= 400 && sr.errBody != "" {
			entry.Error = sr.errBody
		}
		p.proxyLogger.Append(entry)
	}()

	ctx := context.WithValue(r.Context(), modelContextKey, model)
	ctx = context.WithValue(ctx, streamContextKey, isStream)
	r = r.WithContext(ctx)

	sessionID = p.getSessionIDWithModel(r, clientName, model)
	if p.routeRequest(w, r) {
		return
	}

	if sessionID != "" && p.config.Balancing.SessionStickiness && !isEmbeddingsRequest(path) {
		// Для всех клиентских запросов (chat, generate) используем session stickiness
		// Embeddings (/api/embed, /api/embeddings) исключаются — они не требуют привязки к сессии
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
						altBackend := p.findLessLoadedBackendWithModel(model, targetBackend, allowedTypes)
						if altBackend != "" {
							logger.Get().Infow("rebalancing session to less loaded backend (same model)",
								"session", sessionID, "from", targetBackend, "to", altBackend,
								"load_ratio", loadRatio, "force", forceRebalance)
							targetBackend = altBackend
						} else {
							altBackend = p.findLessLoadedBackendAny(model, targetBackend, allowedTypes)
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
		// Round 22 (2026-08-03): read/mgmt endpoints (show, pull, copy, create,
		// delete, push, blobs, models, models/files) НЕ требуют загруженной
		// модели. Пропускаем SyncModelLoad (P3 warmup), иначе balancer
		// зависает на 10-30s ожидая load модели, которая endpoint'у не нужна.
		// ТАКЖЕ пропускаем VRAM headroom check в selectBackend (P1), потому что
		// read operations не нагружают GPU — они могут работать при 95% VRAM.
		skipWarmup := isReadOnlyOrMgmtEndpoint(path)
		if skipWarmup {
			round22SkipWarmupTotal.Add(1)
		}
		if skipWarmup {
			// Для read/mgmt — выбираем backend минуя VRAM check.
			// Сначала ищем backend где модель загружена, потом любой healthy.
			targetBackend = p.findModelOnAnyBackendNoVRAMCheck(model, bt)
			if targetBackend == "" {
				targetBackend = p.selectAnyHealthy(bt)
			}
		} else {
			targetBackend = p.selectBackend(model, bt, false)
		}
		if targetBackend != "" && p.config.Balancing.SessionStickiness && !isEmbeddingsRequest(path) {
			sessionID = p.getSessionIDWithModel(r, clientName, model)
			p.sessionMgr.Set(sessionID, targetBackend, model, clientName, p.getClientRealIP(r), r.UserAgent())
		}
	}

	// Переменная для отслеживания захваченного бэкенда.
	// Используем указатель на строку чтобы корректно обрабатывать
	// освобождение слота в defer без двойного освобождения.
	var acquiredBackend string
	releaseAcquired := func() {
		if acquiredBackend != "" {
			p.releaseSlot(acquiredBackend)
			acquiredBackend = ""
		}
	}
	// Гарантированное освобождение слота при любом выходе из ServeHTTP
	defer releaseAcquired()

	// Round 22 (2026-08-03): read/mgmt endpoints НЕ берут slot. Это короткие
	// операции (HTTP GET, сканирование файлов, ответ из in-memory кэша).
	// Брать slot для них — тратить concurrency budget бэкенда. Если бы
	// взяли slot для /api/show, balancer завис бы на slot-wait когда
	// все слоты заняты inference-запросами (Round 22 BUG #13).
	readOnlyOrMgmt := isReadOnlyOrMgmtEndpoint(path)

	// Атомарный захват слота с retry при неудаче
	if targetBackend != "" && !readOnlyOrMgmt {
		if p.tryAcquireSlot(targetBackend) {
			acquiredBackend = targetBackend
		}
	}

	// Round 22 (2026-08-03): для read/mgmt endpoints — slot не берём,
	// но acquiredBackend всё равно ставим = targetBackend чтобы дальнейший
	// flow (proxyRequest) работал без slot-wait.
	if readOnlyOrMgmt && targetBackend != "" {
		acquiredBackend = targetBackend
	}

	// Если не удалось захватить слот на выбранном бэкенде — пробуем другие
	if targetBackend != "" && acquiredBackend == "" && !readOnlyOrMgmt {
		attemptedBackends := map[string]bool{targetBackend: true}
		const maxRetries = 10
		for retry := 0; retry < maxRetries; retry++ {
			altBackend := p.selectBackendExcluding(model, attemptedBackends, bt)
			if altBackend == "" {
				break
			}
			if p.tryAcquireSlot(altBackend) {
				acquiredBackend = altBackend
				targetBackend = altBackend
				if sessionID != "" && p.config.Balancing.SessionStickiness && !isEmbeddingsRequest(path) {
					p.sessionMgr.Set(sessionID, targetBackend, model, clientName, p.getClientRealIP(r), r.UserAgent())
				}
				logger.Get().Infow("acquired slot on alternate backend",
					"backend", targetBackend, "original", attemptedBackends)
				break
			}
			attemptedBackends[altBackend] = true
		}
	}

	if acquiredBackend == "" {
		// Round 22 (2026-08-03): read/mgmt endpoints не идут в очередь —
		// они короткие операции, queueing тратит время впустую.
		if readOnlyOrMgmt {
			// Fallback: если не выбрали backend (нет healthy) — return 503
			http.Error(w, "Service unavailable - no healthy backends for read endpoint", http.StatusServiceUnavailable)
			return
		}
		if !p.queueRequest(w, r, model) {
			http.Error(w, "Service unavailable - all backends busy", http.StatusServiceUnavailable)
		}
		return
	}

	// Выполняем запрос
	err := p.proxyRequest(w, r, acquiredBackend)
	if err == nil {
		return
	}

	// Проверяем, не является ли ошибка "backend busy" (503) — пробуем другой бэкенд
	var backendBusy *BackendBusyError
	if errors.As(err, &backendBusy) {
		logger.Get().Warnw("backend busy (503), retrying on alternate backend",
			"failed_backend", acquiredBackend, "model", model)
		// Освобождаем слот на текущем бэкенде и пробуем другие
		releaseAcquired()
		attemptedBackends := map[string]bool{targetBackend: true}
		for attempt := 1; attempt < 3; attempt++ {
			altBackend := p.selectBackendExcluding(model, attemptedBackends, bt)
			if altBackend == "" {
				break
			}
			if !p.tryAcquireSlot(altBackend) {
				attemptedBackends[altBackend] = true
				continue
			}
			acquiredBackend = altBackend
			if sessionID != "" && p.config.Balancing.SessionStickiness {
				p.sessionMgr.Set(sessionID, altBackend, model, clientName, p.getClientRealIP(r), r.UserAgent())
			}
			attemptedBackends[altBackend] = true

			err := p.proxyRequest(w, r, altBackend)
			if err == nil {
				return
			}
			logger.Get().Errorw("alternate backend also failed",
				"backend", altBackend, "attempt", attempt, "error", err, "model", model)
			releaseAcquired()
		}
		http.Error(w, "Service unavailable - all backends busy", http.StatusServiceUnavailable)
		return
	}

	// Проверяем, не является ли ошибка "model not found" — запускаем auto-pull
	var modelNotFound *ModelNotFoundError
	if errors.As(err, &modelNotFound) && p.AutoPull != nil && model != "" {
		logger.Get().Warnw("model not found on backend, triggering auto-pull",
			"backend", acquiredBackend, "model", model,
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

			// Освобождаем старый слот и захватываем новый
			releaseAcquired()
			if p.tryAcquireSlot(newBackendID) {
				acquiredBackend = newBackendID
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
		"backend", acquiredBackend, "error", err, "model", model)
	p.UpdateBackendStatus(acquiredBackend, types.StatusUnhealthy)
	p.scheduleRecoveryCheck(acquiredBackend)

	// Освобождаем проблемный бэкенд и пробуем другие
	releaseAcquired()
	attemptedBackends := map[string]bool{targetBackend: true}
	for attempt := 1; attempt < 3; attempt++ {
		altBackend := p.selectBackendExcluding(model, attemptedBackends, bt)
		if altBackend == "" {
			break
		}
		if !p.tryAcquireSlot(altBackend) {
			attemptedBackends[altBackend] = true
			continue
		}
		acquiredBackend = altBackend
		if sessionID != "" && p.config.Balancing.SessionStickiness {
			p.sessionMgr.Set(sessionID, altBackend, model, clientName, p.getClientRealIP(r), r.UserAgent())
		}
		attemptedBackends[altBackend] = true

		err := p.proxyRequest(w, r, altBackend)
		if err == nil {
			return
		}
		logger.Get().Errorw("backend request failed",
			"backend", altBackend, "attempt", attempt, "error", err, "model", model)
		releaseAcquired()
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
	// Round 15 (2026-07-10): нормализуем metrics.ID = backendID. Агент может
	// отправить metrics с ID=agentID (старый handler использовал agentID
	// как ключ). После dedup attach бэкенд имеет другой ID — нормализуем
	// чтобы listBackends нашёл metricsMap[backend.ID].
	metrics.ID = backendID

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
	// Синхронизируем llamaMetrics, чтобы GetClusterState корректно отображал
	// загруженные модели llama.cpp бэкендов, установленные через UpdateMetrics.
	if metrics != nil && len(metrics.LlamaCpp.LoadedModels) > 0 {
		p.metricsMgr.llamaMetrics[backendID] = &metrics.LlamaCpp
	}
	p.metricsMgr.mu.Unlock()
}

// recordRecentClient — трекинг клиента для монитора (TTL 60 секунд)
func (p *Proxy) recordRecentClient(r *http.Request) {
	clientIP := p.getClientRealIP(r)
	clientName := p.getClientName(r)
	userAgent := r.UserAgent()
	key := clientIP + "::" + clientName

	// Определяем модель из запроса (если доступно)
	model := ""
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err == nil {
			r.Body = io.NopCloser(bytes.NewBuffer(body))
			var req map[string]interface{}
			if err := json.Unmarshal(body, &req); err == nil {
				if m, ok := req["model"].(string); ok {
					model = m
				}
			}
		}
	}

	p.recentClientsMu.Lock()
	defer p.recentClientsMu.Unlock()

	now := time.Now()
	if existing, ok := p.recentClients[key]; ok {
		existing.LastRequestAt = now
		existing.RequestCount++
		existing.UserAgent = userAgent
		if model != "" {
			existing.Model = model
		}
		p.recentClients[key] = existing
	} else {
		p.recentClients[key] = &types.RecentClient{
			ClientName:    clientName,
			ClientIP:      clientIP,
			UserAgent:     userAgent,
			LastRequestAt: now,
			RequestCount:  1,
			Model:         model,
		}
	}

	// Очистка старых записей (> 60 секунд)
	cutoff := now.Add(-60 * time.Second)
	for k, rc := range p.recentClients {
		if rc.LastRequestAt.Before(cutoff) && !rc.IsStreamActive {
			delete(p.recentClients, k)
		}
	}
}

// recordRecentClientParsed — трекинг клиента для монитора с использованием уже распарсенного тела.
// Вызывается из ServeHTTP после parseRequestBody чтобы избежать повторного чтения.
func (p *Proxy) recordRecentClientParsed(r *http.Request, parsed *parsedRequest) {
	clientIP := p.getClientRealIP(r)
	clientName := p.getClientName(r)
	userAgent := r.UserAgent()
	key := clientIP + "::" + clientName

	p.recentClientsMu.Lock()
	defer p.recentClientsMu.Unlock()

	now := time.Now()
	if existing, ok := p.recentClients[key]; ok {
		existing.LastRequestAt = now
		existing.RequestCount++
		existing.UserAgent = userAgent
		if parsed.Model != "" {
			existing.Model = parsed.Model
		}
		p.recentClients[key] = existing
	} else {
		p.recentClients[key] = &types.RecentClient{
			ClientName:    clientName,
			ClientIP:      clientIP,
			UserAgent:     userAgent,
			LastRequestAt: now,
			RequestCount:  1,
			Model:         parsed.Model,
		}
	}

	// Очистка старых записей (> 60 секунд)
	cutoff := now.Add(-60 * time.Second)
	for k, rc := range p.recentClients {
		if rc.LastRequestAt.Before(cutoff) && !rc.IsStreamActive {
			delete(p.recentClients, k)
		}
	}
}

// markRecentClientStreamActive — помечает клиента как имеющего активный стриминг
func (p *Proxy) markRecentClientStreamActive(r *http.Request, active bool) {
	clientIP := p.getClientRealIP(r)
	clientName := p.getClientName(r)
	key := clientIP + "::" + clientName

	p.recentClientsMu.Lock()
	defer p.recentClientsMu.Unlock()

	if existing, ok := p.recentClients[key]; ok {
		existing.IsStreamActive = active
		existing.LastRequestAt = time.Now()
		p.recentClients[key] = existing
	}
}

// cleanupZombieSessions — проверяет и очищает зомби-сессии, освобождая бэкенды.
// Вызывается при старте ServeHTTP чтобы предотвратить "все бэкенды заняты"
// из-за старых оборванных сессий.
func (p *Proxy) cleanupZombieSessions() {
	zombies := p.sessionMgr.DetectZombieSessions(p.getZombieSessionThreshold())
	if len(zombies) == 0 {
		return
	}

	logger.Get().Warnw("detected zombie sessions, cleaning up",
		"zombie_count", len(zombies))

	for _, zombie := range zombies {
		logger.Get().Warnw("removing zombie session and releasing backend",
			"session_id", zombie.ID,
			"backend_id", zombie.BackendID,
			"model", zombie.Model,
			"last_request_at", zombie.LastRequestAt,
			"has_active_stream", zombie.HasActiveStream,
		)

		// Освобождаем бэкенд
		if zombie.BackendID != "" {
			p.releaseSlot(zombie.BackendID)
		}

		// Удаляем сессию
		p.sessionMgr.ForceRemove(zombie.ID)
	}
}

// getRecentClients — получение списка активных клиентов для монитора
func (p *Proxy) getRecentClients() []types.RecentClient {
	p.recentClientsMu.RLock()
	defer p.recentClientsMu.RUnlock()

	now := time.Now()
	cutoff := now.Add(-60 * time.Second)
	result := make([]types.RecentClient, 0, len(p.recentClients))

	for _, rc := range p.recentClients {
		if rc.LastRequestAt.After(cutoff) {
			result = append(result, *rc)
		}
	}

	// Сортируем по времени последнего запроса (новые — первые)
	sort.Slice(result, func(i, j int) bool {
		return result[i].LastRequestAt.After(result[j].LastRequestAt)
	})

	return result
}

// R59 (2026-09-03): ComputeAutosuggestions — pure delegation to ComputeSuggestions.
// Accepts loadedByBackend (map backendId → []modelName) and returns ordered suggestions.
func (p *Proxy) ComputeAutosuggestions(loadedByBackend map[string][]string) []Suggestion {
	backends := p.GetAllBackends()
	if len(backends) == 0 {
		return nil
	}
	// GetAllBackends returns []types.Backend; ComputeSuggestions expects []*types.Backend
	ptrs := make([]*types.Backend, len(backends))
	for i := range backends {
		ptrs[i] = &backends[i]
	}
	return ComputeSuggestions(ptrs, loadedByBackend)
}

// R59.2 (2026-09-03): AutoDistributeMove — wire applyFn to real cppworker.
//
// Orchestrates a "move" suggestion by:
//  1. POST /api/models/unload to the source backend (free VRAM)
//  2. POST /api/models/load to the destination backend (use freed VRAM)
//  3. On load failure: rollback by reloading on source (best-effort)
//
// Returns nil on success, error string on failure (with rollback attempted).
// Used by handlers_autosuggest.go (POST /api/v1/admin/cluster/autosuggest/apply).
func (p *Proxy) AutoDistributeMove(fromBackendID, toBackendID, modelName string) error {
	if p.modelManager == nil {
		return fmt.Errorf("modelManager not initialized")
	}

	// Step 1: unload from source
	unloadReq := ModelOpRequest{
		Operation: "unload",
		ModelName: modelName,
	}
	unloadResult := p.modelManager.ExecuteOperation(fromBackendID, unloadReq)
	if unloadResult == nil {
		return fmt.Errorf("unload returned nil result")
	}
	if !unloadResult.Success {
		return fmt.Errorf("unload failed: %s", unloadResult.Error)
	}
	logger.Get().Infow("AutoDistributeMove: unloaded from source",
		"backend", fromBackendID, "model", modelName, "message", unloadResult.Message)

	// Step 2: load on destination
	loadReq := ModelOpRequest{
		Operation: "load",
		ModelName: modelName,
	}
	loadResult := p.modelManager.ExecuteOperation(toBackendID, loadReq)
	if loadResult == nil {
		// Try rollback
		p.rollbackAutoDistributeLoad(fromBackendID, modelName, "load returned nil result")
		return fmt.Errorf("load returned nil result")
	}
	if !loadResult.Success {
		// Try rollback
		p.rollbackAutoDistributeLoad(fromBackendID, modelName, loadResult.Error)
		return fmt.Errorf("load failed (rollback attempted): %s", loadResult.Error)
	}
	logger.Get().Infow("AutoDistributeMove: loaded on destination",
		"backend", toBackendID, "model", modelName, "message", loadResult.Message)
	return nil
}

// rollbackAutoDistributeLoad — best-effort reload on source after a failed load on destination.
// Logs the result but always returns (rollback is best-effort).
func (p *Proxy) rollbackAutoDistributeLoad(sourceBackendID, modelName, originalError string) {
	logger.Get().Warnw("AutoDistributeMove: rolling back — reloading on source after failed load",
		"backend", sourceBackendID, "model", modelName, "originalError", originalError)
	loadReq := ModelOpRequest{
		Operation: "load",
		ModelName: modelName,
	}
	result := p.modelManager.ExecuteOperation(sourceBackendID, loadReq)
	if result != nil && result.Success {
		logger.Get().Infow("AutoDistributeMove: rollback succeeded", "backend", sourceBackendID, "model", modelName)
	} else {
		errMsg := "unknown"
		if result != nil {
			errMsg = result.Error
		}
		logger.Get().Errorw("AutoDistributeMove: rollback FAILED — model lost",
			"backend", sourceBackendID, "model", modelName, "error", errMsg)
	}
}

// parsedRequest — результат однократного разбора тела запроса.
// Используется чтобы избежать тройного чтения тела (recordRecentClient + extractModel + proxyRequest).
type parsedRequest struct {
	Model   string
	Stream  bool
	RawBody []byte
}

// parseRequestBody — читает и разбирает тело запроса ровно один раз.
// Возвращает parsedRequest с извлечёнными model/stream и сырым телом для последующего проксирования.
// Восстанавливает r.Body через NopCloser для повторного чтения в proxyRequest.
func (p *Proxy) parseRequestBody(r *http.Request) *parsedRequest {
	result := &parsedRequest{Stream: true} // default stream=true (Ollama behaviour)

	if r.Method != http.MethodPost || r.Body == nil {
		return result
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return result
	}
	r.Body = io.NopCloser(bytes.NewBuffer(body))
	result.RawBody = body

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return result
	}

	if m, ok := req["model"].(string); ok {
		result.Model = m
	}
	// Round 21: also extract "name" field (used by /api/show, /api/pull,
	// /api/delete, /api/copy, /api/create — Ollama-style endpoints).
	// Without this, parsed.Model="" for these endpoints, balancer thinks
	// there's no model to route to, and times out.
	if result.Model == "" {
		if n, ok := req["name"].(string); ok {
			result.Model = n
		}
	}
	if stream, ok := req["stream"].(bool); ok {
		result.Stream = stream
	}

	return result
}

// statusRecorder — обёртка http.ResponseWriter для захвата HTTP статус-кода.
type statusRecorder struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	// errBody — накопленный body response (для 4xx/5xx). Round 16 (2026-07-10).
	// Только первые 1 KB чтобы не раздувать лог.
	errBody string
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		// Заголовки уже отправлены — повторный вызов WriteHeader
		// вызывает "superfluous response.WriteHeader call" в логах.
		// Просто игнорируем.
		return
	}
	r.statusCode = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
		r.statusCode = http.StatusOK
		r.ResponseWriter.WriteHeader(http.StatusOK)
	}
	// Round 16: для error responses захватываем body чтобы логи могли показать
	// что конкретно вернул бэкенд. Обрезаем до 1 KB.
	if r.statusCode >= 400 && len(r.errBody) < 1024 {
		remaining := 1024 - len(r.errBody)
		if remaining > 0 {
			take := remaining
			if take > len(b) {
				take = len(b)
			}
			r.errBody += string(b[:take])
		}
	}
	return r.ResponseWriter.Write(b)
}

// Round 31 #6 real fix (2026-08-09): hijack support на statusRecorder.
// Без этого Round 31 #6 hijack path не активируется (type assertion w.(http.Hijacker)
// возвращает false для *statusRecorder даже при embed http.ResponseWriter).
// Делегируем к underlying ResponseWriter.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("statusRecorder: underlying ResponseWriter %T does not support hijack", r.ResponseWriter)
	}
	return hj.Hijack()
}

// Round 31 #6 real fix: Flush support (для случая если statusRecorder используется
// вместо w, который раньше кастился в http.Flusher). Hijack path требует Flusher
// для manual chunked writes. Делегируем.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// queueRequest - постановка запроса в очередь
func (p *Proxy) queueRequest(w http.ResponseWriter, r *http.Request, model string) bool {

	// Backpressure: проверяем fill rate очереди
	queueLen := len(p.queueMgr.queue)
	maxSize := p.queueMgr.maxSize
	queueFillPct := float64(queueLen) / float64(maxSize)
	if queueFillPct > 0.90 {
		logger.Get().Warnw("queue overflow, rejecting request with 503",
			"model", model, "queue_fill_pct", queueFillPct, "queue_size", queueLen)
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Service overloaded", http.StatusServiceUnavailable)
		return false
	}

	// Информируем клиента о позиции в очереди и ожидаемом времени
	// Заголовки будут доступны клиенту вместе с финальным ответом
	w.Header().Set("X-Queue-Position", fmt.Sprintf("%d", queueLen))
	estimatedWaitSec := queueLen * 2 // грубая оценка: ~2 сек на запрос
	if estimatedWaitSec < 1 {
		estimatedWaitSec = 1
	}
	queueTimeoutSec := p.config.Balancing.QueueTimeout
	if queueTimeoutSec > 0 && estimatedWaitSec > queueTimeoutSec {
		estimatedWaitSec = queueTimeoutSec
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", estimatedWaitSec))

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
