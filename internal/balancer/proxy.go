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
	llamaCppRouter  *LlamaCppRouter   // Маршрутизатор llama.cpp API endpoint'ов

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
	virtualModels       *virtualmodel.Registry
	virtualModelRouter  *virtualmodel.Router
	rpcCoordinator      *rpccoordinator.ModelCoordinator

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
	// ResponseHeaderTimeout — ключевой параметр для предотвращения UND_ERR_HEADERS_TIMEOUT
	// у клиента (OpenWebUI/undici): если Ollama не отдаёт заголовки ответа за это время,
	// балансер сам обрывает соединение и уходит в retry, не заставляя клиента ждать.
	streamingTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true, // Важно для SSE
		ResponseHeaderTimeout: 45 * time.Second, // Макс. ожидание заголовков от Ollama (меньше чем undici headersTimeout ~60s)
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

	// Инициализация ModelManager для управления моделями на бэкендах
	p.modelManager = NewModelManager(p)
	logger.Get().Infow("model manager initialized")

	return p

}

// maxConcurrentWarmups — пакетная функция для использования до создания Proxy.
func maxConcurrentWarmups(config *types.LoadBalancerConfig) int {
	n := config.Balancing.AdvancedTiming.MaxConcurrentWarmups
	if n <= 0 {
		n = 3
	}
	return n
}

// --- AdvancedTiming helpers (замена хардкодных магических чисел) ---

// getZombieSessionThreshold — порог зомби-сессий в секундах (default: 120).
func (p *Proxy) getZombieSessionThreshold() time.Duration {
	sec := p.config.Balancing.AdvancedTiming.ZombieSessionThresholdSec
	if sec <= 0 {
		sec = 120
	}
	return time.Duration(sec) * time.Second
}

// getStreamingRetryDelay — задержка перед retry streaming запроса (default: 500ms).
func (p *Proxy) getStreamingRetryDelay() time.Duration {
	ms := p.config.Balancing.AdvancedTiming.StreamingRetryDelayMs
	if ms <= 0 {
		ms = 500
	}
	return time.Duration(ms) * time.Millisecond
}

// getMaxConcurrentWarmups — макс. одновременных warmup (default: 3).
func (p *Proxy) getMaxConcurrentWarmups() int {
	n := p.config.Balancing.AdvancedTiming.MaxConcurrentWarmups
	if n <= 0 {
		n = 3
	}
	return n
}

// getHeartbeatInterval — интервал SSE heartbeat (default: 15s).
func (p *Proxy) getHeartbeatInterval() time.Duration {
	sec := p.config.Balancing.AdvancedTiming.HeartbeatIntervalSec
	if sec <= 0 {
		return 15 * time.Second
	}
	return time.Duration(sec) * time.Second
}

// getStreamingIdleTimeout — read-deadline для backend-стрима.
// Если в течение этого времени из бэкенда не приходит ни одного чанка, прокси
// считает, что бэкенд завис/SIGSEGV, и явно завершает стрим с ошибкой
// (вместо того, чтобы молча "завершать" по EOF).
// default: 120s, настройка — Balancing.StreamingIdleTimeout.
func (p *Proxy) getStreamingIdleTimeout() time.Duration {
	sec := p.config.Balancing.StreamingIdleTimeout
	if sec <= 0 {
		return 120 * time.Second
	}
	return time.Duration(sec) * time.Second
}

// getWarmupSemaphoreTimeout — таймаут ожидания семафора warmup (default: 30s).
func (p *Proxy) getWarmupSemaphoreTimeout() time.Duration {
	sec := p.config.Balancing.AdvancedTiming.WarmupSemaphoreTimeoutSec
	if sec <= 0 {
		sec = 30
	}
	return time.Duration(sec) * time.Second
}

// determineRequestBackendType определяет тип бэкенда на основе пути входящего запроса.
// - Ollama API endpoints (/api/generate, /api/chat, /api/tags, ...) →
//   если OperatingMode разрешает оба типа (standard/replication/rpc_coordinator) — возвращает "",
//   позволяя selectBackend выбрать бэкенд любого типа где загружена модель.
//   Если mode жёстко привязан к одному типу — возвращает этот тип.
// - llama.cpp / OpenAI-compatible endpoints (/v1/chat/completions, /v1/completions, ...) → BackendTypeLlamaCpp
// - Прочие пути → "" (будет определено из OperatingMode через getDefaultAllowedTypes)
func (p *Proxy) determineRequestBackendType(r *http.Request) types.BackendType {
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/") {
		// Проверяем, какие типы бэкендов разрешены текущим OperatingMode
		allowed := p.getDefaultAllowedTypes()
		if len(allowed) > 1 {
			// Режим разрешает оба типа (standard/replication/rpc_coordinator) —
			// возвращаем "", чтобы selectBackend мог выбрать бэкенд любого типа
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

// Shutdown — graceful shutdown с завершением активных SSE-сессий и сохранением состояния.
// Порядок остановки:
//  1. Запрет новых запросов (возврат 503)
//  2. Остановка приёма очереди
//  3. Остановка background контроллеров
//  4. Завершение активных SSE-сессий с done:true
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

	// Единый разбор тела запроса (избегаем тройного чтения в recordRecentClient + extractModel + proxyRequest)
	parsed := p.parseRequestBody(r)
	model := parsed.Model
	isStream := parsed.Stream

	// Определяем требуемый тип бэкенда из URL запроса
	bt := p.determineRequestBackendType(r)
	allowedTypes := p.getAllowedTypesList(bt)

	// Трекинг всех клиентов для монитора (использует уже распарсенные данные)
	p.recordRecentClientParsed(r, parsed)

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
		p.proxyLogger.Append(ProxyLogEntry{
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
		})
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
		targetBackend = p.selectBackend(model, bt)
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

	// Атомарный захват слота с retry при неудаче
	if targetBackend != "" {
		if p.tryAcquireSlot(targetBackend) {
			acquiredBackend = targetBackend
		}
	}

	// Если не удалось захватить слот на выбранном бэкенде — пробуем другие
	if targetBackend != "" && acquiredBackend == "" {
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
			Model:          model,
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
			Model:          parsed.Model,
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
	return r.ResponseWriter.Write(b)
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
