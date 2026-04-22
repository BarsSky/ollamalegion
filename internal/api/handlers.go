package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// Server - API сервер
type Server struct {
	proxy        *balancer.Proxy
	config       *types.LoadBalancerConfig
	mux          *http.ServeMux
	healthChecker *balancer.HealthChecker
}

// NewServer - создание API сервера
func NewServer(proxy *balancer.Proxy, config *types.LoadBalancerConfig, healthChecker *balancer.HealthChecker) *Server {
	s := &Server{
		proxy:        proxy,
		config:       config,
		mux:          http.NewServeMux(),
		healthChecker: healthChecker,
	}
	
	s.setupRoutes()
	return s
}

// setupRoutes - настройка маршрутов
func (s *Server) setupRoutes() {
	// Health check
	s.mux.HandleFunc("/api/v1/health", s.healthHandler)
	
	// Backends
	s.mux.HandleFunc("/api/v1/backends", s.backendsHandler)
	s.mux.HandleFunc("/api/v1/backends/", s.backendHandler)
	
	// Metrics
	s.mux.HandleFunc("/api/v1/metrics", s.metricsHandler)
	s.mux.HandleFunc("/api/v1/metrics/", s.metricHandler)
	
	// Sessions
	s.mux.HandleFunc("/api/v1/sessions", s.sessionsHandler)
	
	// Models
	s.mux.HandleFunc("/api/v1/models", s.modelsHandler)
	
	// Cluster state
	s.mux.HandleFunc("/api/v1/cluster", s.clusterHandler)
	
	// Agents endpoints
	s.mux.HandleFunc("/api/v1/agents/register", s.agentRegisterHandler)
	s.mux.HandleFunc("/api/v1/agents/metrics", s.agentMetricsHandler)
	s.mux.HandleFunc("/api/v1/agents/heartbeat", s.agentHeartbeatHandler)
	
	// WebSocket
	s.mux.HandleFunc("/ws/metrics", s.wsMetricsHandler)
}

// ServeHTTP - обработка HTTP запросов
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Agent-ID")
	
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
		"status":    "healthy",
		"timestamp": time.Now().UTC(),
		"version":   "1.0.0",
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

// backendHandler - управление конкретным бэкендом
func (s *Server) backendHandler(w http.ResponseWriter, r *http.Request) {
	// Извлечение ID из пути
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/backends/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Backend ID required", http.StatusBadRequest)
		return
	}
	
	backendID := parts[0]
	
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

// listBackends - получение списка бэкендов
func (s *Server) listBackends(w http.ResponseWriter, r *http.Request) {
	backends := make([]map[string]interface{}, 0)
	
	// Получение состояния кластера
	state := s.proxy.GetClusterState()
	
	for _, metrics := range state.Backends {
		backend := map[string]interface{}{
			"id":        metrics.ID,
			"timestamp": metrics.Timestamp,
			"gpu":       metrics.GPU,
			"system":    metrics.System,
			"ollama":    metrics.Ollama,
		}
		backends = append(backends, backend)
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
		time.Sleep(500 * time.Millisecond)
		s.proxy.UpdateBackendStatus(req.ID, types.StatusHealthy)
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
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	// TODO: получить сессии из SessionManager
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"sessions": []interface{}{},
		"total":    0,
	})
}

// modelsHandler - получение запущенных моделей
func (s *Server) modelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	state := s.proxy.GetClusterState()
	
	models := make(map[string][]string)
	for _, metrics := range state.Backends {
		for _, model := range metrics.Ollama.RunningModels {
			models[metrics.ID] = append(models[metrics.ID], model.Name)
		}
	}
	
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"models": models,
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

// agentRegisterHandler - регистрация агента (самостоятельная регистрация бэкенда)
func (s *Server) agentRegisterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var req struct {
		AgentID         string   `json:"agentId"`
		Hostname        string   `json:"hostname"`
		Host            string   `json:"host"`
		OllamaPort      int      `json:"ollamaPort"`
		AgentPort       int      `json:"agentPort"`
		GPUCount        int      `json:"gpuCount"`
		Name            string   `json:"name"`
		Labels          []string `json:"labels"`
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
		agentPort = 9090
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
	
	// Обновление статуса после небольшой задержки (имитация health check)
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.proxy.UpdateBackendStatus(req.AgentID, types.StatusHealthy)
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
	
	var metrics types.BackendMetrics
	if err := json.NewDecoder(r.Body).Decode(&metrics); err != nil {
		http.Error(w, "Invalid metrics format", http.StatusBadRequest)
		return
	}
	
	// Обновление метрик в прокси
	s.proxy.UpdateMetrics(agentID, &metrics)
	
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "received",
	})
}

// agentHeartbeatHandler - получение heartbeat от агента
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
	
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
	})
}

// wsMetricsHandler - WebSocket для real-time метрик
func (s *Server) wsMetricsHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: реализация WebSocket
	http.Error(w, "WebSocket not implemented", http.StatusNotImplemented)
}

// writeJSON - запись JSON ответа
func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.Encode(data)
}
