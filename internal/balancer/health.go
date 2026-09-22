package balancer

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// HealthCheckResult - результат проверки здоровья
type HealthCheckResult struct {
	BackendID string        `json:"backendId"`
	Healthy   bool          `json:"healthy"`
	Latency   time.Duration `json:"latency"`
	Error     string        `json:"error,omitempty"`
	Timestamp time.Time     `json:"timestamp"`
}

// HealthChecker - проверка здоровья бэкендов
type HealthChecker struct {
	proxy     *Proxy
	interval  time.Duration
	threshold int
	mu        sync.Mutex
	results   map[string]*HealthStatus
	stopChan  chan struct{}
	wg        sync.WaitGroup
}

// HealthStatus - статус здоровья бэкенда
type HealthStatus struct {
	Healthy          bool          `json:"healthy"`
	ConsecutiveFails int           `json:"consecutiveFails"`
	LastCheck        time.Time     `json:"lastCheck"`
	LastSuccess      time.Time     `json:"lastSuccess"`
	LastFailure      time.Time     `json:"lastFailure"`
	AvgLatency       time.Duration `json:"avgLatency"`
	LastLatency      time.Duration `json:"lastLatency"`
	LastError        string        `json:"lastError,omitempty"`
}

// NewHealthChecker - создание проверщика здоровья
func NewHealthChecker(proxy *Proxy, interval time.Duration, threshold int) *HealthChecker {
	hc := &HealthChecker{
		proxy:     proxy,
		interval:  interval,
		threshold: threshold,
		results:   make(map[string]*HealthStatus),
		stopChan:  make(chan struct{}),
	}

	// Инициализация статусов
	for id := range proxy.backends {
		hc.results[id] = &HealthStatus{
			Healthy: true,
		}
	}

	return hc
}

// Start - запуск цикла проверок
func (hc *HealthChecker) Start() {
	// R66d (2026-09-22): wg.Add(1) обязан быть ЗДЕСЬ, до запуска горутины.
	// Раньше он вызывался внутри checkLoop, то есть параллельно с wg.Wait()
	// в Stop() — это и запрещено в sync.WaitGroup (Add после/во время Wait),
	// и ловилось детектором гонок как
	//   Write at ... HealthChecker.Stop health.go:73 (wg.Wait)
	//   Previous read at ... checkLoop health.go:78 (wg.Add)
	// Плюс был реальный баг: Stop(), вызванный до того, как горутина успела
	// стартовать, возвращался мгновенно, не дожидаясь проверки.
	hc.wg.Add(1)
	go hc.checkLoop()
}

// Stop - остановка проверок с ожиданием завершения checkLoop
func (hc *HealthChecker) Stop() {
	close(hc.stopChan)
	hc.wg.Wait()
}

// checkLoop - основной цикл проверок
func (hc *HealthChecker) checkLoop() {
	defer hc.wg.Done()

	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()

	// Первая проверка сразу
	hc.checkAll()

	for {
		select {
		case <-ticker.C:
			hc.checkAll()
		case <-hc.stopChan:
			return
		}
	}
}

// checkAll - проверка всех бэкендов
func (hc *HealthChecker) checkAll() {
	hc.proxy.mu.RLock()
	backendIDs := make([]string, 0, len(hc.proxy.backends))
	for id := range hc.proxy.backends {
		backendIDs = append(backendIDs, id)
	}
	hc.proxy.mu.RUnlock()

	for _, id := range backendIDs {
		hc.checkBackend(id)
	}
}

// checkBackend - проверка конкретного бэкенда
func (hc *HealthChecker) checkBackend(backendID string) {
	hc.proxy.mu.RLock()
	state, ok := hc.proxy.backends[backendID]
	if !ok {
		hc.proxy.mu.RUnlock()
		return
	}

	backend := state.Backend
	hc.proxy.mu.RUnlock()

	// Выполнение health check
	result := hc.performCheck(backend)

	// Обновление статуса
	hc.mu.Lock()
	status := hc.results[backendID]
	if status == nil {
		status = &HealthStatus{}
		hc.results[backendID] = status
	}

	status.LastCheck = time.Now().UTC()
	status.LastLatency = result.Latency

	if result.Healthy {
		status.ConsecutiveFails = 0
		status.LastSuccess = time.Now().UTC()
		status.Healthy = true
		status.LastError = ""

		// Обновление среднего latency
		if status.AvgLatency == 0 {
			status.AvgLatency = result.Latency
		} else {
			status.AvgLatency = (status.AvgLatency*9 + result.Latency) / 10
		}

		// Обновление статуса бэкенда
		hc.proxy.UpdateBackendStatus(backendID, types.StatusHealthy)
	} else {
		status.ConsecutiveFails++
		status.LastFailure = time.Now().UTC()
		status.LastError = result.Error

		// Проверка порога неудач
		if status.ConsecutiveFails >= hc.threshold {
			status.Healthy = false
			// Разделение статусов: агент жив, но ollama не отвечает?
			// Проверяем HasAgent — если агент недавно контактировал, значит ollama_unavailable
			agentAlive := false
			if b := hc.proxy.GetBackend(backendID); b != nil {
				agentAlive = b.HasAgent && time.Since(b.LastAgentContact) < 2*time.Minute
			}
			if agentAlive {
				hc.proxy.UpdateBackendStatus(backendID, types.StatusOllamaUnavailable)
			} else {
				hc.proxy.UpdateBackendStatus(backendID, types.StatusUnhealthy)
			}
		}
	}
	hc.mu.Unlock()
}

