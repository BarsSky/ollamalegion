// Phase 8 P.2 — CRUD REST API для VirtualModels в alias-on-pool mode.
//
// Endpoints:
//   GET    /api/v1/virtual-models                  — list all VMs
//   POST   /api/v1/virtual-models                  — register new VM
//   GET    /api/v1/virtual-models/{name}           — get VM details
//   DELETE /api/v1/virtual-models/{name}           — unregister VM
//   POST   /api/v1/virtual-models/{name}/infer     — test inference (proxy one request)
//
// Legacy pipeline-mode endpoints на /api/v1/virtualmodels (без дефиса) остаются
// в handlers_virtual.go для backward compat.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// =====================================================================
// GET /api/v1/virtual-models — list all virtual models
// =====================================================================

func (s *Server) virtualModelsCRUDListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	registry := s.proxy.GetVirtualModelRegistry()
	router := s.proxy.GetVirtualRouter()

	enabled := registry != nil && registry.IsEnabled()
	out := map[string]interface{}{
		"enabled": enabled,
		"count":   0,
		"models":  []interface{}{},
	}
	if registry == nil {
		s.writeJSON(w, http.StatusOK, out)
		return
	}

	models := registry.List()
	modelInfos := make([]map[string]interface{}, 0, len(models))
	for _, vm := range models {
		info := virtualModelToInfo(vm.Config)
		// Добавляем metrics если router доступен и это alias-on-pool mode.
		if router != nil && vm.Config.IsAliasOnPoolMode() {
			if m := router.GetMetrics(); m != nil {
				snap := m.Snapshot()
				// Backend selections для этой VM.
				selections := snap["backendSelections"].(map[string]int64)
				vmSelections := make(map[string]int64)
				for _, backendID := range vm.Config.BackendPool {
					vmSelections[backendID] = selections[backendID]
				}
				info["selections"] = vmSelections
			}
		}
		modelInfos = append(modelInfos, info)
	}
	out["count"] = len(modelInfos)
	out["models"] = modelInfos
	s.writeJSON(w, http.StatusOK, out)
}

// virtualModelToInfo — converts VirtualModelConfig to API response format.
func virtualModelToInfo(cfg types.VirtualModelConfig) map[string]interface{} {
	out := map[string]interface{}{
		"name":        cfg.Name,
		"description": cfg.Description,
		"mode":        "alias_on_pool",
		"modelName":   cfg.ModelName,
		"backendPool": cfg.BackendPool,
		"selection":   string(cfg.Selection),
		"timeoutMs":   cfg.Coordination.TimeoutMs,
	}
	if !cfg.IsAliasOnPoolMode() {
		// Legacy pipeline mode.
		out["mode"] = "pipeline"
		out["slices"] = cfg.Slices
		out["coordination"] = cfg.Coordination
	}
	return out
}

// =====================================================================
// POST /api/v1/virtual-models — register new VM
// =====================================================================

type virtualModelCreateRequest struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Selection    string   `json:"selection"`
	BackendPool  []string `json:"backendPool"`
	ModelName    string   `json:"modelName"`
	TimeoutMs    int      `json:"timeoutMs"`
}

func (s *Server) virtualModelsCRUDCreateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	registry := s.proxy.GetVirtualModelRegistry()
	if registry == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "virtual_models registry not initialized",
		})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "read body: " + err.Error(),
		})
		return
	}
	r.Body.Close()

	var req virtualModelCreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "parse JSON: " + err.Error(),
		})
		return
	}

	// Validation.
	if err := validateVirtualModelCreate(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}

	// Build config.
	timeout := req.TimeoutMs
	if timeout <= 0 {
		timeout = 30000 // default 30s
	}
	cfg := types.VirtualModelConfig{
		Name:        req.Name,
		Description: req.Description,
		Selection:   virtualmodel.SelectionStrategy(req.Selection),
		BackendPool: req.BackendPool,
		ModelName:   req.ModelName,
		Coordination: types.CoordinationConfig{
			TimeoutMs: timeout,
		},
	}

	if err := registry.Register(cfg); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "register: " + err.Error(),
		})
		return
	}

	logger.Get().Infow("virtual_model registered via API",
		"name", req.Name, "model", req.ModelName,
		"backends", len(req.BackendPool), "selection", req.Selection)

	// Auto-enable registry on first register.
	if !registry.IsEnabled() {
		registry.SetEnabled(true)
	}

	s.writeJSON(w, http.StatusCreated, virtualModelToInfo(cfg))
}

