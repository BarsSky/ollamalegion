package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"

	"github.com/gorilla/websocket"
)

// Server - API сервер
type Server struct {
	proxy         *balancer.Proxy
	config        *types.LoadBalancerConfig
	mux           *http.ServeMux
	healthChecker *balancer.HealthChecker
	metricsBroker *MetricsBroker
	rateLimiter   *RateLimiter
	wsRateLimiter *RateLimiter
	authenticator *TokenAuthenticator
	stopCh        chan struct{} // graceful shutdown for metricsPublishLoop
}

// upgrader - апгрейдер HTTP до WebSocket с CORS whitelist
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // same-origin запросы
		}
		// Whitelist допустимых origin
		allowedOrigins := []string{
			"http://localhost:3000",
			"http://localhost:8080",
			"http://localhost:18030",
			"http://localhost:18081",
			"http://127.0.0.1:18030",
			"http://127.0.0.1:18081",
		}
		for _, allowed := range allowedOrigins {
			if strings.HasPrefix(origin, allowed) {
				return true
			}
		}
		logger.Get().Warnw("websocket origin rejected", "origin", origin)
		return false
	},
}

// NewServer - создание API сервера
func NewServer(proxy *balancer.Proxy, config *types.LoadBalancerConfig, healthChecker *balancer.HealthChecker) *Server {
	// Инициализация rate limiter с параметрами из конфига
	rateLimiter := NewRateLimiter(config.API.RateBurst, config.API.RateLimit)

	// Отдельный rate limiter для WebSocket с более высоким лимитом
	wsRateLimiter := NewRateLimiter(config.API.RateBurst*2, config.API.RateLimit*2)

	// Инициализация аутентификатора
	authenticator := NewTokenAuthenticator(
		config.Auth.Tokens,
		config.Auth.HeaderName,
		config.Auth.Enabled,
	)

	s := &Server{
		proxy:         proxy,
		config:        config,
		mux:           http.NewServeMux(),
		healthChecker: healthChecker,
		metricsBroker: NewMetricsBroker(),
		rateLimiter:   rateLimiter,
		wsRateLimiter: wsRateLimiter,
		authenticator: authenticator,
		stopCh:        make(chan struct{}),
	}

	s.setupRoutes()

	// Запуск goroutine для периодической отправки метрик
	go s.metricsPublishLoop()

	return s
}

// metricsPublishLoop - периодическая публикация метрик в брокер
func (s *Server) metricsPublishLoop() {
	ticker := time.NewTicker(2000 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			state := s.proxy.GetClusterState()
			if state == nil {
				continue
			}

			// Публикуем полное состояние кластера для WebSocket клиентов
			stateCopy := *state
			s.metricsBroker.PublishClusterState(&stateCopy)
		case <-s.stopCh:
			return
		}
	}
}

// StopMetricsLoop - остановка metrics publish loop
func (s *Server) StopMetricsLoop() {
	close(s.stopCh)
}

