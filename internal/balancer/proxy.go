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
}

// BackendState - состояние бэкенда
type BackendState struct {
	Backend         *types.Backend
	ActiveReqs      int
	TotalRequests   int64                 // Atomic: всего запросов на этот бэкенд
	LastUsed        time.Time
	MetricsHistory  []types.MetricsSnapshot // История метрик для прогнозирования
	Prediction      types.Prediction        // Последний прогноз
	RequestHistory  []time.Time             // Таймстемпы запросов для расчёта RPS (окно 60с)
	CalculatedRPS   float64                 // Вычисленный RPS
	mu              sync.Mutex
}

// SessionManager - менеджер сессий
type SessionManager struct {
	sessions map[string]*types.Session
	mu       sync.RWMutex
	ttl      time.Duration
}

// MetricsManager - менеджер метрик
type MetricsManager struct {
	metrics map[string]*types.BackendMetrics
	mu      sync.RWMutex
}

// QueueManager - менеджер очереди с pool workers
type QueueManager struct {
	queue      chan *QueuedRequest
	mu         sync.Mutex
	maxSize    int
	numWorkers int
	processed  int64
	timeout    time.Duration
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	proxy      *Proxy
}

// QueuedRequest - запрос в очереди
type QueuedRequest struct {
	Request  *http.Request
	Writer   http.ResponseWriter
	Model    string
	Enqueued time.Time
	Done     chan bool
	Target   string
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

	p := &Proxy{
		config:     config,
		backends:   make(map[string]*BackendState),
		sessionMgr: NewSessionManager(),
		metricsMgr: NewMetricsManager(),
		predictor:  NewPredictor(),
		statePath:  config.LoadBalancer.StatePath,
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
		fmt.Printf("[Proxy] State file not loaded: %v (using config only)\n", err)
	}

	// Запуск фоновой проверки таймаута агентов (30 секунд)
	go p.StartAgentTimeoutChecker(30 * time.Second)

	return p
}

// SetQueueManagerProxy - установка proxy для QueueManager (вызывается после создания)
func (p *Proxy) SetQueueManagerProxy() {
	p.queueMgr.proxy = p
}

// NewSessionManager - создание менеджера сессий
func NewSessionManager() *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*types.Session),
		ttl:      30 * time.Minute,
	}

	// Запуск очистителя просроченных сессий
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
	fmt.Printf("[QueueManager] Worker %d started\n", id)
	defer fmt.Printf("[QueueManager] Worker %d stopped\n", id)
	
	for {
		select {
		case <-qm.ctx.Done():
			return
		case req := <-qm.queue:
			qm.processRequest(req, id)
		}
	}
}

// processRequest - обработка одного запроса
func (qm *QueueManager) processRequest(req *QueuedRequest, workerID int) {
	fmt.Printf("[QueueManager] Worker %d processing request for model: %s\n", workerID, req.Model)
	
	// Попытка найти доступный бэкенд
	targetBackend := qm.proxy.selectBackend(req.Model)

	if targetBackend == "" {
		fmt.Printf("[QueueManager] Worker %d no backend available, re-queueing request\n", workerID)
		// Нет доступных бэкендов - возвращаем запрос в очередь с задержкой
		time.AfterFunc(100*time.Millisecond, func() {
			select {
			case qm.queue <- req:
			case <-qm.ctx.Done():
				// При остановке отменяем запрос
				select {
				case req.Done <- false:
				default:
				}
			}
		})
		return
	}

	// Устанавливаем целевой бэкенд и выполняем запрос
	req.Target = targetBackend
	fmt.Printf("[QueueManager] Worker %d proxying request to backend: %s\n", workerID, targetBackend)

	// Проксируем запрос
	qm.proxy.proxyRequest(req.Writer, req.Request, targetBackend)

	// Сигнал о успешном выполнении
	select {
	case req.Done <- true:
	default:
	}

	// Обновление счетчика обработанных запросов
	qm.mu.Lock()
	qm.processed++
	qm.mu.Unlock()
	fmt.Printf("[QueueManager] Worker %d completed request. Total processed: %d\n", workerID, qm.processed)
}

