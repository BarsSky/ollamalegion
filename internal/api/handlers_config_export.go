package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// configExportHandler — GET /api/v1/config/export — экспорт полной конфигурации
func (s *Server) configExportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Строим полный экспортный объект
	export := buildConfigExport(s.config)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":    true,
		"config":     export,
		"exportedAt": time.Now().UTC().Format(time.RFC3339),
		"version":    "1.0.0",
	})
}

// configImportHandler — POST /api/v1/config/import — импорт конфигурации
func (s *Server) configImportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Config *types.LoadBalancerConfig `json:"config"`
		Merge  bool                      `json:"merge"` // если true — мержим, иначе заменяем полностью
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid JSON: " + err.Error(),
		})
		return
	}

	if req.Config == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "config field is required",
		})
		return
	}

	imported := req.Config

	// Валидация базовых полей
	if imported.Balancing.OperatingMode != "" {
		if _, ok := types.ModeBackendTypes[imported.Balancing.OperatingMode]; !ok {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("Invalid operatingMode: %s", imported.Balancing.OperatingMode),
			})
			return
		}
	}

	// Проверяем совместимость бэкендов с режимом
	if len(imported.Backends) > 0 {
		modeToCheck := imported.Balancing.OperatingMode
		if modeToCheck == "" {
			modeToCheck = s.config.Balancing.OperatingMode
		}
		if err := config.ValidateModeBackendCompatibility(modeToCheck, imported.Backends); err != nil {
			s.writeJSON(w, http.StatusConflict, map[string]interface{}{
				"success": false,
				"error":   err.Error(),
				"message": "Imported backends are incompatible with the operating mode",
			})
			return
		}
	}

	if req.Merge {
		// Мержим: перезаписываем только указанные поля
		mergeConfig(s.config, imported)
	} else {
		// Полная замена (но сохраняем host/port балансировщика)
		lbHost := s.config.LoadBalancer.Host
		lbPort := s.config.LoadBalancer.Port
		lbAPIPort := s.config.LoadBalancer.APIPort

		*s.config = *imported

		// Восстанавливаем хост и порты балансировщика
		if lbHost != "" && imported.LoadBalancer.Host == "" {
			s.config.LoadBalancer.Host = lbHost
		}
		if lbPort != 0 && imported.LoadBalancer.Port == 0 {
			s.config.LoadBalancer.Port = lbPort
		}
		if lbAPIPort != 0 && imported.LoadBalancer.APIPort == 0 {
			s.config.LoadBalancer.APIPort = lbAPIPort
		}
	}

	// Санитизируем конфигурацию после импорта
	config.SanitizeConfigOnModeSwitch(s.config, s.config.Balancing.OperatingMode)

	// Сбрасываем Initialized если были изменения структуры бэкендов
	if len(imported.Backends) > 0 || !req.Merge {
		s.config.Initialized = true
	}

	// Сохраняем конфигурацию на диск
	if s.configSaver != nil {
		if err := s.configSaver(); err != nil {
			logger.Get().Errorw("failed to save config after import", "error", err)
			s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"error":   "Failed to save configuration: " + err.Error(),
			})
			return
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":     true,
		"config":      buildConfigExport(s.config),
		"importedAt":  time.Now().UTC().Format(time.RFC3339),
		"backendCount": len(s.config.Backends),
		"message":     "Configuration imported successfully",
	})
}

