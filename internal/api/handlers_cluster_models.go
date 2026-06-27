package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

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

// clusterModelItemDispatcher — единый dispatcher для /api/v1/cluster/models/{name}/*.
//
// Маршрутизирует по суффиксу URL:
//   - /{name}/info  → GET  → clusterModelInfoHandler  (см. handlers_cluster_model_info.go)
//   - /{name}/reload → POST → clusterReloadModelHandler (load/unload/reload)
//
// Без dispatcher'а оба handler'а конкурировали бы за один prefix-pattern
// "/api/v1/cluster/models/" в net/http ServeMux, и порядок регистрации
// определял бы, кто "съест" запрос. Этот dispatcher делает routing явным
// и устойчивым к добавлению новых sub-resources в будущем (например,
// /{name}/stats, /{name}/metrics).
func (s *Server) clusterModelItemDispatcher(w http.ResponseWriter, r *http.Request) {
	// Splitting on /info, /reload — оставляем общий splitClusterModelPath для {name}.
	// Конкретный суффикс определяем по последнему сегменту URL.
	const (
		suffixInfo   = "/info"
		suffixReload = "/reload"
	)
	path := r.URL.Path
	switch {
	case hasSuffix(path, suffixInfo):
		// GET /api/v1/cluster/models/{name}/info
		s.clusterModelInfoHandler(w, r)
	case hasSuffix(path, suffixReload):
		// POST /api/v1/cluster/models/{name}/reload
		s.clusterReloadModelHandler(w, r)
	default:
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "unknown cluster model sub-resource",
			"message": "supported: /api/v1/cluster/models/{name}/info (GET), /api/v1/cluster/models/{name}/reload (POST)",
			"path":    path,
		})
	}
}

