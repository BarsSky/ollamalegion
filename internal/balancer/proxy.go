package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	// EventBus
	eventSubs     map[string]chan types.Event
	eventSubsMu   sync.RWMutex
}

// contextKey - типизированный ключ для context.Value
type contextKey string

const modelContextKey contextKey = "model"
const streamContextKey contextKey = "stream"

// BackendState - состояние бэкенда
type BackendState struct {
	Backend         *types.Backend
	ActiveReqs      int
	TotalRequests   int64                          // Atomic: всего запросов на этот бэкенд
	LastUsed        time.Time
	MetricsHistory  []types.MetricsSnapshot        // История метрик для прогнозирования
	Prediction      types.Prediction               // Последний прогноз
	RequestHistory  []time.Time                    // Таймстемпы запросов для расчёта RPS (окно 60с)
	CalculatedRPS   float64                        // Вычисленный RPS
	WarmingUpModels map[string]*types.WarmupState  // Модели в превентивной загрузке
	ErrorCount      int                            // Счётчик ошибок
	TotalAttempts   int                            // Всего попыток
	mu              sync.Mutex
}

// warmupModel — загрузка модели на Ollama через POST /api/pull (асинхронно)
func (p *Proxy) warmupModel(backendID, host string, port int, model string) {
	url := fmt.Sprintf("http://%s:%d/api/pull", host, port)
	body := fmt.Sprintf(`{"name":"%s","stream":false}`, model)
	go func() {
		req, err := http.NewRequest("POST", url, strings.NewReader(body))
		if err != nil {
			logger.Get().Errorw("warmupModel: request creation failed", "backend", backendID, "model", model, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.client.Do(req)
		if err != nil {
			logger.Get().Errorw("warmupModel: request failed", "backend", backendID, "model", model, "error", err)
			return
		}
		resp.Body.Close()
		logger.Get().Infow("warmupModel: model loaded", "backend", backendID, "model", model, "status", resp.StatusCode)
	}()
}

// SessionManager - менеджер сессий
type SessionManager struct {
	sessions map[string]*types.Session
	mu       sync.RWMutex
	ttl      time.Duration
	stopCh   chan struct{}
}

// MetricsManager - менеджер метрик
type MetricsManager struct {
	metrics map[string]*types.BackendMetrics
	mu      sync.RWMutex
}

// CompletedRequest - выполненный запрос для истории
type CompletedRequest struct {
	Model       string    `json:"model"`
	Target      string    `json:"target"`
	Enqueued    time.Time `json:"enqueued"`
	CompletedAt time.Time `json:"completed_at"`
	WaitTimeMs  int64     `json:"wait_time_ms"`
}

// QueueManager - менеджер очереди с pool workers
type QueueManager struct {
	queue             chan *QueuedRequest
	mu                sync.Mutex
	maxSize           int
	numWorkers        int
	processed         int64
	timeout           time.Duration
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	proxy             *Proxy
	pendingMu         sync.RWMutex
	pending           []*QueuedRequest
	processingMu      sync.RWMutex
	processing        []*QueuedRequest
	historyMu         sync.RWMutex
	completedHistory  []*CompletedRequest
}

// QueuedRequest - запрос в очереди
type QueuedRequest struct {
	Request      *http.Request
	Writer       http.ResponseWriter
	Model        string
	Enqueued     time.Time
	Done         chan bool
	Target       string
	RequeueCount int       // Счётчик повторных постановок в очередь
	LoadDeadline time.Time // Дедлайн ожидания загрузки модели (если инициирована)
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
		eventSubs:  make(map[string]chan types.Event),
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
			Backend:    &backend,
			ActiveReqs: 0,
			LastUsed:   time.Time{},
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

	return p
}

// SetQueueManagerProxy - установка proxy для QueueManager (вызывается после создания)
func (p *Proxy) SetQueueManagerProxy() {
	p.queueMgr.proxy = p
}

// NewSessionManager - создание менеджера сессий с дефолтным TTL
func NewSessionManager() *SessionManager {
	return NewSessionManagerWithTTL(15 * time.Minute)
}

// NewSessionManagerWithTTL - создание менеджера сессий с заданным TTL
func NewSessionManagerWithTTL(ttl time.Duration) *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*types.Session),
		ttl:      ttl,
	}

	// Запуск очистителя просроченных сессий с интервалом ttl/3
	go sm.cleanupLoop()

	return sm
}

// NewMetricsManager - создание менеджера метрик
func NewMetricsManager() *MetricsManager {
	return &MetricsManager{
		metrics: make(map[string]*types.BackendMetrics),
	}
}

// NewQueueManager - создание менеджера очереди с pool workers
func NewQueueManager(proxy *Proxy, maxSize int, numWorkers int, timeout time.Duration) *QueueManager {
	ctx, cancel := context.WithCancel(context.Background())
	qm := &QueueManager{
		queue:      make(chan *QueuedRequest, maxSize),
		maxSize:    maxSize,
		numWorkers: numWorkers,
		timeout:    timeout,
		ctx:        ctx,
		cancel:     cancel,
		proxy:      proxy,
	}
	// Запускаем pool workers
	for i := 0; i < qm.numWorkers; i++ {
		qm.wg.Add(1)
		go qm.worker(i)
	}
	return qm
}

// worker - обработчик запросов из очереди
func (qm *QueueManager) worker(id int) {
	defer qm.wg.Done()
	logger.Get().Infow("queue worker started", "worker_id", id)
	
	for {
		select {
		case <-qm.ctx.Done():
			logger.Get().Infow("queue worker stopped", "worker_id", id)
			return
		case req := <-qm.queue:
			qm.removePending(req)
			qm.addProcessing(req)
			qm.processRequest(req, id)
			qm.removeProcessing(req)
		}
	}
}

// processRequest - обработка одного запроса
func (qm *QueueManager) processRequest(req *QueuedRequest, workerID int) {
	// Защита от паники: гарантируем освобождение processing и уведомление клиента
	defer func() {
		if r := recover(); r != nil {
			logger.Get().Errorw("panic in processRequest, recovered",
				"worker_id", workerID,
				"model", req.Model,
				"panic", r,
			)
			select {
			case req.Done <- false:
			default:
			}
		}
	}()

	logger.Get().Debugw("processing queued request",
		"worker_id", workerID,
		"model", req.Model,
		"requeue_count", req.RequeueCount,
	)

	// Попытка найти доступный бэкенд
	var targetBackend string

	// Если запрос requeue'ится более 3 раз — принудительно выбираем любой
	// свободный бэкенд без учёта model affinity и session stickiness.
	const maxRequeues = 3
	if req.RequeueCount >= maxRequeues {
		logger.Get().Warnw("max requeues exceeded, forcing rebalance to any free backend",
			"worker_id", workerID,
			"requeue_count", req.RequeueCount,
			"model", req.Model,
		)
		targetBackend = qm.proxy.selectFreeBackendAny()
		if targetBackend != "" {
			logger.Get().Infow("force rebalanced to free backend",
				"worker_id", workerID,
				"backend", targetBackend,
				"model", req.Model,
			)
		}
	}

	if targetBackend == "" {
		targetBackend = qm.proxy.selectBackend(req.Model)
	}

	if targetBackend == "" {
		req.RequeueCount++
		logger.Get().Warnw("no backend available, re-queueing request",
			"worker_id", workerID,
			"requeue_count", req.RequeueCount,
		)
		time.AfterFunc(100*time.Millisecond, func() {
			select {
			case qm.queue <- req:
			case <-qm.ctx.Done():
				select {
				case req.Done <- false:
				default:
				}
			}
		})
		return
	}

	// Атомарный захват слота
	if !qm.proxy.tryAcquireSlot(targetBackend) {
		// Пробуем другие бэкенды
		attempted := map[string]bool{targetBackend: true}
		const maxAltRetries = 10
		found := false
		for retry := 0; retry < maxAltRetries; retry++ {
			altBackend := qm.proxy.selectBackendExcluding(req.Model, attempted)
			if altBackend == "" {
				break
			}
			if qm.proxy.tryAcquireSlot(altBackend) {
				targetBackend = altBackend
				found = true
				break
			}
			attempted[altBackend] = true
		}
		if !found {
			req.RequeueCount++
			logger.Get().Warnw("could not acquire slot, re-queueing",
				"worker_id", workerID, "requeue_count", req.RequeueCount)
			time.AfterFunc(100*time.Millisecond, func() {
				select {
				case qm.queue <- req:
				default:
					// Канал переполнен или ctx.Done — уведомляем клиента о неудаче
					select {
					case req.Done <- false:
					default:
					}
				}
			})
			return
		}
	}

	req.Target = targetBackend
	logger.Get().Debugw("proxying queued request to backend",
		"worker_id", workerID,
		"backend", targetBackend,
	)

	// Слот захвачен через tryAcquireSlot — гарантируем освобождение
	defer qm.proxy.releaseSlot(targetBackend)

	qm.proxy.proxyRequest(req.Writer, req.Request, targetBackend)

	select {
	case req.Done <- true:
	default:
	}

	now := time.Now()
	waitTimeMs := now.Sub(req.Enqueued).Milliseconds()
	qm.mu.Lock()
	qm.processed++
	processed := qm.processed
	qm.mu.Unlock()

	qm.historyMu.Lock()
	qm.completedHistory = append(qm.completedHistory, &CompletedRequest{
		Model:       req.Model,
		Target:      req.Target,
		Enqueued:    req.Enqueued,
		CompletedAt: now,
		WaitTimeMs:  waitTimeMs,
	})
	const maxHistory = 100
	if len(qm.completedHistory) > maxHistory {
		qm.completedHistory = qm.completedHistory[len(qm.completedHistory)-maxHistory:]
	}
	qm.historyMu.Unlock()

	logger.Get().Debugw("queued request completed",
		"worker_id", workerID,
		"processed_total", processed,
		"wait_time_ms", waitTimeMs,
	)
}