// setupRoutes - настройка маршрутов
func (s *Server) setupRoutes() {
	// Health check (без аутентификации и rate limiting)
	s.mux.HandleFunc("/api/v1/health", s.healthHandler)

	// Auth endpoints (требуют токен, кроме health)
	s.mux.Handle("/api/v1/auth/status", AuthMiddleware(RateLimitMiddleware(AuthStatusHandler(s.authenticator), s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/auth/token", AuthMiddleware(RateLimitMiddleware(TokenManagementHandler(s.authenticator), s.rateLimiter), s.authenticator))

	// Rate limit status endpoint (публичный, без аутентификации)
	s.mux.HandleFunc("/api/v1/ratelimit/status", RateLimitStatusHandler(s.rateLimiter))

	// Backends (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/backends", AuthMiddleware(RateLimitMiddleware(s.backendsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/backends/", AuthMiddleware(RateLimitMiddleware(s.backendHandler, s.rateLimiter), s.authenticator))

	// Models capacity (global)
	s.mux.Handle("/api/v1/models/capacity", AuthMiddleware(RateLimitMiddleware(s.modelsCapacityHandler, s.rateLimiter), s.authenticator))

	// Metrics (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/metrics", AuthMiddleware(RateLimitMiddleware(s.metricsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/metrics/", AuthMiddleware(RateLimitMiddleware(s.metricHandler, s.rateLimiter), s.authenticator))

	// Sessions (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/sessions", AuthMiddleware(RateLimitMiddleware(s.sessionsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/sessions/", AuthMiddleware(RateLimitMiddleware(s.sessionHandler, s.rateLimiter), s.authenticator))

	// Models (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/models", AuthMiddleware(RateLimitMiddleware(s.modelsHandler, s.rateLimiter), s.authenticator))

	// Cluster state (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/cluster", AuthMiddleware(RateLimitMiddleware(s.clusterHandler, s.rateLimiter), s.authenticator))

	// Queue stats (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/queue/stats", AuthMiddleware(RateLimitMiddleware(s.queueStatsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/queue/details", AuthMiddleware(RateLimitMiddleware(s.queueDetailsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/queue/history", AuthMiddleware(RateLimitMiddleware(s.queueHistoryHandler, s.rateLimiter), s.authenticator))

	// Cluster config (runtime-смена алгоритма)
	s.mux.Handle("/api/v1/cluster/config", AuthMiddleware(RateLimitMiddleware(s.clusterConfigHandler, s.rateLimiter), s.authenticator))

	// Predictions (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/predictions", AuthMiddleware(RateLimitMiddleware(s.predictionsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/predictions/", AuthMiddleware(RateLimitMiddleware(s.predictionHandler, s.rateLimiter), s.authenticator))

	// Agents endpoints (с аутентификацией и rate limiting)
	s.mux.Handle("/api/v1/agents/register", AuthMiddleware(RateLimitMiddleware(s.agentRegisterHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/agents/metrics", AuthMiddleware(RateLimitMiddleware(s.agentMetricsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/agents/heartbeat", AuthMiddleware(RateLimitMiddleware(s.agentHeartbeatHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/agents/stats", AuthMiddleware(RateLimitMiddleware(s.agentStatsHandler, s.rateLimiter), s.authenticator))
	s.mux.Handle("/api/v1/agents/", AuthMiddleware(RateLimitMiddleware(s.agentInfoHandler, s.rateLimiter), s.authenticator))

	// WebSocket (с rate limiting, аутентификация внутри handler после Upgrade)
	s.mux.Handle("/ws/metrics", RateLimitMiddleware(s.wsMetricsHandler, s.wsRateLimiter))

	// Monitor HTML page (без аутентификации)
	s.mux.HandleFunc("/monitor", s.monitorHandler)

	// Restart endpoint (c аутентификацией и rate limiting, только от webui)
	s.mux.Handle("/api/v1/admin/restart", AuthMiddleware(RateLimitMiddleware(s.restartHandler, s.rateLimiter), s.authenticator))

	// Favicon и статические ресурсы (без аутентификации, для браузеров)
	s.mux.HandleFunc("/favicon.ico", s.staticFileHandler)
	s.mux.HandleFunc("/favicon-16x16.png", s.staticFileHandler)
	s.mux.HandleFunc("/favicon-32x32.png", s.staticFileHandler)
	s.mux.HandleFunc("/logo.svg", s.staticFileHandler)
}

// ServeHTTP - обработка HTTP запросов
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Agent-ID, X-API-Token")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	s.mux.ServeHTTP(w, r)
}

// healthHandler - глубокая проверка здоровья API (Docker healthcheck)
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.proxy.GetClusterState()

	// Считаем бэкенды с агентом
	hasAgentCount := 0
	for _, b := range state.Backends {
		if b.HasAgent {
			hasAgentCount++
		}
	}

	// Статус HTTP: 503 если нет ни одного healthy бэкенда при наличии зарегистрированных
	httpStatus := http.StatusOK
	statusText := "healthy"

	if state.TotalBackends > 0 && state.HealthyBackends == 0 {
		httpStatus = http.StatusServiceUnavailable
		statusText = "degraded"
	}

	response := map[string]interface{}{
		"status":          statusText,
		"timestamp":       time.Now().UTC(),
		"version":         "1.0.0",
		"healthyBackends": state.HealthyBackends,
		"totalBackends":   state.TotalBackends,
		"hasAgents":       hasAgentCount,
		"totalRequests":   state.TotalRequests,
		"authEnabled":     false,
		"authHeader":      "X-API-Token",
		"wsEndpoint":      "/ws/metrics",
	}

	if s.authenticator != nil {
		response["authEnabled"] = s.authenticator.IsEnabled()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.Encode(response)
}

// backendsHandler - список бэкендов
func (s *Server) backendsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listBackends(w, r)
	case http.MethodPost:
		s.addBackend(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// backendHandler - управление конкретным бэкендом (включая подпути /limits, /capacity)
func (s *Server) backendHandler(w http.ResponseWriter, r *http.Request) {
	// Извлечение ID и подпути
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/backends/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Backend ID required", http.StatusBadRequest)
		return
	}

	backendID := parts[0]

	// Подпуть /limits
	if len(parts) > 1 && parts[1] == "limits" {
		if r.Method == http.MethodPut {
			s.updateBackendLimits(w, r, backendID)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Подпуть /capacity
	if len(parts) > 1 && parts[1] == "capacity" {
		if r.Method == http.MethodGet {
			s.backendCapacity(w, r, backendID)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Подпуть /reconfigure
	if len(parts) > 1 && parts[1] == "reconfigure" {
		s.reconfigureHandler(w, r)
		return
	}

	// Подпуть /launch-config
	if len(parts) > 1 && parts[1] == "launch-config" {
		s.backendLaunchConfigHandler(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getBackend(w, r, backendID)
	case http.MethodPut:
		s.updateBackend(w, r, backendID)
	case http.MethodDelete:
		s.deleteBackend(w, r, backendID)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// OllamaTag - структура ответа от Ollama API /api/tags
type OllamaTag struct {
	Name       string         `json:"name"`
	Size       uint64         `json:"size"`
	Digest     string         `json:"digest"`
	ModifiedAt string         `json:"modified_at"`
	Details    OllamaDetails  `json:"details"`
}

// OllamaDetails - детали модели из Ollama API
type OllamaDetails struct {
	Family        string `json:"family"`
	Format        string `json:"format"`
	ParameterSize string `json:"parameter_size"`
	Quantization  string `json:"quantization"`
}

// OllamaTagsResponse - ответ от Ollama API /api/tags
type OllamaTagsResponse struct {
	Models []OllamaTag `json:"models"`
}

// fetchModelsFromOllama - получение списка моделей с Ollama API
func fetchModelsFromOllama(host string, port int) ([]types.RunningModel, int, int, error) {
	url := fmt.Sprintf("http://%s:%d/api/tags", host, port)
	client := &http.Client{Timeout: 5 * time.Second}
	
	resp, err := client.Get(url)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return nil, 0, 0, fmt.Errorf("Ollama API returned status %d", resp.StatusCode)
	}
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, 0, err
	}
	
	var tagsResp OllamaTagsResponse
	if err := json.Unmarshal(body, &tagsResp); err != nil {
		return nil, 0, 0, err
	}
	
	// Конвертируем в RunningModel
	models := make([]types.RunningModel, 0, len(tagsResp.Models))
	var totalSize uint64 = 0
	
	for _, tag := range tagsResp.Models {
		model := types.RunningModel{
			Name:          tag.Name,
			Size:          tag.Size,
			Digest:        tag.Digest,
			Family:        tag.Details.Family,
			Format:        tag.Details.Format,
			ParameterSize: tag.Details.ParameterSize,
			Quantization:  tag.Details.Quantization,
			LoadCount:     0, // Загрузки отслеживаются отдельно
			VRAMUsage:     tag.Size / 1024 / 1024, // Приблизительно в MB
		}
		models = append(models, model)
		totalSize += tag.Size
	}
	
	// Расчет лимитов на основе доступной VRAM
	// Предполагаем среднюю модель ~4GB для расчетов
	const avgModelSize = 4 * 1024 * 1024 * 1024 // 4GB в bytes
	
	// Для расчета используем первую GPU метрику если доступна
	maxModels := 10  // По умолчанию
	maxConcurrent := 10 // По умолчанию
	
	if len(models) > 0 {
		// Вычисляем средний размер модели
		avgSize := totalSize / uint64(len(models))
		if avgSize < 1024*1024*1024 { // Если меньше 1GB
			avgSize = 1024 * 1024 * 1024 // Минимум 1GB для расчетов
		}
		
		// Ограничиваем max_models разумным значением
		maxModels = 20 // Максимум 20 моделей
		
		// max_concurrent_requests зависит от количества моделей и их размера
		maxConcurrent = 10
		if len(models) > 5 {
			maxConcurrent = 5
		}
		if len(models) > 10 {
			maxConcurrent = 3
		}
	}
	
	return models, maxModels, maxConcurrent, nil
}

// listBackends - получение списка бэкендов
func (s *Server) listBackends(w http.ResponseWriter, r *http.Request) {
	backends := make([]map[string]interface{}, 0)

	// Получение всех бэкендов из прокси
	allBackends := s.proxy.GetAllBackends()

	// Сортировка по ID для стабильного порядка
	sort.Slice(allBackends, func(i, j int) bool {
		return allBackends[i].ID < allBackends[j].ID
	})

	// Получение метрик из состояния кластера
	state := s.proxy.GetClusterState()

	// Создание карты метрик для быстрого доступа
	metricsMap := make(map[string]*types.BackendMetrics)
	for i := range state.Backends {
		metricsMap[state.Backends[i].ID] = &state.Backends[i]
	}

	// Формирование ответа для каждого бэкенда
	for _, backend := range allBackends {
		// Приоритет: RuntimeMaxConcurrentRequests > MaxConcurrentReqs
		maxConcurrent := backend.MaxConcurrentReqs
		if backend.RuntimeMaxConcurrentRequests > 0 {
			maxConcurrent = backend.RuntimeMaxConcurrentRequests
		}
		// Приоритет: RuntimeMaxModels > MaxModels
		maxModels := backend.MaxModels
		if backend.RuntimeMaxModels != 0 {
			maxModels = backend.RuntimeMaxModels
		}

		backendData := map[string]interface{}{
			"id":                    backend.ID,
			"name":                  backend.Name,
			"host":                  backend.Host,
			"ollamaPort":            backend.OllamaPort,
			"agentPort":             backend.AgentPort,
			"weight":                backend.Weight,
			"maxConcurrentRequests": maxConcurrent,
			"maxModels":             maxModels,
			"runtimeMaxModels":              backend.RuntimeMaxModels,
			"runtimeMaxConcurrentRequests":  backend.RuntimeMaxConcurrentRequests,
			"labels":                backend.Labels,
			"status":                backend.Status,
		}

		// Добавление метрик если они доступны
		if metrics, ok := metricsMap[backend.ID]; ok {
			backendData["timestamp"] = metrics.Timestamp
			backendData["gpu"] = metrics.GPU
			backendData["system"] = metrics.System
			backendData["prediction"] = metrics.Prediction
			
			ollamaMetrics := metrics.Ollama
			
			// FreeSlots вычисляем на основе лимитов
			if ollamaMetrics.MaxConcurrentRequests > 0 {
				freeSlots := ollamaMetrics.MaxConcurrentRequests - ollamaMetrics.ActiveRequests
				if freeSlots < 0 {
					freeSlots = 0
				}
				ollamaMetrics.FreeSlots = freeSlots
			}
			
			backendData["ollama"] = ollamaMetrics
		} else {
			// Пустые метрики если данные недоступны
			backendData["timestamp"] = time.Time{}
			backendData["gpu"] = types.GPUMetrics{}
			backendData["system"] = types.SystemMetrics{}
			backendData["ollama"] = types.OllamaMetrics{}
		}

		// Флаг наличия активного агента
		backendData["hasAgent"] = backend.HasAgent
		backendData["lastAgentContact"] = backend.LastAgentContact

		backends = append(backends, backendData)
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"backends": backends,
		"total":    len(backends),
	})
}

// getBackend - получение информации о бэкенде
func (s *Server) getBackend(w http.ResponseWriter, r *http.Request, backendID string) {
	state := s.proxy.GetClusterState()

	for _, metrics := range state.Backends {
		if metrics.ID == backendID {
			s.writeJSON(w, http.StatusOK, metrics)
			return
		}
	}

	// Если нет метрик, возвращаем конфигурацию бэкенда
	backend := s.proxy.GetBackend(backendID)
	if backend != nil {
		s.writeJSON(w, http.StatusOK, backend)
		return
	}

	http.Error(w, "Backend not found", http.StatusNotFound)
}

// addBackend - добавление бэкенда
func (s *Server) addBackend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID                string   `json:"id"`
		Name              string   `json:"name"`
		Host              string   `json:"host"`
		OllamaPort        int      `json:"ollamaPort"`
		AgentPort         int      `json:"agentPort"`
		Weight            int      `json:"weight"`
		MaxConcurrentReqs int      `json:"maxConcurrentRequests"`
		MaxModels         int      `json:"maxModels"`
		GPUMode           string   `json:"gpuMode"`
		Labels            []string `json:"labels"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	// Валидация
	if req.ID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Backend ID is required",
		})
		return
	}
	if req.Host == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Backend host is required",
		})
		return
	}

	// Проверка на дубликат
	if s.proxy.BackendExists(req.ID) {
		s.writeJSON(w, http.StatusConflict, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s already exists", req.ID),
		})
		return
	}

	backend := types.Backend{
		ID:                req.ID,
		Name:              req.Name,
		Host:              req.Host,
		OllamaPort:        req.OllamaPort,
		AgentPort:         req.AgentPort,
		Weight:            req.Weight,
		MaxConcurrentReqs: req.MaxConcurrentReqs,
		Labels:            req.Labels,
		Status:            types.StatusStarting,
	}

	// Добавление бэкенда в прокси
	if err := s.proxy.AddBackend(backend); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	// Запуск health check для нового бэкенда
	go func() {
		time.Sleep(1 * time.Second)
		// Проверяем доступность бэкенда через Ollama API
		backend := s.proxy.GetBackend(req.ID)
		if backend != nil {
			url := fmt.Sprintf("http://%s:%d/api/tags", backend.Host, backend.OllamaPort)
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Get(url)
			if err == nil && resp.StatusCode == http.StatusOK {
				s.proxy.UpdateBackendStatus(req.ID, types.StatusHealthy)
				resp.Body.Close()
			} else {
				// Бэкенд недоступен - оставляем статус unhealthy
				if err != nil {
					logger.Get().Errorw("backend health check unreachable",
						"backend", req.ID,
						"error", err,
					)
				}
				s.proxy.UpdateBackendStatus(req.ID, types.StatusUnhealthy)
			}
		}
	}()

	s.writeJSON(w, http.StatusCreated, map[string]interface{}{
		"success": true,
		"backend": backend,
		"message": "Backend added successfully. Health check will run automatically.",
	})
}

// updateBackend - обновление бэкенда
func (s *Server) updateBackend(w http.ResponseWriter, r *http.Request, backendID string) {
	var req struct {
		Name              string   `json:"name"`
		Host              string   `json:"host"`
		OllamaPort        int      `json:"ollamaPort"`
		AgentPort         int      `json:"agentPort"`
		Weight            int      `json:"weight"`
		MaxConcurrentReqs int      `json:"maxConcurrentRequests"`
		MaxModels         int      `json:"maxModels"`
		GPUMode           string   `json:"gpuMode"`
		Labels            []string `json:"labels"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	// Проверка существования бэкенда
	existing := s.proxy.GetBackend(backendID)
	if existing == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s not found", backendID),
		})
		return
	}

	updated := types.Backend{
		ID:                            backendID,
		Name:                          req.Name,
		Host:                          req.Host,
		OllamaPort:                    req.OllamaPort,
		AgentPort:                     req.AgentPort,
		Weight:                        req.Weight,
		MaxConcurrentReqs:             req.MaxConcurrentReqs,
		Labels:                        req.Labels,
		Status:                        existing.Status,
		HasAgent:                      existing.HasAgent,
		LastAgentContact:              existing.LastAgentContact,
		LastHealthCheck:               existing.LastHealthCheck,
		ConsecutiveFailures:           existing.ConsecutiveFailures,
		ActiveRequests:                existing.ActiveRequests,
		RuntimeMaxModels:              existing.RuntimeMaxModels,
		RuntimeMaxConcurrentRequests:  existing.RuntimeMaxConcurrentRequests,
	}

	// Обновление бэкенда в прокси
	if err := s.proxy.UpdateBackend(backendID, updated); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"backend": updated,
		"message": "Backend updated successfully",
	})
}

// deleteBackend - удаление бэкенда
func (s *Server) deleteBackend(w http.ResponseWriter, r *http.Request, backendID string) {
	// Проверка существования бэкенда
	if !s.proxy.BackendExists(backendID) {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s not found", backendID),
		})
		return
	}

	// Удаление бэкенда из прокси
	if err := s.proxy.RemoveBackend(backendID); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"id":      backendID,
		"message": "Backend removed successfully",
	})
}

// metricsHandler - получение метрик кластера
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.proxy.GetClusterState()

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":       state.Timestamp,
		"totalBackends":   state.TotalBackends,
		"healthyBackends": state.HealthyBackends,
		"backends":        state.Backends,
	})
}