// Stop - остановка всех workers
func (qm *QueueManager) Stop() {
	// Отменяем контекст - это остановит всех workers
	qm.cancel()
	// Ждем завершения всех workers
	qm.wg.Wait()
	// Отменяем все запросы в очереди
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
	// Получение модели из запроса
	model := p.extractModel(r)

	// Проверка сессии
	sessionID := p.getSessionID(r)
	var targetBackend string

	if sessionID != "" && p.config.Balancing.SessionStickiness {
		// Есть сессия - используем тот же бэкенд
		if session := p.sessionMgr.Get(sessionID); session != nil {
			targetBackend = session.BackendID
			
			// Проверяем статус бэкенда из сессии
			p.mu.RLock()
			backendState, exists := p.backends[targetBackend]
			p.mu.RUnlock()
			
			if !exists || backendState.Backend.Status != types.StatusHealthy {
				// Бэкенд из сессии недоступен, выбираем новый
				fmt.Printf("[%s] Session backend %s unavailable (exists: %v, status: %s), selecting new backend\n",
					time.Now().Format(time.RFC3339), targetBackend, exists, backendState.Backend.Status)
				targetBackend = ""
			}
		}
	}

	// Если нет сессии или сессия не найдена - выбираем бэкенд
	if targetBackend == "" {
		targetBackend = p.selectBackend(model)
		
		// Обновляем сессию с новым бэкендом
		if sessionID != "" && targetBackend != "" && p.config.Balancing.SessionStickiness {
			p.sessionMgr.Set(sessionID, targetBackend, model)
		}
	}

	// Если бэкенд не выбран - ставим в очередь
	if targetBackend == "" {
		if !p.queueRequest(w, r, model) {
			http.Error(w, "Service unavailable - all backends busy", http.StatusServiceUnavailable)
		}
		return
	}

	// Проксирование запроса с retry/failover (до 3 попыток)
	attemptedBackends := make(map[string]bool)
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			// Retry: выбираем другой бэкенд, исключая уже попробованные
			fmt.Printf("[%s] Retry attempt %d: selecting new backend (excluding: %v)\n",
				time.Now().Format(time.RFC3339), attempt, attemptedBackends)
			targetBackend = p.selectBackendExcluding(model, attemptedBackends)
			if targetBackend == "" {
				break
			}
			// Обновляем сессию для retry
			if sessionID != "" && p.config.Balancing.SessionStickiness {
				p.sessionMgr.Set(sessionID, targetBackend, model)
			}
		}

		attemptedBackends[targetBackend] = true
		err := p.proxyRequest(w, r, targetBackend)
		if err == nil {
			return // Успешно
		}

		fmt.Printf("[%s] Backend %s failed on attempt %d: %v\n",
			time.Now().Format(time.RFC3339), targetBackend, attempt, err)

		// Помечаем бэкенд как недоступный и планируем проверку восстановления
		p.UpdateBackendStatus(targetBackend, types.StatusUnhealthy)
		p.scheduleRecoveryCheck(targetBackend)
	}

	// Все попытки исчерпаны
	http.Error(w, "Service unavailable - all backends failed", http.StatusServiceUnavailable)
}

// extractModel - извлечение модели из запроса
func (p *Proxy) extractModel(r *http.Request) string {
	// Для POST запросов читаем тело
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return ""
		}

		// Восстанавливаем тело для дальнейшего использования
		r.Body = io.NopCloser(bytes.NewBuffer(body))

		// Парсим JSON
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			return ""
		}

		if model, ok := req["model"].(string); ok {
			return model
		}
	}

	return ""
}

// getSessionID - получение ID сессии из запроса
func (p *Proxy) getSessionID(r *http.Request) string {
	// Проверяем заголовок X-Session-ID
	if sessionID := r.Header.Get("X-Session-ID"); sessionID != "" {
		return sessionID
	}

	// Проверяем cookie
	if cookie, err := r.Cookie("session_id"); err == nil {
		return cookie.Value
	}

	// Используем IP как идентификатор сессии
	return r.RemoteAddr
}

// selectBackend - выбор бэкенда для запроса
func (p *Proxy) selectBackend(model string) string {
	p.mu.RLock()

	// Model Affinity - проверяем, есть ли модель уже загружена
	if p.config.Balancing.ModelAffinity && model != "" {
		if backend := p.findBackendWithModel(model); backend != "" {
			p.mu.RUnlock()
			return backend
		}
	}
	p.mu.RUnlock()

	// Resource-Aware выбор (с проверкой, что есть доступные бэкенды)
	backend := p.selectByResources()
	if backend == "" {
		fmt.Printf("[%s] ⚠️  Нет доступных бэкендов - балансировщик ждёт восстановления...\n", time.Now().Format(time.RFC3339))
	}
	return backend
}

