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
	"strings"
	"sync"
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
	client          *http.Client      // Клиент для обычных запросов
	streamingClient *http.Client      // Клиент для streaming/SSE запросов (без таймаута)
}

// BackendState - состояние бэкенда
type BackendState struct {
	Backend    *types.Backend
	ActiveReqs int
	LastUsed   time.Time
	mu         sync.Mutex
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
		queueMgr:   NewQueueManager(nil, config.Balancing.QueueMaxSize, config.Balancing.QueueWorkers, time.Duration(config.Balancing.QueueTimeout)*time.Second),
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

	// Инициализация бэкендов
	for i := range config.Backends {
		backend := config.Backends[i]
		p.backends[backend.ID] = &BackendState{
			Backend:    &backend,
			ActiveReqs: 0,
			LastUsed:   time.Time{},
		}
	}

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
		
		// Проверяем наличие модели через метрики
		p.metricsMgr.mu.RLock()
		metrics, hasMetrics := p.metricsMgr.metrics[id]
		p.metricsMgr.mu.RUnlock()
		
		if !hasMetrics {
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
	defer p.metricsMgr.mu.RUnlock()

	metrics, ok := p.metricsMgr.metrics[backendID]
	if !ok {
		return true // Нет метрик - разрешаем
	}

	limits := p.config.Resources

	// Проверка GPU
	if metrics.GPU.UsagePercent > limits.GPU.MaxUsagePercent {
		return false
	}
	if limits.GPU.MaxVRAMUsagePercent > 0 {
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
	if limits.Memory.MaxUsagePercent > 0 {
		memPercent := float64(metrics.System.MemoryUsed) * 100 / float64(metrics.System.MemoryTotal)
		if memPercent > limits.Memory.MaxUsagePercent {
			return false
		}
	}

	// Проверка диска
	if metrics.System.DiskFree < limits.Disk.MinFreeMB {
		return false
	}

	return true
}

// calculateScore - вычисление scores для бэкенда
func (p *Proxy) calculateScore(backendID string) float64 {
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()

	state := p.backends[backendID]
	if state == nil {
		return 0
	}

	metrics, ok := p.metricsMgr.metrics[backendID]
	if !ok {
		// Нет метрик - используем weight
		return float64(state.Backend.Weight)
	}

	// Score на основе доступных ресурсов
	gpuFree := 100 - metrics.GPU.UsagePercent
	vramFree := float64(metrics.GPU.MemoryFree) * 100 / float64(metrics.GPU.MemoryTotal)
	cpuFree := 100 - metrics.System.CPUUsagePercent

	// Weighted score
	score := (gpuFree*0.4 + vramFree*0.3 + cpuFree*0.3) * float64(state.Backend.Weight) / 100

	return score
}

// proxyRequest - проксирование запроса к бэкенду с поддержкой streaming/SSE
// Возвращает ошибку, если запрос не удалось выполнить (для retry/failover)
func (p *Proxy) proxyRequest(w http.ResponseWriter, r *http.Request, backendID string) error {
	state, ok := p.backends[backendID]
	if !ok {
		return fmt.Errorf("backend %s not found", backendID)
	}

	// Увеличение счетчика активных запросов
	state.mu.Lock()
	state.ActiveReqs++
	state.mu.Unlock()

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

// UpdateMetrics - обновление метрик бэкенда
func (p *Proxy) UpdateMetrics(backendID string, metrics *types.BackendMetrics) {
	p.metricsMgr.mu.Lock()
	defer p.metricsMgr.mu.Unlock()

	p.metricsMgr.metrics[backendID] = metrics
}

// UpdateBackendStatus - обновление статуса бэкенда
func (p *Proxy) UpdateBackendStatus(backendID string, status types.BackendStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if state, ok := p.backends[backendID]; ok {
		state.Backend.Status = status
	}
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

	for id, backendState := range backendsCopy {
		if backendState.Backend.Status == types.StatusHealthy {
			state.HealthyBackends++
		}

		// Создаем метрики для каждого бэкенда, даже если нет данных от агента
		metrics := types.BackendMetrics{
			ID:        id,
			Timestamp: time.Now().UTC(),
			Status:    backendState.Backend.Status,
			GPU:       types.GPUMetrics{},
			System:    types.SystemMetrics{},
			Ollama:    types.OllamaMetrics{RunningModels: []types.RunningModel{}},
		}

		// Если есть метрики от агента - используем их
		if agentMetrics, ok := p.metricsMgr.metrics[id]; ok {
			metrics = *agentMetrics
			// Сохраняем статус из конфигурации бэкенда
			metrics.Status = backendState.Backend.Status
			state.ActiveRequests += agentMetrics.Ollama.ActiveRequests
			state.RPS += agentMetrics.Ollama.RequestsPerSecond
			state.TotalGPUUsage += agentMetrics.GPU.UsagePercent
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

// getQueueStats - получение статистики очереди
func (p *Proxy) getQueueStats() QueueStats {
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
		backend.AgentPort = 9090
	}
	if backend.Status == "" {
		backend.Status = types.StatusStarting
	}

	p.backends[backend.ID] = &BackendState{
		Backend:    &backend,
		ActiveReqs: 0,
		LastUsed:   time.Time{},
	}

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

	return nil
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

// BackendExists - проверка существования бэкенда
func (p *Proxy) BackendExists(backendID string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	_, exists := p.backends[backendID]
	return exists
}
