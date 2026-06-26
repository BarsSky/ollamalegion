package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/internal/rptensor"
	"ollama-loadbalancer/pkg/logger"
)

// =====================================================================
// B8.8 — TP (tensor parallelism) RPC API endpoints
// =====================================================================
//
// Доступ через балансировщик (порт 18081):
//
//   POST /api/v1/rpc/tp/infer   — выполнить TP inference через coordinator.
//   GET  /api/v1/rpc/tp/status  — статус TP coordinator'а (enabled, worldSize, ranks).
//
// Все endpoints требуют X-API-Token (через AuthMiddleware).
//
// B8.8 stub: использует StubTPRuntime (см. internal/rptensor/tp_runtime.go).
// Реальная llama.cpp/ggml-интеграция — отдельная фаза (post-1.0).

// tpCoordinatorRegistry — глобальный registry TP-coordinator'ов по model_name.
//
// В production этот registry управляется balancer'ом через конфиг,
// для тестов — через RegisterTPCoordinator / UnregisterTPCoordinator.
var (
	tpRegistryMu sync.RWMutex
	tpRegistry   = make(map[string]*rptensor.TensorParallelCoordinator)
)

// RegisterTPCoordinator — регистрирует TP coordinator для модели.
//
// Используется:
//   - balancer'ом при старте (из config);
//   - тестами (e2e pipeline).
func RegisterTPCoordinator(modelName string, coord *rptensor.TensorParallelCoordinator) {
	tpRegistryMu.Lock()
	defer tpRegistryMu.Unlock()
	if coord == nil {
		delete(tpRegistry, modelName)
		return
	}
	tpRegistry[modelName] = coord
}

// UnregisterTPCoordinator — удаляет TP coordinator.
func UnregisterTPCoordinator(modelName string) {
	tpRegistryMu.Lock()
	defer tpRegistryMu.Unlock()
	delete(tpRegistry, modelName)
}

// LookupTPCoordinator — поиск TP coordinator по имени модели.
func LookupTPCoordinator(modelName string) *rptensor.TensorParallelCoordinator {
	tpRegistryMu.RLock()
	defer tpRegistryMu.RUnlock()
	return tpRegistry[modelName]
}

// ListTPCoordinators — список всех зарегистрированных TP coordinator'ов.
func ListTPCoordinators() map[string]*rptensor.TensorParallelCoordinator {
	tpRegistryMu.RLock()
	defer tpRegistryMu.RUnlock()
	out := make(map[string]*rptensor.TensorParallelCoordinator, len(tpRegistry))
	for k, v := range tpRegistry {
		out[k] = v
	}
	return out
}

// =====================================================================
// API endpoint types
// =====================================================================

// TPInferAPIRequest — запрос на TP inference через API.
type TPInferAPIRequest struct {
	ModelName string `json:"model_name"`
	Prompt    string `json:"prompt,omitempty"`
	Input     []byte `json:"input,omitempty"`
	TierCount int    `json:"tier_count,omitempty"` // informational; для проверки
	SessionID string `json:"session_id,omitempty"`
}

// TPInferAPIResponse — ответ с агрегированным output'ом и метаданными.
type TPInferAPIResponse struct {
	RequestID  string                   `json:"request_id"`
	Output     string                   `json:"output"`
	LatencyMs  int64                    `json:"latency_ms"`
	WorldSize  int                      `json:"world_size"`
	NumLayers  int                      `json:"num_layers"`
	Degraded   bool                     `json:"degraded"`
	LayerStats []rptensor.LayerStat    `json:"layer_stats,omitempty"`
	RankErrors []rptensor.RankError    `json:"rank_errors,omitempty"`
}

// TPStatusAPIResponse — статус TP coordinator'ов в кластере.
type TPStatusAPIResponse struct {
	Count  int                              `json:"count"`
	Models []TPStatusModelEntry             `json:"models,omitempty"`
}

