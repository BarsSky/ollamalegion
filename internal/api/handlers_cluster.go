package api

import (
	"encoding/json"
	"net/http"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Cluster & Config API handlers
// ============================================================

// clusterHandler - состояние кластера
func (s *Server) clusterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := s.proxy.GetClusterState()

	s.writeJSON(w, http.StatusOK, state)
}

// clusterConfigHandler - runtime конфигурация кластера (смена алгоритма и т.д.)
func (s *Server) clusterConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"algorithm":           s.config.Balancing.Algorithm,
			"modelAffinity":       s.config.Balancing.ModelAffinity,
			"sessionStickiness":   s.config.Balancing.SessionStickiness,
			"queueMaxSize":        s.config.Balancing.QueueMaxSize,
			"queueTimeout":        s.config.Balancing.QueueTimeout,
			"requestTimeout":      s.config.Balancing.RequestTimeout,
			"useEnhancedScoring":  s.config.Balancing.UseEnhancedScoring,
			"predictionFiltering": true, // совместимость: всегда true
			"gpuMaxUsage":         s.config.Resources.GPU.MaxUsagePercent,
			"vramMaxUsage":        s.config.Resources.GPU.MaxVRAMUsagePercent,
			"cpuMaxUsage":         s.config.Resources.CPU.MaxUsagePercent,
			"ramMaxUsage":         s.config.Resources.Memory.MaxUsagePercent,
			"minFreeDisk":         s.config.Resources.Disk.MinFreeMB,
			"operatingMode":       s.config.Balancing.OperatingMode,
			"initialized":         s.config.Initialized,
		})
	case http.MethodPut:
		var req struct {
			Algorithm           string   `json:"algorithm"`
			ModelAffinity       *bool    `json:"modelAffinity"`
			SessionStickiness   *bool    `json:"sessionStickiness"`
			UseEnhancedScoring  *bool    `json:"useEnhancedScoring"`
			PredictionFiltering *bool    `json:"predictionFiltering"`
			GPUUsage            *float64 `json:"gpuMaxUsage"`
			VRAMUsage           *float64 `json:"vramMaxUsage"`
			CPUUsage            *float64 `json:"cpuMaxUsage"`
			RAMUsage            *float64 `json:"ramMaxUsage"`
			MinFreeDisk         *float64 `json:"minFreeDisk"`
			OperatingMode       string   `json:"operatingMode"`
			Initialized         *bool    `json:"initialized"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   "Invalid request body",
			})
			return
		}

		// Валидация алгоритма
		if req.Algorithm != "" {
			validAlgorithms := map[string]bool{
				"roundrobin":     true,
				"leastconn":      true,
				"resource-aware": true,
				"model-affinity": true,
			}
			if !validAlgorithms[req.Algorithm] {
				s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"error":   "Invalid algorithm. Valid: roundrobin, leastconn, resource-aware, model-affinity",
				})
				return
			}
			s.config.Balancing.Algorithm = types.BalancingAlgorithm(req.Algorithm)
		}

		if req.ModelAffinity != nil {
			s.config.Balancing.ModelAffinity = *req.ModelAffinity
		}
		if req.SessionStickiness != nil {
			s.config.Balancing.SessionStickiness = *req.SessionStickiness
		}
		if req.UseEnhancedScoring != nil {
			s.config.Balancing.UseEnhancedScoring = *req.UseEnhancedScoring
		}
		if req.GPUUsage != nil {
			s.config.Resources.GPU.MaxUsagePercent = *req.GPUUsage
		}
		if req.VRAMUsage != nil {
			s.config.Resources.GPU.MaxVRAMUsagePercent = *req.VRAMUsage
		}
		if req.CPUUsage != nil {
			s.config.Resources.CPU.MaxUsagePercent = *req.CPUUsage
		}
		if req.RAMUsage != nil {
			s.config.Resources.Memory.MaxUsagePercent = *req.RAMUsage
		}
		if req.MinFreeDisk != nil {
			s.config.Resources.Disk.MinFreeMB = uint64(*req.MinFreeDisk)
		}
		if req.Initialized != nil {
			s.config.Initialized = *req.Initialized
		}

		// --- Operating Mode Switch ---
		if req.OperatingMode != "" {
			validModes := map[string]bool{
				"standard":              true,
				"replication":           true,
				"rpc_coordinator":       true,
				"virtual_router":        true,
				"distributed_inference": true,
			}
			if !validModes[req.OperatingMode] {
				s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"error":   "Invalid operatingMode. Valid: standard, replication, rpc_coordinator, virtual_router, distributed_inference",
				})
				return
			}

			// Сбрасываем все enabled-флаги и включаем нужный
			s.config.Balancing.ModelReplication.Enabled = false
			s.config.Balancing.RpcCoordinator.Enabled = false
			s.config.Balancing.VirtualModels.Enabled = false
			s.config.Balancing.DistInference.Enabled = false

			switch req.OperatingMode {
			case "replication":
				s.config.Balancing.ModelReplication.Enabled = true
			case "rpc_coordinator":
				s.config.Balancing.RpcCoordinator.Enabled = true
			case "virtual_router":
				s.config.Balancing.VirtualModels.Enabled = true
			case "distributed_inference":
				s.config.Balancing.DistInference.Enabled = true
			}

			s.config.Balancing.OperatingMode = req.OperatingMode
		}

		// Гарантированно сохраняем конфигурацию на диск при изменении initialized
		// (критично для wizard: если не сохранить, он перезапустится после рестарта)
		if s.configSaver != nil {
			if err := s.configSaver(); err != nil {
				logger.Get().Warnw("failed to save config after cluster config update", "error", err)
			}
		}

		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"config": map[string]interface{}{
				"algorithm":           s.config.Balancing.Algorithm,
				"modelAffinity":       s.config.Balancing.ModelAffinity,
				"sessionStickiness":   s.config.Balancing.SessionStickiness,
				"useEnhancedScoring":  s.config.Balancing.UseEnhancedScoring,
				"predictionFiltering": true,
				"gpuMaxUsage":         s.config.Resources.GPU.MaxUsagePercent,
				"vramMaxUsage":        s.config.Resources.GPU.MaxVRAMUsagePercent,
				"cpuMaxUsage":         s.config.Resources.CPU.MaxUsagePercent,
				"ramMaxUsage":         s.config.Resources.Memory.MaxUsagePercent,
				"minFreeDisk":         s.config.Resources.Disk.MinFreeMB,
				"operatingMode":       s.config.Balancing.OperatingMode,
				"initialized":         s.config.Initialized,
			},
			"message": "Cluster configuration updated successfully",
		})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}