// validateVirtualModelCreate — basic validation.
//
// Phase 8 P.2: поддерживает 2 режима VirtualModel:
//   1. Alias-on-pool (Phase 8 P.2) — требует modelName + backendPool.
//   2. Pipeline (legacy) — требует slices (валидируется в handler, не здесь).
//
// Если modelName+backendPool заданы → alias-on-pool (требуем оба).
// Иначе → pipeline mode (только name обязателен).
func validateVirtualModelCreate(req *virtualModelCreateRequest) error {
	if req.Name == "" {
		return fmt.Errorf("name is required")
	}

	hasModelName := req.ModelName != ""
	hasBackendPool := len(req.BackendPool) > 0

	if hasModelName || hasBackendPool {
		// Alias-on-pool mode → требуем оба поля.
		if !hasModelName {
			return fmt.Errorf("modelName is required for alias-on-pool mode")
		}
		if !hasBackendPool {
			return fmt.Errorf("backendPool must contain at least 1 backend for alias-on-pool mode")
		}
		// Validate each backend address.
		for i, b := range req.BackendPool {
			if !strings.Contains(b, ":") {
				return fmt.Errorf("backendPool[%d]=%q invalid (expected host:port)", i, b)
			}
		}
		// Validate selection strategy.
		switch virtualmodel.SelectionStrategy(req.Selection) {
		case "", virtualmodel.SelectionRoundRobin, virtualmodel.SelectionLeastLoaded, virtualmodel.SelectionRandom:
			// OK
		default:
			return fmt.Errorf("selection=%q invalid (expected round_robin/least_loaded/random)", req.Selection)
		}
	}
	// Pipeline mode (legacy) — slices validated отдельно, не здесь.
	return nil
}

// =====================================================================
// GET /api/v1/virtual-models/{name} — get VM details
// =====================================================================

func (s *Server) virtualModelsCRUDGetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := extractVirtualModelNameFromPath(r.URL.Path, "/api/v1/virtual-models/")
	if name == "" || strings.Contains(name, "/") {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "virtual model name required (single path segment)",
		})
		return
	}

	registry := s.proxy.GetVirtualModelRegistry()
	if registry == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "virtual_models registry not initialized",
		})
		return
	}

	vm := registry.Get(name)
	if vm == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("virtual model '%s' not found", name),
		})
		return
	}

	info := virtualModelToInfo(vm.Config)
	s.writeJSON(w, http.StatusOK, info)
}

// =====================================================================
// DELETE /api/v1/virtual-models/{name} — unregister VM
// =====================================================================

func (s *Server) virtualModelsCRUDDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := extractVirtualModelNameFromPath(r.URL.Path, "/api/v1/virtual-models/")
	if name == "" || strings.Contains(name, "/") {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "virtual model name required",
		})
		return
	}

	registry := s.proxy.GetVirtualModelRegistry()
	if registry == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "virtual_models registry not initialized",
		})
		return
	}

	if registry.Get(name) == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("virtual model '%s' not found", name),
		})
		return
	}

	registry.Unregister(name)
	logger.Get().Infow("virtual_model unregistered via API", "name", name)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"deleted": name,
	})
}

// =====================================================================
// POST /api/v1/virtual-models/{name}/infer — test inference (proxy 1 request)
// =====================================================================

func (s *Server) virtualModelsCRUDInferHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Path: /api/v1/virtual-models/{name}/infer
	prefix := "/api/v1/virtual-models/"
	suffix := "/infer"
	if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "expected path /api/v1/virtual-models/{name}/infer",
		})
		return
	}
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), suffix)
	if name == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "virtual model name required",
		})
		return
	}

	router := s.proxy.GetVirtualRouter()
	if router == nil || !router.IsActive() {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "virtual_router not active",
		})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "read body: " + err.Error(),
		})
		return
	}
	r.Body.Close()

	// Force model = name (override any model field in body).
	var env map[string]interface{}
	if err := json.Unmarshal(body, &env); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "parse JSON: " + err.Error(),
		})
		return
	}
	env["model"] = name
	rewritten, _ := json.Marshal(env)

	// Forward to VirtualRouter.
	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	// Synthesize a /v1/chat/completions path (Ollama-compatible).
	// Use a path that's accepted by VirtualRouter.
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/v1/chat/completions"
	r2.URL.RawPath = ""
	r2.Body = io.NopCloser(bytes.NewReader(rewritten))
	r2.ContentLength = int64(len(rewritten))

	w2 := &responseRecorder{header: http.Header{}}
	router.ServeHTTP(w2, r2)

	// Copy response.
	for k, v := range w2.header {
		w.Header()[k] = v
	}
	w.WriteHeader(w2.statusCode)
	w.Write(w2.body.Bytes())
}

// =====================================================================
// Helpers
// =====================================================================

// extractVirtualModelNameFromPath — extracts {name} from /api/v1/virtual-models/{name}.
// Returns "" если path не подходит.
func extractVirtualModelNameFromPath(path, prefix string) string {
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	return strings.TrimPrefix(path, prefix)
}

// responseRecorder — minimal http.ResponseWriter для internal proxy.
type responseRecorder struct {
	header     http.Header
	body       bytes.Buffer
	statusCode int
}

func (r *responseRecorder) Header() http.Header {
	if r.header == nil {
		r.header = http.Header{}
	}
	return r.header
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return r.body.Write(b)
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
}

// Compile-time check: time used in timeout default.
var _ = time.Second