// Stop - остановка всех workers
func (qm *QueueManager) Stop() {
	qm.cancel()
	qm.wg.Wait()
	close(qm.queue)
	for req := range qm.queue {
		select {
		case req.Done <- false:
		default:
		}
	}
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
					"status", backendState.Backend.Status,
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
		if targetBackend == "" {
			// Даже selectBackend не нашёл ни одного — сразу в очередь
		}
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

	logger.Get().Errorw("backend request failed", "backend", targetBackend, "error", err)
	p.UpdateBackendStatus(targetBackend, types.StatusUnhealthy)
	p.scheduleRecoveryCheck(targetBackend)

	// При ошибке пробуем другие бэкенды (без освобождения исходного — releaseSlot уже вызван в defer proxyRequest)
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
		logger.Get().Errorw("backend request failed", "backend", altBackend, "attempt", attempt, "error", err)
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

// getClientRealIP - извлечение реального IP клиента с учётом reverse proxy заголовков
func (p *Proxy) getClientRealIP(r *http.Request) string {
	// X-Forwarded-For: client, proxy1, proxy2...
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Берём первый IP в цепочке (реальный клиент)
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return ip
			}
		}
	}
	// X-Real-IP (используется nginx)
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	// Fallback: RemoteAddr (IP прямого TCP-соединения)
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// getSessionID - получение ID сессии из запроса (стабильный, без эфемерного порта)
func (p *Proxy) getSessionID(r *http.Request, clientName string) string {
	if clientID := r.Header.Get("X-Client-ID"); clientID != "" {
		return clientID
	}
	if sessionID := r.Header.Get("X-Session-ID"); sessionID != "" {
		return sessionID
	}
	if cookie, err := r.Cookie("session_id"); err == nil {
		return cookie.Value
	}

	ip := p.getClientRealIP(r)
	return ip + "::" + clientName
}

// getSessionIDWithModel - версия с учётом модели (используется при создании сессии)
func (p *Proxy) getSessionIDWithModel(r *http.Request, clientName, model string) string {
	if clientID := r.Header.Get("X-Client-ID"); clientID != "" {
		return clientID + "::" + model
	}
	if sessionID := r.Header.Get("X-Session-ID"); sessionID != "" {
		return sessionID + "::" + model
	}
	if cookie, err := r.Cookie("session_id"); err == nil {
		return cookie.Value + "::" + model
	}

	ip := p.getClientRealIP(r)
	return ip + "::" + clientName + "::" + model
}

// getClientName - извлечение имени клиента из запроса (Cline, OpenWebUI, etc.)
func (p *Proxy) getClientName(r *http.Request) string {
	if name := r.Header.Get("X-Client-Name"); name != "" {
		return name
	}
	ua := r.UserAgent()
	if strings.Contains(ua, "cline") || strings.Contains(ua, "Cline") {
		return "Cline"
	}
	if strings.Contains(ua, "open-webui") || strings.Contains(ua, "OpenWebUI") {
		return "OpenWebUI"
	}
	return ua
}

// CandidateGroup — группа бэкендов-кандидатов одного приоритета
type CandidateGroup struct {
	Priority   int      // 1=LOADED, 2=WARMING, 3=FREE, 4=FALLBACK
	BackendIDs []string
}

// CandidateGroups — упорядоченный список групп кандидатов
type CandidateGroups []CandidateGroup

// expandCandidates — группирует бэкенды по приоритетам для заданной модели
// P1 (LOADED): healthy бэкенды с моделью в памяти, loadRatio < 80%
// P2 (WARMING): healthy бэкенды где модель в WarmingUpModels
// P3 (FREE): healthy бэкенды со свободными слотами, без модели
// P4 (FALLBACK): все healthy бэкенды для resource-based scoring
func (p *Proxy) expandCandidates(modelName string) CandidateGroups {
	p.mu.RLock()
	defer p.mu.RUnlock()

	threshold := p.config.Balancing.Prewarm.TriggerLoadThreshold
	if threshold <= 0 {
		threshold = 0.80
	}

	var loaded, warming, free, fallback []string

	for id, state := range p.backends {
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		if !p.checkResourceLimits(id) {
			continue
		}

		pred := state.Prediction
		if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 300 {
			continue
		}

		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		_, isWarming := state.WarmingUpModels[modelName]
		state.mu.Unlock()

		loadRatio := 0.0
		if maxReqs > 0 {
			loadRatio = float64(active) / float64(maxReqs)
		}

		p.metricsMgr.mu.RLock()
		metrics, hasMetrics := p.metricsMgr.metrics[id]
		p.metricsMgr.mu.RUnlock()
		if !hasMetrics {
			fallback = append(fallback, id)
			continue
		}

		hasModel := false
		for _, m := range metrics.Ollama.RunningModels {
			if m.Name == modelName || strings.Contains(m.Name, modelName) {
				hasModel = true
				break
			}
		}

		if hasModel && loadRatio < threshold {
			loaded = append(loaded, id)
		} else if isWarming {
			warming = append(warming, id)
		} else if loadRatio < 1.0 {
			free = append(free, id)
		}

		// Все healthy с метриками идут в fallback
		fallback = append(fallback, id)
	}

	groups := make(CandidateGroups, 0, 4)
	if len(loaded) > 0 {
		groups = append(groups, CandidateGroup{Priority: 1, BackendIDs: loaded})
	}
	if len(warming) > 0 {
		groups = append(groups, CandidateGroup{Priority: 2, BackendIDs: warming})
	}
	if len(free) > 0 {
		groups = append(groups, CandidateGroup{Priority: 3, BackendIDs: free})
	}
	if len(fallback) > 0 {
		groups = append(groups, CandidateGroup{Priority: 4, BackendIDs: fallback})
	}

	return groups
}

