package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// aggregateLoadedModel — плоское представление загруженной модели для cluster-уровня.
// Позволяет UI и пользовательским скриптам видеть, на каких бэкендах загружена модель
// и с какими параметрами — через один запрос к балансировщику (порт 18081).
type aggregateLoadedModel struct {
	Name          string `json:"name"`
	BackendID     string `json:"backendId"`
	BackendType   string `json:"backendType"`
	Engine        string `json:"engine,omitempty"`
	ContextLength int    `json:"contextLength,omitempty"`
	BatchSize     int    `json:"batchSize,omitempty"`
	NumGPULayers  int    `json:"numGpuLayers,omitempty"`
	Quantization  string `json:"quantization,omitempty"`
	VRAMUsage     uint64 `json:"vramUsage,omitempty"`
	RAMUsage      uint64 `json:"ramUsage,omitempty"`
	Size          uint64 `json:"size,omitempty"`
	State         string `json:"state,omitempty"`
}

// aggregateLoadingModel — модель, которая сейчас загружается/перезагружается.
type aggregateLoadingModel struct {
	Name             string  `json:"name"`
	BackendID        string  `json:"backendId"`
	StartedAt        string  `json:"startedAt,omitempty"` // RFC3339Nano, если задан
	ContextLength    int     `json:"contextLength,omitempty"`
	BatchSize        int     `json:"batchSize,omitempty"`
	NumGPULayers     int     `json:"numGpuLayers,omitempty"`
	Quantization     string  `json:"quantization,omitempty"`
	LoadingError     string  `json:"loadingError,omitempty"`
	LoadingSizeBytes int64   `json:"loadingSizeBytes,omitempty"`
}

// clusterLoadedModelsResponse — агрегированный список загруженных моделей по всему кластеру.
type clusterLoadedModelsResponse struct {
	Count           int                    `json:"count"`
	Models          []aggregateLoadedModel `json:"models"`
	ModelsPerBackend map[string]int        `json:"modelsPerBackend,omitempty"`
}

// clusterLoadingModelsResponse — список моделей в процессе загрузки.
type clusterLoadingModelsResponse struct {
	Count  int                    `json:"count"`
	Models []aggregateLoadingModel `json:"models"`
}

