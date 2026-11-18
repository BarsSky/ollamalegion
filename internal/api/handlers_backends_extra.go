package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"ollama-loadbalancer/pkg/version"
	"strings"
	"time"
)

// measureBackendLink — R118.15: POST /api/v1/backends/{id}/measure-link.
func (s *Server) measureBackendLink(w http.ResponseWriter, r *http.Request, backendID string) {
	if s.proxy == nil {
		http.Error(w, `{"error":"proxy not available"}`, http.StatusInternalServerError)
		return
	}
	mbps, err := s.proxy.MeasureRPCLinkForBackend(backendID)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"success": false,
			"backend": backendID,
			"error":   err.Error(),
		})
		return
	}
	link := s.rpcLinkAutoInfo(backendID)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"backend":  backendID,
		"mbps":     mbps,
		"measured": link.MeasuredAt,
		"node":     link.Node,
		"source":   link.Source,
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
				"message":   i18n.T("api.handlers_backends.current_config_optimal"),
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

// applyWorkerIdentityR486 — запомнить релизную идентичность воркера (R486).
//
// Пустое значение НЕ стирает известное: сборки, которые этих полей ещё не знают
// (образы до R486), не должны обнулять уже полученную идентичность — иначе
// проверка «узел из того же релиза» мигала бы на каждом heartbeat.
func applyWorkerIdentityR486(b *types.Backend, version, commit, buildDate, rpcProto string) {
	if b == nil {
		return
	}
	if v := strings.TrimSpace(version); v != "" {
		b.WorkerVersion = v
	}
	if v := strings.TrimSpace(commit); v != "" {
		b.WorkerCommit = v
	}
	if v := strings.TrimSpace(buildDate); v != "" {
		b.WorkerBuildDate = v
	}
	if v := strings.TrimSpace(rpcProto); v != "" {
		b.WorkerRPCProto = v
	}
}

// applyAgentIdentityR493 — R493-12i: идентичность АГЕНТА и источник worker*-полей.
//
// Отдельно от applyWorkerIdentityR486, потому что это разные сущности: агент может
// быть собран другим коммитом, чем воркер, и раньше именно смешение давало
// «workerCommit не совпадает с образом». Пустое значение, как и там, не стирает
// известное; source, наоборот, перезаписывается — он описывает ТЕКУЩИЙ способ
// получения, а не историю.
func applyAgentIdentityR493(b *types.Backend, agentVersion, agentCommit, source string) {
	if b == nil {
		return
	}
	if v := strings.TrimSpace(agentVersion); v != "" {
		b.AgentVersion = v
	}
	if v := strings.TrimSpace(agentCommit); v != "" {
		b.AgentCommit = v
	}
	if v := strings.TrimSpace(source); v != "" {
		b.WorkerIdentitySource = v
	}
}

// workerReleaseMismatchR486 — собран ли узел из ДРУГОГО релиза, чем балансер.
//
// true только когда известны оба коммита и они различаются: пустой коммит узла
// (образ до R486) — это «неизвестно», а не «совпало» и не «разошлось».
func workerReleaseMismatchR486(b *types.Backend) bool {
	if b == nil || strings.TrimSpace(b.WorkerCommit) == "" {
		return false
	}
	own := strings.TrimSpace(version.Commit)
	if own == "" {
		return false
	}
	return !strings.EqualFold(own, strings.TrimSpace(b.WorkerCommit))
}
