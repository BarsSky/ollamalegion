package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// Proxy - HTTP прокси для балансировки запросов
type Proxy struct {
	config      *types.LoadBalancerConfig
	backends    map[string]*BackendState
	mu          sync.RWMutex
	roundRobin  int
	sessionMgr  *SessionManager
	metricsMgr  *MetricsManager
	queueMgr    *QueueManager
	client      *http.Client
}

// BackendState - состояние бэкенда
type BackendState struct {
	Backend       *types.Backend
	ActiveReqs    int
	LastUsed      time.Time
	mu            sync.Mutex
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

// QueueManager - менеджер очереди
type QueueManager struct {
	queue      []*QueuedRequest
	mu         sync.Mutex
	maxSize    int
	notify     chan struct{}
	processed  int64
	timeout    time.Duration
	shutdownCh chan struct{}
	wg         sync.WaitGroup
}

// QueuedRequest - запрос в очереди
type QueuedRequest struct {
	Request   *http.Request
	Writer    http.ResponseWriter
	Model     string
	Enqueued  time.Time
	Done      chan bool
	Target    string
}

// NewProxy - создание нового прокси
func NewProxy(config *types.LoadBalancerConfig) *Proxy {
	p := &Proxy{
		config:     config,
		backends:   make(map[string]*BackendState),
		sessionMgr: NewSessionManager(),
		metricsMgr: NewMetricsManager(),
		queueMgr:   NewQueueManager(config.Balancing.QueueMaxSize, time.Duration(config.Balancing.QueueTimeout)*time.Second),
		client: &http.Client{
			Timeout: time.Duration(config.Balancing.RequestTimeout) * time.Second,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
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
	
	// Запуск обработчика очереди
	p.queueMgr.StartWorkers(p)
	
	return p
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

// NewQueueManager - создание менеджера очереди
func NewQueueManager(maxSize int, timeout time.Duration) *QueueManager {
	return &QueueManager{
		queue:      make([]*QueuedRequest, 0, maxSize),
		maxSize:    maxSize,
		notify:     make(chan struct{}, 1),
		timeout:    timeout,
		shutdownCh: make(chan struct{}),
	}
}

// StartWorkers - запуск обработчиков очереди
func (qm *QueueManager) StartWorkers(proxy *Proxy) {
	qm.wg.Add(1)
	go qm.queueWorker(proxy)
}

// Stop - остановка обработчиков очереди
func (qm *QueueManager) Stop() {
	close(qm.shutdownCh)
	qm.wg.Wait()
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
		}
	}
	
	// Если нет сессии или сессия не найдена - выбираем бэкенд
	if targetBackend == "" {
		targetBackend = p.selectBackend(model)
	}
	
	// Если бэкенд не выбран - ставим в очередь
	if targetBackend == "" {
		if !p.queueRequest(w, r, model) {
			http.Error(w, "Service unavailable - all backends busy", http.StatusServiceUnavailable)
		}
		return
	}
	
	// Проксирование запроса
	p.proxyRequest(w, r, targetBackend)
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
	defer p.mu.RUnlock()
	
	// Model Affinity - проверяем, есть ли модель уже загружена
	if p.config.Balancing.ModelAffinity && model != "" {
		if backend := p.findBackendWithModel(model); backend != "" {
			return backend
		}
	}
	
	// Resource-Aware выбор
	return p.selectByResources()
}

// findBackendWithModel - поиск бэкенда с загруженной моделью
func (p *Proxy) findBackendWithModel(model string) string {
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	
	for backendID, metrics := range p.metricsMgr.metrics {
		for _, m := range metrics.Ollama.RunningModels {
			if strings.Contains(m.Name, model) {
				// Проверяем, что бэкенд здоров и имеет ресурсы
				if state, ok := p.backends[backendID]; ok {
					if state.Backend.Status == types.StatusHealthy {
						return backendID
					}
				}
			}
		}
	}
	
	return ""
}

// selectByResources - выбор бэкенда по ресурсам
func (p *Proxy) selectByResources() string {
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

// proxyRequest - проксирование запроса к бэкенду
func (p *Proxy) proxyRequest(w http.ResponseWriter, r *http.Request, backendID string) {
	state, ok := p.backends[backendID]
	if !ok {
		http.Error(w, "Backend not found", http.StatusInternalServerError)
		return
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
		http.Error(w, fmt.Sprintf("Invalid backend URL: %v", err), http.StatusInternalServerError)
		return
	}
	
	// Создание reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(target)
	
	// Кастомный ErrorHandler
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		p.logError(backendID, err)
		http.Error(w, "Backend error", http.StatusBadGateway)
	}
	
	// Модификация запроса
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host
		req.URL.Host = target.Host
		req.URL.Scheme = target.Scheme
		
		// Добавление заголовков
		req.Header.Set("X-Forwarded-For", r.RemoteAddr)
		req.Header.Set("X-Real-IP", r.RemoteAddr)
	}
	
	// Модификация ответа для добавления session ID
	originalModifyResponse := proxy.ModifyResponse
	proxy.ModifyResponse = func(resp *http.Response) error {
		if originalModifyResponse != nil {
			if err := originalModifyResponse(resp); err != nil {
				return err
			}
		}
		
		// Добавление заголовка сессии
		sessionID := p.getSessionID(r)
		if sessionID != "" {
			p.sessionMgr.Set(sessionID, backendID, p.extractModel(r))
			resp.Header.Set("X-Session-ID", sessionID)
		}
		
		return nil
	}
	
	proxy.ServeHTTP(w, r)
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
	p.metricsMgr.mu.RLock()
	defer p.mu.RUnlock()
	defer p.metricsMgr.mu.RUnlock()
	
	state := &types.ClusterState{
		Timestamp:      time.Now().UTC(),
		TotalBackends:  len(p.backends),
		HealthyBackends: 0,
		Backends:       make([]types.BackendMetrics, 0),
	}
	
	for id, backendState := range p.backends {
		if backendState.Backend.Status == types.StatusHealthy {
			state.HealthyBackends++
		}
		
		if metrics, ok := p.metricsMgr.metrics[id]; ok {
			state.Backends = append(state.Backends, *metrics)
			state.ActiveRequests += metrics.Ollama.ActiveRequests
		}
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
	p.queueMgr.mu.Lock()
	
	// Проверка переполнения очереди
	if len(p.queueMgr.queue) >= p.queueMgr.maxSize {
		p.queueMgr.mu.Unlock()
		return false
	}
	
	// Создание запроса в очереди
	done := make(chan bool, 1)
	queuedReq := &QueuedRequest{
		Request:  r,
		Writer:   w,
		Model:    model,
		Enqueued: time.Now(),
		Done:     done,
	}
	
	// Добавление в очередь
	p.queueMgr.queue = append(p.queueMgr.queue, queuedReq)
	p.queueMgr.mu.Unlock()
	
	// Сигнал обработчику
	select {
	case p.queueMgr.notify <- struct{}{}:
	default:
		// Обработчик уже уведомлен
	}
	
	// Ожидание результата с таймаутом
	select {
	case success := <-done:
		return success
	case <-time.After(p.queueMgr.timeout):
		// Таймаут ожидания - удаляем запрос из очереди
		p.queueMgr.mu.Lock()
		for i, req := range p.queueMgr.queue {
			if req == queuedReq {
				p.queueMgr.queue = append(p.queueMgr.queue[:i], p.queueMgr.queue[i+1:]...)
				break
			}
		}
		p.queueMgr.mu.Unlock()
		return false
	case <-p.queueMgr.shutdownCh:
		return false
	}
}

// queueWorker - обработчик очереди запросов
func (qm *QueueManager) queueWorker(proxy *Proxy) {
	defer qm.wg.Done()
	
	for {
		select {
		case <-qm.shutdownCh:
			// Остановка - отменяем все ожидающие запросы
			qm.mu.Lock()
			for _, req := range qm.queue {
				select {
				case req.Done <- false:
				default:
				}
			}
			qm.queue = nil
			qm.mu.Unlock()
			return
			
		case <-qm.notify:
			// Попытка обработать очередь
			qm.processQueue(proxy)
		}
	}
}

// processQueue - обработка очереди
func (qm *QueueManager) processQueue(proxy *Proxy) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	
	if len(qm.queue) == 0 {
		return
	}
	
	// Берем первый запрос из очереди
	req := qm.queue[0]
	qm.queue = append(qm.queue[:0], qm.queue[1:]...)
	
	// Попытка найти доступный бэкенд
	targetBackend := proxy.selectBackend(req.Model)
	
	if targetBackend == "" {
		// Нет доступных бэкендов - возвращаем запрос в начало очереди
		qm.queue = append([]*QueuedRequest{req}, qm.queue...)
		
		// Повторное уведомление через небольшую задержку
		time.AfterFunc(100*time.Millisecond, func() {
			select {
			case qm.notify <- struct{}{}:
			default:
			}
		})
		return
	}
	
	// Устанавливаем целевой бэкенд и выполняем запрос
	req.Target = targetBackend
	
	// Проксируем запрос
	proxy.proxyRequest(req.Writer, req.Request, targetBackend)
	
	// Сигнал о успешном выполнении
	select {
	case req.Done <- true:
	default:
	}
	
	// Обновление счетчика
	qm.processed++
}

// getQueueStats - получение статистики очереди
func (p *Proxy) getQueueStats() QueueStats {
	p.queueMgr.mu.Lock()
	defer p.queueMgr.mu.Unlock()
	
	return QueueStats{
		CurrentSize:   len(p.queueMgr.queue),
		MaxSize:       p.queueMgr.maxSize,
		Processed:     p.queueMgr.processed,
		WaitTimeAvgMs: p.calculateAvgWaitTime(),
	}
}

// calculateAvgWaitTime - вычисление среднего времени ожидания
func (p *Proxy) calculateAvgWaitTime() int64 {
	p.queueMgr.mu.Lock()
	defer p.queueMgr.mu.Unlock()
	
	if len(p.queueMgr.queue) == 0 {
		return 0
	}
	
	var totalWait int64
	now := time.Now()
	for _, req := range p.queueMgr.queue {
		totalWait += int64(now.Sub(req.Enqueued))
	}
	
	return totalWait / int64(len(p.queueMgr.queue)) / int64(time.Millisecond)
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