// performCheck - выполнение проверки здоровья
func (hc *HealthChecker) performCheck(backend *types.Backend) *HealthCheckResult {
	result := &HealthCheckResult{
		BackendID: backend.ID,
		Timestamp: time.Now().UTC(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Определяем движок и endpoint для health-check
	engine := types.ResolveEngine(backend.Engine, backend.Type)
	var url string
	switch engine {
	case types.EngineLlamaCPP:
		port := backend.CppWorkerPort
		if port <= 0 {
			// Modern llama.cpp CppWorker default port (18091 is legacy).
			port = 18092
		}
		url = fmt.Sprintf("http://%s:%d/health", backend.Host, port)
	default:
		url = fmt.Sprintf("http://%s:%d/api/tags", backend.Host, backend.OllamaPort)
	}

	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		result.Healthy = false
		result.Error = fmt.Sprintf("request error: %v", err)
		return result
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		result.Healthy = false
		result.Error = fmt.Sprintf("connection error: %v", err)
		return result
	}
	defer resp.Body.Close()

	result.Latency = time.Since(start)

	if resp.StatusCode != http.StatusOK {
		result.Healthy = false
		result.Error = fmt.Sprintf("status code: %d", resp.StatusCode)
		return result
	}

	result.Healthy = true
	return result
}

// GetStatus - получение статуса бэкенда
// GetStatus возвращает КОПИЮ статуса бэкенда.
//
// R66d (2026-09-22): раньше возвращался живой указатель из hc.results, а
// checkBackend() пишет те же поля (LastCheck/LastLatency/Healthy) под hc.mu уже
// после того, как вызывающий отпустил мьютекс, — то есть чтение статуса из
// другого потока было гонкой данных (-race: Write at health.go:135/140 by
// checkLoop, Previous read at tests/noagent_test.go:601). Копия сохраняет
// прежний API (указатель) и убирает окно.
func (hc *HealthChecker) GetStatus(backendID string) *HealthStatus {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	status, ok := hc.results[backendID]
	if !ok || status == nil {
		return nil
	}
	cp := *status
	return &cp
}

// MarkUnhealthy — пометить backend как unhealthy (вызывается из proxy при persistent
// connection-level failures). Healthcheck (checkLoop) снимет метку через threshold
// успешных проверок. Используется чтобы не routing'ить запросы к мёртвому backend
// пока он не восстановится.
//
// Round 8 (2026-07-10): после 3 retry с backoff на connection_refused balancer
// вызывает MarkUnhealthy(reason), чтобы selectBackend исключил этот backend.
func (hc *HealthChecker) MarkUnhealthy(backendID string, reason string) {
	if hc == nil {
		return
	}
	hc.mu.Lock()
	defer hc.mu.Unlock()
	status, ok := hc.results[backendID]
	if !ok {
		status = &HealthStatus{Healthy: true}
		hc.results[backendID] = status
	}
	if status.Healthy {
		status.Healthy = false
		status.LastFailure = time.Now()
		if status.LastError == "" {
			status.LastError = reason
		}
		// ConsecutiveFails увеличим — при следующем checkLoop backend будет проверен
		// с более высоким приоритетом для восстановления статуса.
		status.ConsecutiveFails++
	}
}

// GetAllStatuses - получение всех статусов.
//
// R66d (2026-09-22): как и GetStatus, отдаём КОПИИ — иначе вызывающий
// (WebUI/health-handler) читает поля параллельно с записью из checkBackend.
func (hc *HealthChecker) GetAllStatuses() map[string]*HealthStatus {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	result := make(map[string]*HealthStatus, len(hc.results))
	for id, status := range hc.results {
		if status == nil {
			continue
		}
		cp := *status
		result[id] = &cp
	}
	return result
}

// GetHealthyBackends - получение списка здоровых бэкендов
func (hc *HealthChecker) GetHealthyBackends() []string {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	var healthy []string
	for id, status := range hc.results {
		if status.Healthy {
			healthy = append(healthy, id)
		}
	}
	return healthy
}

// SetStatusForTest — устанавливает произвольный HealthStatus для backend'а.
// Используется ТОЛЬКО в unit-тестах (F.γ handlers_health_test.go) для имитации
// healthcheck failures без выполнения реальных HTTP-запросов.
//
// Не thread-safe с горячей заменой состояния; для тестов достаточно
// (вызывается ДО чтения GetAllStatuses).
func (hc *HealthChecker) SetStatusForTest(backendID string, status *HealthStatus) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if hc.results == nil {
		hc.results = make(map[string]*HealthStatus)
	}
	if status == nil {
		delete(hc.results, backendID)
		return
	}
	hc.results[backendID] = status
}