// dispatchWithModelLoad — инициирует загрузку модели на free бэкенде
// Возвращает backendID и deadline для ожидания загрузки. Если модель уже загружена — возвращает "".
func (p *Proxy) dispatchWithModelLoad(model string) (string, time.Time) {
	candidates := p.expandCandidates(model)

	// Ищем лучший free backend (P3) с максимальным свободным VRAM
	var bestBackend string
	var maxFreeVRAM uint64
	for _, group := range candidates {
		if group.Priority != 3 {
			continue
		}
		for _, backendID := range group.BackendIDs {
			_, ok := p.backends[backendID]
			if !ok {
				continue
			}
			p.metricsMgr.mu.RLock()
			metrics, hasMetrics := p.metricsMgr.metrics[backendID]
			p.metricsMgr.mu.RUnlock()
			if !hasMetrics {
				continue
			}
			freeVRAM := metrics.GPU.MemoryFree
			if freeVRAM > maxFreeVRAM {
				maxFreeVRAM = freeVRAM
				bestBackend = backendID
			}
			if bestBackend == "" {
				bestBackend = backendID
			}
		}
	}

	if bestBackend == "" {
		return "", time.Time{}
	}

	// Инициируем загрузку модели
	backendState := p.backends[bestBackend]
	p.warmupModel(bestBackend, backendState.Backend.Host, backendState.Backend.OllamaPort, model)

	// Добавляем в WarmingUpModels
	backendState.mu.Lock()
	loadTimeout := 120
	if p.config.Balancing.ModelLoadTimeout > 0 {
		loadTimeout = p.config.Balancing.ModelLoadTimeout
	}
	backendState.WarmingUpModels[model] = &types.WarmupState{
		StartedAt:        time.Now(),
		EstimatedReadyAt: time.Now().Add(time.Duration(loadTimeout) * time.Second),
		TriggerReason:    "dispatch",
	}
	backendState.mu.Unlock()

	deadline := time.Now().Add(time.Duration(loadTimeout) * time.Second)
	logger.Get().Infow("dispatchWithModelLoad: initiated model load",
		"backend", bestBackend, "model", model, "deadline", deadline)

	return bestBackend, deadline
}

