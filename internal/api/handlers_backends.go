package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Backends API handlers
// ============================================================

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

// backendHandler - управление конкретным бэкендом (включая подпути /limits, /capacity, /models)
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

	// Подпуть /evacuate — плавная эвакуация бэкенда
	if len(parts) > 1 && parts[1] == "evacuate" {
		if r.Method == http.MethodPost {
			s.evacuateBackend(w, r, backendID)
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Подпуть /models — управление моделями на бэкенде (GET — список, POST — операция)
	if len(parts) > 1 && parts[1] == "models" {
		s.modelManageHandler(w, r)
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

// listBackends - получение списка бэкендов
func (s *Server) listBackends(w http.ResponseWriter, r *http.Request) {
	backends := make([]map[string]interface{}, 0)

	// По умолчанию скрываем нерабочие бэкенды, чтобы в WebUI не отображались
	// заглушки/недоступные ноды. Параметр includeUnhealthy=true возвращает всё.
	includeUnhealthy := r.URL.Query().Get("includeUnhealthy") == "true"
	unhealthyStatuses := map[types.BackendStatus]bool{
		types.StatusUnhealthy:         true,
		types.StatusOffline:           true,
		types.StatusDraining:          true,
		types.StatusOllamaUnavailable: true,
	}

	// Получение всех бэкендов из прокси
	allBackends := s.proxy.GetAllBackends()

	// Сортировка по ID для стабильного порядка
	sort.Slice(allBackends, func(i, j int) bool {
		return allBackends[i].ID < allBackends[j].ID
	})

	// 2026-06-30: de-dup по (host, CppWorkerPort). Без этого на bundled-стеке
	// (cppworker + agent) в /api/v1/backends видно 2-3 записи на один
	// физический контейнер (cppworker-gpu-bundled + cppworker-gpu от агента).
	// На вкладке Backends WebUI это выглядит как «один и тот же бэкенд дважды»,
	// а в selectBackend вызывает race. См. internal/api/dedup.go.
	allBackends = dedupBackendsByHostPort(
		allBackends,
		func(b types.Backend) string { return b.Host },
		func(b types.Backend) int { return backendEffectivePort(b.OllamaPort, b.CppWorkerPort) },
		func(b types.Backend) bool { return b.HasAgent },
		true, // preferAgent: бэкенд с агентом даёт реальные GPU/VRAM метрики
	)

	// Получение метрик из состояния кластера
	state := s.proxy.GetClusterState()

	// Создание карты метрик для быстрого доступа
	metricsMap := make(map[string]*types.BackendMetrics)
	for i := range state.Backends {
		metricsMap[state.Backends[i].ID] = &state.Backends[i]
	}

	// Формирование ответа для каждого бэкенда
	for _, backend := range allBackends {
		// Фильтруем нерабочие бэкенды, если не запрошено явное включение.
		if !includeUnhealthy && unhealthyStatuses[backend.Status] {
			continue
		}
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
			"id":                           backend.ID,
			"name":                         backend.Name,
			"host":                         backend.Host,
			"ollamaPort":                   backend.OllamaPort,
			"agentPort":                    backend.AgentPort,
			"cppWorkerPort":                backend.CppWorkerPort,
			"weight":                       backend.Weight,
			"maxConcurrentRequests":        maxConcurrent,
			"maxModels":                    maxModels,
			"runtimeMaxModels":             backend.RuntimeMaxModels,
			"runtimeMaxConcurrentRequests": backend.RuntimeMaxConcurrentRequests,
			"labels":                       backend.Labels,
			"status":                       backend.Status,
			"type":                         backend.Type,
			"engine":                       backend.Engine,
		}

		// Добавление метрик если они доступны
		if metrics, ok := metricsMap[backend.ID]; ok {
			backendData["timestamp"] = metrics.Timestamp
			backendData["gpu"] = metrics.GPU
			backendData["system"] = metrics.System
			backendData["prediction"] = metrics.Prediction

			// === Шаг «отображение загрузки в мониторе и вкладке бэкендов» ===
			// Если бэкенд — llama_cpp, передаём loadingModels напрямую из LlamaCppMetrics
			// (UI использует это для отображения спиннера и elapsed-времени).
			if backend.Type == types.BackendTypeLlamaCpp {
				if lm := s.proxy.GetMetricsManager().GetLlamaCppMetrics(backend.ID); lm != nil {
					loading := lm.LoadingModels
					if loading == nil {
						loading = []types.LlamaCppModel{}
					}
					backendData["loadingModels"] = loading
					backendData["loadingModelCount"] = len(loading)
				} else {
					backendData["loadingModels"] = []types.LlamaCppModel{}
					backendData["loadingModelCount"] = 0
				}
			}

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
		CppWorkerPort     int      `json:"cppWorkerPort"`
		Weight            int      `json:"weight"`
		MaxConcurrentReqs int      `json:"maxConcurrentRequests"`
		MaxModels         int      `json:"maxModels"`
		GPUMode           string   `json:"gpuMode"`
		Labels            []string `json:"labels"`
		BackendType       string   `json:"backendType"`
		BackendEngine     string   `json:"backendEngine"`
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

	// Нормализация типа бэкенда.
	// Приоритет: req.BackendType > req.BackendEngine > s.config.BackendEngine > ollama
	backendType := types.BackendType(req.BackendType)
	if backendType == "" {
		// Пытаемся определить из BackendEngine в запросе (клиент мог прислать)
		if req.BackendEngine != "" {
			backendType = types.BackendEngine(req.BackendEngine).ToBackendType()
		}
		// Если клиент не прислал — берём из глобального конфига сервера
		if backendType == "" && s.config.BackendEngine != "" {
			backendType = s.config.BackendEngine.ToBackendType()
		}
		// Последний fallback — ollama (обратная совместимость)
		if backendType == "" {
			backendType = types.BackendTypeOllama
		}
	}

	// Валидация типа бэкенда
	if backendType != types.BackendTypeOllama && backendType != types.BackendTypeLlamaCpp {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Invalid backend type: %s. Allowed: ollama, llama_cpp", req.BackendType),
		})
		return
	}

	// Проверка совместимости типа бэкенда с OperatingMode
	opMode := s.config.Balancing.OperatingMode
	if !types.IsModeCompatibleWithBackendType(opMode, backendType) {
		allowedTypes := types.ModeBackendTypes[opMode]
		allowedStr := ""
		for i, t := range allowedTypes {
			if i > 0 {
				allowedStr += ", "
			}
			allowedStr += string(t)
		}
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend type '%s' is not allowed in operating mode '%s'. Allowed types: [%s]", backendType, opMode, allowedStr),
		})
		return
	}

	// Парсинг GPU mode
	gpuMode := types.ModeAuto
	switch req.GPUMode {
	case "gpu":
		gpuMode = types.ModeGPU
	case "cpu":
		gpuMode = types.ModeCPU
	}

	// Если CppWorkerPort не задан явно, используем актуальный default 18092
	// (18091 — legacy; современные llama.cpp CppWorker слушают на 18092).
	cppWorkerPort := req.CppWorkerPort
	if cppWorkerPort == 0 && backendType == types.BackendTypeLlamaCpp {
		cppWorkerPort = 18092
	}

	backend := types.Backend{
		ID:                req.ID,
		Name:              req.Name,
		Host:              req.Host,
		OllamaPort:        req.OllamaPort,
		AgentPort:         req.AgentPort,
		CppWorkerPort:     cppWorkerPort,
		Weight:            req.Weight,
		MaxConcurrentReqs: req.MaxConcurrentReqs,
		MaxModels:         req.MaxModels,
		Labels:            req.Labels,
		Status:            types.StatusStarting,
		GPUMode:           gpuMode,
		Type:              backendType,
		Engine:            types.ResolveEngine(types.BackendEngine(req.BackendEngine), backendType),
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
		backend := s.proxy.GetBackend(req.ID)
		if backend != nil {
			var healthURL string
			if backendType == types.BackendTypeLlamaCpp {
				healthURL = fmt.Sprintf("http://%s:%d/health", backend.Host, cppWorkerPort)
			} else {
				healthURL = fmt.Sprintf("http://%s:%d/api/tags", backend.Host, backend.OllamaPort)
			}
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Get(healthURL)
			if err == nil && resp.StatusCode == http.StatusOK {
				s.proxy.UpdateBackendStatus(req.ID, types.StatusHealthy)
				resp.Body.Close()
			} else {
				if err != nil {
					logger.Get().Errorw("backend health check unreachable",
						"backend", req.ID,
						"url", healthURL,
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
		CppWorkerPort     int      `json:"cppWorkerPort"`
		Weight            int      `json:"weight"`
		MaxConcurrentReqs int      `json:"maxConcurrentRequests"`
		MaxModels         int      `json:"maxModels"`
		GPUMode           string   `json:"gpuMode"`
		Labels            []string `json:"labels"`
		BackendType       string   `json:"backendType"`
		BackendEngine     string   `json:"backendEngine"`
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

	// Парсинг GPU mode
	gpuMode := existing.GPUMode
	switch req.GPUMode {
	case "gpu":
		gpuMode = types.ModeGPU
	case "cpu":
		gpuMode = types.ModeCPU
	case "auto":
		gpuMode = types.ModeAuto
	}

	// Нормализация типа бэкенда
	backendType := existing.Type
	if req.BackendType != "" {
		newType := types.BackendType(req.BackendType)
		if newType != existing.Type {
			// Проверка совместимости с OperatingMode
			if !types.IsModeCompatibleWithBackendType(s.config.Balancing.OperatingMode, newType) {
				allowedTypes := types.ModeBackendTypes[s.config.Balancing.OperatingMode]
				allowedStr := ""
				for i, t := range allowedTypes {
					if i > 0 {
						allowedStr += ", "
					}
					allowedStr += string(t)
				}
				s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"error":   fmt.Sprintf("Backend type '%s' is not allowed in operating mode '%s'. Allowed types: [%s]", newType, s.config.Balancing.OperatingMode, allowedStr),
				})
				return
			}
			// Запрет смены типа при активных запросах
			if existing.ActiveRequests > 0 {
				s.writeJSON(w, http.StatusConflict, map[string]interface{}{
					"success": false,
					"error":   fmt.Sprintf("Cannot change backend type from '%s' to '%s': backend has %d active requests. Wait for requests to complete or drain the backend first.", existing.Type, newType, existing.ActiveRequests),
				})
				return
			}
			logger.Get().Warnw("backend type changed via API",
				"backend", backendID,
				"oldType", existing.Type,
				"newType", newType,
			)
		}
		backendType = newType
	}

	// Применяем CppWorkerPort если передан, иначе сохраняем существующий
	cppWorkerPort := existing.CppWorkerPort
	if req.CppWorkerPort > 0 {
		cppWorkerPort = req.CppWorkerPort
	}

	updated := types.Backend{
		ID:                           backendID,
		Name:                         req.Name,
		Host:                         req.Host,
		OllamaPort:                   req.OllamaPort,
		AgentPort:                    req.AgentPort,
		CppWorkerPort:                cppWorkerPort,
		Weight:                       req.Weight,
		MaxConcurrentReqs:            req.MaxConcurrentReqs,
		MaxModels:                    req.MaxModels,
		Labels:                       req.Labels,
		Status:                       existing.Status,
		HasAgent:                     existing.HasAgent,
		LastAgentContact:             existing.LastAgentContact,
		LastHealthCheck:              existing.LastHealthCheck,
		ConsecutiveFailures:          existing.ConsecutiveFailures,
		ActiveRequests:               existing.ActiveRequests,
		RuntimeMaxModels:             existing.RuntimeMaxModels,
		RuntimeMaxConcurrentRequests: existing.RuntimeMaxConcurrentRequests,
		GPUMode:                      gpuMode,
		Type:                         backendType,
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
		"success":   true,
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
		"timestamp":     time.Now().UTC(),
		"totalBackends": len(state.Backends),
		"totalLoadable": totalLoadable,
		"backends":      summaries,
	})
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
			if metrics.GPU.MemoryTotal > 0 {
				gpuCount = 1
			}
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

	// Публикуем событие переформирования
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

// evacuateBackend — плавная эвакуация бэкенда: переключает сессии на другие бэкенды.
// POST /api/v1/backends/{backendId}/evacuate
func (s *Server) evacuateBackend(w http.ResponseWriter, r *http.Request, backendID string) {
	if !s.proxy.BackendExists(backendID) {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Backend with ID %s not found", backendID),
		})
		return
	}

	count, err := s.proxy.EvacuateBackend(backendID)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":           true,
		"backendId":         backendID,
		"evacuatedSessions": count,
		"message":           fmt.Sprintf("Backend %s evacuated: %d sessions reassigned", backendID, count),
	})
}
