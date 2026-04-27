package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
}

// upgrader - апгрейдер HTTP до WebSocket
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// Разрешаем все origin (для production нужно ограничить)
		return true
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
	}

	s.setupRoutes()

	// Запуск goroutine для периодической отправки метрик
	go s.metricsPublishLoop()

	return s
}

// metricsPublishLoop - периодическая публикация метрик в брокер
func (s *Server) metricsPublishLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		<-ticker.C
		state := s.proxy.GetClusterState()
		if state == nil {
			continue
		}

		// Публикуем полное состояние кластера для WebSocket клиентов
		stateCopy := *state
		s.metricsBroker.PublishClusterState(&stateCopy)
	}
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

// healthHandler - проверка здоровья API
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := map[string]interface{}{
		"status":      "healthy",
		"timestamp":   time.Now().UTC(),
		"version":     "1.0.0",
		"authEnabled": false,
		"authHeader":  "X-API-Token",
		"wsEndpoint":  "/ws/metrics",
	}

	if s.authenticator != nil {
		response["authEnabled"] = s.authenticator.IsEnabled()
	}

	s.writeJSON(w, http.StatusOK, response)
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

// backendHandler - управление конкретным бэкендом (включая подпути /limits)
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

	// Получение метрик из состояния кластера
	state := s.proxy.GetClusterState()

	// Создание карты метрик для быстрого доступа
	metricsMap := make(map[string]*types.BackendMetrics)
	for i := range state.Backends {
		metricsMap[state.Backends[i].ID] = &state.Backends[i]
	}

	// Формирование ответа для каждого бэкенда
	for _, backend := range allBackends {
		backendData := map[string]interface{}{
			"id":                    backend.ID,
			"name":                  backend.Name,
			"host":                  backend.Host,
			"ollamaPort":            backend.OllamaPort,
			"agentPort":             backend.AgentPort,
			"weight":                backend.Weight,
			"maxConcurrentRequests": backend.MaxConcurrentReqs,
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
			
			// Fallback: получаем модели напрямую с Ollama API (только если агент не прислал)
			if len(ollamaMetrics.RunningModels) == 0 {
				models, _, _, err := fetchModelsFromOllama(backend.Host, backend.OllamaPort)
				if err == nil {
					ollamaMetrics.RunningModels = models
				}
			}
			
			// Вычисляем свободные слоты (fallback для старых агентов без proxy-счётчика)
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
		ID:                backendID,
		Name:              req.Name,
		Host:              req.Host,
		OllamaPort:        req.OllamaPort,
		AgentPort:         req.AgentPort,
		Weight:            req.Weight,
		MaxConcurrentReqs: req.MaxConcurrentReqs,
		Labels:            req.Labels,
		Status:            existing.Status,
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
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"sessions": sessions,
			"total":    len(sessions),
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
			Weight:            existing.Weight,
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
		Weight:            1,
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

	// Обновление статуса
	s.proxy.UpdateBackendStatus(agentID, types.StatusHealthy)

	// Обновляем флаг активного агента
	s.proxy.UpdateBackendAgentStatus(agentID, true)

	now := time.Now().UTC()

	// Получаем runtime-конфигурацию бэкенда
	backend := s.proxy.GetBackend(agentID)
	config := map[string]interface{}{
		"maxModels":             -1,
		"maxConcurrentRequests": -1,
	}
	if backend != nil {
		if backend.RuntimeMaxModels != 0 {
			config["maxModels"] = backend.RuntimeMaxModels
		}
		if backend.RuntimeMaxConcurrentRequests != 0 {
			config["maxConcurrentRequests"] = backend.RuntimeMaxConcurrentRequests
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

	// Подписка на EventBus (event-driven)
	eventSubID, eventChan := s.proxy.SubscribeEvents()
	defer s.proxy.UnsubscribeEvents(eventSubID)

	// Подписка на periodic snapshot (fallback / heartbeat)
	metricsChan := s.metricsBroker.Subscribe(clientID, done)
	defer s.metricsBroker.Unsubscribe(clientID)

	// Канал для ошибок чтения
	errChan := make(chan error, 1)

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

	// Ping ticker (каждые 30 секунд)
	pingTicker := time.NewTicker(30 * time.Second)
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
			// Отправляем событие клиенту с eventType wrapper
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
			// Periodic snapshot — отправляем как clusterState
			// data приходит из metrics_broker как []byte (json.Marshal),
			// поэтому нужно сначала unmarshal в структуру,
			// иначе data будет сериализована как base64-строка в map[string]interface{}.
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
			// Ping для поддержания соединения
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

// writeJSON - запись JSON ответа
func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.Encode(data)
}