// selectBackend - выбор бэкенда для запроса (5-этапный алгоритм)
func (p *Proxy) selectBackend(model string) string {
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
			syncTimeout := 30 * time.Second
			if p.config.Balancing.SyncModelLoad.Timeout != "" {
				if d, err := time.ParseDuration(p.config.Balancing.SyncModelLoad.Timeout); err == nil {
					syncTimeout = d
				}
			}
			if eta > 0 && eta < syncTimeout {
				start := time.Now()
				for time.Since(start) < syncTimeout {
					if p.checkModelReadyUnsafe(backendID, model) {
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
				state := p.backends[backendID]
				p.warmupModel(backendID, state.Backend.Host, state.Backend.OllamaPort, model)
				syncTimeout := 30 * time.Second
				if p.config.Balancing.SyncModelLoad.Timeout != "" {
					if d, err := time.ParseDuration(p.config.Balancing.SyncModelLoad.Timeout); err == nil {
						syncTimeout = d
					}
				}
				start := time.Now()
				for time.Since(start) < syncTimeout {
					if p.checkModelReadyUnsafe(backendID, model) {
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

		// Согласно документации: model affinity только при loadRatio < 80%
		if maxReqs > 0 && float64(active)/float64(maxReqs) >= 0.80 {
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
	if modelName == "" { return false }
	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()
	if !ok { return false }
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

		// Согласно документации: model affinity только при loadRatio < 80%
		if maxReqs > 0 && float64(active)/float64(maxReqs) >= 0.80 {
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
		if state.Backend.Status != types.StatusHealthy { continue }
		state.mu.Lock()
		ws, exists := state.WarmingUpModels[model]
		state.mu.Unlock()
		if exists && ws != nil && time.Now().Before(ws.EstimatedReadyAt) { return id }
	}
	return ""
}

// findFreeBackendForModelUnsafe — свободный бэкенд с достаточным VRAM (без блокировки)
func (p *Proxy) findFreeBackendForModelUnsafe(model string) string {
	var best string
	var maxFree uint64
	for id, state := range p.backends {
		if state.Backend.Status != types.StatusHealthy { continue }
		if !p.checkResourceLimits(id) { continue }
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		if active > 0 || (maxReqs > 0 && active >= maxReqs) { continue }
		metrics, ok := p.metricsMgr.metrics[id]
		if !ok || metrics.GPU.MemoryTotal == 0 { continue }
		freeVRAM := metrics.GPU.MemoryTotal - metrics.GPU.MemoryUsed
		needed := estimateModelVRAM(model)
		if freeVRAM <= needed { continue }
		hasModel := false
		for _, m := range metrics.Ollama.RunningModels {
			if m.Name == model || strings.Contains(m.Name, model) { hasModel = true; break }
		}
		if !hasModel && freeVRAM > maxFree { maxFree = freeVRAM; best = id }
	}
	return best
}

// checkModelReadyUnsafe — готова ли модель на бэкенде (без блокировки)
func (p *Proxy) checkModelReadyUnsafe(backendID, model string) bool {
	metrics, ok := p.metricsMgr.metrics[backendID]
	if !ok { return false }
	for _, m := range metrics.Ollama.RunningModels {
		if m.Name == model || strings.Contains(m.Name, model) { return true }
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
	}
	return bestBackend
}

// tryAcquireSlot — атомарная проверка и захват слота на бэкенде.
// Возвращает true если слот успешно захвачен (ActiveReqs < MaxConcurrentReqs).
// Вызывающий код обязан вызвать releaseSlot после завершения запроса.
func (p *Proxy) tryAcquireSlot(backendID string) bool {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return false
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if state.Backend.Status != types.StatusHealthy {
		return false
	}

	maxReqs := state.Backend.MaxConcurrentReqs
	// Используем RuntimeMaxConcurrentRequests если задан
	if state.Backend.RuntimeMaxConcurrentRequests > 0 {
		maxReqs = state.Backend.RuntimeMaxConcurrentRequests
	}
	if maxReqs > 0 && state.ActiveReqs >= maxReqs {
		return false
	}

	state.ActiveReqs++
	state.LastUsed = time.Now()
	return true
}

// releaseSlot — освобождение слота на бэкенде.
func (p *Proxy) releaseSlot(backendID string) {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return
	}

	state.mu.Lock()
	if state.ActiveReqs > 0 {
		state.ActiveReqs--
	}
	state.mu.Unlock()
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
	}

	return bestBackend
}

// checkResourceLimits - проверка лимитов ресурсов (SOFT-режим)
func (p *Proxy) checkResourceLimits(backendID string) bool {
	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()

	if !ok {
		p.mu.RLock()
		state, exists := p.backends[backendID]
		p.mu.RUnlock()
		if !exists {
			return false
		}
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		if maxReqs > 0 && active >= maxReqs {
			return false
		}
		return true
	}

	limits := p.config.Resources

	// Headroom reservation: учитываем GPU headroom из конфигурации balancing
	headroom := p.config.Balancing.ResourceReservation.GPUHeadroomPercent
	if headroom > 0 && metrics.GPU.MemoryTotal > 0 {
		vramPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
		if vramPercent > (100 - headroom) {
			logger.Get().Warnw("backend VRAM exceeds headroom reservation, blocking",
				"backend", backendID, "vram_percent", vramPercent, "headroom_pct", headroom)
			return false
		}
	}

	if metrics.GPU.UsagePercent > 90 {
		logger.Get().Warnw("backend GPU usage critical, blocking", "backend", backendID, "gpu_usage", metrics.GPU.UsagePercent)
		return false
	}

	if limits.GPU.MaxVRAMUsagePercent > 0 && metrics.GPU.MemoryTotal > 0 {
		vramPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
		if vramPercent > limits.GPU.MaxVRAMUsagePercent {
			logger.Get().Warnw("backend VRAM usage above threshold (degraded, not blocked)",
				"backend", backendID, "vram_percent", vramPercent, "threshold", limits.GPU.MaxVRAMUsagePercent)
		}
	}

	if metrics.System.CPUUsagePercent > 95 {
		logger.Get().Warnw("backend CPU usage critical, blocking", "backend", backendID, "cpu_usage", metrics.System.CPUUsagePercent)
		return false
	}

	if limits.Memory.MaxUsagePercent > 0 && metrics.System.MemoryTotal > 0 {
		memPercent := float64(metrics.System.MemoryUsed) * 100 / float64(metrics.System.MemoryTotal)
		if memPercent > 98 {
			logger.Get().Warnw("backend RAM usage critical, blocking", "backend", backendID, "ram_percent", memPercent)
			return false
		}
	}

	if metrics.System.DiskFree < limits.Disk.MinFreeMB {
		logger.Get().Warnw("backend disk space critical, blocking", "backend", backendID,
			"disk_free_mb", metrics.System.DiskFree, "min_required_mb", limits.Disk.MinFreeMB)
		return false
	}

	if metrics.Ollama.MaxModels > 0 {
		if len(metrics.Ollama.RunningModels) >= metrics.Ollama.MaxModels {
			logger.Get().Debugw("backend model slots full (degraded, not blocked)",
				"backend", backendID, "loaded_models", len(metrics.Ollama.RunningModels), "max_models", metrics.Ollama.MaxModels)
		}
	}

	// Проверка слотов запросов вынесена в tryAcquireSlot для атомарности.
	// checkResourceLimits отвечает только за аппаратные/ресурсные лимиты.

	return true
}

// calculateScore - вычисление scores для бэкенда (v2: model affinity, queue depth, error rate)
func (p *Proxy) calculateScore(backendID string) float64 {
	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()

	state := p.backends[backendID]
	if state == nil {
		return 0
	}

	// Веса из конфига (с дефолтами)
	sc := p.config.Balancing.Scoring
	wModelLoaded := sc.ModelAlreadyLoaded; if wModelLoaded <= 0 { wModelLoaded = 0.15 }
	wModelCost := sc.ModelLoadingCost; if wModelCost <= 0 { wModelCost = 0.10 }
	wQueueDepth := sc.QueueDepthPenalty; if wQueueDepth <= 0 { wQueueDepth = 0.05 }
	wErrorRate := sc.ErrorRatePenalty; if wErrorRate <= 0 { wErrorRate = 0.05 }
	wPrediction := sc.PredictionBonus; if wPrediction <= 0 { wPrediction = 0.10 }

	if !ok {
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		score := float64(state.Backend.Weight)
		if maxReqs > 0 {
			score -= float64(active) / float64(maxReqs) * 20.0
		}
		if score < 1 { score = 1 }
		return score
	}

	// Базовые ресурсы (обновлённые веса: GPU*0.30, VRAM*0.20, CPU*0.15)
	gpuFree := 100 - metrics.GPU.UsagePercent
	vramFree := 100.0
	if metrics.GPU.MemoryTotal > 0 { vramFree = float64(metrics.GPU.MemoryFree) * 100 / float64(metrics.GPU.MemoryTotal) }
	cpuFree := 100 - metrics.System.CPUUsagePercent
	baseScore := gpuFree*0.30 + vramFree*0.20 + cpuFree*0.15

	// Request penalty
	requestPenalty := 0.0
	if metrics.Ollama.MaxConcurrentRequests > 0 {
		requestPenalty = float64(metrics.Ollama.ActiveRequests) / float64(metrics.Ollama.MaxConcurrentRequests) * 15.0
	} else if metrics.Ollama.ActiveRequests > 5 {
		requestPenalty = float64(metrics.Ollama.ActiveRequests) * 1.5
	}

	// Model capacity score (сохранено)
	modelCapacityScore := 0.0
	if metrics.Ollama.MaxModels > 0 {
		loaded := len(metrics.Ollama.RunningModels)
		modelCapacityScore = float64(metrics.Ollama.MaxModels-loaded) / float64(metrics.Ollama.MaxModels) * 10.0
	} else if metrics.Ollama.BackendCapacity.LoadableModelCount > 0 {
		switch {
		case metrics.Ollama.BackendCapacity.LoadableModelCount >= 5: modelCapacityScore = 10.0
		case metrics.Ollama.BackendCapacity.LoadableModelCount >= 3: modelCapacityScore = 7.0
		case metrics.Ollama.BackendCapacity.LoadableModelCount >= 1: modelCapacityScore = 4.0
		default: modelCapacityScore = -3.0
		}
	} else if metrics.GPU.MemoryTotal > 0 {
		var loadedVRAM uint64
		for _, m := range metrics.Ollama.RunningModels { loadedVRAM += m.VRAMUsage }
		vramRatio := float64(loadedVRAM) * 100 / float64(metrics.GPU.MemoryTotal)
		switch {
		case vramRatio > 80: modelCapacityScore = -5.0
		case vramRatio > 50: modelCapacityScore = 2.0
		default: modelCapacityScore = 8.0
		}
	}

	// Model already loaded bonus (model affinity)
	modelLoadedBonus := float64(len(metrics.Ollama.RunningModels)) * wModelLoaded * 10.0

	// Queue depth penalty (глобальная очередь)
	queueDepthPenalty := float64(len(p.queueMgr.queue)) * wQueueDepth

	// Error rate penalty
	errorRatePenalty := 0.0
	state.mu.Lock()
	if state.TotalAttempts > 10 {
		errorRatePenalty = float64(state.ErrorCount) / float64(state.TotalAttempts) * wErrorRate * 100
	}
	state.mu.Unlock()

	// Prediction bonus
	predictionBonus := 0.0
	if pred := state.Prediction; pred.SecondsToCritical < 0 || pred.SecondsToCritical >= 600 {
		predictionBonus = 3.0 * wPrediction
	} else if pred.SecondsToCritical >= 300 {
		predictionBonus = 1.5 * wPrediction
	} else if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 120 {
		predictionBonus = -5.0 * wPrediction
	}

	weightFactor := float64(state.Backend.Weight)
	if weightFactor <= 0 { weightFactor = 1 }
	score := (baseScore - requestPenalty + modelCapacityScore + modelLoadedBonus - queueDepthPenalty - errorRatePenalty + predictionBonus) * weightFactor
	if score < 0 { score = 0 }
	return score
}

// calculateModelLoadPenalty - оценка стоимости загрузки модели на бэкенд
func (p *Proxy) calculateModelLoadPenalty(backendID string, modelName string) float64 {
	if modelName == "" {
		return 0
	}

	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()
	if !ok {
		return 5.0
	}

	for _, m := range metrics.Ollama.RunningModels {
		if m.Name == modelName || strings.Contains(m.Name, modelName) {
			return 0
		}
	}

	penalty := 5.0
	if metrics.GPU.MemoryTotal > 0 {
		vramUsedPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
		if vramUsedPercent > 80 {
			penalty = 20.0
		} else if vramUsedPercent > 60 {
			penalty = 10.0
		}
	}

	return penalty
}

// getBackendModelCapacity - оценка оставшихся слотов для моделей на бэкенде
func (p *Proxy) getBackendModelCapacity(backendID string) int {
	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()
	if !ok {
		return 0
	}

	if metrics.Ollama.MaxModels > 0 {
		available := metrics.Ollama.MaxModels - len(metrics.Ollama.RunningModels)
		if available < 0 {
			return 0
		}
		return available
	}

	if metrics.GPU.MemoryTotal > 0 && metrics.GPU.MemoryTotal > metrics.GPU.MemoryUsed {
		freeVRAM := metrics.GPU.MemoryTotal - metrics.GPU.MemoryUsed
		avgModelSize := uint64(4096)
		return int(freeVRAM / avgModelSize)
	}

	return 0
}

// estimateModelVRAM - оценка VRAM, необходимого для модели
func estimateModelVRAM(modelName string) uint64 {
	lower := strings.ToLower(modelName)

	var sizeGB uint64 = 4

	sizeMap := map[string]uint64{
		":0.5b": 1, ":1b": 1, ":1.5b": 2,
		":3b": 3, ":4b": 4, ":7b": 5, ":8b": 6,
		":13b": 9, ":14b": 10, ":20b": 14,
		":32b": 22, ":34b": 24, ":40b": 28,
		":65b": 45, ":70b": 48, ":72b": 50,
		":110b": 75, ":405b": 250,
	}

	for suffix, vram := range sizeMap {
		if strings.Contains(lower, suffix) {
			sizeGB = vram
			break
		}
	}

	if strings.Contains(lower, "q4") || strings.Contains(lower, "4bit") {
		sizeGB = sizeGB * 6 / 10
	} else if strings.Contains(lower, "q5") || strings.Contains(lower, "5bit") {
		sizeGB = sizeGB * 7 / 10
	} else if strings.Contains(lower, "q8") || strings.Contains(lower, "8bit") {
		sizeGB = sizeGB * 8 / 10
	} else if strings.Contains(lower, "q2") {
		sizeGB = sizeGB * 4 / 10
	}

	if sizeGB < 1 {
		sizeGB = 1
	}

	return sizeGB * 1024
}

// recordRequest - записывает таймстемп запроса для расчёта RPS
func (p *Proxy) recordRequest(backendID string) {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return
	}

	now := time.Now()
	state.mu.Lock()
	state.RequestHistory = append(state.RequestHistory, now)
	cutoff := now.Add(-60 * time.Second)
	var startIdx int
	for i, t := range state.RequestHistory {
		if t.After(cutoff) {
			startIdx = i
			break
		}
	}
	if startIdx > 0 {
		state.RequestHistory = state.RequestHistory[startIdx:]
	}
	state.CalculatedRPS = float64(len(state.RequestHistory)) / 60.0
	state.mu.Unlock()
}

// proxyRequest - проксирование запроса к бэкенду с поддержкой streaming/SSE.
// Вызывающий код (ServeHTTP / processRequest) должен предварительно захватить слот
// через tryAcquireSlot и гарантировать вызов releaseSlot через defer после возврата.
func (p *Proxy) proxyRequest(w http.ResponseWriter, r *http.Request, backendID string) error {
	state, ok := p.backends[backendID]
	if !ok {
		return fmt.Errorf("backend %s not found", backendID)
	}

	p.recordRequest(backendID)

	targetURL := fmt.Sprintf("http://%s:%d", state.Backend.Host, state.Backend.OllamaPort)

	target, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("invalid backend URL: %v", err)
	}

	isStreamingRequest := p.isStreamingRequest(r)

	client := p.client
	if isStreamingRequest {
		client = p.streamingClient
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL+r.URL.String(), r.Body)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Пробрасываем реальный IP клиента, а не адрес контейнера Docker
	clientRealIP := p.getClientRealIP(r)
	if existingXFF := r.Header.Get("X-Forwarded-For"); existingXFF != "" {
		req.Header.Set("X-Forwarded-For", existingXFF)
	} else {
		req.Header.Set("X-Forwarded-For", clientRealIP)
	}
	req.Header.Set("X-Real-IP", clientRealIP)
	req.Host = target.Host

	resp, err := client.Do(req)
	if err != nil {
		p.logStreamingError(backendID, err)
		if p.isStreamingRequest(r) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":"Backend unreachable"}`))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		} else {
			http.Error(w, "Backend unreachable", http.StatusBadGateway)
		}
		return fmt.Errorf("backend error: %v", err)
	}
	defer resp.Body.Close()

	atomic.AddInt64(&p.totalRequests, 1)
	atomic.AddInt64(&state.TotalRequests, 1)

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Сессии создаются только для реальных клиентских запросов chat/generate/embeddings.
	// Технические запросы (монитор, агенты, Ollama API) не должны порождать ghost-сессии.
	path := r.URL.Path
	isClientRequest := (path == "/api/generate" || path == "/api/chat" || path == "/api/embeddings")
	if isClientRequest {
		clientNameForSession := p.getClientName(r)
		modelFromCtx := ""
		if m, ok := r.Context().Value(modelContextKey).(string); ok {
			modelFromCtx = m
		}
		sessionID := p.getSessionIDWithModel(r, clientNameForSession, modelFromCtx)
		if sessionID != "" {
			p.sessionMgr.Set(sessionID, backendID, modelFromCtx, clientNameForSession, p.getClientRealIP(r), r.UserAgent())
			w.Header().Set("X-Session-ID", sessionID)
		}
	}

	isStreaming := p.isStreamingResponse(resp)

	if isStreaming {
		p.handleStreamingResponse(w, r, resp, backendID)
	} else {
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}

	return nil
}

// isStreamingRequest - проверка, является ли запрос streaming запросом
func (p *Proxy) isStreamingRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}

	if isStream, ok := r.Context().Value(streamContextKey).(bool); ok {
		return isStream
	}

	if r.Body == nil {
		return false
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}

	r.Body = io.NopCloser(bytes.NewBuffer(body))

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}

	if stream, ok := req["stream"].(bool); ok {
		return stream
	}

	return true
}