// selectBackendExcluding - выбор бэкенда, исключая указанные
func (p *Proxy) selectBackendExcluding(model string, exclude map[string]bool) string {
	p.mu.RLock()

	// Model Affinity - проверяем, есть ли модель уже загружена (исключая failed)
	if p.config.Balancing.ModelAffinity && model != "" {
		if backend := p.findBackendWithModelExcluding(model, exclude); backend != "" {
			p.mu.RUnlock()
			return backend
		}
	}
	p.mu.RUnlock()

	// Resource-Aware выбор с исключением
	backend := p.selectByResourcesExcluding(exclude)
	if backend == "" {
		fmt.Printf("[%s] ⚠️  Нет доступных бэкендов (с исключениями)...\n", time.Now().Format(time.RFC3339))
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

		// Проверка лимитов ресурсов
		if !p.checkResourceLimits(id) {
			continue
		}

		p.metricsMgr.mu.RLock()
		metrics, hasMetrics := p.metricsMgr.metrics[id]
		p.metricsMgr.mu.RUnlock()
		if !hasMetrics {
			continue
		}

		// Проверка лимита concurrent requests
		state.mu.Lock()
		if state.ActiveReqs >= state.Backend.MaxConcurrentReqs {
			state.mu.Unlock()
			continue
		}
		state.mu.Unlock()

		// 5.1 Prediction-based filtering
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

// findBackendWithModel - поиск бэкенда с загруженной моделью
func (p *Proxy) findBackendWithModel(modelName string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	
	var bestBackendID string
	var bestScore float64 = -1
	
	for id, state := range p.backends {
		// Проверяем статус бэкенда (только healthy)
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		// Проверка лимитов ресурсов
		if !p.checkResourceLimits(id) {
			continue
		}

		// Проверка лимита concurrent requests
		state.mu.Lock()
		if state.ActiveReqs >= state.Backend.MaxConcurrentReqs {
			state.mu.Unlock()
			continue
		}
		state.mu.Unlock()
		
		// 5.1 Prediction-based filtering
		pred := state.Prediction
		if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 300 {
			continue
		}
		
		// Проверяем наличие модели через метрики
		p.metricsMgr.mu.RLock()
		metrics, hasMetrics := p.metricsMgr.metrics[id]
		p.metricsMgr.mu.RUnlock()
		
		if !hasMetrics {
			// Бэкенд без метрик — не можем проверить наличие модели, пропускаем
			continue
		}
		
		// Проверяем наличие модели (точное совпадение или частичное)
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
		
		// Вычисляем score для бэкенда
		score := p.calculateScore(id)
		if score > bestScore {
			bestScore = score
			bestBackendID = id
		}
	}
	
	return bestBackendID
}

// selectByResources - выбор бэкенда по ресурсам
func (p *Proxy) selectByResources() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var bestBackend string
	var bestScore float64 = -1

	for id, state := range p.backends {
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		// Проверка лимитов
		if !p.checkResourceLimits(id) {
			continue
		}

		// Проверка максимального количества запросов
		state.mu.Lock()
		if state.ActiveReqs >= state.Backend.MaxConcurrentReqs {
			state.mu.Unlock()
			continue
		}
		state.mu.Unlock()

		// 5.1 Prediction-based filtering: skip backends predicted critical within 5 min
		pred := state.Prediction
		if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 300 {
			continue // Backend will be critical within 5 minutes — avoid routing new requests
		}

		// Вычисление scores
		score := p.calculateScore(id)
		if score > bestScore {
			bestScore = score
			bestBackend = id
		}
	}

	return bestBackend
}

// checkResourceLimits - проверка лимитов ресурсов
func (p *Proxy) checkResourceLimits(backendID string) bool {
	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()

	if !ok {
		// Нет метрик от агента — используем консервативные проверки по proxy-счётчикам
		p.mu.RLock()
		state, exists := p.backends[backendID]
		p.mu.RUnlock()
		if !exists {
			return false
		}
		// Если активных запросов >= 50% от лимита — считаем что бэкенд под нагрузкой
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()
		if maxReqs > 0 && active >= maxReqs/2 {
			return false
		}
		return true
	}

	limits := p.config.Resources

	// Проверка GPU
	if metrics.GPU.UsagePercent > limits.GPU.MaxUsagePercent {
		return false
	}
	if limits.GPU.MaxVRAMUsagePercent > 0 && metrics.GPU.MemoryTotal > 0 {
		vramPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
		if vramPercent > limits.GPU.MaxVRAMUsagePercent {
			return false
		}
	}

	// Проверка CPU
	if metrics.System.CPUUsagePercent > limits.CPU.MaxUsagePercent {
		return false
	}

	// Проверка RAM
	if limits.Memory.MaxUsagePercent > 0 && metrics.System.MemoryTotal > 0 {
		memPercent := float64(metrics.System.MemoryUsed) * 100 / float64(metrics.System.MemoryTotal)
		if memPercent > limits.Memory.MaxUsagePercent {
			return false
		}
	}

	// Проверка диска
	if metrics.System.DiskFree < limits.Disk.MinFreeMB {
		return false
	}

	// Проверка лимита моделей Ollama (если агент сообщил MaxModels > 0)
	if metrics.Ollama.MaxModels > 0 {
		if len(metrics.Ollama.RunningModels) >= metrics.Ollama.MaxModels {
			return false
		}
	}

	// Проверка лимита одновременных запросов Ollama (если агент сообщил MaxConcurrentRequests > 0)
	if metrics.Ollama.MaxConcurrentRequests > 0 {
		if metrics.Ollama.ActiveRequests >= metrics.Ollama.MaxConcurrentRequests {
			return false
		}
	}

	return true
}

// calculateScore - вычисление scores для бэкенда с учётом Ollama-специфических метрик
func (p *Proxy) calculateScore(backendID string) float64 {
	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()

	state := p.backends[backendID]
	if state == nil {
		return 0
	}

	if !ok {
		// Нет метрик от агента — используем fallback scoring на основе proxy-счётчиков
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()

		// Базовый score = weight, штраф за активные запросы
		score := float64(state.Backend.Weight)
		if maxReqs > 0 {
			loadRatio := float64(active) / float64(maxReqs)
			score -= loadRatio * 20.0 // штраф за загрузку
		}
		if score < 1 {
			score = 1 // минимальный score чтобы бэкенд оставался в пуле
		}
		return score
	}

	// --- Базовый score на основе доступных ресурсов ---
	gpuFree := 100 - metrics.GPU.UsagePercent
	var vramFree float64 = 100
	if metrics.GPU.MemoryTotal > 0 {
		vramFree = float64(metrics.GPU.MemoryFree) * 100 / float64(metrics.GPU.MemoryTotal)
	}
	cpuFree := 100 - metrics.System.CPUUsagePercent

	baseScore := gpuFree*0.35 + vramFree*0.25 + cpuFree*0.20

	// --- Штраф за нагрузку запросами Ollama ---
	// Чем больше активных запросов относительно максимума — тем ниже score
	requestPenalty := 0.0
	if metrics.Ollama.MaxConcurrentRequests > 0 {
		reqRatio := float64(metrics.Ollama.ActiveRequests) / float64(metrics.Ollama.MaxConcurrentRequests)
		requestPenalty = reqRatio * 15.0 // max 15 points penalty
	} else {
		// Fallback: штраф за количество активных запросов от агента
		if metrics.Ollama.ActiveRequests > 5 {
			requestPenalty = float64(metrics.Ollama.ActiveRequests) * 1.5
		}
	}

	// --- Score за capacity моделей ---
	// Чем больше свободных слотов для моделей — тем лучше
	modelCapacityScore := 0.0
	if metrics.Ollama.MaxModels > 0 {
		loaded := len(metrics.Ollama.RunningModels)
		capacityRatio := float64(metrics.Ollama.MaxModels-loaded) / float64(metrics.Ollama.MaxModels)
		modelCapacityScore = capacityRatio * 10.0 // max 10 points
	} else {
		// Fallback: оценка по VRAM если MaxModels неизвестен
		if metrics.GPU.MemoryTotal > 0 {
			// Суммарный размер загруженных моделей
			var loadedModelVRAM uint64
			for _, m := range metrics.Ollama.RunningModels {
				loadedModelVRAM += m.VRAMUsage
			}
			// Если загруженные модели занимают больше 80% VRAM — штраф
			if metrics.GPU.MemoryTotal > 0 {
				vramUsedByModels := float64(loadedModelVRAM) * 100 / float64(metrics.GPU.MemoryTotal)
				if vramUsedByModels > 80 {
					modelCapacityScore = -5.0
				} else if vramUsedByModels > 50 {
					modelCapacityScore = 2.0
				} else {
					modelCapacityScore = 8.0
				}
			}
		}
	}

	// --- 5.2 Prediction-based bonus ---
	predictionBonus := 0.0
	pred := state.Prediction
	if pred.SecondsToCritical < 0 || pred.SecondsToCritical >= 600 {
		// Backend safe for >10 min — bonus
		predictionBonus = 3.0
	} else if pred.SecondsToCritical >= 300 {
		// Backend safe for >5 min — small bonus
		predictionBonus = 1.5
	} else if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 120 {
		// Backend critical soon — penalty
		predictionBonus = -5.0
	}

	// --- Итоговый score ---
	score := (baseScore - requestPenalty + modelCapacityScore + predictionBonus) * float64(state.Backend.Weight) / 100

	// Не меньше 0
	if score < 0 {
		score = 0
	}

	return score
}

// calculateModelLoadPenalty - оценка стоимости загрузки модели на бэкенд
// Возвращает 0 если модель уже загружена, положительное значение если требуется загрузка
func (p *Proxy) calculateModelLoadPenalty(backendID string, modelName string) float64 {
	if modelName == "" {
		return 0
	}

	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()
	if !ok {
		return 5.0 // небольшой штраф за отсутствие метрик (неизвестно, загружена ли модель)
	}

	// Проверяем, загружена ли модель
	for _, m := range metrics.Ollama.RunningModels {
		if m.Name == modelName || strings.Contains(m.Name, modelName) {
			return 0 // модель уже загружена — без штрафа
		}
	}

	// Модель не загружена — штраф зависит от загруженности VRAM
	penalty := 5.0 // базовый штраф за загрузку модели
	if metrics.GPU.MemoryTotal > 0 {
		vramUsedPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
		if vramUsedPercent > 80 {
			penalty = 20.0 // высокий штраф при малом свободном VRAM
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

	// Fallback: оценка по VRAM
	if metrics.GPU.MemoryTotal > 0 && metrics.GPU.MemoryTotal > metrics.GPU.MemoryUsed {
		freeVRAM := metrics.GPU.MemoryTotal - metrics.GPU.MemoryUsed
		// Предполагаем среднюю модель 4GB
		avgModelSize := uint64(4096) // MB
		return int(freeVRAM / avgModelSize)
	}

	return 0
}

// estimateModelVRAM - оценка VRAM, необходимого для модели
func estimateModelVRAM(modelName string) uint64 {
	// Простая эвристика на основе названия модели
	// Форматы: llama3.1:8b, qwen2.5:14b, deepseek-r1:32b, etc.
	lower := strings.ToLower(modelName)

	// Извлекаем размер из названия (последнее число с 'b' в конце)
	var sizeGB uint64 = 4 // default 4GB

	// Проверяем известные шаблоны
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

	// Учитываем квантование (Q4 занимает ~60% от fp16, Q8 ~80%)
	if strings.Contains(lower, "q4") || strings.Contains(lower, "4bit") {
		sizeGB = sizeGB * 6 / 10
	} else if strings.Contains(lower, "q5") || strings.Contains(lower, "5bit") {
		sizeGB = sizeGB * 7 / 10
	} else if strings.Contains(lower, "q8") || strings.Contains(lower, "8bit") {
		sizeGB = sizeGB * 8 / 10
	} else if strings.Contains(lower, "fp16") || strings.Contains(lower, "f16") {
		// full precision, no change
	} else if strings.Contains(lower, "q2") {
		sizeGB = sizeGB * 4 / 10
	}

	// Минимум 512MB
	if sizeGB < 1 {
		sizeGB = 1
	}

	return sizeGB * 1024 // MB
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
	// Оставляем только записи за последние 60 секунд
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

// proxyRequest - проксирование запроса к бэкенду с поддержкой streaming/SSE
// Возвращает ошибку, если запрос не удалось выполнить (для retry/failover)
func (p *Proxy) proxyRequest(w http.ResponseWriter, r *http.Request, backendID string) error {
	state, ok := p.backends[backendID]
	if !ok {
		return fmt.Errorf("backend %s not found", backendID)
	}

	// Увеличение счетчика активных запросов + запись для RPS
	state.mu.Lock()
	state.ActiveReqs++
	state.mu.Unlock()
	p.recordRequest(backendID)

	defer func() {
		state.mu.Lock()
		state.ActiveReqs--
		state.mu.Unlock()
	}()

	// Создание URL для бэкенда
	targetURL := fmt.Sprintf("http://%s:%d", state.Backend.Host, state.Backend.OllamaPort)

	target, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("invalid backend URL: %v", err)
	}

	// Проверяем, является ли запрос streaming запросом (по телу запроса)
	isStreamingRequest := p.isStreamingRequest(r)

	// Выбираем клиент: streaming для streaming запросов, обычный для остальных
	client := p.client
	if isStreamingRequest {
		client = p.streamingClient
	}

	// Создание нового запроса с тем же телом и заголовками
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL+r.URL.String(), r.Body)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	// Копирование заголовков запроса
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Добавление заголовков проксирования
	req.Header.Set("X-Forwarded-For", r.RemoteAddr)
	req.Header.Set("X-Real-IP", r.RemoteAddr)
	req.Host = target.Host

	// Выполнение запроса к бэкенду
	resp, err := client.Do(req)
	if err != nil {
		p.logStreamingError(backendID, err)
		return fmt.Errorf("backend error: %v", err)
	}
	defer resp.Body.Close()

	// Инкремент total requests (atomic)
	atomic.AddInt64(&p.totalRequests, 1)
	atomic.AddInt64(&state.TotalRequests, 1)

	// Копирование заголовков ответа
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Добавление заголовка сессии
	sessionID := p.getSessionID(r)
	if sessionID != "" {
		p.sessionMgr.Set(sessionID, backendID, p.extractModel(r))
		w.Header().Set("X-Session-ID", sessionID)
	}

	// Проверка на streaming ответ (SSE или chunked transfer)
	isStreaming := p.isStreamingResponse(resp)

	if isStreaming {
		p.handleStreamingResponse(w, r, resp, backendID)
	} else {
		// Обычный ответ - копируем тело
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}

	return nil
}

// isStreamingRequest - проверка, является ли запрос streaming запросом
// Ollama API использует параметр "stream" в теле запроса
func (p *Proxy) isStreamingRequest(r *http.Request) bool {
	// Только POST запросы могут быть streaming
	if r.Method != http.MethodPost {
		return false
	}

	// Читаем тело для проверки параметра stream
	if r.Body == nil {
		return false
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}

	// Восстанавливаем тело для дальнейшего использования
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	// Парсим JSON
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}

	// Проверяем параметр stream (по умолчанию true для Ollama)
	if stream, ok := req["stream"].(bool); ok {
		return stream
	}

	// По умолчанию считаем запрос streaming (Ollama API behavior)
	return true
}

// isStreamingResponse - проверка на streaming ответ (SSE или chunked)
func (p *Proxy) isStreamingResponse(resp *http.Response) bool {
	// Проверяем Content-Type на text/event-stream (SSE)
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		return true
	}

	// Проверяем Transfer-Encoding на chunked
	if resp.Header.Get("Transfer-Encoding") == "chunked" {
		return true
	}

	// Проверяем ContentLength = -1 (неизвестная длина = streaming)
	if resp.ContentLength == -1 {
		return true
	}

	// Проверяем заголовок X-Accel-Buffering (для nginx)
	if resp.Header.Get("X-Accel-Buffering") == "no" {
		return true
	}

	return false
}

