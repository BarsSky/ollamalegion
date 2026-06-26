package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/rpccoordinator"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// =====================================================================
// Local helpers (используем локальные функции вместо методов Server.writeJSON,
// чтобы handlers_rpc был самодостаточным).
// =====================================================================

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSONResponse(w, status, map[string]interface{}{
		"error":   code,
		"message": message,
		"status":  status,
	})
}

func writeJSONResponse(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload != nil {
		_ = json.NewEncoder(w).Encode(payload)
	}
}

// =====================================================================
// B2 — RPC Management API endpoints
// =====================================================================
//
// Эти endpoints дают UI/скриптам единую точку управления RPC Coordinator'ом
// без необходимости прямого доступа к worker'ам:
//
//   GET    /api/v1/rpc/workers                  — список всех зарегистрированных worker'ов
//   POST   /api/v1/rpc/workers/register         — зарегистрировать или обновить worker
//   GET    /api/v1/rpc/workers/{id}             — инфо по worker'у
//   DELETE /api/v1/rpc/workers/{id}             — удалить worker
//   GET    /api/v1/rpc/models                   — список distributed моделей
//   POST   /api/v1/rpc/models/{name}/infer      — inference через coordinator
//
// Все endpoints требуют X-API-Token (через AuthMiddleware).

// handleRpcWorkersListOrRegister — диспетчер для /api/v1/rpc/workers.
// GET  — список
// POST — регистрация (алиас для /api/v1/rpc/workers/register).
func (s *Server) handleRpcWorkersListOrRegister(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleRpcWorkers_GET(w, r)
	case http.MethodPost:
		s.handleRpcWorkersRegister_POST(w, r)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"GET or POST required")
	}
}

// handleRpcWorkers_GET — GET /api/v1/rpc/workers.
func (s *Server) handleRpcWorkers_GET(w http.ResponseWriter, r *http.Request) {
	coord := s.proxy.GetRpcCoordinator()
	if coord == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "rpc_disabled",
			"RPC Coordinator is not enabled (set Balancing.RpcCoordinator.Enabled=true)")
		return
	}

	workers := coord.ListWorkers()
	out := make([]map[string]interface{}, 0, len(workers))
	for _, w := range workers {
		out = append(out, map[string]interface{}{
			"worker_id":    w.WorkerID,
			"host":         w.Host,
			"port":         w.Port,
			"slice_layers": w.SliceLayers,
		})
	}

	writeJSONResponse(w, http.StatusOK, map[string]interface{}{
		"count":   len(out),
		"workers": out,
	})
}

// handleRpcWorkersRegister_POST — POST /api/v1/rpc/workers/register.
func (s *Server) handleRpcWorkersRegister_POST(w http.ResponseWriter, r *http.Request) {
	coord := s.proxy.GetRpcCoordinator()
	if coord == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "rpc_disabled",
			"RPC Coordinator is not enabled")
		return
	}

	var req struct {
		WorkerID    string `json:"worker_id"`
		Host        string `json:"host"`
		Port        int    `json:"port"`
		SliceLayers string `json:"slice_layers,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	if req.WorkerID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "worker_id is required")
		return
	}
	if req.Host == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "host is required")
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		writeJSONError(w, http.StatusBadRequest, "invalid_port",
			"port must be 1..65535")
		return
	}

	if err := coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID:    req.WorkerID,
		Host:        req.Host,
		Port:        req.Port,
		SliceLayers: req.SliceLayers,
	}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "register_failed", err.Error())
		return
	}

	writeJSONResponse(w, http.StatusOK, map[string]interface{}{
		"status":    "registered",
		"worker_id": req.WorkerID,
	})
}

// handleRpcWorkerItem — GET/DELETE /api/v1/rpc/workers/{id}.
func (s *Server) handleRpcWorkerItem(w http.ResponseWriter, r *http.Request) {
	coord := s.proxy.GetRpcCoordinator()
	if coord == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "rpc_disabled",
			"RPC Coordinator is not enabled")
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/api/v1/rpc/workers/")
	id = strings.TrimSuffix(id, "/")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_id", "worker id required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		// Ищем среди зарегистрированных worker'ов.
		for _, wk := range coord.ListWorkers() {
			if wk.WorkerID == id {
				writeJSONResponse(w, http.StatusOK, map[string]interface{}{
					"worker_id":    wk.WorkerID,
					"host":         wk.Host,
					"port":         wk.Port,
					"slice_layers": wk.SliceLayers,
				})
				return
			}
		}
		writeJSONError(w, http.StatusNotFound, "not_found",
			"worker not found: "+id)

	case http.MethodDelete:
		coord.UnregisterWorker(id)
		writeJSONResponse(w, http.StatusOK, map[string]interface{}{
			"status":    "unregistered",
			"worker_id": id,
		})

	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"GET or DELETE required")
	}
}

// handleRpcModels_GET — GET /api/v1/rpc/models.
func (s *Server) handleRpcModels_GET(w http.ResponseWriter, r *http.Request) {
	coord := s.proxy.GetRpcCoordinator()
	if coord == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "rpc_disabled",
			"RPC Coordinator is not enabled")
		return
	}

	models := coord.ListDistributedModels()
	writeJSONResponse(w, http.StatusOK, map[string]interface{}{
		"count":  len(models),
		"models": models,
	})
}

// handleRpcModelInfer_POST — POST /api/v1/rpc/models/{name}/infer.
func (s *Server) handleRpcModelInfer_POST(w http.ResponseWriter, r *http.Request, modelName string) {
	coord := s.proxy.GetRpcCoordinator()
	if coord == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "rpc_disabled",
			"RPC Coordinator is not enabled")
		return
	}

	var req struct {
		Prompt    string            `json:"prompt"`
		SessionID string            `json:"session_id,omitempty"`
		Params    map[string]string `json:"params,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "prompt is required")
		return
	}

	// Inference через coordinator.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	resp, err := coord.Infer(ctx, rpccoordinator.InferRequest{
		ModelName: modelName,
		Prompt:    req.Prompt,
		SessionID: req.SessionID,
		Params:    req.Params,
	})
	if err != nil {
		// Если модель не зарегистрирована — 404; иначе 500.
		if !coord.HasDistributedModel(modelName) {
			writeJSONError(w, http.StatusNotFound, "model_not_found",
				"distributed model not registered: "+modelName)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "infer_failed", err.Error())
		return
	}

	out := map[string]interface{}{
		"model_name": modelName,
		"output":     resp.Output,
		"total_ms":   resp.TotalMs,
		"slice_stats": resp.SliceStats,
	}
	if resp.RequestID != "" {
		out["request_id"] = resp.RequestID
	}
	writeJSONResponse(w, http.StatusOK, out)
}

// handleRpcModelsRouter — общий роутер для /api/v1/rpc/models/.
//
// GET  /api/v1/rpc/models                  — список
// POST /api/v1/rpc/models/{name}/infer     — inference
func (s *Server) handleRpcModelsRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/rpc/models")
	path = strings.Trim(path, "/")

	if path == "" {
		if r.Method == http.MethodGet {
			s.handleRpcModels_GET(w, r)
			return
		}
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}

	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[1] != "infer" {
		writeJSONError(w, http.StatusNotFound, "not_found",
			"unknown endpoint: "+r.URL.Path)
		return
	}
	modelName := parts[0]
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"POST required")
		return
	}
	s.handleRpcModelInfer_POST(w, r, modelName)
}

func init() {
	logger.Get().Debug("handlers_rpc.go initialized (B2)")
}