// isStreamingResponse - проверка на streaming ответ (SSE или chunked)
func (p *Proxy) isStreamingResponse(resp *http.Response) bool {
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		return true
	}

	if resp.Header.Get("Transfer-Encoding") == "chunked" {
		return true
	}

	if resp.ContentLength == -1 {
		return true
	}

	if resp.Header.Get("X-Accel-Buffering") == "no" {
		return true
	}

	return false
}

// handleStreamingResponse - обработка streaming ответа с использованием Flusher.
// При разрыве соединения с бэкендом отправляет финальный chunked-маркер,
// чтобы клиент (OpenWebUI) не получал TransferEncodingError.
func (p *Proxy) handleStreamingResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, backendID string) {
	// Проверяем, является ли ответ chunked (Ollama всегда отдаёт chunked для streaming)
	isChunked := resp.Header.Get("Transfer-Encoding") == "chunked" || resp.ContentLength == -1

	w.WriteHeader(resp.StatusCode)

	flusher, ok := w.(http.Flusher)
	if !ok {
		logger.Get().Warnw("streaming detected but Flusher not supported, falling back to regular copy", "backend", backendID)
		io.Copy(w, resp.Body)
		return
	}

	logger.Get().Infow("starting streaming session", "backend", backendID,
		"content_type", resp.Header.Get("Content-Type"),
		"is_chunked", isChunked)

	buf := make([]byte, 32*1024)
	bytesStreamed := 0
	streamError := false

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				logger.Get().Errorw("error writing to client", "backend", backendID, "error", writeErr)
				streamError = true
				break
			}
			bytesStreamed += n
			flusher.Flush()
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			logger.Get().Errorw("error reading from backend", "backend", backendID, "error", err)
			streamError = true
			break
		}
	}

	// Всегда отправляем финальный chunked-маркер для chunked ответов.
	// Это необходимо для корректного завершения HTTP-ответа по протоколу chunked transfer encoding.
	// Без этого клиент получает "TransferEncodingError: Not enough data to satisfy transfer length header".
	// Даже при чистом EOF от бэкенда, Ollama не всегда отправляет финальный chunk — мы гарантируем его.
	if isChunked {
		// Финальный chunk: "0\r\n\r\n" — последний chunk нулевой длины + завершающие CRLF
		w.Write([]byte("0\r\n\r\n"))
		flusher.Flush()
	}

	logger.Get().Infow("streaming session completed", "backend", backendID,
		"bytes_streamed", bytesStreamed, "had_error", streamError, "final_chunk_sent", isChunked)
}

// logStreamingError - логирование ошибок streaming
func (p *Proxy) logStreamingError(backendID string, err error) {
	logger.Get().Errorw("streaming error", "backend", backendID, "error", err)
}