// handleStreamingResponse - обработка streaming ответа с использованием Flusher
func (p *Proxy) handleStreamingResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, backendID string) {
	// Отправляем статус ответа
	w.WriteHeader(resp.StatusCode)

	// Пытаемся получить Flusher для потоковой передачи
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Если Flusher недоступен, просто копируем тело
		fmt.Printf("[%s] Streaming detected but Flusher not supported, falling back to regular copy\n", time.Now().Format(time.RFC3339))
		io.Copy(w, resp.Body)
		return
	}

	// Логирование начала streaming сессии
	fmt.Printf("[%s] Starting streaming session for backend %s (Content-Type: %s)\n",
		time.Now().Format(time.RFC3339), backendID, resp.Header.Get("Content-Type"))

	// Буфер для чтения данных
	buf := make([]byte, 32*1024)
	bytesStreamed := 0

	// Потоковая передача данных
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				fmt.Printf("[%s] Error writing to client: %v\n", time.Now().Format(time.RFC3339), writeErr)
				break
			}
			bytesStreamed += n
			flusher.Flush()
		}
		if err != nil {
			if err == io.EOF {
				// Нормальное завершение streaming
				break
			}
			// Логирование других ошибок
			fmt.Printf("[%s] Error reading from backend: %v\n", time.Now().Format(time.RFC3339), err)
			break
		}
	}

	// Логирование завершения streaming сессии
	fmt.Printf("[%s] Streaming session completed for backend %s (total bytes: %d)\n",
		time.Now().Format(time.RFC3339), backendID, bytesStreamed)
}