// buildConfigExport строит полный объект экспорта конфигурации
func buildConfigExport(cfg *types.LoadBalancerConfig) map[string]interface{} {
	export := map[string]interface{}{
		"loadBalancer": map[string]interface{}{
			"host":           cfg.LoadBalancer.Host,
			"port":           cfg.LoadBalancer.Port,
			"apiPort":        cfg.LoadBalancer.APIPort,
			"tlsHost":        cfg.LoadBalancer.TLSHost,
			"tlsPort":        cfg.LoadBalancer.TLSPort,
			"trustedProxies": cfg.LoadBalancer.TrustedProxies,
			"clientIPHeaders": cfg.LoadBalancer.ClientIPHeaders,
			"statePath":      cfg.LoadBalancer.StatePath,
		},
		"backends":      cfg.Backends,
		"backendEngine": string(cfg.BackendEngine),
		"initialized":   cfg.Initialized,
		"balancing": map[string]interface{}{
			"algorithm":           string(cfg.Balancing.Algorithm),
			"modelAffinity":       cfg.Balancing.ModelAffinity,
			"sessionStickiness":   cfg.Balancing.SessionStickiness,
			"useEnhancedScoring":  cfg.Balancing.UseEnhancedScoring,
			"operatingMode":       cfg.Balancing.OperatingMode,
			"healthCheckInterval": cfg.Balancing.HealthCheckInterval,
			"metricsInterval":     cfg.Balancing.MetricsInterval,
			"requestTimeout":      cfg.Balancing.RequestTimeout,
			"firstByteTimeout":    cfg.Balancing.FirstByteTimeout,
			"streamingIdleTimeout": cfg.Balancing.StreamingIdleTimeout,
			"queueTimeout":        cfg.Balancing.QueueTimeout,
			"queueMaxSize":        cfg.Balancing.QueueMaxSize,
			"queueWorkers":        cfg.Balancing.QueueWorkers,
			"sessionTTL":          cfg.Balancing.SessionTTL,
		},
		"resources": map[string]interface{}{
			"gpu": map[string]interface{}{
				"maxUsagePercent":     cfg.Resources.GPU.MaxUsagePercent,
				"maxVRAMUsagePercent": cfg.Resources.GPU.MaxVRAMUsagePercent,
				"maxTemperature":      cfg.Resources.GPU.MaxTemperature,
			},
			"cpu": map[string]interface{}{
				"maxUsagePercent": cfg.Resources.CPU.MaxUsagePercent,
			},
			"memory": map[string]interface{}{
				"maxUsagePercent": cfg.Resources.Memory.MaxUsagePercent,
			},
			"disk": map[string]interface{}{
				"minFreeMB": cfg.Resources.Disk.MinFreeMB,
			},
		},
		"modelReplication": map[string]interface{}{
			"enabled":             cfg.Balancing.ModelReplication.Enabled,
			"defaultMinInstances": cfg.Balancing.ModelReplication.DefaultMinInstances,
			"defaultMaxInstances": cfg.Balancing.ModelReplication.DefaultMaxInstances,
			"idleUnloadAfter":     cfg.Balancing.ModelReplication.IdleUnloadAfter,
		},
		"rpcCoordinator": map[string]interface{}{
			"enabled":        cfg.Balancing.RpcCoordinator.Enabled,
			"coordinatorURL": cfg.Balancing.RpcCoordinator.CoordinatorURL,
			"workerPort":     cfg.Balancing.RpcCoordinator.WorkerPort,
			"protocol":       cfg.Balancing.RpcCoordinator.Protocol,
			"timeout":        cfg.Balancing.RpcCoordinator.Timeout,
			"maxRetries":     cfg.Balancing.RpcCoordinator.MaxRetries,
		},
		"virtualModels": map[string]interface{}{
			"enabled":     cfg.Balancing.VirtualModels.Enabled,
			"modelCount":  len(cfg.Balancing.VirtualModels.Models),
		},
		"distInference": map[string]interface{}{
			"enabled":  cfg.Balancing.DistInference.Enabled,
			"grpcPort": cfg.Balancing.DistInference.GrpcPort,
		},
		"prewarm": map[string]interface{}{
			"enabled":             cfg.Balancing.Prewarm.Enabled,
			"triggerLoadThreshold": cfg.Balancing.Prewarm.TriggerLoadThreshold,
			"maxPrewarmPerCycle":  cfg.Balancing.Prewarm.MaxPrewarmPerCycle,
			"checkIntervalSec":    cfg.Balancing.Prewarm.CheckIntervalSec,
		},
		"modelInstances": map[string]interface{}{
			"defaultMinInstances": cfg.Balancing.ModelInstances.DefaultMinInstances,
			"defaultMaxInstances": cfg.Balancing.ModelInstances.DefaultMaxInstances,
			"idleUnloadAfter":     cfg.Balancing.ModelInstances.IdleUnloadAfter,
		},
		"autoPull": map[string]interface{}{
			"enabled":       cfg.Balancing.AutoPull.Enabled,
			"maxConcurrent": cfg.Balancing.AutoPull.MaxConcurrent,
			"pullTimeout":   cfg.Balancing.AutoPull.PullTimeout,
			"retryCount":    cfg.Balancing.AutoPull.RetryCount,
		},
	}
	return export
}