// logError - логирование ошибки
func (p *Proxy) logError(backendID string, err error) {
	logger.Get().Errorw("proxy error", "backend", backendID, "error", err)
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

// UpdateBackendStatus - обновление статуса бэкенда
func (p *Proxy) UpdateBackendStatus(backendID string, status types.BackendStatus) {
	p.mu.Lock()

	if state, ok := p.backends[backendID]; ok {
		state.mu.Lock()
		oldStatus := state.Backend.Status
		if oldStatus != status {
			state.Backend.Status = status
		}
		state.mu.Unlock()

		if oldStatus != status {
			p.PublishEvent(types.Event{
				Type:      types.EventStatusChange,
				Timestamp: time.Now().UTC(),
				BackendID: backendID,
				Data: map[string]interface{}{
					"oldStatus": string(oldStatus),
					"newStatus": string(status),
				},
			})
		}
	}
	p.mu.Unlock()
}

// UpdateBackendAgentStatus - обновление флага активного агента
func (p *Proxy) UpdateBackendAgentStatus(backendID string, hasAgent bool) {
	p.mu.Lock()

	if state, ok := p.backends[backendID]; ok {
		state.mu.Lock()
		state.Backend.HasAgent = hasAgent
		state.Backend.LastAgentContact = time.Now()
		state.mu.Unlock()
	}
	p.mu.Unlock()
}

// StartAgentTimeoutChecker - запуск фоновой проверки таймаута агентов
func (p *Proxy) StartAgentTimeoutChecker(timeout time.Duration) {
	go func() {
		ticker := time.NewTicker(timeout / 2)
		defer ticker.Stop()

		for range ticker.C {
			p.mu.Lock()
			for _, state := range p.backends {
				if state.Backend.HasAgent && time.Since(state.Backend.LastAgentContact) > timeout {
					state.Backend.HasAgent = false
					logger.Get().Warnw("agent timeout", "backend", state.Backend.ID, "timeout", timeout)
				}
			}
			p.mu.Unlock()
		}
	}()
}

// GetClusterState - получение состояния кластера
func (p *Proxy) GetClusterState() *types.ClusterState {
	p.mu.RLock()
	
	backendsCopy := make(map[string]*BackendState, len(p.backends))
	for id, state := range p.backends {
		backendsCopy[id] = state
	}
	
	p.mu.RUnlock()
	
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()

	state := &types.ClusterState{
		Timestamp:       time.Now().UTC(),
		TotalBackends:   len(backendsCopy),
		HealthyBackends: 0,
		Backends:        make([]types.BackendMetrics, 0, len(backendsCopy)),
	}

	state.TotalRequests = atomic.LoadInt64(&p.totalRequests)

	for id, backendState := range backendsCopy {
		backendState.mu.Lock()
		status := backendState.Backend.Status
		hasAgent := backendState.Backend.HasAgent
		prediction := backendState.Prediction
		backendState.mu.Unlock()

		if status == types.StatusHealthy {
			state.HealthyBackends++
		}

		backendConfig := *backendState.Backend

		// Приоритет: RuntimeMaxConcurrentRequests > MaxConcurrentReqs
		maxConcurrent := backendConfig.MaxConcurrentReqs
		if backendConfig.RuntimeMaxConcurrentRequests > 0 {
			maxConcurrent = backendConfig.RuntimeMaxConcurrentRequests
		}
		metrics := types.BackendMetrics{
			ID:        id,
			Timestamp: time.Now().UTC(),
			Status:    status,
			HasAgent:  hasAgent,
			Host:      backendConfig.Host,
			OllamaPort: backendConfig.OllamaPort,
			GPU:       types.GPUMetrics{},
			System:    types.SystemMetrics{},
			Ollama:    types.OllamaMetrics{RunningModels: []types.RunningModel{}},
			MaxConcurrentRequests: maxConcurrent,
		}

		if agentMetrics, ok := p.metricsMgr.metrics[id]; ok {
			metrics = *agentMetrics
			metrics.Status = status
			metrics.HasAgent = hasAgent
			metrics.Prediction = prediction
			metrics.MaxConcurrentRequests = maxConcurrent
			state.ActiveRequests += agentMetrics.Ollama.ActiveRequests
			backendState.mu.Lock()
			if backendState.CalculatedRPS > 0 {
				state.RPS += backendState.CalculatedRPS
				metrics.Ollama.RequestsPerSecond = backendState.CalculatedRPS
			} else {
				state.RPS += agentMetrics.Ollama.RequestsPerSecond
			}
			metrics.Ollama.TotalRequests = backendState.TotalRequests
			backendState.mu.Unlock()
			state.TotalGPUUsage += agentMetrics.GPU.UsagePercent
		} else {
			metrics.MaxConcurrentRequests = backendConfig.MaxConcurrentReqs
			backendState.mu.Lock()
			metrics.Ollama.TotalRequests = backendState.TotalRequests
			backendState.mu.Unlock()
		}

		state.Backends = append(state.Backends, metrics)
	}

	return state
}

// Stop - остановка cleanup loop
func (sm *SessionManager) Stop() {
	select {
	case <-sm.stopCh:
	default:
		close(sm.stopCh)
	}
}

// cleanupLoop - цикл очистки сессий
func (sm *SessionManager) cleanupLoop() {
	interval := sm.ttl / 3
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			sm.cleanup()
		case <-sm.stopCh:
			return
		}
	}
}

// cleanup - очистка просроченных сессий
func (sm *SessionManager) cleanup() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	for id, session := range sm.sessions {
		if now.Sub(session.LastRequestAt) > sm.ttl {
			delete(sm.sessions, id)
		}
	}
}

// Get - получение сессии
func (sm *SessionManager) Get(id string) *types.Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	return sm.sessions[id]
}