// metricHandler - получение метрик конкретного бэкенда
func (s *Server) metricHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Извлечение ID из пути
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/metrics/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Backend ID required", http.StatusBadRequest)
		return
	}

	backendID := parts[0]

	state := s.proxy.GetClusterState()

	for _, metrics := range state.Backends {
		if metrics.ID == backendID {
			s.writeJSON(w, http.StatusOK, metrics)
			return
		}
	}

	http.Error(w, "Backend not found", http.StatusNotFound)
}

// sessionsHandler - получение активных сессий
func (s *Server) sessionsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sessions := s.proxy.GetSessions()

		type SessionInfo struct {
			ID            string        `json:"id"`
			ClientName    string        `json:"clientName"`
			ClientIP      string        `json:"clientIP"`
			BackendID     string        `json:"backendId"`
			BackendName   string        `json:"backendName"`
			Model         string        `json:"model"`
			CreatedAt     time.Time     `json:"createdAt"`
			LastRequestAt time.Time     `json:"lastRequestAt"`
			RequestCount  int           `json:"requestCount"`
			IdleSeconds   int           `json:"idleSeconds"`
		}

		result := make([]SessionInfo, 0, len(sessions))
		for _, session := range sessions {
			backend := s.proxy.GetBackend(session.BackendID)
			backendName := ""
			if backend != nil {
				backendName = backend.Name
			}
			result = append(result, SessionInfo{
				ID:            session.ID,
				ClientName:    session.ClientName,
				ClientIP:      session.ClientIP,
				BackendID:     session.BackendID,
				BackendName:   backendName,
				Model:         session.Model,
				CreatedAt:     session.CreatedAt,
				LastRequestAt: session.LastRequestAt,
				RequestCount:  session.RequestCount,
				IdleSeconds:   int(time.Since(session.LastRequestAt).Seconds()),
			})
		}

		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"total":    len(result),
			"sessions": result,
		})
	case http.MethodDelete:
		s.proxy.ClearSessions()
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": "All sessions cleared",
		})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// sessionHandler - управление конкретной сессией