// clusterLoadedModelsHandler — GET /api/v1/cluster/models/loaded.
//
// Возвращает список всех загруженных моделей по всем бэкендам. Для llama.cpp (cppworker)
// бэкендов в ответ добавляются параметры загрузки (n_ctx, batch_size, gpu_layers,
// quantization, vram/ram usage). Это позволяет UI и скриптам пользователя проверять
// состояние моделей через один запрос к балансировщику (порт 18081), а не опрашивать
// каждый cppworker (порт 18092) отдельно.
//
// Требует аутентификации (X-API-Token).
func (s *Server) clusterLoadedModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "method not allowed",
		})
		return
	}

	cs := s.getClusterState()
	if cs == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "cluster state unavailable",
		})
		return
	}

	resp := clusterLoadedModelsResponse{
		Count:           0,
		Models:          []aggregateLoadedModel{},
		ModelsPerBackend: map[string]int{},
	}

	for _, b := range cs.Backends {
		if b.BackendType != types.BackendTypeLlamaCpp {
			continue
		}
		for _, m := range b.LlamaCpp.LoadedModels {
			entry := aggregateLoadedModel{
				Name:          m.Name,
				BackendID:     b.ID,
				BackendType:   string(b.BackendType),
				Engine:        string(b.Engine),
				ContextLength: m.ContextLength,
				BatchSize:     m.BatchSize,
				NumGPULayers:  m.NumGPULayers,
				Quantization:  m.Quantization,
				VRAMUsage:     m.VRAMUsage,
				RAMUsage:      m.RAMUsage,
				Size:          m.Size,
				State:         m.State,
			}
			resp.Models = append(resp.Models, entry)
			resp.Count++
			resp.ModelsPerBackend[b.ID]++
		}
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// clusterLoadingModelsHandler — GET /api/v1/cluster/models/loading.
//
// Возвращает список моделей, которые сейчас загружаются или перезагружаются
// (reload с новым n_ctx). Полезно для диагностики зависших загрузок — UI может
// показать, что модель уже несколько минут в статусе loading.
//
// Требует аутентификации.
func (s *Server) clusterLoadingModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "method not allowed",
		})
		return
	}

	cs := s.getClusterState()
	if cs == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "cluster state unavailable",
		})
		return
	}

	resp := clusterLoadingModelsResponse{
		Count:  0,
		Models: []aggregateLoadingModel{},
	}

	for _, b := range cs.Backends {
		if b.BackendType != types.BackendTypeLlamaCpp {
			continue
		}
		for _, m := range b.LlamaCpp.LoadingModels {
			entry := aggregateLoadingModel{
				Name:             m.Name,
				BackendID:        b.ID,
				ContextLength:    m.ContextLength,
				BatchSize:        m.BatchSize,
				NumGPULayers:     m.NumGPULayers,
				Quantization:     m.Quantization,
				LoadingError:     m.LoadingError,
				LoadingSizeBytes: m.LoadingSizeBytes,
			}
			if m.LoadingStartedAt != nil {
				entry.StartedAt = *m.LoadingStartedAt
			}
			resp.Models = append(resp.Models, entry)
			resp.Count++
		}
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// clusterReloadModelRequest — тело POST /api/v1/cluster/models/{name}/reload.
//
// Поддерживаемые операции (поле Operation):
//   - "load"   — загрузить модель (по умолчанию, если Operation пусто).
//   - "unload" — выгрузить модель.
//   - "reload" — выгрузить и затем загрузить с новыми параметрами
//                (синоним "all" для обратной совместимости с предыдущей версией API).
//
// Параметры загрузки:
//   - contextSize, gpuLayers — опциональные overrides (если не заданы,
//                              берутся из профиля модели в config.bundled.json).
//   - insecure, stream        — проксируются в ModelOpRequest.
type clusterReloadModelRequest struct {
	Operation   string `json:"operation,omitempty"`   // "load" | "unload" | "reload" (default: "load")
	BackendID   string `json:"backendId,omitempty"`   // "" = все llama_cpp бэкенды
	ContextSize *int   `json:"contextSize,omitempty"` // n_ctx override
	GPULayers   *int   `json:"gpuLayers,omitempty"`   // gpu_layers override (-1=auto)
	Insecure    bool   `json:"insecure,omitempty"`
	Stream      bool   `json:"stream,omitempty"`
	Reason      string `json:"reason,omitempty"` // комментарий для логов
}

// clusterReloadModelResponse — результат reload-операции на каждом бэкенде.
type clusterReloadModelResponse struct {
	Model     string                            `json:"model"`
	Operation string                            `json:"operation"`
	Results   []clusterReloadModelBackendResult `json:"results"`
}

// clusterReloadModelBackendResult — результат операции на одном бэкенде.
type clusterReloadModelBackendResult struct {
	BackendID  string `json:"backendId"`
	Status     string `json:"status"` // "ok" | "skipped" | "error" | "started"
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
	HTTPStatus int    `json:"httpStatus,omitempty"`
}

// clusterReloadModelHandler — POST /api/v1/cluster/models/{name}/reload.
//
// Управляет жизненным циклом модели через балансировщик: пользователь может
// загружать/выгружать/перезагружать модели, не обращаясь напрямую к cppworker
// (порт 18092). Особенно полезно, когда cppworker находится в Docker-network,
// недоступной из внешнего мира — все запросы идут через balancer (18080/18081).
//
// Формат запроса:
//
//	POST /api/v1/cluster/models/gemma-4-E4B-it-Q4_K_M/reload
//	{
//	  "operation":   "load",     // или "unload" / "reload"
//	  "contextSize": 32768,
//	  "gpuLayers":   -2,         // -2 = auto
//	  "reason":      "manual reload from UI"
//	}
//
// Endpoint проксирует запросы на cppworker через существующий
// /api/v1/backends/{id}/models (POST) handler через ModelManager.ExecuteOperation,
// который уже умеет делать load/unload и возвращать корректные статусы.
//
// Требует аутентификации.
func (s *Server) clusterReloadModelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "method not allowed",
		})
		return
	}

	// Извлекаем имя модели из URL: /api/v1/cluster/models/{name}/reload
	name, ok := splitClusterModelPath(r.URL.Path)
	if !ok || name == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing model name in URL (expected /api/v1/cluster/models/{name}/reload)",
		})
		return
	}

	var req clusterReloadModelRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid request body",
				"message": err.Error(),
			})
			return
		}
	}

	// По умолчанию operation = "load".
	if req.Operation == "" {
		req.Operation = "load"
	}

	// Валидация операции.
	switch req.Operation {
	case "load", "unload", "reload", "all": // "all" — обратная совместимость с предыдущей версией
		// OK
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid operation",
			"message": "supported operations: load, unload, reload",
		})
		return
	}

	// Семантический алиас: "all" / "reload" → unload + load.
	if req.Operation == "all" || req.Operation == "reload" {
		req.Operation = "reload"
	}

	// Собираем список целевых бэкендов.
	targets := s.selectReloadTargets(req.BackendID)
	if len(targets) == 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "no llama_cpp backends available",
			"message": "request a model reload requires at least one llama_cpp backend",
		})
		return
	}

	logger.Get().Infow("cluster model operation requested",
		"model", name,
		"operation", req.Operation,
		"backend_id", req.BackendID,
		"context_size", ctxSizeForLog(req.ContextSize),
		"gpu_layers", gpuLayersForLog(req.GPULayers),
		"reason", req.Reason,
	)

	resp := clusterReloadModelResponse{
		Model:     name,
		Operation: req.Operation,
		Results:   make([]clusterReloadModelBackendResult, 0, len(targets)),
	}

	for _, backendID := range targets {
		result := s.executeReloadOnBackend(backendID, name, &req)
		resp.Results = append(resp.Results, result)
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// splitClusterModelPath — извлекает имя модели из URL вида
// /api/v1/cluster/models/{name}/reload.
//
// Возвращает (name, true) если URL корректный.
func splitClusterModelPath(path string) (string, bool) {
	const prefix = "/api/v1/cluster/models/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" {
		return "", false
	}
	// rest = "{name}/reload" или "{name}/..." или просто "{name}"
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

// selectReloadTargets — выбирает ID бэкендов для reload-операции.
// Если backendId задан — возвращает только его (если llama_cpp).
// Если не задан — возвращает все llama_cpp бэкенды.
//
// Возвращает []string (backend IDs) — мы потом резолвим через ModelManager,
// который сам вызовет proxy.GetBackend().
func (s *Server) selectReloadTargets(backendID string) []string {
	cs := s.getClusterState()
	if cs == nil {
		return nil
	}

	if backendID != "" {
		for _, b := range cs.Backends {
			if b.ID == backendID && b.BackendType == types.BackendTypeLlamaCpp {
				return []string{backendID}
			}
		}
		return nil
	}

	targets := make([]string, 0, len(cs.Backends))
	for _, b := range cs.Backends {
		if b.BackendType == types.BackendTypeLlamaCpp {
			targets = append(targets, b.ID)
		}
	}
	return targets
}

// executeReloadOnBackend — выполняет операцию load/unload/reload на одном бэкенде
// через ModelManager.ExecuteOperation.
//
// Семантика operation:
//   - "load"   → одна операция load.
//   - "unload" → одна операция unload.
//   - "reload" → сначала unload (если модель загружена), затем load.
func (s *Server) executeReloadOnBackend(backendID, modelName string, req *clusterReloadModelRequest) clusterReloadModelBackendResult {
	result := clusterReloadModelBackendResult{BackendID: backendID}

	mm := s.proxy.GetModelManager()
	if mm == nil {
		result.Status = "error"
		result.Error = "model manager not available"
		result.HTTPStatus = http.StatusServiceUnavailable
		return result
	}

	// Для reload сначала unload, если модель загружена.
	if req.Operation == "reload" {
		loaded := s.isModelLoaded(backendID, modelName)
		if loaded {
			unloadOp := balancer.ModelOpRequest{
				Operation: "unload",
				ModelName: modelName,
			}
			unloadResult := mm.ExecuteOperation(backendID, unloadOp)
			if !unloadResult.Success {
				result.Status = "error"
				result.Error = "unload failed: " + unloadResult.Error
				result.HTTPStatus = http.StatusInternalServerError
				return result
			}
		}
		// После успешного unload (или если модель не была загружена) — выполняем load.
		req.Operation = "load"
	}

	opReq := balancer.ModelOpRequest{
		Operation:   req.Operation,
		ModelName:   modelName,
		ContextSize: req.ContextSize,
		GPULayers:   req.GPULayers,
		Insecure:    req.Insecure,
		Stream:      req.Stream,
	}

	opResult := mm.ExecuteOperation(backendID, opReq)

	result.HTTPStatus = http.StatusOK
	if !opResult.Success {
		result.HTTPStatus = http.StatusInternalServerError
		result.Status = "error"
		result.Error = opResult.Error
		if opResult.Message != "" {
			result.Message = opResult.Message
		}
		return result
	}

	result.Status = "ok"
	result.Message = opResult.Message
	return result
}

// isModelLoaded — проверяет, загружена ли модель на бэкенде (через cluster state).
func (s *Server) isModelLoaded(backendID, modelName string) bool {
	cs := s.getClusterState()
	if cs == nil {
		return false
	}
	for _, b := range cs.Backends {
		if b.ID != backendID {
			continue
		}
		for _, m := range b.LlamaCpp.LoadedModels {
			if m.Name == modelName {
				return true
			}
		}
	}
	return false
}

// getClusterState — единая точка получения cluster state с проверкой proxy.
func (s *Server) getClusterState() *types.ClusterState {
	if s.proxy == nil {
		return nil
	}
	return s.proxy.GetClusterState()
}

// ctxSizeForLog — возвращает значение *int для structured-логов (nil-safe).
func ctxSizeForLog(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

// gpuLayersForLog — возвращает значение *int для structured-логов (nil-safe).
func gpuLayersForLog(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}