// GetAll - получение всех сессий
func (sm *SessionManager) GetAll() []*types.Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	sessions := make([]*types.Session, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// Delete - удаление сессии по ID
func (sm *SessionManager) Delete(id string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if _, exists := sm.sessions[id]; exists {
		delete(sm.sessions, id)
		return true
	}
	return false
}

// Clear - удаление всех сессий
func (sm *SessionManager) Clear() {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.sessions = make(map[string]*types.Session)
}

// Set - установка сессии (с clientName, clientIP и userAgent)
func (sm *SessionManager) Set(id, backendID, model, clientName, clientIP, userAgent string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	if session, ok := sm.sessions[id]; ok {
		session.BackendID = backendID
		session.Model = model
		session.ClientName = clientName
		session.ClientIP = clientIP
		session.UserAgent = userAgent
		session.LastRequestAt = now
		session.RequestCount++
	} else {
		if clientIP == "" {
			clientIP = id
			if idx := strings.Index(id, "::"); idx > 0 {
				clientIP = id[:idx]
			} else if ip, _, err := net.SplitHostPort(id); err == nil {
				clientIP = ip
			}
		}
		sm.sessions[id] = &types.Session{
			ID:            id,
			BackendID:     backendID,
			Model:         model,
			ClientName:    clientName,
			ClientIP:      clientIP,
			UserAgent:     userAgent,
			CreatedAt:     now,
			LastRequestAt: now,
			RequestCount:  1,
		}
	}
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

// addPending - добавление запроса в pending список
func (qm *QueueManager) addPending(req *QueuedRequest) {
	qm.pendingMu.Lock()
	qm.pending = append(qm.pending, req)
	qm.pendingMu.Unlock()
}

// removePending - удаление запроса из pending списка
func (qm *QueueManager) removePending(req *QueuedRequest) {
	qm.pendingMu.Lock()
	for i, r := range qm.pending {
		if r == req {
			qm.pending = append(qm.pending[:i], qm.pending[i+1:]...)
			break
		}
	}
	qm.pendingMu.Unlock()
}

// addProcessing - добавление запроса в processing список
func (qm *QueueManager) addProcessing(req *QueuedRequest) {
	qm.processingMu.Lock()
	qm.processing = append(qm.processing, req)
	qm.processingMu.Unlock()
}

// removeProcessing - удаление запроса из processing списка
func (qm *QueueManager) removeProcessing(req *QueuedRequest) {
	qm.processingMu.Lock()
	for i, r := range qm.processing {
		if r == req {
			qm.processing = append(qm.processing[:i], qm.processing[i+1:]...)
			break
		}
	}
	qm.processingMu.Unlock()
}

// getProcessingDTOs - получение DTO processing запросов для API
func (qm *QueueManager) getProcessingDTOs() []map[string]interface{} {
	qm.processingMu.RLock()
	defer qm.processingMu.RUnlock()

	result := make([]map[string]interface{}, 0, len(qm.processing))
	now := time.Now()
	for _, req := range qm.processing {
		result = append(result, map[string]interface{}{
			"model":      req.Model,
			"enqueued":   req.Enqueued.UTC().Format(time.RFC3339),
			"waitTimeMs": now.Sub(req.Enqueued).Milliseconds(),
			"target":     req.Target,
			"status":     "processing",
		})
	}
	return result
}

// getPendingDTOs - получение DTO pending запросов для API
func (qm *QueueManager) getPendingDTOs() []map[string]interface{} {
	qm.pendingMu.RLock()
	defer qm.pendingMu.RUnlock()

	result := make([]map[string]interface{}, 0, len(qm.pending))
	now := time.Now()
	for _, req := range qm.pending {
		result = append(result, map[string]interface{}{
			"model":      req.Model,
			"enqueued":   req.Enqueued.UTC().Format(time.RFC3339),
			"waitTimeMs": now.Sub(req.Enqueued).Milliseconds(),
			"target":     req.Target,
			"status":     "pending",
		})
	}
	return result
}

// GetQueuePendingRequests — получение списка ожидающих запросов в очереди
func (p *Proxy) GetQueuePendingRequests() []map[string]interface{} {
	return p.queueMgr.getPendingDTOs()
}

// GetQueueProcessingRequests — получение списка обрабатываемых запросов
func (p *Proxy) GetQueueProcessingRequests() []map[string]interface{} {
	return p.queueMgr.getProcessingDTOs()
}

// GetQueueHistory - получение истории выполненных запросов
func (p *Proxy) GetQueueHistory() []map[string]interface{} {
	p.queueMgr.historyMu.RLock()
	defer p.queueMgr.historyMu.RUnlock()

	result := make([]map[string]interface{}, 0, len(p.queueMgr.completedHistory))
	for _, req := range p.queueMgr.completedHistory {
		result = append(result, map[string]interface{}{
			"model":        req.Model,
			"target":       req.Target,
			"enqueued":     req.Enqueued.UTC().Format(time.RFC3339),
			"completed_at": req.CompletedAt.UTC().Format(time.RFC3339),
			"wait_time_ms": req.WaitTimeMs,
		})
	}
	return result
}

// GetQueueStats - получение статистики очереди
func (p *Proxy) GetQueueStats() QueueStats {
	p.queueMgr.mu.Lock()
	processed := p.queueMgr.processed
	workers := p.queueMgr.numWorkers
	timeout := p.queueMgr.timeout
	p.queueMgr.mu.Unlock()

	// Вычисляем среднее время ожидания из истории завершённых запросов
	var avgWaitMs int64
	p.queueMgr.historyMu.RLock()
	if len(p.queueMgr.completedHistory) > 0 {
		// Берём последние 100 записей для актуального среднего
		history := p.queueMgr.completedHistory
		start := 0
		if len(history) > 100 {
			start = len(history) - 100
		}
		var totalMs int64
		count := 0
		for i := start; i < len(history); i++ {
			totalMs += history[i].WaitTimeMs
			count++
		}
		if count > 0 {
			avgWaitMs = totalMs / int64(count)
		}
	}
	p.queueMgr.historyMu.RUnlock()

	return QueueStats{
		CurrentSize:   len(p.queueMgr.queue),
		MaxSize:       p.queueMgr.maxSize,
		Processed:     processed,
		WaitTimeAvgMs: avgWaitMs,
		Workers:       workers,
		TimeoutSec:    int(timeout.Seconds()),
	}
}

// QueueStats - статистика очереди
type QueueStats struct {
	CurrentSize   int   `json:"current_size"`
	MaxSize       int   `json:"max_size"`
	Processed     int64 `json:"processed_total"`
	WaitTimeAvgMs int64 `json:"avg_wait_time_ms"`
	Workers       int   `json:"workers"`
	TimeoutSec    int   `json:"timeout_sec"`
}

// AddBackend - добавление нового бэкенда
func (p *Proxy) AddBackend(backend types.Backend) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.backends[backend.ID]; exists {
		return fmt.Errorf("backend with ID %s already exists", backend.ID)
	}

	if backend.Weight == 0 {
		backend.Weight = 1
	}
	if backend.MaxConcurrentReqs == 0 {
		backend.MaxConcurrentReqs = 10
	}
	if backend.OllamaPort == 0 {
		backend.OllamaPort = 11434
	}
	if backend.AgentPort == 0 {
		backend.AgentPort = 18032
	}
	if backend.Status == "" {
		backend.Status = types.StatusStarting
	}

	p.backends[backend.ID] = &BackendState{
		Backend:    &backend,
		ActiveReqs: 0,
		LastUsed:   time.Time{},
	}

	p.PublishEvent(types.Event{
		Type:      types.EventBackendAdd,
		Timestamp: time.Now().UTC(),
		BackendID: backend.ID,
		Data: map[string]interface{}{
			"name": backend.Name,
			"host": backend.Host,
		},
	})

	p.scheduleSave()

	return nil
}

// Restart - инициирует перезапуск балансера через graceful shutdown
func (p *Proxy) Restart() error {
	logger.Get().Infow("restart triggered, flushing state and exiting")

	if err := p.FlushState(); err != nil {
		logger.Get().Errorw("failed to flush state during restart", "error", err)
	}

	time.Sleep(100 * time.Millisecond)

	os.Exit(0)
	return nil
}

// RemoveBackend - удаление бэкенда с graceful drain активных запросов.
// Помечает бэкенд как Draining, ждёт завершения активных запросов (с таймаутом 30с),
// затем удаляет из backends, metrics и принудительно сохраняет state.json.
func (p *Proxy) RemoveBackend(backendID string) error {
	p.mu.Lock()
	state, exists := p.backends[backendID]
	if !exists {
		p.mu.Unlock()
		return fmt.Errorf("backend with ID %s not found", backendID)
	}

	// Помечаем как Draining — новые запросы не будут направляться
	oldStatus := state.Backend.Status
	state.Backend.Status = types.StatusDraining
	p.mu.Unlock()

	if oldStatus != types.StatusDraining {
		p.PublishEvent(types.Event{
			Type:      types.EventStatusChange,
			Timestamp: time.Now().UTC(),
			BackendID: backendID,
			Data: map[string]interface{}{
				"oldStatus": string(oldStatus),
				"newStatus": string(types.StatusDraining),
			},
		})
	}

	// Ждём завершения активных запросов с таймаутом 30 секунд
	drainTimeout := time.After(30 * time.Second)
	drainTicker := time.NewTicker(200 * time.Millisecond)
	defer drainTicker.Stop()

drainLoop:
	for {
		select {
		case <-drainTimeout:
			logger.Get().Warnw("drain timeout reached, force-removing backend",
				"backend", backendID)
			break drainLoop
		case <-drainTicker.C:
			state.mu.Lock()
			active := state.ActiveReqs
			state.mu.Unlock()
			if active == 0 {
				logger.Get().Infow("all active requests drained, removing backend",
					"backend", backendID)
				break drainLoop
			}
		}
	}

	// Удаление бэкенда
	p.mu.Lock()
	delete(p.backends, backendID)
	p.mu.Unlock()

	p.metricsMgr.mu.Lock()
	delete(p.metricsMgr.metrics, backendID)
	p.metricsMgr.mu.Unlock()

	p.PublishEvent(types.Event{
		Type:      types.EventBackendRemove,
		Timestamp: time.Now().UTC(),
		BackendID: backendID,
		Data:      map[string]interface{}{},
	})

	// Немедленное сохранение state.json, чтобы удалённый бэкенд не восстановился при перезагрузке
	if err := p.FlushState(); err != nil {
		logger.Get().Errorw("failed to flush state after backend removal", "backend", backendID, "error", err)
	}

	logger.Get().Infow("backend removed and state flushed", "backend", backendID)
	return nil
}

// UpdateBackend - обновление бэкенда
func (p *Proxy) UpdateBackend(backendID string, updated types.Backend) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	state, exists := p.backends[backendID]
	if !exists {
		return fmt.Errorf("backend with ID %s not found", backendID)
	}

	currentStatus := state.Backend.Status
	currentActiveReqs := state.ActiveReqs

	updated.ID = backendID
	updated.Status = currentStatus
	state.Backend = &updated
	state.ActiveReqs = currentActiveReqs

	p.scheduleSave()

	return nil
}

// scheduleRecoveryCheck - планирование проверки восстановления бэкенда
func (p *Proxy) scheduleRecoveryCheck(backendID string) {
	logger.Get().Infow("scheduled recovery check", "backend", backendID)
}

// GetBackend - получение бэкенда по ID
func (p *Proxy) GetBackend(backendID string) *types.Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if state, exists := p.backends[backendID]; exists {
		return state.Backend
	}
	return nil
}

// GetSessions - получение всех сессий
func (p *Proxy) GetSessions() []*types.Session {
	return p.sessionMgr.GetAll()
}

// DeleteSession - удаление сессии
func (p *Proxy) DeleteSession(id string) bool {
	return p.sessionMgr.Delete(id)
}

// ClearSessions - очистка всех сессий
func (p *Proxy) ClearSessions() {
	p.sessionMgr.Clear()
}

// GetAllBackends - получение всех бэкендов
func (p *Proxy) GetAllBackends() []types.Backend {
	p.mu.RLock()
	defer p.mu.RUnlock()

	backends := make([]types.Backend, 0, len(p.backends))
	for _, state := range p.backends {
		backends = append(backends, *state.Backend)
	}
	return backends
}