// hasSuffix — строковый helper, аналог strings.HasSuffix (избегаем импорта strings здесь).
func hasSuffix(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
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

// ===== Bulk operations (Session A — Q3 W4) =====

// bulkOperationMaxModels — максимальное число моделей в одном bulk-запросе.
// Лимит защищает cppworker от одновременной обработки слишком большого пакета:
// на больших кластерах (50+ моделей) единичный запрос может занять десятки секунд,
// а UI должен иметь возможность отменить/обновить состояние.
const bulkOperationMaxModels = 100

// bulkOperationConcurrency — максимальное число параллельных операций внутри
// одного backend'а (на одну модель). Используется только при >50 моделях.
// На практике cppworker сам сериализует load/unload через tryAcquireOp, поэтому
// распараллеливание по бэкендам безопасно.
const bulkOperationConcurrency = 4

// bulkModelItem — одна модель в bulk-запросе.
type bulkModelItem struct {
	Model       string `json:"model"`                 // имя модели (обязательно)
	Operation   string `json:"operation,omitempty"`   // "load" | "unload" | "reload" (default: req.Operation)
	BackendID   string `json:"backendId,omitempty"`   // override глобального BackendID
	ContextSize *int   `json:"contextSize,omitempty"` // override глобального ContextSize
	GPULayers   *int   `json:"gpuLayers,omitempty"`   // override глобального GPULayers
	Reason      string `json:"reason,omitempty"`      // комментарий для логов
}

// clusterBulkModelsRequest — тело POST /api/v1/cluster/models/bulk.
//
// Поддерживает два режима:
//   1. **Явный список моделей** (Models != nil && len > 0) — для multi-select UI
//      (Load/Unload/Delete Selected). Каждый элемент может переопределить
//      BackendID/ContextSize/GPULayers глобально или оставить их пустыми.
//   2. **Операция без списка** (Models == nil или пустой массив) — для командных
//      сценариев типа "Load All Available" / "Unload All Loaded". В этом случае:
//      - "unload" без списка → выгружаются все загруженные модели из cluster state.
//      - "load" без списка → 400 (опасно: нет списка моделей для загрузки).
//
// Приоритет параметров (от высшего к низшему):
//   1. Per-model override в BulkOperation.
//   2. Глобальный BackendID/ContextSize/GPULayers/Operation в запросе.
//   3. Значения по умолчанию ("load", n_ctx из профиля, gpu_layers=-1=auto).
type clusterBulkModelsRequest struct {
	Operation   string         `json:"operation,omitempty"`   // "load" | "unload" | "reload" (default: "load")
	Models      []bulkModelItem `json:"models"`              // список моделей (опционально для unload)
	BackendID   string         `json:"backendId,omitempty"`  // глобальный backendId override
	ContextSize *int           `json:"contextSize,omitempty"` // глобальный n_ctx override
	GPULayers   *int           `json:"gpuLayers,omitempty"`  // глобальный gpu_layers override (-1=auto)
	Insecure    bool           `json:"insecure,omitempty"`
	Stream      bool           `json:"stream,omitempty"`
	Reason      string         `json:"reason,omitempty"`
}

// clusterBulkModelsResponse — агрегированный результат bulk-операции по всем моделям.
//
// HTTP-статус всегда 200, кроме критичных ошибок (cluster state unavailable,
// невалидный JSON, превышение лимита моделей). Per-model и per-backend ошибки
// не ломают общий ответ — это согласуется с семантикой cluster endpoints из Session 17.
type clusterBulkModelsResponse struct {
	Operation  string                   `json:"operation"`  // фактически применённая операция
	Total      int                      `json:"total"`      // всего моделей в запросе
	Succeeded  int                      `json:"succeeded"`  // успешно выполнено (все бэкенды status=ok)
	Failed     int                      `json:"failed"`     // хотя бы один бэкенд ответил error
	Results    []clusterBulkModelResult `json:"results"`    // детали по каждой модели
	StartedAt  string                   `json:"startedAt"`  // RFC3339Nano начала выполнения
	DurationMs int64                    `json:"durationMs"` // общее время выполнения
}

// clusterBulkModelResult — результат одной модели из bulk-запроса.
type clusterBulkModelResult struct {
	Model     string                            `json:"model"`
	Operation string                            `json:"operation"`   // применённая операция
	Succeeded bool                              `json:"succeeded"`   // все бэкенды status=ok
	Results   []clusterReloadModelBackendResult `json:"results"`     // детали per-backend (переиспользуем Session 17)
}

// clusterBulkModelsHandler — POST /api/v1/cluster/models/bulk.
//
// Массовая операция над списком моделей через балансировщик. Переиспользует
// executeReloadOnBackend (Session 17) для per-backend выполнения, добавляя только
// агрегацию результатов и контроль concurrency при больших списках.
//
// Семантика response (как у других cluster endpoints):
//   - HTTP 200 + per-model results[] даже если все операции упали.
//   - HTTP 4xx только для критичных ошибок (validation).
//   - HTTP 5xx только при недоступности cluster state / model manager.
//
// Требует аутентификации (X-API-Token).
func (s *Server) clusterBulkModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "method not allowed",
		})
		return
	}

	var req clusterBulkModelsRequest
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
	case "load", "unload", "reload", "all":
		// OK
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid operation",
			"message": "supported operations: load, unload, reload",
		})
		return
	}

	// Семантический алиас: "all" → "reload".
	if req.Operation == "all" {
		req.Operation = "reload"
	}

	// Проверяем доступность cluster state.
	if s.getClusterState() == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "cluster state unavailable",
		})
		return
	}

	// Резолвим список моделей: явный список или derived (для unload без списка).
	models := req.Models
	if len(models) == 0 {
		// "unload" без списка → берём все загруженные модели из cluster state.
		// Это позволяет UI реализовать "Unload All" одной кнопкой.
		if req.Operation == "unload" {
			models = s.collectAllLoadedModels(req.BackendID)
		} else {
			// "load" / "reload" без списка — опасно, нужен явный список моделей.
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "models list required for operation",
				"message": "provide a non-empty 'models' array or use operation='unload' to unload all loaded",
			})
			return
		}
	}

	// Проверка лимита на количество моделей (защита от перегрузки cppworker).
	if len(models) > bulkOperationMaxModels {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "too many models in bulk request",
			"message": "max " + intToString(bulkOperationMaxModels) + " models per request",
		})
		return
	}

	// Проверяем что есть llama_cpp бэкенды для выполнения операции.
	targets := s.selectReloadTargets(req.BackendID)
	if len(targets) == 0 {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "no llama_cpp backends available",
			"message": "bulk operation requires at least one llama_cpp backend",
		})
		return
	}

	startedAt := time.Now().UTC()
	startedAtRFC := startedAt.Format(time.RFC3339Nano)

	logger.Get().Infow("cluster bulk operation requested",
		"operation", req.Operation,
		"models_count", len(models),
		"backend_id", req.BackendID,
		"context_size", ctxSizeForLog(req.ContextSize),
		"gpu_layers", gpuLayersForLog(req.GPULayers),
		"reason", req.Reason,
	)

	resp := clusterBulkModelsResponse{
		Operation: req.Operation,
		Total:     len(models),
		Results:   make([]clusterBulkModelResult, 0, len(models)),
	}

	// Выполняем операции. Для малых списков (<=50) — последовательно,
	// для больших (>50) — параллельно с ограничением concurrency.
	if len(models) <= bulkOperationConcurrency*2 {
		for i := range models {
			result := s.executeBulkModelItem(targets, &models[i], &req)
			resp.Results = append(resp.Results, result)
			if result.Succeeded {
				resp.Succeeded++
			} else {
				resp.Failed++
			}
		}
	} else {
		resp.Results = s.executeBulkModelItemsParallel(targets, models, &req, &resp)
	}

	resp.DurationMs = time.Since(startedAt).Milliseconds()
	resp.StartedAt = startedAtRFC

	logger.Get().Infow("cluster bulk operation completed",
		"operation", req.Operation,
		"total", resp.Total,
		"succeeded", resp.Succeeded,
		"failed", resp.Failed,
		"duration_ms", resp.DurationMs,
	)

	s.writeJSON(w, http.StatusOK, resp)
}

