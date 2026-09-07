// Package api — R60.11 (2026-09-07) handler для /api/v1/balancer/load-backoff.
//
// Endpoints:
//   GET  /api/v1/balancer/load-backoff
//     — JSON snapshot всех circuit breaker states (failures count,
//       breakerOpenUntil timestamp). Используется для diagnostics.
//
//   POST /api/v1/balancer/load-backoff/reset[?backend=X&model=Y]
//     — Manual reset of balancer's loadBackoff circuit breaker.
//       Без query params: reset ALL states.
//       С ?backend=X&model=Y: reset specific (backend, model) pair.
//
// R60.11 motivation: pre-R60.11 единственный способ сбросить breaker —
// подождать TTL (60s default) или рестартовать balancer container.
// Это создавало UX-проблему: operator может починить cppworker (docker
// restart, config fix), но balancer всё равно возвращает 502 "circuit
// breaker open" пока TTL не истечёт. POST endpoint даёт мгновенный reset.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
)

// loadBackoffSnapshotResponse — JSON-friendly snapshot of one backoff state.
type loadBackoffSnapshotResponse struct {
	BackendID        string `json:"backendId"`
	ModelName        string `json:"modelName"`
	Failures         int    `json:"failures"`
	LastFailureAt    string `json:"lastFailureAt,omitempty"`
	BreakerOpenUntil string `json:"breakerOpenUntil,omitempty"`
	BreakerIsOpen    bool   `json:"breakerIsOpen"`
}

// handleGetLoadBackoffSnapshot — GET /api/v1/balancer/load-backoff
// Returns JSON array of all circuit breaker states.
func (s *Server) handleGetLoadBackoffSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	if s.proxy == nil {
		http.Error(w, "proxy not initialized", http.StatusServiceUnavailable)
		return
	}

	states := s.getLoadBackoffSnapshot()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(states); err != nil {
		logger.Get().Warnw("handleGetLoadBackoffSnapshot: encode failed", "error", err)
	}
}

// handleResetLoadBackoff — POST /api/v1/balancer/load-backoff/reset[?backend=X&model=Y]
//
// Returns 200 with {"cleared": N} where N is number of states cleared.
// 404 if specific (backend, model) not found.
func (s *Server) handleResetLoadBackoff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if s.proxy == nil {
		http.Error(w, "proxy not initialized", http.StatusServiceUnavailable)
		return
	}

	backendID := r.URL.Query().Get("backend")
	modelName := r.URL.Query().Get("model")

	type resetResp struct {
		Cleared   int    `json:"cleared"`
		BackendID string `json:"backendId,omitempty"`
		ModelName string `json:"modelName,omitempty"`
	}

	var resp resetResp
	if backendID == "" || modelName == "" {
		// Reset all
		resp.Cleared = s.resetLoadBackoffAll()
	} else {
		// Reset specific
		ok := s.resetLoadBackoff(backendID, modelName)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": fmt.Sprintf("no loadBackoff state for backend=%s model=%s", backendID, modelName),
			})
			return
		}
		resp.Cleared = 1
		resp.BackendID = backendID
		resp.ModelName = modelName
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
	logger.Get().Infow("loadBackoff reset endpoint called (R60.11)",
		"cleared", resp.Cleared, "backend", backendID, "model", modelName)
}

// computeBreakerIsOpen — helper: returns true if breaker is currently open.
// (If expired, returns false.)
func computeBreakerIsOpen(breakerOpenUntil time.Time) bool {
	if breakerOpenUntil.IsZero() {
		return false
	}
	return time.Now().Before(breakerOpenUntil)
}

// getLoadBackoff — get the loadBackoff instance from the proxy's
// llamaCppRouter. Returns nil if not available.
func (s *Server) getLoadBackoff() interface {
	Reset(backendID, modelName string) bool
	ResetAll() int
	Snapshot() []balancer.LoadBackoffSnapshot
} {
	if s.proxy == nil {
		return nil
	}
	lr := s.proxy.GetLlamaCppRouter()
	if lr == nil {
		return nil
	}
	return lr.GetLoadBackoff()
}

// getLoadBackoffSnapshot — R60.11: serialize all loadBackoff states.
func (s *Server) getLoadBackoffSnapshot() []loadBackoffSnapshotResponse {
	lb := s.getLoadBackoff()
	if lb == nil {
		return []loadBackoffSnapshotResponse{}
	}
	rawSnap := lb.Snapshot()
	out := make([]loadBackoffSnapshotResponse, 0, len(rawSnap))
	for _, s := range rawSnap {
		out = append(out, loadBackoffSnapshotResponse{
			BackendID:        s.BackendID,
			ModelName:        s.ModelName,
			Failures:         s.Failures,
			LastFailureAt:    s.LastFailureAt,
			BreakerOpenUntil: s.BreakerOpenUntil,
			BreakerIsOpen:    computeBreakerIsOpen(parseTime(s.BreakerOpenUntil)),
		})
	}
	return out
}

// resetLoadBackoff — R60.11: reset circuit for specific (backend, model).
// Returns true if state was actually cleared.
func (s *Server) resetLoadBackoff(backendID, modelName string) bool {
	lb := s.getLoadBackoff()
	if lb == nil {
		return false
	}
	return lb.Reset(backendID, modelName)
}

// resetLoadBackoffAll — R60.11: reset all circuit breaker states.
func (s *Server) resetLoadBackoffAll() int {
	lb := s.getLoadBackoff()
	if lb == nil {
		return 0
	}
	return lb.ResetAll()
}

// parseTime — RFC3339 time parser helper.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