// logStreamingError - логирование ошибок streaming
func (p *Proxy) logStreamingError(backendID string, err error) {
	fmt.Printf("[%s] Streaming error for backend %s: %v\n", time.Now().Format(time.RFC3339), backendID, err)
}

// logError - логирование ошибки
func (p *Proxy) logError(backendID string, err error) {
	// TODO: реализовать логирование
	fmt.Printf("[%s] Error: %v\n", time.Now().Format(time.RFC3339), err)
}

// UpdateMetrics - обновление метрик бэкенда с прогнозированием
func (p *Proxy) UpdateMetrics(backendID string, metrics *types.BackendMetrics) {
	// Получаем proxy-счётчик активных запросов
	p.mu.RLock()
	state, exists := p.backends[backendID]
	p.mu.RUnlock()

	if exists {
		state.mu.Lock()
		proxyActiveReqs := state.ActiveReqs
		state.mu.Unlock()

		// Переопределяем ActiveRequests точным значением от proxy
		// (агент не может достоверно определить количество HTTP-запросов)
		if metrics.Ollama.ActiveRequests == 0 && proxyActiveReqs > 0 {
			metrics.Ollama.ActiveRequests = proxyActiveReqs
		}

		// Вычисляем свободные слоты
		freeSlots := metrics.Ollama.MaxConcurrentRequests - metrics.Ollama.ActiveRequests
		if freeSlots < 0 {
			freeSlots = 0
		}
		metrics.Ollama.FreeSlots = freeSlots

		// Обновляем историю и прогноз
		p.predictor.UpdateHistory(state, metrics)
	}

	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics[backendID] = metrics
	p.metricsMgr.mu.Unlock()
}