// executeBulkModelItem — выполняет одну модель из bulk-запроса на всех целевых бэкендах.
// Переиспользует executeReloadOnBackend (Session 17), собирая результаты в clusterBulkModelResult.
func (s *Server) executeBulkModelItem(targets []string, item *bulkModelItem, req *clusterBulkModelsRequest) clusterBulkModelResult {
	// Резолвим per-item операцию: приоритет item.Operation > req.Operation.
	op := req.Operation
	if item.Operation != "" {
		op = item.Operation
	}
	// Нормализация "all" → "reload".
	if op == "all" {
		op = "reload"
	}

	// Резолвим per-item backendId.
	backendID := req.BackendID
	if item.BackendID != "" {
		backendID = item.BackendID
	}

	// Резолвим per-item contextSize / gpuLayers.
	contextSize := req.ContextSize
	if item.ContextSize != nil {
		contextSize = item.ContextSize
	}
	gpuLayers := req.GPULayers
	if item.GPULayers != nil {
		gpuLayers = item.GPULayers
	}

	// Резолвим per-item reason.
	reason := req.Reason
	if item.Reason != "" {
		reason = item.Reason
	}

	// Строим clusterReloadModelRequest для executeReloadOnBackend.
	perBackendReq := &clusterReloadModelRequest{
		Operation:   op,
		BackendID:   backendID,
		ContextSize: contextSize,
		GPULayers:   gpuLayers,
		Insecure:    req.Insecure,
		Stream:      req.Stream,
		Reason:      reason,
	}

	// Резолвим фактические targets для этой модели.
	modelTargets := s.selectReloadTargets(backendID)
	if len(modelTargets) == 0 {
		// Нет доступных бэкендов для этой модели → результат с ошибкой.
		return clusterBulkModelResult{
			Model:     item.Model,
			Operation: op,
			Succeeded: false,
			Results: []clusterReloadModelBackendResult{{
				BackendID:  backendID,
				Status:     "error",
				Error:      "no llama_cpp backends available for backendId='" + backendID + "'",
				HTTPStatus: http.StatusNotFound,
			}},
		}
	}

	results := make([]clusterReloadModelBackendResult, 0, len(modelTargets))
	allOK := true
	for _, bid := range modelTargets {
		r := s.executeReloadOnBackend(bid, item.Model, perBackendReq)
		if r.Status != "ok" {
			allOK = false
		}
		results = append(results, r)
	}

	return clusterBulkModelResult{
		Model:     item.Model,
		Operation: op,
		Succeeded: allOK,
		Results:   results,
	}
}

// executeBulkModelItemsParallel — параллельное выполнение bulk-операций с лимитом concurrency.
// Используется только для больших списков (>10 моделей), чтобы не блокировать UI.
func (s *Server) executeBulkModelItemsParallel(targets []string, models []bulkModelItem, req *clusterBulkModelsRequest, resp *clusterBulkModelsResponse) []clusterBulkModelResult {
	results := make([]clusterBulkModelResult, len(models))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, bulkOperationConcurrency)

	for i := range models {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			result := s.executeBulkModelItem(targets, &models[idx], req)
			results[idx] = result
			mu.Lock()
			if result.Succeeded {
				resp.Succeeded++
			} else {
				resp.Failed++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	return results
}

// collectAllLoadedModels — собирает список загруженных моделей из cluster state
// для операции "unload без явного списка моделей".
//
// Возвращает []bulkModelItem с уникальными именами моделей (если модель загружена
// на нескольких бэкендах, она попадёт в список один раз — executeReloadOnBackend
// сам обработает все бэкенды для каждого имени).
//
// Если backendID задан, фильтрует только модели с этого бэкенда.
func (s *Server) collectAllLoadedModels(backendID string) []bulkModelItem {
	cs := s.getClusterState()
	if cs == nil {
		return nil
	}

	seen := make(map[string]bool)
	result := make([]bulkModelItem, 0)

	for _, b := range cs.Backends {
		if b.BackendType != types.BackendTypeLlamaCpp {
			continue
		}
		if backendID != "" && b.ID != backendID {
			continue
		}
		for _, m := range b.LlamaCpp.LoadedModels {
			if seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			result = append(result, bulkModelItem{
				Model:     m.Name,
				Operation: "unload",
			})
		}
	}

	return result
}

// intToString — простой helper для сообщений об ошибках (избегаем strconv импорта в этом файле).
func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
