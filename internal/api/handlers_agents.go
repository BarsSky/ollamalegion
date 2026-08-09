package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Agents API handlers
// ============================================================

// agentRegisterHandler - регистрация агента (самостоятельная регистрация бэкенда)
func (s *Server) agentRegisterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		AgentID       string   `json:"agentId"`
		Hostname      string   `json:"hostname"`
		Host          string   `json:"host"`
		OllamaPort    int      `json:"ollamaPort"`
		AgentPort     int      `json:"agentPort"`
		CppWorkerPort int      `json:"cppWorkerPort"`
		GPUCount      int      `json:"gpuCount"`
		Name          string   `json:"name"`
		Labels        []string `json:"labels"`
		Weight        int      `json:"weight"`
		BackendType   string   `json:"backendType"`
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

	// Нормализация типа бэкенда с эвристикой
	// Если тип не задан, но указан CppWorkerPort > 0 — считаем llama.cpp
	backendType := types.BackendType(req.BackendType)
	if backendType == "" {
		if req.CppWorkerPort > 0 {
			backendType = types.BackendTypeLlamaCpp
		} else {
			backendType = types.BackendTypeOllama
		}
	}

	// Round 12 (2026-07-10): agent attach to existing cppworker backend.
	// Bundled-режим: cppworker первично регистрирует inference endpoint,
	// agent при старте должен прикрепиться к этому же бэкенду (как metrics
	// provider), а не создавать второй бэкенд с тем же host:cppWorkerPort.
	// Lookup по (host, CppWorkerPort) → если найден cppworker-бэкенд →
	// attach через AttachAgentToBackend, не создаём новый.
	//
	// Round 30 (2026-08-09):
	//   1. Используем FindBackendByHostPortExcluding(req.AgentID) — иначе
	//      FindBackendByHostPort может вернуть stale standalone (с тем же
	//      ID что и req.AgentID, Go maps iteration order is random) →
	//      existingID == req.AgentID → dedup не сработает.
	//   2. После успешного attach — удаляем stale standalone бэкенд с
	//      ID == req.AgentID, если он был создан до введения dedup-логики
	//      (например, на 2026-08-06 когда cppworker ещё не был зарегистрирован).
	//      Иначе в /api/v1/backends висят 2 записи на один физический endpoint.
	if req.CppWorkerPort > 0 {
		if existingID := s.proxy.FindBackendByHostPortExcluding(host, req.CppWorkerPort, req.AgentID); existingID != "" {
			logger.Get().Infow("agentRegisterHandler: attaching to existing cppworker backend (dedup)",
				"agentId", req.AgentID,
				"existingBackendId", existingID,
				"host", host,
				"cppWorkerPort", req.CppWorkerPort)
			s.proxy.AttachAgentToBackend(existingID, req.AgentID, agentPort)

			// Удаляем stale standalone бэкенд с тем же agentId (если есть).
			// Это нужно когда агент ранее был зарегистрирован как standalone
			// (когда dedup ещё не было), а потом cppworker зарегистрировался
			// отдельно. Теперь агент attach'ится к правильному бэкенду и
			// старый standalone должен быть удалён, чтобы избежать дублей.
			if s.proxy.BackendExists(req.AgentID) {
				logger.Get().Infow("agentRegisterHandler: removing stale standalone backend with same agentId",
					"staleBackendId", req.AgentID,
					"attachedTo", existingID)
				if err := s.proxy.RemoveBackend(req.AgentID); err != nil {
					logger.Get().Warnw("agentRegisterHandler: failed to remove stale standalone backend",
						"staleBackendId", req.AgentID, "error", err)
					// Не блокируем attach — просто warning.
				}
			}

			existing := s.proxy.GetBackend(existingID)
			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"success":         true,
				"action":          "attached",
				"agentId":         req.AgentID,
				"backendId":       existingID,
				"backend":         existing,
				"hasAgent":        true,
				"message":         "Agent attached to existing cppworker backend (dedup).",
			})
			return
		}
	}

	// Проверка, существует ли уже бэкенд (legacy: ID-based lookup)
	if s.proxy.BackendExists(req.AgentID) {
		// Обновляем существующий
		existing := s.proxy.GetBackend(req.AgentID)
		updated := types.Backend{
			ID:                req.AgentID,
			Name:              req.Name,
			Host:              host,
			OllamaPort:        ollamaPort,
			AgentPort:         agentPort,
			CppWorkerPort:     req.CppWorkerPort,
			Weight:            weight,
			MaxConcurrentReqs: existing.MaxConcurrentReqs,
			Labels:            req.Labels,
			Status:            types.StatusHealthy,
			Type:              backendType,
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
		CppWorkerPort:     req.CppWorkerPort,
		Weight:            weight,
		MaxConcurrentReqs: 10,
		Labels:            req.Labels,
		Status:            types.StatusStarting,
		Type:              backendType,
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
		backend := s.proxy.GetBackend(req.AgentID)
		if backend == nil {
			return
		}

		// 2026-06-30: для llama_cpp-бэкенда проверяем cppworker /health,
		// а не Ollama /api/tags. Иначе agent-бэкенд, указывающий на
		// физический cppworker (host=cppworker-gpu:18092) и не имеющий
		// Ollama API на :11434, помечается как unhealthy сразу после
		// регистрации и "мигает" (пропадает/появляется) на странице GGUF.
		var healthURL string
		if backend.Type == types.BackendTypeLlamaCpp && backend.CppWorkerPort > 0 {
			healthURL = fmt.Sprintf("http://%s:%d/health", backend.Host, backend.CppWorkerPort)
		} else {
			healthURL = fmt.Sprintf("http://%s:%d/api/tags", backend.Host, backend.OllamaPort)
		}

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(healthURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			s.proxy.UpdateBackendStatus(req.AgentID, types.StatusHealthy)
			if resp != nil {
				resp.Body.Close()
			}
		} else {
			if err != nil {
				logger.Get().Errorw("agent health check unreachable",
					"agent", req.AgentID,
					"url", healthURL,
					"error", err,
				)
			} else if resp != nil {
				resp.Body.Close()
			}
			s.proxy.UpdateBackendStatus(req.AgentID, types.StatusUnhealthy)
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
		// RequestTimeout: приоритет — runtime-значение (адаптивный), затем статический
		if backend.RuntimeRequestTimeout != 0 {
			config["requestTimeout"] = backend.RuntimeRequestTimeout
		} else if backend.RequestTimeout != 0 {
			config["requestTimeout"] = backend.RequestTimeout
		}
	}

	// Формируем ответ
	response := map[string]interface{}{
		"status":       "ok",
		"serverTime":   now,
		"acknowledged": now,
		"config":       config,
	}

	// Добавляем ollamaConfig, если заданы желаемые runtime-флаги Ollama
	if backend != nil && backend.OllamaConfig != nil {
		response["ollamaConfig"] = map[string]interface{}{
			"numGpuLayers":    backend.OllamaConfig.NumGPULayers,
			"contextLength":   backend.OllamaConfig.ContextLength,
			"numParallel":     backend.OllamaConfig.NumParallel,
			"numThreads":      backend.OllamaConfig.NumThreads,
			"batchSize":       backend.OllamaConfig.BatchSize,
			"maxLoadedModels": backend.OllamaConfig.MaxLoadedModels,
			"flashAttention":  backend.OllamaConfig.FlashAttention,
			"kvCacheQuant":    backend.OllamaConfig.KVCacheQuant,
			"source":          "balancer",
		}
	}

	s.writeJSON(w, http.StatusOK, response)
}

// agentStatsHandler - получение статистики агентов
func (s *Server) agentStatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := s.proxy.GetAllBackends()

	// 2026-06-30: de-dup по (host, CppWorkerPort). Без этого на bundled-стеке
	// (cppworker + agent) в /api/v1/agents/stats видно 2 записи на один
	// физический контейнер: cppworker-gpu-bundled (без агента) и cppworker-gpu
	// (с агентом). В WebUI на вкладке Agents это выглядит как дубль.
	// preferAgent=true: оставляем запись с hasAgent=true, чтобы карточка
	// показывала реальные GPU/VRAM метрики.
	backends = dedupBackendsByHostPort(
		backends,
		func(b types.Backend) string { return b.Host },
		func(b types.Backend) int { return backendEffectivePort(b.OllamaPort, b.CppWorkerPort) },
		func(b types.Backend) bool { return b.HasAgent },
		true,
	)

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

// agentInfoHandler - получение информации о конкретном агенте и управление им
func (s *Server) agentInfoHandler(w http.ResponseWriter, r *http.Request) {
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

	// Подпуть /restart — прокси к агенту
	if len(parts) > 1 && parts[1] == "restart" {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Прокси-запрос к агенту
		agentURL := fmt.Sprintf("http://%s:%d/api/restart", backend.Host, backend.AgentPort)
		s.proxyAgentRequest(w, r, agentURL)
		return
	}

	// Подпуть /logs — прокси к агенту
	if len(parts) > 1 && parts[1] == "logs" {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		agentURL := fmt.Sprintf("http://%s:%d/api/logs", backend.Host, backend.AgentPort)
		s.proxyAgentRequest(w, r, agentURL)
		return
	}

	// Без подпути — возвращаем информацию о бэкенде/агенте
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.writeJSON(w, http.StatusOK, backend)
}

// proxyAgentRequest — проксирует HTTP-запрос к агенту и возвращает ответ клиенту
func (s *Server) proxyAgentRequest(w http.ResponseWriter, r *http.Request, targetURL string) {
	// Создаем новый запрос к агенту
	body := &bytes.Buffer{}
	if r.Body != nil {
		io.Copy(body, r.Body)
		r.Body.Close()
	}

	proxyReq, err := http.NewRequest(r.Method, targetURL, bytes.NewReader(body.Bytes()))
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Failed to create proxy request: %v", err),
		})
		return
	}

	// Копируем заголовки
	proxyReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	proxyReq.Header.Set("X-Agent-ID", r.Header.Get("X-Agent-ID"))

	// Выполняем запрос к агенту
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(proxyReq)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Agent unreachable: %v", err),
		})
		return
	}
	defer resp.Body.Close()

	// Копируем ответ агента клиенту
	respBody, _ := io.ReadAll(resp.Body)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}
