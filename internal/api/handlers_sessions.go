package api

import (
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Sessions API handlers
// ============================================================

// sessionsHandler - получение активных сессий
func (s *Server) sessionsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sessions := s.proxy.GetSessions()

		type SessionInfo struct {
			ID            string    `json:"id"`
			ClientName    string    `json:"clientName"`
			ClientIP      string    `json:"clientIP"`
			BackendID     string    `json:"backendId"`
			BackendName   string    `json:"backendName"`
			Model         string    `json:"model"`
			CreatedAt     time.Time `json:"createdAt"`
			LastRequestAt time.Time `json:"lastRequestAt"`
			RequestCount  int       `json:"requestCount"`
			IdleSeconds   int       `json:"idleSeconds"`
		}

		result := make([]SessionInfo, 0, len(sessions))
		for _, session := range sessions {
			backend := s.proxy.GetBackend(session.BackendID)
			backendName := ""
			if backend != nil {
				backendName = backend.Name
			}
			result = append(result, SessionInfo{
				ID:            session.ID,
				ClientName:    session.ClientName,
				ClientIP:      session.ClientIP,
				BackendID:     session.BackendID,
				BackendName:   backendName,
				Model:         session.Model,
				CreatedAt:     session.CreatedAt,
				LastRequestAt: session.LastRequestAt,
				RequestCount:  session.RequestCount,
				IdleSeconds:   int(time.Since(session.LastRequestAt).Seconds()),
			})
		}

		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"total":    len(result),
			"sessions": result,
		})
	case http.MethodDelete:
		s.proxy.ClearSessions()
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": "All sessions cleared",
		})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// sessionHandler - управление конкретной сессией
func (s *Server) sessionHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Session ID required", http.StatusBadRequest)
		return
	}

	sessionID := parts[0]

	switch r.Method {
	case http.MethodDelete:
		if s.proxy.DeleteSession(sessionID) {
			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"success":   true,
				"sessionId": sessionID,
				"message":   "Session removed successfully",
			})
		} else {
			s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"success": false,
				"error":   "Session " + sessionID + " not found",
			})
		}
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// modelsHandler - получение запущенных моделей с полными details
func (s *Server) modelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.proxy.GetClusterState()

	type BackendModels struct {
		BackendID     string               `json:"backendId"`
		BackendName   string               `json:"backendName"`
		BackendStatus types.BackendStatus  `json:"backendStatus"`
		HasAgent      bool                 `json:"hasAgent"`
		Models        []types.RunningModel `json:"models"`
	}

	result := make([]BackendModels, 0, len(state.Backends))
	for _, metrics := range state.Backends {
		backend := s.proxy.GetBackend(metrics.ID)
		name := ""
		if backend != nil {
			name = backend.Name
		}
		result = append(result, BackendModels{
			BackendID:     metrics.ID,
			BackendName:   name,
			BackendStatus: metrics.Status,
			HasAgent:      metrics.HasAgent,
			Models:        metrics.Ollama.RunningModels,
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"backends": result,
		"total":    len(result),
	})
}