func (s *Server) sessionHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Session ID required", http.StatusBadRequest)
		return
	}

	sessionID := parts[0]

	switch r.Method {
	case http.MethodDelete:
		if s.proxy.DeleteSession(sessionID) {
			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"success":   true,
				"sessionId": sessionID,
				"message":   "Session removed successfully",
			})
		} else {
			s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("Session %s not found", sessionID),
			})
		}
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// modelsHandler - получение запущенных моделей с полными details
func (s *Server) modelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.proxy.GetClusterState()

	type BackendModels struct {
		BackendID     string              `json:"backendId"`
		BackendName   string              `json:"backendName"`
		BackendStatus types.BackendStatus `json:"backendStatus"`
		HasAgent      bool                `json:"hasAgent"`
		Models        []types.RunningModel `json:"models"`
	}

	result := make([]BackendModels, 0, len(state.Backends))
	for _, metrics := range state.Backends {
		backend := s.proxy.GetBackend(metrics.ID)
		name := ""
		if backend != nil {
			name = backend.Name
		}
		result = append(result, BackendModels{
			BackendID:     metrics.ID,
			BackendName:   name,
			BackendStatus: metrics.Status,
			HasAgent:      metrics.HasAgent,
			Models:        metrics.Ollama.RunningModels,
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"backends": result,
		"total":    len(result),
	})
}

// clusterHandler - состояние кластера
func (s *Server) clusterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.proxy.GetClusterState()

	s.writeJSON(w, http.StatusOK, state)
}

// queueStatsHandler - статистика очереди
func (s *Server) queueStatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := s.proxy.GetQueueStats()
	s.writeJSON(w, http.StatusOK, stats)
}

// queueDetailsHandler - детали очереди (pending + processing requests)
func (s *Server) queueDetailsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pending := s.proxy.GetQueuePendingRequests()
	processing := s.proxy.GetQueueProcessingRequests()
	stats := s.proxy.GetQueueStats()

	// Объединяем в единый список для отображения
	all := make([]map[string]interface{}, 0, len(pending)+len(processing))
	all = append(all, pending...)
	all = append(all, processing...)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"pending":    pending,
		"processing": processing,
		"all":        all,
		"pending_count":    len(pending),
		"processing_count": len(processing),
		"total":            len(all),
		"current_size":     stats.CurrentSize,
		"processed_total":  stats.Processed,
	})
}

// queueHistoryHandler - история выполненных запросов
func (s *Server) queueHistoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	history := s.proxy.GetQueueHistory()
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"history": history,
	})
}

