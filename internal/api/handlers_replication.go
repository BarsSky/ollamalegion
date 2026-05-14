package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Model Replication API handlers (Variant A)
// ============================================================

// replicationGroupsHandler - GET/POST групп репликации
func (s *Server) replicationGroupsHandler(w http.ResponseWriter, r *http.Request) {
	mgr := s.proxy.GetModelReplicationManager()
	if mgr == nil || !mgr.IsEnabled() {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "model replication not enabled",
		})
		return
	}

	switch r.Method {
	case http.MethodGet:
		groups := mgr.GetGroups()
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"groups": groups,
		})

	case http.MethodPost:
		var cfg types.ModelGroupConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("invalid request body: %v", err),
			})
			return
		}
		if err := mgr.CreateGroup(cfg); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(),
			})
			return
		}
		s.writeJSON(w, http.StatusCreated, map[string]interface{}{
			"message": "group created",
			"group":   cfg,
		})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// replicationGroupHandler - GET/PUT/DELETE конкретной группы
func (s *Server) replicationGroupHandler(w http.ResponseWriter, r *http.Request) {
	mgr := s.proxy.GetModelReplicationManager()
	if mgr == nil || !mgr.IsEnabled() {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "model replication not enabled",
		})
		return
	}

	// Извлекаем имя модели из URL: /api/v1/replication/groups/{modelName}
	modelName := strings.TrimPrefix(r.URL.Path, "/api/v1/replication/groups/")
	if modelName == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "model name required",
		})
		return
	}

	switch r.Method {
	case http.MethodGet:
		cfg := mgr.GetGroup(modelName)
		if cfg == nil {
			s.writeJSON(w, http.StatusNotFound, map[string]string{
				"error": fmt.Sprintf("group '%s' not found", modelName),
			})
			return
		}
		states := mgr.GetInstanceStates(modelName)
		stats := mgr.GetGroupStats(modelName)
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"config": cfg,
			"states": states,
			"stats":  stats,
		})

	case http.MethodPut:
		var cfg types.ModelGroupConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("invalid request body: %v", err),
			})
			return
		}
		cfg.ModelName = modelName
		if err := mgr.UpdateGroup(cfg); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": err.Error(),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"message": "group updated",
			"group":   cfg,
		})

	case http.MethodDelete:
		if err := mgr.DeleteGroup(modelName); err != nil {
			s.writeJSON(w, http.StatusNotFound, map[string]string{
				"error": err.Error(),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"message": fmt.Sprintf("group '%s' deleted", modelName),
		})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// replicationStatsHandler - GET расширенной статистики
func (s *Server) replicationStatsHandler(w http.ResponseWriter, r *http.Request) {
	mgr := s.proxy.GetModelReplicationManager()
	if mgr == nil || !mgr.IsEnabled() {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "model replication not enabled",
		})
		return
	}

	groups := mgr.GetGroups()
	stats := make([]map[string]interface{}, 0, len(groups))
	for _, g := range groups {
		groupStats := mgr.GetGroupStats(g.ModelName)
		if groupStats != nil {
			stats = append(stats, groupStats)
		}
	}

	ctrl := s.proxy.GetReplicationController()
	isRunning := false
	if ctrl != nil {
		isRunning = ctrl.IsRunning()
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":       mgr.IsEnabled(),
		"groupCount":    len(groups),
		"stats":         stats,
		"autoReconcile": isRunning,
	})
}

// replicationReconcileHandler - POST принудительной реконсиляции
func (s *Server) replicationReconcileHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctrl := s.proxy.GetReplicationController()
	if ctrl == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "replication controller not available",
		})
		return
	}

	ctrl.TriggerReconcile()
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "reconciliation triggered",
	})
}