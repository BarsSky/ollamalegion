// handlers_autotune_admin.go — R54.6 (2026-08-24): AutoTune admin endpoints.
//
// POST /api/v1/admin/autotune/{backendID}/apply — apply AutoTune recommendations manually.
// GET  /api/v1/admin/autotune — get AutoTune state per backend (with circuit info).
// GET  /api/v1/admin/autotune/{backendID} — get state for one backend.

package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// adminAutotuneApplyRequest — body for POST /api/v1/admin/autotune/{id}/apply.
//
// Можно либо передать plan напрямую, либо (предпочтительно) передать
// model_name и позволить серверу построить plan из текущего AutoTuneAnalysis.
type adminAutotuneApplyRequest struct {
	// ModelName — имя модели (опционально если передан Plan).
	ModelName string `json:"modelName,omitempty"`
	// Plan — готовый план (для advanced use cases, обычно не нужен).
	Plan *balancer.AutoTuneReloadPlan `json:"plan,omitempty"`
	// ForceApply — игнорировать circuit breaker (только для manual ops).
	ForceApply bool `json:"forceApply,omitempty"`
}

// adminAutotuneApplyResponse — результат apply.
type adminAutotuneApplyResponse struct {
	Success bool                          `json:"success"`
	Error   string                        `json:"error,omitempty"`
	Message string                        `json:"message,omitempty"`
	Result  *balancer.AutoTuneApplyResult `json:"result,omitempty"`
}