// UpdateBackendStatus - обновление статуса бэкенда
func (p *Proxy) UpdateBackendStatus(backendID string, status types.BackendStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if state, ok := p.backends[backendID]; ok {
		state.Backend.Status = status
	}
}

// UpdateBackendAgentStatus - обновление флага активного агента
func (p *Proxy) UpdateBackendAgentStatus(backendID string, hasAgent bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if state, ok := p.backends[backendID]; ok {
		state.Backend.HasAgent = hasAgent
		state.Backend.LastAgentContact = time.Now()
	}
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
					fmt.Printf("[%s] Agent timeout for backend %s - no metrics received for %v\n",
						time.Now().Format(time.RFC3339), state.Backend.ID, timeout)
				}
			}
			p.mu.Unlock()
		}
	}()
}

// GetClusterState - получение состояния кластера
func (p *Proxy) GetClusterState() *types.ClusterState {
	p.mu.RLock()
	
	// Создаем копию бэкендов для безопасной итерации
	// Это предотвращает race condition при модификации мапы во время итерации
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

	// Заполняем общий счётчик запросов
	state.TotalRequests = atomic.LoadInt64(&p.totalRequests)

	for id, backendState := range backendsCopy {
		if backendState.Backend.Status == types.StatusHealthy {
			state.HealthyBackends++
		}

		// Создаем метрики для каждого бэкенда, даже если нет данных от агента
		metrics := types.BackendMetrics{
			ID:        id,
			Timestamp: time.Now().UTC(),
			Status:    backendState.Backend.Status,
			HasAgent:  backendState.Backend.HasAgent,
			GPU:       types.GPUMetrics{},
			System:    types.SystemMetrics{},
			Ollama:    types.OllamaMetrics{RunningModels: []types.RunningModel{}},
		}

		// Если есть метрики от агента - используем их
		if agentMetrics, ok := p.metricsMgr.metrics[id]; ok {
			metrics = *agentMetrics
			// Сохраняем статус из конфигурации бэкенда
			metrics.Status = backendState.Backend.Status
			// Сохраняем флаг агента из конфигурации бэкенда
			metrics.HasAgent = backendState.Backend.HasAgent
			// Добавляем прогноз из состояния бэкенда
			metrics.Prediction = backendState.Prediction
			state.ActiveRequests += agentMetrics.Ollama.ActiveRequests
			// Используем proxy-calculated RPS (более точный, чем агентский)
			backendState.mu.Lock()
			if backendState.CalculatedRPS > 0 {
				state.RPS += backendState.CalculatedRPS
				metrics.Ollama.RequestsPerSecond = backendState.CalculatedRPS
			} else {
				state.RPS += agentMetrics.Ollama.RequestsPerSecond
			}
			// Переносим proxy-calculated total requests в метрики
			metrics.Ollama.TotalRequests = backendState.TotalRequests
			backendState.mu.Unlock()
			state.TotalGPUUsage += agentMetrics.GPU.UsagePercent
		} else {
			// Нет метрик от агента — всё равно показываем proxy-calculated total requests
			backendState.mu.Lock()
			metrics.Ollama.TotalRequests = backendState.TotalRequests
			backendState.mu.Unlock()
		}

		state.Backends = append(state.Backends, metrics)
	}

	return state
}

