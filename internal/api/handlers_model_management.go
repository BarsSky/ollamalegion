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
//
// Порядок полей — как требует govet fieldalignment (enable-all в .golangci.yml):
// указатели, затем строки, затем слайсы, затем bool.
type backendModelOpRequest struct {
	// R66d (2026-09-23): расширенные параметры загрузки. Раньше здесь их не
	// было — WebUI отправлял их (loadOnSelectedBackend → manageModel), а
	// cppworker до них не доходил: значения сбрасывались при декодировании.
	// Ключи совпадают с balancer.ModelOpRequest и cppworker /api/models/load.
	KVCacheType         *string  `json:"kvCacheType,omitempty"` // f16 | q8_0 | q4_0
	ContextSize         *int     `json:"contextSize,omitempty"`
	GPULayers           *int     `json:"gpuLayers,omitempty"`
	Force               *bool    `json:"force,omitempty"`
	UseMmap             *bool    `json:"useMmap,omitempty"`
	FlashAttn           *int     `json:"flashAttn,omitempty"` // -1=auto, 0=off, 1=on
	BatchSize           *int     `json:"batchSize,omitempty"`
	ModelName           string   `json:"modelName"`
	Operation           string   `json:"operation"` // pull, push, delete, load, unload
	OverrideTensors     []string `json:"overrideTensors,omitempty"`
	OverrideTensorBufts []string `json:"overrideTensorBufts,omitempty"`
	Insecure            bool     `json:"insecure,omitempty"`
	Stream              bool     `json:"stream,omitempty"`
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

	// R257 (2026-10-28): сколько моделей РЕАЛЬНО загружено — отдельным полем.
	//
	// `total` — это модели на диске (в списке есть и незагруженные), поэтому
	// панель не могла отличить «10 моделей на диске» от «2 загружены». После
	// исправления флага Loaded (источник истины — снапшот поллера) в ответе нужен
	// и счётчик загруженных: по нему оператор видит, что память занята одной
	// моделью, а не двумя.
	loadedCount := 0
	for i := range models {
		if models[i].Loaded {
			loadedCount++
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":          true,
		"backend":          backendID,
		"models":           models,
		"total":            len(models),
		"loadedModelCount": loadedCount,
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

	statusCode := modelOpStatusCode(result)

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

// modelOpStatusCode — HTTP-статус по результату операции с моделью.
//
// R185 (2026-10-20): вынесено из обработчика, чтобы правило было проверяемым
// без поднятия всего сервера. Живой разбор: воркер собран не под ту карту, и
// клиент получал 500 на КАЖДЫЙ повтор — 5xx для клиентов значит «повтори,
// станет лучше», а архитектура образа от повтора не меняется. Отказ обязан быть
// 4xx, иначе балансер «бесконечно крутит», не показывая причину.
func modelOpStatusCode(result *balancer.ModelOpResult) int {
	if result == nil || result.Success {
		return http.StatusOK
	}
	// «Модель занята» — 409: клиенту нужно отличить «повтори с force=true».
	if result.Busy {
		return http.StatusConflict
	}
	if balancer.IsCUDAArchRefusalText(result.Error) {
		if balancer.IsLoadFitRefusalText(result.Error) {
			// R188: «модель не влезает в память бэкенда» — терминально, 4xx останавливает повторы.
			return http.StatusBadRequest
		}
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