// handleAdminAutotuneApply — POST /api/v1/admin/autotune/{backendID}/apply
//
// Body (опционально):
//   {"modelName": "Qwen3-..."}   — построить plan из текущего AutoTune report
//   {"plan": {...}, "forceApply": true}   — explicit plan + bypass circuit
//
// Response 200/202 (async apply returns 202).
// Response 400 если plan пустой.
// Response 404 если backend не найден.
func (s *Server) handleAdminAutotuneApply(w http.ResponseWriter, r *http.Request, backendID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req adminAutotuneApplyRequest
	if r.Body != nil && r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   "invalid request body: " + err.Error(),
			})
			return
		}
	}

	// Get Proxy from server
	proxy := s.proxy
	if proxy == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"error":   "balancer proxy not initialized",
		})
		return
	}

	// Build plan if not provided
	var plan *balancer.AutoTuneReloadPlan
	if req.Plan != nil {
		plan = req.Plan
	} else {
		// Build from AutoTune report
		plan = s.buildPlanFromReport(proxy, backendID, req.ModelName)
		if plan == nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   "no plan provided and no actionable recommendations found (model is optimal or not loaded)",
			})
			return
		}
	}

	// Check circuit (unless ForceApply)
	if !req.ForceApply && proxy.AutoTuneTracker() != nil {
		circuit := proxy.AutoTuneTracker().GetCircuit(backendID, plan.ModelName)
		if circuit != nil {
			canReload, reason := circuit.CanReload(proxy.AutoTuneTracker().GetConfig())
			if !canReload {
				s.writeJSON(w, http.StatusServiceUnavailable, adminAutotuneApplyResponse{
					Success: false,
					Error:   "circuit breaker open: " + reason,
				})
				return
			}
		}
	}

	// Record attempt + apply (sync for now; could be async later)
	if proxy.AutoTuneTracker() != nil {
		circuit := proxy.AutoTuneTracker().GetCircuit(backendID, plan.ModelName)
		if circuit != nil {
			circuit.RecordAttempt()
		}
	}

	logger.Get().Infow("admin: applying AutoTune plan",
		"backend", backendID, "model", plan.ModelName,
		"context_size", plan.ContextSize, "kv_cache", plan.KVCacheType,
		"gpu_layers", plan.NumGPULayers, "force", req.ForceApply)

	result := proxy.ApplyAutoTunePlan(backendID, plan)

	// Update circuit based on result
	if proxy.AutoTuneTracker() != nil {
		circuit := proxy.AutoTuneTracker().GetCircuit(backendID, plan.ModelName)
		if circuit != nil {
			if result.Success {
				circuit.RecordSuccess()
				proxy.AutoTuneTracker().ResetCircuit(backendID, plan.ModelName)
			} else {
				circuit.RecordError(result.Error)
			}
		}
	}

	resp := adminAutotuneApplyResponse{
		Success: result.Success,
		Result:  result,
	}
	if !result.Success {
		resp.Error = result.Error
	}
	if result.Success {
		resp.Message = result.Message
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// buildPlanFromReport — R54.6: builds a plan from current AutoTune report.
//
// Если modelName не задан, берёт первую sub-optimal модель на бэкенде.
func (s *Server) buildPlanFromReport(proxy *balancer.Proxy, backendID, modelName string) *balancer.AutoTuneReloadPlan {
	// Get loaded models from metrics
	metrics := proxy.GetMetricsManager()
	if metrics == nil {
		return nil
	}
	lm := metrics.GetLlamaCppMetrics(backendID)
	if lm == nil {
		return nil
	}

	// Find loaded model
	var loadedModel *types.LlamaCppModel
	if modelName != "" {
		for i, m := range lm.LoadedModels {
			if m.Name == modelName && m.State == "loaded" {
				loadedModel = &lm.LoadedModels[i]
				break
			}
		}
	} else {
		// Pick first loaded model
		for i, m := range lm.LoadedModels {
			if m.State == "loaded" {
				loadedModel = &lm.LoadedModels[i]
				break
			}
		}
	}
	if loadedModel == nil {
		return nil
	}

	// Get hardware info
	clusterState := proxy.GetClusterState()
	var freeVRAM, freeRAM, totalVRAM uint64
	for _, b := range clusterState.Backends {
		if b.ID == backendID {
			totalVRAM = b.GPU.MemoryTotal
			if b.GPU.MemoryFree > 0 {
				freeVRAM = b.GPU.MemoryFree
			}
			freeRAM = b.System.MemoryFree
			break
		}
	}

	// Build profile + analysis
	prof := &balancer.ModelProfileInfo{
		Name:                 loadedModel.Name,
		Architecture:         loadedModel.Architecture,
		NLayers:              loadedModel.NLayers,
		NEmbd:                loadedModel.NEmbd,
		NKvHeads:             loadedModel.NKvHeads,
		HeadDimK:             loadedModel.HeadDimK,
		SizeBytes:            loadedModel.Size,
		GgufMaxContext:       loadedModel.MaxContext,
		CurrentContextLength: loadedModel.ContextLength,
		CurrentKVCacheType:   loadedModel.KvCacheType,
		CurrentNumGPULayers:  loadedModel.NumGPULayers,
		FreeVRAMBytes:        freeVRAM,
		FreeRAMBytes:         freeRAM,
		TotalVRAMBytes:       totalVRAM,
		FeasibleMaxContext:   loadedModel.FeasibleMaxContext,
		AutoTuneEnabled:      balancer.IsAutoTuneEnabled(proxy, loadedModel.Name),
	}
	analysis := balancer.AnalyzeLoadedModelPublic(prof)
	if analysis == nil || !analysis.IsSubOptimal {
		return nil
	}
	return balancer.PlanApplyAutoTune(analysis, *loadedModel)
}

// handleAdminAutotune — GET /api/v1/admin/autotune
//
// Возвращает AutoTune state для всех бэкендов + circuit info.
func (s *Server) handleAdminAutotune(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	proxy := s.proxy
	if proxy == nil {
		http.Error(w, "balancer proxy not initialized", http.StatusInternalServerError)
		return
	}

	// Get all backends
	allBackends := proxy.GetAllBackends()
	resp := map[string]interface{}{
		"backends": []map[string]interface{}{},
	}

	for _, b := range allBackends {
		backendResp := map[string]interface{}{
			"id": b.ID,
		}
		// Get AutoTune report
		plan := s.buildPlanFromReport(proxy, b.ID, "")
		if plan != nil {
			backendResp["plan"] = plan
		}

		// Get circuit state for all loaded models
		if proxy.AutoTuneTracker() != nil {
			circuits := map[string]balancer.AutoTuneCircuitSnapshot{}
			// For each loaded model on this backend
			metrics := proxy.GetMetricsManager()
			if metrics != nil {
				if lm := metrics.GetLlamaCppMetrics(b.ID); lm != nil {
					for _, m := range lm.LoadedModels {
						circuits[m.Name] = proxy.AutoTuneTracker().GetCircuitSnapshot(b.ID, m.Name)
					}
				}
			}
			backendResp["circuits"] = circuits
		}
		resp["backends"] = append(resp["backends"].([]map[string]interface{}), backendResp)
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// routeAdminAutotuneByID — R54.6 (2026-08-24): диспетчер для /api/v1/admin/autotune/{backendID}/...
//
// Маршруты:
//   GET  /{backendID}           → handleAdminAutotuneByID
//   POST /{backendID}/apply     → handleAdminAutotuneApply
func (s *Server) routeAdminAutotuneByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/admin/autotune/")
	parts := strings.Split(path, "/")

	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Backend ID required", http.StatusBadRequest)
		return
	}

	// POST /{backendID}/apply
	if len(parts) == 2 && parts[1] == "apply" && r.Method == http.MethodPost {
		s.handleAdminAutotuneApply(w, r, parts[0])
		return
	}

	// GET /{backendID}
	if len(parts) == 1 && r.Method == http.MethodGet {
		s.handleAdminAutotuneByID(w, r)
		return
	}

	http.Error(w, "Method not allowed or unknown endpoint", http.StatusMethodNotAllowed)
}

// handleAdminAutotuneByID — GET /api/v1/admin/autotune/{backendID}
//
// Возвращает AutoTune state для конкретного бэкенда.
func (s *Server) handleAdminAutotuneByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract backend ID from URL path (вызывается через routeAdminAutotuneByID dispatcher).
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/autotune/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Backend ID required", http.StatusBadRequest)
		return
	}
	backendID := parts[0]

	proxy := s.proxy
	if proxy == nil {
		http.Error(w, "balancer proxy not initialized", http.StatusInternalServerError)
		return
	}

	// Get loaded models
	metrics := proxy.GetMetricsManager()
	if metrics == nil {
		http.Error(w, "metrics not available", http.StatusInternalServerError)
		return
	}
	lm := metrics.GetLlamaCppMetrics(backendID)
	if lm == nil {
		http.Error(w, "backend not found", http.StatusNotFound)
		return
	}

	resp := map[string]interface{}{
		"backendId":    backendID,
		"loadedModels": lm.LoadedModels,
	}
	// Get plan
	plan := s.buildPlanFromReport(proxy, backendID, "")
	if plan != nil {
		resp["plan"] = plan
	}
	// Get circuits
	if proxy.AutoTuneTracker() != nil {
		circuits := map[string]balancer.AutoTuneCircuitSnapshot{}
		for _, m := range lm.LoadedModels {
			circuits[m.Name] = proxy.AutoTuneTracker().GetCircuitSnapshot(backendID, m.Name)
		}
		resp["circuits"] = circuits
	}
	s.writeJSON(w, http.StatusOK, resp)
}