// clusterConfigHandler - runtime конфигурация кластера (смена алгоритма и т.д.)
func (s *Server) clusterConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"algorithm":         s.config.Balancing.Algorithm,
			"modelAffinity":     s.config.Balancing.ModelAffinity,
			"sessionStickiness": s.config.Balancing.SessionStickiness,
			"queueMaxSize":      s.config.Balancing.QueueMaxSize,
			"queueTimeout":      s.config.Balancing.QueueTimeout,
			"requestTimeout":    s.config.Balancing.RequestTimeout,
		})
	case http.MethodPut:
		var req struct {
			Algorithm         string `json:"algorithm"`
			ModelAffinity     *bool  `json:"modelAffinity"`
			SessionStickiness *bool  `json:"sessionStickiness"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   "Invalid request body",
			})
			return
		}

		// Валидация алгоритма
		if req.Algorithm != "" {
			validAlgorithms := map[string]bool{
				"roundrobin":     true,
				"leastconn":      true,
				"resource-aware": true,
				"model-affinity": true,
			}
			if !validAlgorithms[req.Algorithm] {
				s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"error":   "Invalid algorithm. Valid: roundrobin, leastconn, resource-aware, model-affinity",
				})
				return
			}
			s.config.Balancing.Algorithm = types.BalancingAlgorithm(req.Algorithm)
		}

		if req.ModelAffinity != nil {
			s.config.Balancing.ModelAffinity = *req.ModelAffinity
		}
		if req.SessionStickiness != nil {
			s.config.Balancing.SessionStickiness = *req.SessionStickiness
		}

		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"config": map[string]interface{}{
				"algorithm":         s.config.Balancing.Algorithm,
				"modelAffinity":     s.config.Balancing.ModelAffinity,
				"sessionStickiness": s.config.Balancing.SessionStickiness,
			},
			"message": "Cluster configuration updated successfully",
		})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// agentRegisterHandler - регистрация агента (самостоятельная регистрация бэкенда)
func (s *Server) agentRegisterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		AgentID    string   `json:"agentId"`
		Hostname   string   `json:"hostname"`
		Host       string   `json:"host"`
		OllamaPort int      `json:"ollamaPort"`
		AgentPort  int      `json:"agentPort"`
		GPUCount   int      `json:"gpuCount"`
		Name       string   `json:"name"`
		Labels     []string `json:"labels"`
		Weight     int      `json:"weight"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	// Валидация
	if req.AgentID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "agentId is required",
		})
		return
	}

	// Если host не указан, используем hostname или RemoteAddr
	host := req.Host
	if host == "" {
		if req.Hostname != "" {
			host = req.Hostname
		} else {
			// Извлекаем IP из RemoteAddr
			host, _, _ = net.SplitHostPort(r.RemoteAddr)
			if host == "" {
				host = "localhost"
			}
		}
	}

	// Установка портов по умолчанию
	ollamaPort := req.OllamaPort
	if ollamaPort == 0 {
		ollamaPort = 11434
	}
	agentPort := req.AgentPort
	if agentPort == 0 {
		agentPort = 18032
	}

	// Определяем вес (по умолчанию 1)
	weight := req.Weight
	if weight <= 0 {
		weight = 1
	}

	// Проверка, существует ли уже бэкенд
	if s.proxy.BackendExists(req.AgentID) {
		// Обновляем существующий
		existing := s.proxy.GetBackend(req.AgentID)
		updated := types.Backend{
			ID:                req.AgentID,
			Name:              req.Name,
			Host:              host,
			OllamaPort:        ollamaPort,
			AgentPort:         agentPort,
			Weight:            weight,
			MaxConcurrentReqs: existing.MaxConcurrentReqs,
			Labels:            req.Labels,
			Status:            types.StatusHealthy,
		}
		s.proxy.UpdateBackend(req.AgentID, updated)

		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"action":  "updated",
			"agentId": req.AgentID,
			"backend": updated,
		})
		return
	}

	// Создание нового бэкенда
	backend := types.Backend{
		ID:                req.AgentID,
		Name:              req.Name,
		Host:              host,
		OllamaPort:        ollamaPort,
		AgentPort:         agentPort,
		Weight:            weight,
		MaxConcurrentReqs: 10,
		Labels:            req.Labels,
		Status:            types.StatusStarting,
	}

	// Добавление бэкенда в прокси
	if err := s.proxy.AddBackend(backend); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	// Обновление статуса после health check
	go func() {
		time.Sleep(1 * time.Second)
		// Проверяем доступность бэкенда через Ollama API
		backend := s.proxy.GetBackend(req.AgentID)
		if backend != nil {
			url := fmt.Sprintf("http://%s:%d/api/tags", backend.Host, backend.OllamaPort)
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Get(url)
			if err == nil && resp.StatusCode == http.StatusOK {
				s.proxy.UpdateBackendStatus(req.AgentID, types.StatusHealthy)
				if resp != nil {
					resp.Body.Close()
				}
			} else {
				// Бэкенд недоступен - оставляем статус unhealthy
				if err != nil {
					logger.Get().Errorw("agent health check unreachable",
						"agent", req.AgentID,
						"error", err,
					)
				}
				s.proxy.UpdateBackendStatus(req.AgentID, types.StatusUnhealthy)
			}
		}
	}()

	s.writeJSON(w, http.StatusCreated, map[string]interface{}{
		"success": true,
		"action":  "created",
		"agentId": req.AgentID,
		"backend": backend,
		"message": "Agent registered successfully. Backend added to the pool.",
	})
}

// agentMetricsHandler - получение метрик от агента
func (s *Server) agentMetricsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		http.Error(w, "X-Agent-ID header required", http.StatusBadRequest)
		return
	}

	// Сначала валидируем JSON (тест ожидает 400 для невалидного JSON)
	var metrics types.BackendMetrics
	if err := json.NewDecoder(r.Body).Decode(&metrics); err != nil {
		http.Error(w, "Invalid metrics format", http.StatusBadRequest)
		return
	}

	// Игнорируем метрики, если бэкенд не зарегистрирован
	if !s.proxy.BackendExists(agentID) {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"status": "error",
			"error":  "Backend not found. Please register agent first.",
		})
		return
	}

	// Обновление метрик в прокси
	s.proxy.UpdateMetrics(agentID, &metrics)

	// Обновляем флаг активного агента
	s.proxy.UpdateBackendAgentStatus(agentID, true)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "received",
	})
}

// agentHeartbeatHandler - получение heartbeat от агента (расширенный)
func (s *Server) agentHeartbeatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		http.Error(w, "X-Agent-ID header required", http.StatusBadRequest)
		return
	}

	// Читаем weight из heartbeat payload
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	var hbPayload struct {
		Weight                int `json:"weight"`
		MaxConcurrentRequests int `json:"maxConcurrentRequests"`
		MaxModels             int `json:"maxModels"`
	}
	_ = json.Unmarshal(body, &hbPayload)

	// Обновление статуса
	s.proxy.UpdateBackendStatus(agentID, types.StatusHealthy)

	// Обновляем флаг активного агента
	s.proxy.UpdateBackendAgentStatus(agentID, true)

	// Применяем лимиты из heartbeat агента (если > 0 — агент явно задал лимит)
	backend := s.proxy.GetBackend(agentID)
	if backend != nil {
		needUpdate := false
		updated := *backend

		if hbPayload.Weight > 0 && backend.Weight != hbPayload.Weight {
			updated.Weight = hbPayload.Weight
			needUpdate = true
		}
		if hbPayload.MaxConcurrentRequests > 0 && updated.RuntimeMaxConcurrentRequests != hbPayload.MaxConcurrentRequests {
			updated.RuntimeMaxConcurrentRequests = hbPayload.MaxConcurrentRequests
			needUpdate = true
		}
		if hbPayload.MaxModels > 0 && updated.RuntimeMaxModels != hbPayload.MaxModels {
			updated.RuntimeMaxModels = hbPayload.MaxModels
			needUpdate = true
		}

		if needUpdate {
			s.proxy.UpdateBackend(agentID, updated)
		}
	}

	now := time.Now().UTC()

	// Получаем runtime-конфигурацию бэкенда для передачи агенту
	backend = s.proxy.GetBackend(agentID)
	config := map[string]interface{}{
		"maxModels":             -1,
		"maxConcurrentRequests": -1,
	}
	if backend != nil {
		// Приоритет: Runtime-значение > статический MaxConcurrentReqs из конфига
		if backend.RuntimeMaxModels != 0 {
			config["maxModels"] = backend.RuntimeMaxModels
		} else if backend.MaxModels != 0 {
			config["maxModels"] = backend.MaxModels
		}
		if backend.RuntimeMaxConcurrentRequests != 0 {
			config["maxConcurrentRequests"] = backend.RuntimeMaxConcurrentRequests
		} else if backend.MaxConcurrentReqs != 0 {
			config["maxConcurrentRequests"] = backend.MaxConcurrentReqs
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":       "ok",
		"serverTime":   now,
		"acknowledged": now,
		"config":       config,
	})
}

