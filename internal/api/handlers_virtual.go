package api

import (
	"fmt"
	"net/http"
	"strings"
)

// ============================================================
// Virtual Model Router API handlers (Variant C)
// ============================================================

// virtualModelsListHandler - GET список всех виртуальных моделей
func (s *Server) virtualModelsListHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	router := s.proxy.GetVirtualModelRouter()
	if router == nil {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled": false,
			"count":   0,
			"models":  []interface{}{},
		})
		return
	}

	models := router.ListVirtualModels()
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": true,
		"count":   len(models),
		"models":  models,
	})
}

// virtualModelsStatusHandler - GET статуса конкретной виртуальной модели
func (s *Server) virtualModelsStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	router := s.proxy.GetVirtualModelRouter()
	if router == nil {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled": false,
		})
		return
	}

	// Извлекаем имя виртуальной модели из URL: /api/v1/virtualmodels/{name}
	modelName := strings.TrimPrefix(r.URL.Path, "/api/v1/virtualmodels/")
	if modelName == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "virtual model name required",
		})
		return
	}

	status := router.GetVirtualModelStatus(modelName)
	if status == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("virtual model '%s' not found", modelName),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, status)
}

// candidatesHandler - GET /api/v1/candidates — получение групп кандидатов для всех моделей
func (s *Server) candidatesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	candidates := s.proxy.GetCandidates()
	if candidates == nil {
		s.writeJSON(w, http.StatusOK, []interface{}{})
		return
	}

	s.writeJSON(w, http.StatusOK, candidates)
}