// cleanupLoop - цикл очистки сессий
func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		sm.cleanup()
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

// Set - установка сессии
func (sm *SessionManager) Set(id, backendID, model string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	now := time.Now()
	if session, ok := sm.sessions[id]; ok {
		session.LastRequestAt = now
		session.RequestCount++
	} else {
		sm.sessions[id] = &types.Session{
			ID:            id,
			BackendID:     backendID,
			Model:         model,
			CreatedAt:     now,
			LastRequestAt: now,
			RequestCount:  1,
		}
	}
}

// queueRequest - постановка запроса в очередь
func (p *Proxy) queueRequest(w http.ResponseWriter, r *http.Request, model string) bool {
	done := make(chan bool, 1)
	queuedReq := &QueuedRequest{
		Request:  r,
		Writer:   w,
		Model:    model,
		Enqueued: time.Now(),
		Done:     done,
	}

	// Попытка отправить запрос в очередь
	select {
	case p.queueMgr.queue <- queuedReq:
		// Запрос успешно отправлен в очередь
	case <-p.queueMgr.ctx.Done():
		return false
	default:
		// Очередь переполнена
		return false
	}

	// Ожидание результата с таймаутом
	select {
	case success := <-done:
		return success
	case <-time.After(p.queueMgr.timeout):
		return false
	case <-p.queueMgr.ctx.Done():
		return false
	}
}

