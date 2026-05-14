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
	mm := s.proxy.GetModelManager()
	if mm == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   "Model manager not available",
		})
		return
	}

	var req struct {
		Operation string `json:"operation"` // pull, push, delete, load, unload
		ModelName string `json:"modelName"`
		Insecure  bool   `json:"insecure,omitempty"`
		Stream    bool   `json:"stream,omitempty"`
	}
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

	opReq := balancer.ModelOpRequest{
		Operation: req.Operation,
		ModelName: req.ModelName,
		Insecure:  req.Insecure,
		Stream:    req.Stream,
	}

	result := mm.ExecuteOperation(backendID, opReq)

	statusCode := http.StatusOK
	if !result.Success {
		statusCode = http.StatusInternalServerError
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