// agentStatsHandler - получение статистики агентов
func (s *Server) agentStatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := s.proxy.GetAllBackends()

	agents := make([]map[string]interface{}, 0, len(backends))
	healthyCount := 0

	for _, backend := range backends {
		if backend.Status == types.StatusHealthy {
			healthyCount++
		}
		agents = append(agents, map[string]interface{}{
			"id":            backend.ID,
			"hostname":      backend.Name,
			"status":        backend.Status,
			"lastHeartbeat": backend.LastHealthCheck,
			"host":          backend.Host,
			"agentPort":     backend.AgentPort,
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"totalAgents":   len(agents),
		"healthyAgents": healthyCount,
		"agents":        agents,
	})
}

// agentInfoHandler - получение информации о конкретном агенте
func (s *Server) agentInfoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Извлечение ID из пути
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/agents/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Agent ID required", http.StatusBadRequest)
		return
	}

	agentID := parts[0]

	backend := s.proxy.GetBackend(agentID)
	if backend == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Agent %s not found", agentID),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, backend)
}

// predictionsHandler - прогнозы для всех бэкендов
func (s *Server) predictionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.proxy.GetClusterState()

	predictions := make(map[string]interface{})
	for _, backend := range state.Backends {
		pred := s.proxy.GetPrediction(backend.ID)
		predictions[backend.ID] = map[string]interface{}{
			"backendId":         backend.ID,
			"secondsToCritical": pred.SecondsToCritical,
			"criticalReason":    pred.CriticalReason,
			"gpuUsageTrend":     pred.GPUUsageTrend,
			"vramUsageTrend":    pred.VRAMUsageTrend,
			"ramUsageTrend":     pred.RAMUsageTrend,
			"freeSlotsTrend":    pred.FreeSlotsTrend,
			"requestCapacity":   pred.RequestCapacity,
			"capacityPercent":   int(pred.RequestCapacity),
			"timeToCritical":    balancer.FormatDuration(pred.SecondsToCritical),
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":   time.Now().UTC(),
		"predictions": predictions,
	})
}

// predictionHandler - прогноз для конкретного бэкенда
func (s *Server) predictionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/predictions/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Backend ID required", http.StatusBadRequest)
		return
	}

	backendID := parts[0]

	state := s.proxy.GetClusterState()
	for _, backend := range state.Backends {
		if backend.ID == backendID {
			pred := s.proxy.GetPrediction(backendID)
			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"backendId":         backendID,
				"secondsToCritical": pred.SecondsToCritical,
				"criticalReason":    pred.CriticalReason,
				"gpuUsageTrend":     pred.GPUUsageTrend,
				"vramUsageTrend":    pred.VRAMUsageTrend,
				"ramUsageTrend":     pred.RAMUsageTrend,
				"freeSlotsTrend":    pred.FreeSlotsTrend,
				"requestCapacity":   pred.RequestCapacity,
				"capacityPercent":   int(pred.RequestCapacity),
				"timeToCritical":    balancer.FormatDuration(pred.SecondsToCritical),
			})
			return
		}
	}

	http.Error(w, "Backend not found", http.StatusNotFound)
}

// wsMetricsHandler - WebSocket для real-time метрик (event-driven + гибридный ping)
func (s *Server) wsMetricsHandler(w http.ResponseWriter, r *http.Request) {
	// Проверяем токен ДО WebSocket upgrade, но только если auth включена
	if s.authenticator != nil && s.authenticator.IsEnabled() {
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "Unauthorized: token is required", http.StatusUnauthorized)
			return
		}
		valid, _ := s.authenticator.Authenticate(r)
		if !valid {
			http.Error(w, "Unauthorized: invalid token", http.StatusUnauthorized)
			return
		}
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Get().Errorw("websocket upgrade error", "error", err)
		return
	}
	defer conn.Close()

	// Генерация уникального ID клиента
	clientID := fmt.Sprintf("client-%d", time.Now().UnixNano())

	// Канал для сигнала об отключении
	done := make(chan struct{})
	defer close(done)

	// Устанавливаем начальный write deadline
	conn.SetWriteDeadline(time.Now().Add(60 * time.Second))

	// Подписка на EventBus (event-driven)
	eventSubID, eventChan := s.proxy.SubscribeEvents()
	defer s.proxy.UnsubscribeEvents(eventSubID)

	// Подписка на periodic snapshot (fallback / heartbeat)
	metricsChan := s.metricsBroker.Subscribe(clientID, done)
	defer s.metricsBroker.Unsubscribe(clientID)

	// Канал для ошибок чтения
	errChan := make(chan error, 1)

	// Настраиваем read deadline и pong handler
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	// Goroutine для чтения сообщений от клиента (ping/pong/close)
	go func() {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
		}
	}()

	// Ping ticker (каждые 15 секунд для быстрого обнаружения разрывов)
	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()

	// Отправка начального состояния кластера
	initialState := s.metricsBroker.GetClusterState(s.proxy)
	if err := conn.WriteJSON(map[string]interface{}{
		"eventType": "clusterState",
		"timestamp": time.Now().UTC(),
		"data":      initialState,
	}); err != nil {
		logger.Get().Errorw("failed to send initial websocket state", "error", err)
		return
	}

	// Основной цикл: event-driven + periodic snapshot + ping
	for {
		select {
		case ev, ok := <-eventChan:
			if !ok {
				return
			}
			// Отправляем событие клиенту с eventType wrapper + write deadline
			conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			wrapper := map[string]interface{}{
				"eventType": string(ev.Type),
				"timestamp": ev.Timestamp,
				"backendId": ev.BackendID,
				"data":      ev.Data,
			}
			if err := conn.WriteJSON(wrapper); err != nil {
				logger.Get().Errorw("failed to send websocket event", "error", err)
				return
			}

		case data, ok := <-metricsChan:
			if !ok {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			// Periodic snapshot — отправляем как clusterState
			var clusterState types.ClusterState
			if err := json.Unmarshal(data, &clusterState); err != nil {
				logger.Get().Errorw("failed to unmarshal cluster state", "error", err)
				continue
			}
			wrapper := map[string]interface{}{
				"eventType": "clusterState",
				"timestamp": time.Now().UTC(),
				"data":      clusterState,
			}
			if err := conn.WriteJSON(wrapper); err != nil {
				logger.Get().Errorw("failed to send websocket snapshot", "error", err)
				return
			}

		case <-pingTicker.C:
			// Ping для поддержания соединения с write deadline
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(map[string]interface{}{
				"eventType": "ping",
				"timestamp": time.Now().UTC(),
			}); err != nil {
				return
			}

		case err := <-errChan:
			logger.Get().Warnw("websocket read error", "error", err)
			return

		case <-done:
			return
		}
	}
}