// TPStatusModelEntry — entry для одной модели.
type TPStatusModelEntry struct {
	ModelName  string `json:"model_name"`
	WorldSize  int    `json:"world_size"`
	NumLayers  int    `json:"num_layers"`
	Enabled    bool   `json:"enabled"`
	TotalInfer int64  `json:"total_infer"`
	TotalErr   int64  `json:"total_errors"`
}

// =====================================================================
// HTTP handlers
// =====================================================================

// handleRPCModelTPInfer — POST /api/v1/rpc/tp/infer.
//
// Body: TPInferAPIRequest.
// Response: TPInferAPIResponse (200) или error JSON (4xx/5xx).
func (s *Server) handleRPCModelTPInfer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}

	var req TPInferAPIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	if req.ModelName == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "model_name is required")
		return
	}

	coord := LookupTPCoordinator(req.ModelName)
	if coord == nil {
		writeJSONError(w, http.StatusNotFound, "tp_not_configured",
			"no TP coordinator registered for model "+req.ModelName)
		return
	}
	if !coord.Enabled() {
		writeJSONError(w, http.StatusServiceUnavailable, "tp_disabled",
			"TP coordinator is disabled for model "+req.ModelName)
		return
	}

	// Конвертируем в rptensor.TPInferRequest.
	input := req.Input
	if len(input) == 0 && req.Prompt != "" {
		input = []byte(req.Prompt)
	}
	tpReq := rptensor.TPInferRequest{
		ModelName: req.ModelName,
		Prompt:    req.Prompt,
		Input:     input,
		TierCount: req.TierCount,
		SessionID: req.SessionID,
	}

	// Выполняем с timeout (по умолчанию 60s).
	ctx, cancel := withTimeoutFromContext(r, 60*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := coord.Infer(ctx, tpReq)
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "tp_infer_failed", err.Error())
		return
	}

	apiResp := TPInferAPIResponse{
		RequestID:  generateRequestID(),
		Output:     resp.Output,
		LatencyMs:  elapsed,
		WorldSize:  resp.WorldSize,
		NumLayers:  resp.NumLayers,
		Degraded:   resp.Degraded,
		LayerStats: resp.LayerStats,
		RankErrors: resp.RankErrors,
	}
	writeJSONResponse(w, http.StatusOK, apiResp)

	logger.Get().Debugw("tp_infer completed",
		"model", req.ModelName,
		"world_size", resp.WorldSize,
		"latency_ms", elapsed,
		"degraded", resp.Degraded)
}

// handleRPCModelTPStatus — GET /api/v1/rpc/tp/status.
//
// Response: TPStatusAPIResponse со списком всех зарегистрированных
// TP coordinator'ов и их статистикой.
func (s *Server) handleRPCModelTPStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}

	coordinators := ListTPCoordinators()
	entries := make([]TPStatusModelEntry, 0, len(coordinators))

	for modelName, coord := range coordinators {
		stats := coord.Stats()
		entries = append(entries, TPStatusModelEntry{
			ModelName:  modelName,
			WorldSize:  coord.WorldSize(),
			NumLayers:  coord.NumLayers(),
			Enabled:    coord.Enabled(),
			TotalInfer: stats.TotalInfer,
			TotalErr:   stats.TotalErrors,
		})
	}

	writeJSONResponse(w, http.StatusOK, TPStatusAPIResponse{
		Count:  len(entries),
		Models: entries,
	})
}

// withTimeoutFromContext — обёртка: если r.Context() уже с таймаутом,
// использует его; иначе применяет defaultTimeout.
func withTimeoutFromContext(r *http.Request, defaultTimeout time.Duration) (context.Context, context.CancelFunc) {
	if r != nil && r.Context() != nil && r.Context().Err() == nil {
		// Используем request context как parent (caller может отменить).
		return context.WithTimeout(r.Context(), defaultTimeout)
	}
	return context.WithTimeout(context.Background(), defaultTimeout)
}

// generateRequestID — простой UUID-подобный ID для request tracking.
func generateRequestID() string {
	// Используем timestamp + counter для уникальности в пределах сессии.
	return fmt.Sprintf("tp-%d", time.Now().UnixNano())
}