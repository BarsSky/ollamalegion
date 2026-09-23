package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
)

// modelManageHandler — обрабатывает запросы к /api/v1/backends/{id}/models
// POST — выполнение операции с моделью (pull/push/delete/load/unload)
// GET — получение списка моделей на бэкенде
func (s *Server) modelManageHandler(w http.ResponseWriter, r *http.Request) {
	// Извлекаем ID бэкенда и подпуть
	// Путь: /api/v1/backends/{id}/models или /api/v1/backends/{id}/models/{modelName}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/backends/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] != "models" {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	backendID := parts[0]

	switch r.Method {
	case http.MethodGet:
		s.listBackendModels(w, r, backendID)
	case http.MethodPost:
		s.executeBackendModelOp(w, r, backendID)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// backendModelOpRequest — тело POST /api/v1/backends/{id}/models.
//
// R66d (2026-09-23): вынесено из executeBackendModelOp в тип уровня пакета,
// чтобы можно было протестировать маппинг JSON → balancer.ModelOpRequest.
//
// ЗАЧЕМ ТЕСТ: balancer.ModelOpRequest умеет BatchSize/FlashAttn/UseMmap/
// KVCacheType (их прокидывает executeLlamaCppLoad в cppworker), но HTTP-слой
// их НЕ декодировал — поля молча терялись. Из-за этого из WebUI нельзя было
// применить ни batch, ни flash_attn, ни mmap, ни kv-cache: форма «настройки
// модели» сохранялась, но при загрузке на cppworker уходили только
// contextSize/gpuLayers. Такой тест ловит именно этот класс расхождений
// (поле добавлено в ModelOpRequest, но не в HTTP-слой).
type backendModelOpRequest struct {
	Operation string `json:"operation"` // pull, push, delete, load, unload
	ModelName string `json:"modelName"`
	// Round 19 (2026-07-10): WebUI GGUF tab sends these fields when the user
	// sets gpuLayers/ctxSize/overrideTensors in the load dialog. Before this
	// change they were silently dropped here, then resolveOverrideTensors()
	// (which prefers explicit > profile > none) couldn't see them — only the
	// saved profile (which was a dead config block until Round 19 fix #2) was
	// consulted. For MoE models with override-tensors profiles this caused
	// /api/models/load to be used (no per-tensor routing) instead of
	// /api/models/load-with-params, which OOM'd the 21GB Qwen3-A3B on 24GB A10.
	ContextSize         *int     `json:"contextSize,omitempty"`
	GPULayers           *int     `json:"gpuLayers,omitempty"`
	OverrideTensors     []string `json:"overrideTensors,omitempty"`
	OverrideTensorBufts []string `json:"overrideTensorBufts,omitempty"`
	Insecure            bool     `json:"insecure,omitempty"`
	Stream              bool     `json:"stream,omitempty"`
	// Force — R66c (2026-09-22): выгрузка ЗАНЯТОЙ модели (cppworker
	// ?force=true). Без него unload модели с активными запросами
	// возвращал 409 и модель оставалась в памяти без выхода из UI.
	Force *bool `json:"force,omitempty"`
	// R66d (2026-09-23): расширенные параметры загрузки. Раньше здесь их не
	// было — WebUI отправлял их (loadOnSelectedBackend → manageModel), а
	// cppworker до них не доходил: значения сбрасывались при декодировании.
	// Ключи совпадают с balancer.ModelOpRequest и cppworker /api/models/load.
	KVCacheType *string `json:"kvCacheType,omitempty"` // f16 | q8_0 | q4_0
	UseMmap     *bool   `json:"useMmap,omitempty"`
	FlashAttn   *int    `json:"flashAttn,omitempty"` // -1=auto, 0=off, 1=on
	BatchSize   *int    `json:"batchSize,omitempty"`
}

// toModelOpRequest — маппинг тела HTTP-запроса в ModelOpRequest.
// Отдельная функция (а не инлайн-литерал), чтобы её покрывал unit-тест:
// каждое поле ModelOpRequest должно иметь соответствующий JSON-ключ выше.
func (req backendModelOpRequest) toModelOpRequest() balancer.ModelOpRequest {
	return balancer.ModelOpRequest{
		Operation:           req.Operation,
		ModelName:           req.ModelName,
		ContextSize:         req.ContextSize,
		GPULayers:           req.GPULayers,
		OverrideTensors:     req.OverrideTensors,
		OverrideTensorBufts: req.OverrideTensorBufts,
		Insecure:            req.Insecure,
		Stream:              req.Stream,
		Force:               req.Force,
		KVCacheType:         req.KVCacheType,
		UseMmap:             req.UseMmap,
		FlashAttn:           req.FlashAttn,
		BatchSize:           req.BatchSize,
	}
}

// listBackendModels — GET /api/v1/backends/{id}/models — получить список моделей на бэкенде
func (s *Server) listBackendModels(w http.ResponseWriter, r *http.Request, backendID string) {
	mm := s.proxy.GetModelManager()
	if mm == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   "Model manager not available",
		})
		return
	}

	models, err := mm.ListModels(backendID)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"backend":  backendID,
		"models":   models,
		"total":    len(models),
	})
}

// executeBackendModelOp — POST /api/v1/backends/{id}/models — выполнить операцию с моделью
func (s *Server) executeBackendModelOp(w http.ResponseWriter, r *http.Request, backendID string) {
	// R66d: load может занять минуты (холодная загрузка GGUF), а серверный
	// WriteTimeout=60s (cmd/balancer/main.go:383) обрывает ответ — клиент
	// получает HTTP 000 без тела и WebUI показывает "Load failed" при успешной
	// загрузке. Снимаем deadline на время операции.
	extendWriteDeadline(w)
	mm := s.proxy.GetModelManager()
	if mm == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   "Model manager not available",
		})
		return
	}

	var req backendModelOpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Invalid request body: %v", err),
		})
		return
	}

	// Валидация
	if req.Operation == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Operation is required (pull, push, delete, load, unload)",
		})
		return
	}
	if req.ModelName == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Model name is required",
		})
		return
	}

	validOps := map[string]bool{"pull": true, "push": true, "delete": true, "load": true, "unload": true}
	if !validOps[req.Operation] {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   fmt.Sprintf("Invalid operation '%s'. Valid: pull, push, delete, load, unload", req.Operation),
		})
		return
	}

	opReq := req.toModelOpRequest()

	result := mm.ExecuteOperation(backendID, opReq)

	statusCode := http.StatusOK
	if !result.Success {
		statusCode = http.StatusInternalServerError
	}
	// R66c: «модель занята» — это 409 Conflict, а не 500: клиенту нужно
	// отличить «повтори с force=true» от реальной ошибки сервера.
	if result.Busy {
		statusCode = http.StatusConflict
	}

	logger.Get().Infow("model operation result",
		"operation", req.Operation,
		"model", req.ModelName,
		"backend", backendID,
		"success", result.Success,
		"error", result.Error)

	s.writeJSON(w, statusCode, result)
}

// modelOpsStatusHandler — GET /api/v1/models/operations — статус активных операций
func (s *Server) modelOpsStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mm := s.proxy.GetModelManager()
	if mm == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   "Model manager not available",
		})
		return
	}

	ops := mm.GetActiveOps()
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":    true,
		"operations": ops,
	})
}