// GetQueueStats - получение статистики очереди
func (p *Proxy) GetQueueStats() QueueStats {
	p.queueMgr.mu.Lock()
	processed := p.queueMgr.processed
	p.queueMgr.mu.Unlock()

	return QueueStats{
		CurrentSize:   len(p.queueMgr.queue),
		MaxSize:       p.queueMgr.maxSize,
		Processed:     processed,
		WaitTimeAvgMs: 0, // Упрощено для новой архитектуры
	}
}

// QueueStats - статистика очереди
type QueueStats struct {
	CurrentSize   int   `json:"current_size"`
	MaxSize       int   `json:"max_size"`
	Processed     int64 `json:"processed_total"`
	WaitTimeAvgMs int64 `json:"avg_wait_time_ms"`
}

// AddBackend - добавление нового бэкенда
func (p *Proxy) AddBackend(backend types.Backend) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Проверка на дубликат
	if _, exists := p.backends[backend.ID]; exists {
		return fmt.Errorf("backend with ID %s already exists", backend.ID)
	}

	// Установка значений по умолчанию
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

	// Запланировать autosave
	p.scheduleSave()

	return nil
}

// RemoveBackend - удаление бэкенда
func (p *Proxy) RemoveBackend(backendID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.backends[backendID]; !exists {
		return fmt.Errorf("backend with ID %s not found", backendID)
	}

	delete(p.backends, backendID)

	// Также удаляем метрики
	p.metricsMgr.mu.Lock()
	delete(p.metricsMgr.metrics, backendID)
	p.metricsMgr.mu.Unlock()

	// Запланировать autosave
	p.scheduleSave()

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

	// Сохраняем текущий статус и активные запросы
	currentStatus := state.Backend.Status
	currentActiveReqs := state.ActiveReqs

	// Обновляем конфигурацию
	updated.ID = backendID
	updated.Status = currentStatus
	state.Backend = &updated
	state.ActiveReqs = currentActiveReqs

	// Запланировать autosave
	p.scheduleSave()

	return nil
}

// scheduleRecoveryCheck - планирование проверки восстановления бэкенда
// Заглушка: recovery check выполняется health checker автоматически через периодические проверки
func (p *Proxy) scheduleRecoveryCheck(backendID string) {
	fmt.Printf("[%s] Scheduled recovery check for backend %s\n",
		time.Now().Format(time.RFC3339), backendID)
	// Реальная реализация может быть добавлена через HealthChecker
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

	// Создаём директорию если нужно
	dir := filepath.Dir(p.statePath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create state directory: %w", err)
		}
	}

	if err := os.WriteFile(p.statePath, data, 0644); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	fmt.Printf("[Proxy] State saved to %s (%d backends)\n", p.statePath, len(backends))
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
		fmt.Printf("[Proxy] Warning: state version mismatch (got %d, expected %d)\n", state.Version, types.StateVersion)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Merge: state-данные приоритетнее для runtime-полей
	stateBackendMap := make(map[string]types.Backend)
	for _, b := range state.Backends {
		stateBackendMap[b.ID] = b
	}

	for i := range p.config.Backends {
		if saved, ok := stateBackendMap[p.config.Backends[i].ID]; ok {
			// Сохраняем runtime-поля из state
			p.config.Backends[i].Status = saved.Status
			p.config.Backends[i].LastHealthCheck = saved.LastHealthCheck
			p.config.Backends[i].ConsecutiveFailures = saved.ConsecutiveFailures
			p.config.Backends[i].ActiveRequests = saved.ActiveRequests
			p.config.Backends[i].HasAgent = saved.HasAgent
			p.config.Backends[i].LastAgentContact = saved.LastAgentContact

			// Обновляем BackendState
			if bs, exists := p.backends[p.config.Backends[i].ID]; exists {
				bs.Backend = &p.config.Backends[i]
			}
		}
	}

	// Добавляем backends из state, которых нет в config
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

	fmt.Printf("[Proxy] State loaded from %s (%d backends)\n", p.statePath, len(state.Backends))
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
			fmt.Printf("[Proxy] Autosave error: %v\n", err)
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