// ============================================================
// Agent V2 handlers — metrics-only agent, no duplicate backend
// ============================================================

// agentV2RegisterHandler — регистрация агента v2 (без создания бэкенда).
// POST /api/v1/agents/v2/register
// Прикрепляет агента к существующему cppworker-бэкенду по backendID
// или находит бэкенд по (host, cppWorkerPort).
func (s *Server) agentV2RegisterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		AgentID       string `json:"agentId"`
		BackendID     string `json:"backendId"`     // ID существующего cppworker-бэкенда
		Host          string `json:"host"`           // host cppworker (если backendID не указан)
		CppWorkerPort int    `json:"cppWorkerPort"`  // порт cppworker
		AgentPort     int    `json:"agentPort"`      // порт агента
		Name          string `json:"name"`
		Labels        []string `json:"labels"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	if req.AgentID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "agentId is required",
		})
		return
	}

	host := req.Host
	if host == "" {
		if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			host = h
		}
	}

	// Находим целевой бэкенд
	backendID := req.BackendID
	if backendID == "" && req.CppWorkerPort > 0 {
		// Ищем бэкенд по (host, cppWorkerPort)
		backendID = s.proxy.FindBackendByHostPort(host, req.CppWorkerPort)
	}

	if backendID == "" {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   "No cppworker backend found. Register a cppworker backend first.",
		})
		return
	}

	// Прикрепляем агента к бэкенду (не создавая новый)
	s.proxy.AttachAgentToBackend(backendID, req.AgentID, req.AgentPort)

	logger.Get().Infow("agent v2 attached to backend",
		"agentID", req.AgentID,
		"backendID", backendID,
	)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"action":    "attached",
		"agentId":   req.AgentID,
		"backendId": backendID,
		"message":   "Agent v2 attached to existing backend.",
	})
}

// agentV2MetricsHandler — приём метрик от агента v2.
// POST /api/v1/agents/v2/metrics
func (s *Server) agentV2MetricsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "X-Agent-ID header is required",
		})
		return
	}

	var metrics types.BackendMetrics
	if err := json.NewDecoder(r.Body).Decode(&metrics); err != nil {
		http.Error(w, "Invalid metrics format", http.StatusBadRequest)
		return
	}

	// Ищем бэкенд, к которому прикреплён этот агент
	backendID := s.proxy.FindBackendByAgentID(agentID)
	if backendID == "" {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   "Agent not attached to any backend. Register first.",
		})
		return
	}

	s.proxy.UpdateMetrics(backendID, &metrics)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
	})
}

// agentV2HeartbeatHandler — heartbeat от агента v2.
// POST /api/v1/agents/v2/heartbeat
func (s *Server) agentV2HeartbeatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "X-Agent-ID header is required",
		})
		return
	}

	// Обновляем LastAgentContact для бэкенда агента
	backendID := s.proxy.FindBackendByAgentID(agentID)
	if backendID == "" {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   "Agent not attached to any backend.",
		})
		return
	}

	s.proxy.TouchAgentContact(backendID)

	// Возвращаем конфигурационные параметры
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":                true,
		"maxConcurrentRequests":  -1,
		"maxModels":              -1,
	})
}

// agentBackendHeartbeatHandler — heartbeat agent для конкретного backendID.
// В отличие от старого /agents/heartbeat (который использовал agentID как
// backendID, что было неправильно при attached режиме), здесь backendID
// приходит из path — это и есть canonical ID бэкенда.
//
// Валидация: X-Agent-ID должен совпадать с AgentID бэкенда (защита от
// подмены heartbeat от чужого агента).
func (s *Server) agentBackendHeartbeatHandler(w http.ResponseWriter, r *http.Request, backendID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		http.Error(w, "X-Agent-ID header required", http.StatusBadRequest)
		return
	}

	// Проверяем, что бэкенд существует
	backend := s.proxy.GetBackend(backendID)
	if backend == nil {
		http.Error(w, "backend not found", http.StatusNotFound)
		return
	}

	// Валидация: X-Agent-ID должен совпадать с прикреплённым AgentID бэкенда.
	// Если бэкенд был создан cppworker'ом (а не agent'ом), AgentID может быть пуст —
	// разрешаем attach от любого agent (первый attach становится владельцем).
	if backend.AgentID != "" && backend.AgentID != agentID {
		http.Error(w, "agent ID mismatch", http.StatusForbidden)
		return
	}

	// Читаем payload
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	var hbPayload struct {
		Weight                int    `json:"weight"`
		MaxConcurrentRequests int    `json:"maxConcurrentRequests"`
		MaxModels             int    `json:"maxModels"`
		Status                string `json:"status"`
		BackendType           string `json:"backendType"`
		// Round 14 (2026-07-10): agent шлёт свой agentPort чтобы cppworker-бэкенд
		// (который не знал про agent при создании) получил правильный port.
		// Раньше AttachAgentToBackend использовал backend.AgentPort=0, что было
		// бесполезно для UI/API.
		AgentPort             int    `json:"agentPort"`
	}
	_ = json.Unmarshal(body, &hbPayload)

	// Обновление статуса бэкенда
	s.proxy.UpdateBackendStatus(backendID, types.StatusHealthy)

	// Определяем agentPort: из payload (новый способ, agent шлёт свой порт)
	// или fallback на текущий backend.AgentPort. Это решает проблему "agentPort=0"
	// когда cppworker создал бэкенд без знания про agent port.
	effectiveAgentPort := hbPayload.AgentPort
	if effectiveAgentPort == 0 {
		effectiveAgentPort = backend.AgentPort
	}

	// Если X-Agent-ID не был прикреплён к бэкенду ранее — attach'им сейчас
	if backend.AgentID == "" {
		s.proxy.AttachAgentToBackend(backendID, agentID, effectiveAgentPort)
	} else {
		// Уже прикреплён — обновляем только LastAgentContact
		s.proxy.MarkAgentContact(backendID, agentID)
		// Если agentPort ещё 0 (cppworker не знал), обновляем с effective
		if effectiveAgentPort > 0 && backend.AgentPort == 0 {
			s.proxy.UpdateAgentPort(backendID, effectiveAgentPort)
		}
	}

	// Применяем runtime-лимиты из heartbeat
	updated := *backend
	needUpdate := false
	if hbPayload.Weight > 0 && updated.Weight != hbPayload.Weight {
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
		s.proxy.UpdateBackend(backendID, updated)
	}

	logger.Get().Debugw("agent heartbeat via backends/{id}/agent/heartbeat",
		"backend", backendID, "agent", agentID,
		"weight", hbPayload.Weight, "maxConcurrent", hbPayload.MaxConcurrentRequests)

	// Возвращаем runtime-лимиты от балансера
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"config": map[string]interface{}{
			"maxConcurrentRequests": -1, // не меняем
			"maxModels":             -1,
		},
	})
}

// agentBackendMetricsHandler — приём метрик от прикреплённого агента по правильному backendID.
// Round 15 (2026-07-10): в Round 12 dedup сделал 1 бэкенд на физический inference
// endpoint (cppworker-gpu-bundled). Agent ID стал отличаться от backend ID
// (cppworker-gpu-bundled-agent). Старый endpoint /api/v1/agents/metrics использовал
// agentID как ключ для BackendExists() → 404 "Backend not found" → метрики
// не доходили до балансера и WebUI показывал пустые GPU/VRAM/CPU/RAM.
//
// Новый endpoint /api/v1/backends/{backendID}/agent/metrics принимает backendID
// из path (тот самый что был получен при register через response.backendId).
func (s *Server) agentBackendMetricsHandler(w http.ResponseWriter, r *http.Request, backendID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	agentID := r.Header.Get("X-Agent-ID")
	if agentID == "" {
		http.Error(w, "X-Agent-ID header required", http.StatusBadRequest)
		return
	}

	// Парсим метрики
	var metrics types.BackendMetrics
	if err := json.NewDecoder(r.Body).Decode(&metrics); err != nil {
		http.Error(w, "Invalid metrics format", http.StatusBadRequest)
		return
	}

	// Проверяем что backend существует
	backend := s.proxy.GetBackend(backendID)
	if backend == nil {
		http.Error(w, "backend not found", http.StatusNotFound)
		return
	}

	// Валидация: X-Agent-ID должен совпадать с прикреплённым AgentID бэкенда
	// (защита от подмены — чужой агент не может слать метрики от имени чужого бэкенда).
	// При первом attach (backend.AgentID пуст) — allow, потом attach ниже.
	if backend.AgentID != "" && backend.AgentID != agentID {
		http.Error(w, "agent ID mismatch", http.StatusForbidden)
		return
	}

	// Auto-attach если ещё не прикреплён
	if backend.AgentID == "" {
		s.proxy.AttachAgentToBackend(backendID, agentID, backend.AgentPort)
	}

	// Обновляем метрики для правильного backendID
	s.proxy.UpdateMetrics(backendID, &metrics)

	// Обновляем флаг активного агента
	s.proxy.UpdateBackendAgentStatus(backendID, true)

	// Обновляем LastAgentContact (лёгкий путь — без race-condition)
	s.proxy.MarkAgentContact(backendID, agentID)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "received",
	})
}