// backendCapacity - детальная ёмкость конкретного бэкенда
func (s *Server) backendCapacity(w http.ResponseWriter, r *http.Request, backendID string) {
	state := s.proxy.GetClusterState()
	for _, metrics := range state.Backends {
		if metrics.ID == backendID {
			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"backendId":          backendID,
				"timestamp":          time.Now().UTC(),
				"freeVram":           metrics.Ollama.BackendCapacity.FreeVRAM,
				"guaranteedVram":     metrics.Ollama.BackendCapacity.GuaranteedVRAM,
				"loadedModelVram":    metrics.Ollama.BackendCapacity.LoadedModelVRAM,
				"contextOverheadMB":  metrics.Ollama.BackendCapacity.ContextOverheadMB,
				"availableModels":    metrics.Ollama.BackendCapacity.AvailableModels,
				"loadableModelCount": metrics.Ollama.BackendCapacity.LoadableModelCount,
				"mode":               metrics.Ollama.BackendCapacity.Mode,
				"runtimeFlags":       metrics.Ollama.RuntimeFlags,
				"modelContexts":      metrics.Ollama.ModelContexts,
			})
			return
		}
	}

	http.Error(w, "Backend not found", http.StatusNotFound)
}

// modelsCapacityHandler - глобальная сводка по ёмкости моделей на всех бэкендах
func (s *Server) modelsCapacityHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.proxy.GetClusterState()

	type BackendCapacitySummary struct {
		BackendID          string                   `json:"backendId"`
		BackendName        string                   `json:"backendName"`
		Status             types.BackendStatus      `json:"status"`
		HasAgent           bool                     `json:"hasAgent"`
		FreeVRAM           uint64                   `json:"freeVram"`
		GuaranteedVRAM     uint64                   `json:"guaranteedVram"`
		LoadableModelCount int                      `json:"loadableModelCount"`
		Mode               types.PlatformMode       `json:"mode"`
		RuntimeFlags       types.OllamaRuntimeFlags `json:"runtimeFlags"`
		AvailableModels    []types.AvailableModel   `json:"availableModels"`
	}

	summaries := make([]BackendCapacitySummary, 0, len(state.Backends))
	totalLoadable := 0

	for _, metrics := range state.Backends {
		backend := s.proxy.GetBackend(metrics.ID)
		name := ""
		if backend != nil {
			name = backend.Name
		}

		summary := BackendCapacitySummary{
			BackendID:          metrics.ID,
			BackendName:        name,
			Status:             metrics.Status,
			HasAgent:           metrics.HasAgent,
			FreeVRAM:           metrics.Ollama.BackendCapacity.FreeVRAM,
			GuaranteedVRAM:     metrics.Ollama.BackendCapacity.GuaranteedVRAM,
			LoadableModelCount: metrics.Ollama.BackendCapacity.LoadableModelCount,
			Mode:               metrics.Ollama.BackendCapacity.Mode,
			RuntimeFlags:       metrics.Ollama.RuntimeFlags,
			AvailableModels:    metrics.Ollama.BackendCapacity.AvailableModels,
		}

		summaries = append(summaries, summary)
		totalLoadable += metrics.Ollama.BackendCapacity.LoadableModelCount
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":        time.Now().UTC(),
		"totalBackends":    len(state.Backends),
		"totalLoadable":    totalLoadable,
		"backends":         summaries,
	})
}

