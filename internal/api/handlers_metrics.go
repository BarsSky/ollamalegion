package api

import (
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/balancer"
)

// ============================================================
// Metrics & Predictions API handlers
// ============================================================

// metricsHandler - получение метрик кластера
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.proxy.GetClusterState()

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":       state.Timestamp,
		"totalBackends":   state.TotalBackends,
		"healthyBackends": state.HealthyBackends,
		"backends":        state.Backends,
	})
}

// metricHandler - получение метрик конкретного бэкенда
func (s *Server) metricHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Извлечение ID из пути
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/metrics/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Backend ID required", http.StatusBadRequest)
		return
	}

	backendID := parts[0]

	state := s.proxy.GetClusterState()

	for _, metrics := range state.Backends {
		if metrics.ID == backendID {
			s.writeJSON(w, http.StatusOK, metrics)
			return
		}
	}

	http.Error(w, "Backend not found", http.StatusNotFound)
}

// predictionsHandler - прогнозы для всех бэкендов
func (s *Server) predictionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.proxy.GetClusterState()

	predictions := make(map[string]interface{})
	for _, backend := range state.Backends {
		pred := s.proxy.GetPrediction(backend.ID)
		predictions[backend.ID] = map[string]interface{}{
			"backendId":         backend.ID,
			"secondsToCritical": pred.SecondsToCritical,
			"criticalReason":    pred.CriticalReason,
			"gpuUsageTrend":     pred.GPUUsageTrend,
			"vramUsageTrend":    pred.VRAMUsageTrend,
			"ramUsageTrend":     pred.RAMUsageTrend,
			"freeSlotsTrend":    pred.FreeSlotsTrend,
			"requestCapacity":   pred.RequestCapacity,
			"capacityPercent":   int(pred.RequestCapacity),
			"timeToCritical":    balancer.FormatDuration(pred.SecondsToCritical),
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"timestamp":   time.Now().UTC(),
		"predictions": predictions,
	})
}

// predictionHandler - прогноз для конкретного бэкенда
func (s *Server) predictionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/predictions/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "Backend ID required", http.StatusBadRequest)
		return
	}

	backendID := parts[0]

	state := s.proxy.GetClusterState()
	for _, backend := range state.Backends {
		if backend.ID == backendID {
			pred := s.proxy.GetPrediction(backendID)
			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"backendId":         backendID,
				"secondsToCritical": pred.SecondsToCritical,
				"criticalReason":    pred.CriticalReason,
				"gpuUsageTrend":     pred.GPUUsageTrend,
				"vramUsageTrend":    pred.VRAMUsageTrend,
				"ramUsageTrend":     pred.RAMUsageTrend,
				"freeSlotsTrend":    pred.FreeSlotsTrend,
				"requestCapacity":   pred.RequestCapacity,
				"capacityPercent":   int(pred.RequestCapacity),
				"timeToCritical":    balancer.FormatDuration(pred.SecondsToCritical),
			})
			return
		}
	}

	http.Error(w, "Backend not found", http.StatusNotFound)
}