// GetPrediction - получение прогноза для бэкенда
func (p *Proxy) GetPrediction(backendID string) types.Prediction {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if state, ok := p.backends[backendID]; ok {
		return state.Prediction
	}
	return types.Prediction{
		SecondsToCritical: -1,
		CriticalReason:      "none",
	}
}

// BackendExists - проверка существования бэкенда
func (p *Proxy) BackendExists(backendID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	_, exists := p.backends[backendID]
	return exists
}

// SaveState - сохранение текущего состояния backends в state.json
func (p *Proxy) SaveState() error {
	p.mu.RLock()
	backends := make([]types.Backend, 0, len(p.backends))
	for _, state := range p.backends {
		backends = append(backends, *state.Backend)
	}
	p.mu.RUnlock()

	state := types.StateFile{
		Version:  types.StateVersion,
		Updated:  time.Now().UTC(),
		Backends: backends,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	dir := filepath.Dir(p.statePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create state directory: %w", err)
		}
	}

	if err := os.WriteFile(p.statePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	logger.Get().Infow("state saved", "path", p.statePath, "backends", len(backends))
	return nil
}

// LoadState - загрузка состояния из state.json (merge поверх текущих backends)
func (p *Proxy) LoadState() error {
	data, err := os.ReadFile(p.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("state file does not exist")
		}
		return fmt.Errorf("failed to read state file: %w", err)
	}

	var state types.StateFile
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("failed to parse state file: %w", err)
	}

	if state.Version != types.StateVersion {
		logger.Get().Warnw("state version mismatch", "got", state.Version, "expected", types.StateVersion)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	stateBackendMap := make(map[string]types.Backend)
	for _, b := range state.Backends {
		stateBackendMap[b.ID] = b
	}

	for i := range p.config.Backends {
		if saved, ok := stateBackendMap[p.config.Backends[i].ID]; ok {
			p.config.Backends[i].Status = saved.Status
			p.config.Backends[i].LastHealthCheck = saved.LastHealthCheck
			p.config.Backends[i].ConsecutiveFailures = saved.ConsecutiveFailures
			p.config.Backends[i].ActiveRequests = saved.ActiveRequests
			p.config.Backends[i].HasAgent = saved.HasAgent
			p.config.Backends[i].LastAgentContact = saved.LastAgentContact

			if bs, exists := p.backends[p.config.Backends[i].ID]; exists {
				bs.Backend = &p.config.Backends[i]
			}
		}
	}

	for _, saved := range state.Backends {
		if _, exists := p.backends[saved.ID]; !exists {
			backend := saved
			p.backends[backend.ID] = &BackendState{
				Backend:    &backend,
				ActiveReqs: 0,
				LastUsed:   time.Time{},
			}
			p.config.Backends = append(p.config.Backends, backend)
		}
	}

	logger.Get().Infow("state loaded", "path", p.statePath, "backends", len(state.Backends))
	return nil
}

// scheduleSave - debounced autosave через 5 секунд
func (p *Proxy) scheduleSave() {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()

	if p.saveTimer != nil {
		p.saveTimer.Stop()
	}

	p.saveTimer = time.AfterFunc(5*time.Second, func() {
		if err := p.SaveState(); err != nil {
			logger.Get().Errorw("autosave error", "error", err)
		}
	})
}

// FlushState - немедленное сохранение состояния (для graceful shutdown)
func (p *Proxy) FlushState() error {
	p.saveMu.Lock()
	if p.saveTimer != nil {
		p.saveTimer.Stop()
		p.saveTimer = nil
	}
	p.saveMu.Unlock()

	return p.SaveState()
}

// SubscribeEvents — подписка на события, возвращает канал и ID подписки
func (p *Proxy) SubscribeEvents() (string, <-chan types.Event) {
	p.eventSubsMu.Lock()
	defer p.eventSubsMu.Unlock()

	id := fmt.Sprintf("sub-%d", time.Now().UnixNano())
	ch := make(chan types.Event, 64)
	p.eventSubs[id] = ch
	return id, ch
}

// UnsubscribeEvents — отписка от событий
func (p *Proxy) UnsubscribeEvents(id string) {
	p.eventSubsMu.Lock()
	defer p.eventSubsMu.Unlock()

	if ch, ok := p.eventSubs[id]; ok {
		close(ch)
		delete(p.eventSubs, id)
	}
}

// PublishEvent — публикация события всем подписчикам (неблокирующая)
func (p *Proxy) PublishEvent(ev types.Event) {
	p.eventSubsMu.RLock()
	subs := make([]chan types.Event, 0, len(p.eventSubs))
	for _, ch := range p.eventSubs {
		subs = append(subs, ch)
	}
	p.eventSubsMu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// --- Exported test helpers ---

// ExpandCandidates — экспортируемая обёртка над expandCandidates для тестов
func (p *Proxy) ExpandCandidates(modelName string) CandidateGroups {
	return p.expandCandidates(modelName)
}

// DispatchWithModelLoad — экспортируемая обёртка над dispatchWithModelLoad для тестов
func (p *Proxy) DispatchWithModelLoad(model string) (string, time.Time) {
	return p.dispatchWithModelLoad(model)
}

// SetBackendMetrics — установка метрик в MetricsManager для тестов
func (p *Proxy) SetBackendMetrics(backendID string, metrics *types.BackendMetrics) {
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics[backendID] = metrics
	p.metricsMgr.mu.Unlock()
}

// SelectBackend — экспортируемая обёртка для тестов
func (p *Proxy) SelectBackend(model string) string {
	return p.selectBackend(model)
}

// GetBackendState — экспортируемый доступ к BackendState для тестов
func (p *Proxy) GetBackendState(backendID string) *BackendState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.backends[backendID]
}

// GetWarmingUpModels — получение WarmingUpModels для тестов (thread-safe)
func (p *Proxy) GetWarmingUpModels(backendID string) map[string]*types.WarmupState {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	// Return a copy
	result := make(map[string]*types.WarmupState, len(state.WarmingUpModels))
	for k, v := range state.WarmingUpModels {
		result[k] = v
	}
	return result
}

// GetConfig — получение конфигурации для тестов
func (p *Proxy) GetConfig() *types.LoadBalancerConfig {
	return p.config
}

// SetWarmingUpModel — установка warming-модели для тестов
func (p *Proxy) SetWarmingUpModel(backendID, modelName string, readyAt time.Time) {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return
	}
	state.mu.Lock()
	if state.WarmingUpModels == nil {
		state.WarmingUpModels = make(map[string]*types.WarmupState)
	}
	state.WarmingUpModels[modelName] = &types.WarmupState{
		StartedAt:        time.Now(),
		EstimatedReadyAt: readyAt,
		TriggerReason:    "test",
	}
	state.mu.Unlock()
}

// StopQueue — остановка QueueManager и всех workers
func (p *Proxy) StopQueue() {
	if p.queueMgr != nil {
		p.queueMgr.Stop()
	}
}

// StopSessionManager — остановка SessionManager (cleanup loop)
func (p *Proxy) StopSessionManager() {
	if p.sessionMgr != nil {
		p.sessionMgr.Stop()
	}
}

// StopAgentTimeoutChecker — остановка проверки таймаута агентов
func (p *Proxy) StopAgentTimeoutChecker() {
}

// UpdateBackendLimits — обновление runtime-лимитов бэкенда
func (p *Proxy) UpdateBackendLimits(backendID string, maxModels, maxConcurrentRequests int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	state, exists := p.backends[backendID]
	if !exists {
		return fmt.Errorf("backend with ID %s not found", backendID)
	}

	state.Backend.RuntimeMaxModels = maxModels
	state.Backend.RuntimeMaxConcurrentRequests = maxConcurrentRequests

	p.PublishEvent(types.Event{
		Type:      types.EventLimitsChange,
		Timestamp: time.Now().UTC(),
		BackendID: backendID,
		Data: map[string]interface{}{
			"maxModels":             maxModels,
			"maxConcurrentRequests": maxConcurrentRequests,
		},
	})

	p.scheduleSave()

	return nil
}