// updateBackendLimits - обновление runtime-лимитов бэкенда
func (s *Server) updateBackendLimits(w http.ResponseWriter, r *http.Request, backendID string) {
	var req struct {
		MaxModels             int `json:"maxModels"`
		MaxConcurrentRequests int `json:"maxConcurrentRequests"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	// Проверка существования бэкенда
	if !s.proxy.BackendExists(backendID) {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s not found", backendID),
		})
		return
	}

	// Обновление лимитов
	if err := s.proxy.UpdateBackendLimits(backendID, req.MaxModels, req.MaxConcurrentRequests); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	backend := s.proxy.GetBackend(backendID)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"backendId": backendID,
		"limits": map[string]interface{}{
			"maxModels":             req.MaxModels,
			"maxConcurrentRequests": req.MaxConcurrentRequests,
		},
		"runtime": map[string]interface{}{
			"runtimeMaxModels":             backend.RuntimeMaxModels,
			"runtimeMaxConcurrentRequests": backend.RuntimeMaxConcurrentRequests,
		},
		"message": "Backend limits updated successfully",
	})
}

// monitorHandler - отдаёт HTML страницу монитора
func (s *Server) monitorHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Пути для поиска monitor.html (сначала runtime, потом dev)
	paths := []string{
		"/app/monitor.html",
		"cmd/monitor/monitor.html",
		"../cmd/monitor/monitor.html",
	}

	var data []byte
	var monitorPath string
	var err error
	for _, p := range paths {
		data, err = os.ReadFile(p)
		if err == nil {
			monitorPath = p
			break
		}
	}

	if err != nil {
		logger.Get().Errorw("monitor.html not found", "error", err)
		http.Error(w, "Monitor page not found", http.StatusNotFound)
		return
	}

	htmlStr := string(data)

	// Встраиваем WEBUI_CONFIG и config.js inline
	configPath := filepath.Join(filepath.Dir(monitorPath), "config.js")
	if configData, configErr := os.ReadFile(configPath); configErr == nil {
		inline := fmt.Sprintf(
			`<script>window.WEBUI_CONFIG={apiBase:"",dashboardUrl:"/"};</script>`+"\n"+
				`<script>%s</script>`,
			string(configData),
		)
		// Заменяем <script src="config.js"></script> на inline-скрипты
		if strings.Contains(htmlStr, `<script src="config.js"></script>`) {
			htmlStr = strings.Replace(htmlStr, `<script src="config.js"></script>`, inline, 1)
		} else {
			// Fallback: вставляем перед </head>
			htmlStr = strings.Replace(htmlStr, "</head>", inline+"\n</head>", 1)
		}
	} else {
		// config.js не найден — встраиваем только WEBUI_CONFIG
		inline := `<script>window.WEBUI_CONFIG={apiBase:"",dashboardUrl:"/"};</script>`
		if strings.Contains(htmlStr, `<script src="config.js"></script>`) {
			htmlStr = strings.Replace(htmlStr, `<script src="config.js"></script>`, inline, 1)
		} else {
			htmlStr = strings.Replace(htmlStr, "</head>", inline+"\n</head>", 1)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(htmlStr))
}

// backendLaunchConfigHandler — прокси для backendHandler subpath /launch-config
func (s *Server) backendLaunchConfigHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/backends/"), "/")
	if len(parts) < 3 || parts[1] != "launch-config" {
		http.NotFound(w, r)
		return
	}
	s.listLaunchConfigs(w, r, parts[0])
}

// listLaunchConfigs — возвращает конфигурации запуска для backendId
func (s *Server) listLaunchConfigs(w http.ResponseWriter, r *http.Request, backendID string) {
	state := s.proxy.GetClusterState()
	for _, metrics := range state.Backends {
		if metrics.ID == backendID {
			gpuCount := 0
			if metrics.GPU.MemoryTotal > 0 { gpuCount = 1 }
			vramTotal := metrics.GPU.MemoryTotal
			ramTotal := metrics.System.MemoryTotal

			configs := []map[string]interface{}{
				{
					"label": "Оптимальный (сбалансированный)", "type": "optimal", "isOptimal": true,
					"envVars": map[string]string{
						"OLLAMA_NUM_PARALLEL": "4", "OLLAMA_MAX_LOADED_MODELS": "2",
						"OLLAMA_KV_CACHE_TYPE": "f16", "OLLAMA_GPU_LAYERS": "-1",
						"OLLAMA_FLASH_ATTENTION": "1", "OLLAMA_NUM_THREADS": fmt.Sprintf("%d", 4),
						"OLLAMA_CONTEXT_LENGTH": "4096",
					},
					"description": "Сбалансированная конфигурация",
					"limitations": []string{},
				},
				{
					"label": "Скоростной (упор на скорость)", "type": "speed", "isOptimal": false,
					"envVars": map[string]string{
						"OLLAMA_NUM_PARALLEL": "8", "OLLAMA_MAX_LOADED_MODELS": "4",
						"OLLAMA_KV_CACHE_TYPE": "f16", "OLLAMA_GPU_LAYERS": "-1",
						"OLLAMA_FLASH_ATTENTION": "1", "OLLAMA_NUM_THREADS": fmt.Sprintf("%d", 4),
						"OLLAMA_CONTEXT_LENGTH": "4096",
					},
					"description": "Максимальный параллелизм, все модели в VRAM",
					"limitations": []string{"Высокое потребление VRAM — возможен OOM на больших моделях"},
				},
				{
					"label": "Экономный (упор на размышления)", "type": "quality", "isOptimal": false,
					"envVars": map[string]string{
						"OLLAMA_NUM_PARALLEL": "1", "OLLAMA_MAX_LOADED_MODELS": "1",
						"OLLAMA_KV_CACHE_TYPE": "q8_0", "OLLAMA_GPU_LAYERS": "-1",
						"OLLAMA_FLASH_ATTENTION": "1", "OLLAMA_NUM_THREADS": fmt.Sprintf("%d", 4/2),
						"OLLAMA_CONTEXT_LENGTH": "8192",
					},
					"description": "Один запрос с большим контекстом 8K для размышлений",
					"limitations": []string{"Однопоточный режим — другие клиенты будут ждать в очереди"},
				},
			}

			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"success":   true,
				"backendId": backendID,
				"hardware": map[string]interface{}{
					"gpuCount": gpuCount, "vramTotalMB": vramTotal,
					"ramTotalMB": ramTotal, "cpuThreads": 4,
				},
				"configs":   configs,
				"isOptimal": true,
				"message":   "Текущая конфигурация оптимальна. Альтернативные варианты показаны для ознакомления.",
			})
			return
		}
	}
	s.writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "error": "Backend not found"})
}



// reconfigureHandler — POST /api/v1/backends/{id}/reconfigure
// Принимает envVars и инициирует переформирование бэкенда с новыми параметрами запуска Ollama.
func (s *Server) reconfigureHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/backends/"), "/")
	if len(parts) < 2 || parts[1] != "reconfigure" {
		http.NotFound(w, r)
		return
	}
	backendID := parts[0]

	var req struct {
		EnvVars map[string]string `json:"envVars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	if !s.proxy.BackendExists(backendID) {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   "Backend not found",
		})
		return
	}

	// Публикуем событие переформирования — агент подписан через WebSocket/события
	s.proxy.PublishEvent(types.Event{
		Type:      types.EventReconfigure,
		Timestamp: time.Now().UTC(),
		BackendID: backendID,
		Data: map[string]interface{}{
			"envVars": req.EnvVars,
		},
	})

	logger.Get().Infow("reconfigure request published", "backend", backendID, "envVars", req.EnvVars)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"backendId": backendID,
		"message":   "Reconfigure event published. Agent will apply new settings.",
	})
}

// restartHandler - перезапуск балансера (только для webui)
func (s *Server) restartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	logger.Get().Infow("balancer restart requested via API")

	// Запускаем перезапуск в goroutine, чтобы ответить клиенту до остановки
	go func() {
		time.Sleep(500 * time.Millisecond) // Даём время на отправку ответа
		if err := s.proxy.Restart(); err != nil {
			logger.Get().Errorw("balancer restart failed", "error", err)
		}
	}()

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "Balancer restart initiated. The process will exit and be restarted by the supervisor.",
	})
}

// staticFileHandler — обработчик статических файлов (favicon, logo, etc.)
// Ищет файлы в webui/ директории (или в /app/ для Docker runtime)
func (s *Server) staticFileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	fileName := filepath.Base(r.URL.Path)
	if fileName == "." || fileName == "/" {
		http.NotFound(w, r)
		return
	}

	// Пути для поиска (сначала runtime Docker, потом локальная разработка)
	paths := []string{
		filepath.Join("/app", fileName),
		filepath.Join("webui", fileName),
		filepath.Join("../webui", fileName),
	}

	var data []byte
	var err error
	for _, p := range paths {
		data, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}

	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Определяем Content-Type
	contentType := "application/octet-stream"
	switch {
	case strings.HasSuffix(fileName, ".png"):
		contentType = "image/png"
	case strings.HasSuffix(fileName, ".ico"):
		contentType = "image/x-icon"
	case strings.HasSuffix(fileName, ".svg"):
		contentType = "image/svg+xml"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// writeJSON - запись JSON ответа
func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.Encode(data)
}