// mergeConfig мержит импортированную конфигурацию поверх текущей
func mergeConfig(current *types.LoadBalancerConfig, imported *types.LoadBalancerConfig) {
	if imported.LoadBalancer.Host != "" {
		current.LoadBalancer.Host = imported.LoadBalancer.Host
	}
	if imported.LoadBalancer.Port != 0 {
		current.LoadBalancer.Port = imported.LoadBalancer.Port
	}
	if imported.LoadBalancer.APIPort != 0 {
		current.LoadBalancer.APIPort = imported.LoadBalancer.APIPort
	}
	if imported.LoadBalancer.TLSHost != "" {
		current.LoadBalancer.TLSHost = imported.LoadBalancer.TLSHost
	}
	if imported.LoadBalancer.TLSPort != 0 {
		current.LoadBalancer.TLSPort = imported.LoadBalancer.TLSPort
	}
	if len(imported.LoadBalancer.TrustedProxies) > 0 {
		current.LoadBalancer.TrustedProxies = imported.LoadBalancer.TrustedProxies
	}
	if len(imported.LoadBalancer.ClientIPHeaders) > 0 {
		current.LoadBalancer.ClientIPHeaders = imported.LoadBalancer.ClientIPHeaders
	}
	if len(imported.Backends) > 0 {
		current.Backends = imported.Backends
	}
	if imported.BackendEngine != "" {
		current.BackendEngine = imported.BackendEngine
	}
	if imported.Balancing.Algorithm != "" {
		current.Balancing.Algorithm = imported.Balancing.Algorithm
	}
	if imported.Balancing.OperatingMode != "" {
		current.Balancing.OperatingMode = imported.Balancing.OperatingMode
	}
	// Булевые поля и числовые — обновляем, если отличаются от нулевых
	if imported.Balancing.ModelAffinity {
		current.Balancing.ModelAffinity = true
	}
	if imported.Balancing.SessionStickiness {
		current.Balancing.SessionStickiness = true
	}
	if imported.Balancing.UseEnhancedScoring {
		current.Balancing.UseEnhancedScoring = true
	}
	if imported.Balancing.HealthCheckInterval != 0 {
		current.Balancing.HealthCheckInterval = imported.Balancing.HealthCheckInterval
	}
	if imported.Balancing.MetricsInterval != 0 {
		current.Balancing.MetricsInterval = imported.Balancing.MetricsInterval
	}
	if imported.Balancing.RequestTimeout != 0 {
		current.Balancing.RequestTimeout = imported.Balancing.RequestTimeout
	}
	if imported.Balancing.FirstByteTimeout != 0 {
		current.Balancing.FirstByteTimeout = imported.Balancing.FirstByteTimeout
	}
	if imported.Balancing.StreamingIdleTimeout != 0 {
		current.Balancing.StreamingIdleTimeout = imported.Balancing.StreamingIdleTimeout
	}
	if imported.Balancing.QueueTimeout != 0 {
		current.Balancing.QueueTimeout = imported.Balancing.QueueTimeout
	}
	if imported.Balancing.QueueMaxSize != 0 {
		current.Balancing.QueueMaxSize = imported.Balancing.QueueMaxSize
	}
	if imported.Balancing.QueueWorkers != 0 {
		current.Balancing.QueueWorkers = imported.Balancing.QueueWorkers
	}
	if imported.Balancing.SessionTTL != 0 {
		current.Balancing.SessionTTL = imported.Balancing.SessionTTL
	}
	if imported.Resources.GPU.MaxUsagePercent != 0 {
		current.Resources.GPU.MaxUsagePercent = imported.Resources.GPU.MaxUsagePercent
	}
	if imported.Resources.GPU.MaxVRAMUsagePercent != 0 {
		current.Resources.GPU.MaxVRAMUsagePercent = imported.Resources.GPU.MaxVRAMUsagePercent
	}
	if imported.Resources.GPU.MaxTemperature != 0 {
		current.Resources.GPU.MaxTemperature = imported.Resources.GPU.MaxTemperature
	}
	if imported.Resources.CPU.MaxUsagePercent != 0 {
		current.Resources.CPU.MaxUsagePercent = imported.Resources.CPU.MaxUsagePercent
	}
	if imported.Resources.Memory.MaxUsagePercent != 0 {
		current.Resources.Memory.MaxUsagePercent = imported.Resources.Memory.MaxUsagePercent
	}
	if imported.Resources.Disk.MinFreeMB != 0 {
		current.Resources.Disk.MinFreeMB = imported.Resources.Disk.MinFreeMB
	}
	if imported.Balancing.ModelReplication.Enabled {
		current.Balancing.ModelReplication = imported.Balancing.ModelReplication
	}
	if imported.Balancing.RpcCoordinator.Enabled {
		current.Balancing.RpcCoordinator = imported.Balancing.RpcCoordinator
	}
	if imported.Balancing.VirtualModels.Enabled {
		current.Balancing.VirtualModels = imported.Balancing.VirtualModels
	}
	if imported.Balancing.DistInference.Enabled {
		current.Balancing.DistInference = imported.Balancing.DistInference
	}
	if imported.Balancing.Prewarm.Enabled {
		current.Balancing.Prewarm = imported.Balancing.Prewarm
	}
}