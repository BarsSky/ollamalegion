package api

import (
	"encoding/json"
	"net/http"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// configDefaults хранит значения по умолчанию для полного сброса конфига
func configDefaults(cfg *types.LoadBalancerConfig) {
	// LoadBalancer defaults
	if cfg.LoadBalancer.Host == "" {
		cfg.LoadBalancer.Host = "0.0.0.0"
	}
	if cfg.LoadBalancer.Port == 0 {
		cfg.LoadBalancer.Port = 18080
	}
	if cfg.LoadBalancer.APIPort == 0 {
		cfg.LoadBalancer.APIPort = 18081
	}
	if cfg.LoadBalancer.TLSPort == 0 {
		cfg.LoadBalancer.TLSPort = 8443
	}
	if cfg.LoadBalancer.StatePath == "" {
		cfg.LoadBalancer.StatePath = "data/state.json"
	}

	// TLS defaults
	if cfg.TLS.MinVersion == "" {
		cfg.TLS.MinVersion = "TLS12"
	}
	if cfg.TLS.CertFile == "" {
		cfg.TLS.CertFile = "certs/server.crt"
	}
	if cfg.TLS.KeyFile == "" {
		cfg.TLS.KeyFile = "certs/server.key"
	}

	// Auth defaults
	if cfg.Auth.HeaderName == "" {
		cfg.Auth.HeaderName = "X-API-Token"
	}

	// API Rate Limiting defaults
	if cfg.API.RateLimit == 0 {
		cfg.API.RateLimit = 100
	}
	if cfg.API.RateBurst == 0 {
		cfg.API.RateBurst = 200
	}

	// Balancing defaults
	if cfg.Balancing.Algorithm == "" {
		cfg.Balancing.Algorithm = types.AlgorithmResourceAware
	}
	if cfg.Balancing.HealthCheckInterval == 0 {
		cfg.Balancing.HealthCheckInterval = 10
	}
	if cfg.Balancing.MetricsInterval == 0 {
		cfg.Balancing.MetricsInterval = 5
	}
	if cfg.Balancing.RequestTimeout == 0 {
		cfg.Balancing.RequestTimeout = 120
	}
	if cfg.Balancing.QueueTimeout == 0 {
		cfg.Balancing.QueueTimeout = 300
	}
	if cfg.Balancing.QueueMaxSize == 0 {
		cfg.Balancing.QueueMaxSize = 100
	}
	if cfg.Balancing.QueueWorkers == 0 {
		cfg.Balancing.QueueWorkers = 4
	}
	// ModelAffinity и SessionStickiness — true по умолчанию
	cfg.Balancing.ModelAffinity = true
	cfg.Balancing.SessionStickiness = true
	cfg.Balancing.UseEnhancedScoring = true

	// Resource limits defaults
	if cfg.Resources.GPU.MaxUsagePercent == 0 {
		cfg.Resources.GPU.MaxUsagePercent = 90.0
	}
	if cfg.Resources.GPU.MaxVRAMUsagePercent == 0 {
		cfg.Resources.GPU.MaxVRAMUsagePercent = 85.0
	}
	if cfg.Resources.GPU.MaxTemperature == 0 {
		cfg.Resources.GPU.MaxTemperature = 85
	}
	if cfg.Resources.CPU.MaxUsagePercent == 0 {
		cfg.Resources.CPU.MaxUsagePercent = 80.0
	}
	if cfg.Resources.Memory.MaxUsagePercent == 0 {
		cfg.Resources.Memory.MaxUsagePercent = 85.0
	}
	if cfg.Resources.Disk.MinFreeMB == 0 {
		cfg.Resources.Disk.MinFreeMB = 10240
	}

	// Logging defaults
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "json"
	}

	// Model Replication defaults
	mr := &cfg.Balancing.ModelReplication
	if mr.DefaultMinInstances == 0 {
		mr.DefaultMinInstances = 1
	}
	if mr.DefaultMaxInstances == 0 {
		mr.DefaultMaxInstances = 3
	}
	if mr.IdleUnloadAfter == "" {
		mr.IdleUnloadAfter = "10m"
	}

	// RPC Coordinator defaults
	rc := &cfg.Balancing.RpcCoordinator
	if rc.WorkerPort == 0 {
		rc.WorkerPort = 18050
	}
	if rc.Timeout == "" {
		rc.Timeout = "30s"
	}
	if rc.Protocol == "" {
		rc.Protocol = "http"
	}
	if rc.MaxRetries == 0 {
		rc.MaxRetries = 3
	}

	// Distributed Inference defaults
	di := &cfg.Balancing.DistInference
	if di.GrpcPort == 0 {
		di.GrpcPort = 19000
	}
}

// ============================================================
// Cluster & Config API handlers
// ============================================================

// buildVirtualModelsResponse собирает плоский ответ для VirtualModels
// (фронтенд ожидает coordMode/timeout на верхнем уровне, а не в Models[].Coordination)
func buildVirtualModelsResponse(vm types.VirtualModelsConfig) map[string]interface{} {
	resp := map[string]interface{}{
		"enabled":   vm.Enabled,
		"coordMode": "sequential",
		"timeout":   30000,
	}
	if len(vm.Models) > 0 {
		m := vm.Models[0]
		if m.Coordination.Mode != "" {
			resp["coordMode"] = m.Coordination.Mode
		}
		if m.Coordination.TimeoutMs > 0 {
			resp["timeout"] = m.Coordination.TimeoutMs
		}
	}
	return resp
}

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
		response := map[string]interface{}{
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
			"backendEngine":       string(s.config.BackendEngine),
			// RPC-режимы: детальные настройки для RESET с сервера
			"modelReplication": map[string]interface{}{
				"enabled":             s.config.Balancing.ModelReplication.Enabled,
				"defaultMinInstances": s.config.Balancing.ModelReplication.DefaultMinInstances,
				"defaultMaxInstances": s.config.Balancing.ModelReplication.DefaultMaxInstances,
				"idleUnloadAfter":     s.config.Balancing.ModelReplication.IdleUnloadAfter,
			},
			"rpcCoordinator": map[string]interface{}{
				"enabled":        s.config.Balancing.RpcCoordinator.Enabled,
				"coordinatorURL": s.config.Balancing.RpcCoordinator.CoordinatorURL,
				"workerPort":     s.config.Balancing.RpcCoordinator.WorkerPort,
				"protocol":       s.config.Balancing.RpcCoordinator.Protocol,
				"timeout":        s.config.Balancing.RpcCoordinator.Timeout,
				"maxRetries":     s.config.Balancing.RpcCoordinator.MaxRetries,
			},
			"virtualModels": buildVirtualModelsResponse(s.config.Balancing.VirtualModels),
			"distInference": map[string]interface{}{
				"enabled":  s.config.Balancing.DistInference.Enabled,
				"grpcPort": s.config.Balancing.DistInference.GrpcPort,
			},
			// Session 17 (2026-07-27): возвращаем llamaCpp-секцию, чтобы WebUI
			// мог сразу отобразить текущие runtime-значения (включая override-эффект).
			"llamaCpp": s.config.LlamaCpp,
		}
		s.writeJSON(w, http.StatusOK, response)
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
			BackendEngine       string   `json:"backendEngine"`
			// Session 17 (2026-07-27): глобальные настройки llama.cpp (per-model
			// profile и per-backend CppWorkerConfig имеют приоритет). Pointer
			// чтобы отличить "не прислано" от "прислано как zero value".
			LlamaCpp *types.LlamaCppConfig `json:"llamaCpp,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   "Invalid request body",
			})
			return
		}

		// Track which fields were updated (for response)
		updatedFields := make([]string, 0, 10)

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
			updatedFields = append(updatedFields, "algorithm")
		}

		if req.ModelAffinity != nil {
			s.config.Balancing.ModelAffinity = *req.ModelAffinity
			updatedFields = append(updatedFields, "modelAffinity")
		}
		if req.SessionStickiness != nil {
			s.config.Balancing.SessionStickiness = *req.SessionStickiness
			updatedFields = append(updatedFields, "sessionStickiness")
		}
		if req.UseEnhancedScoring != nil {
			s.config.Balancing.UseEnhancedScoring = *req.UseEnhancedScoring
			updatedFields = append(updatedFields, "useEnhancedScoring")
		}
		if req.GPUUsage != nil {
			s.config.Resources.GPU.MaxUsagePercent = *req.GPUUsage
			updatedFields = append(updatedFields, "gpuMaxUsage")
		}
		if req.VRAMUsage != nil {
			s.config.Resources.GPU.MaxVRAMUsagePercent = *req.VRAMUsage
			updatedFields = append(updatedFields, "vramMaxUsage")
		}
		if req.CPUUsage != nil {
			s.config.Resources.CPU.MaxUsagePercent = *req.CPUUsage
			updatedFields = append(updatedFields, "cpuMaxUsage")
		}
		if req.RAMUsage != nil {
			s.config.Resources.Memory.MaxUsagePercent = *req.RAMUsage
			updatedFields = append(updatedFields, "ramMaxUsage")
		}
		if req.MinFreeDisk != nil {
			s.config.Resources.Disk.MinFreeMB = uint64(*req.MinFreeDisk)
			updatedFields = append(updatedFields, "minFreeDisk")
		}
		if req.Initialized != nil {
			s.config.Initialized = *req.Initialized
			updatedFields = append(updatedFields, "initialized")
		}

		// --- Backend Engine Switch (Ollama / llama.cpp) ---
		if req.BackendEngine != "" {
			switch req.BackendEngine {
			case "llama_cpp":
				s.config.BackendEngine = types.EngineLlamaCPP
			case "ollama_api":
				s.config.BackendEngine = types.EngineOllamaAPI
			default:
				s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"error":   "Invalid backendEngine. Valid: ollama_api, llama_cpp",
				})
				return
			}
			updatedFields = append(updatedFields, "backendEngine")
			logger.Get().Infow("clusterConfigHandler: backendEngine changed",
				"requested", req.BackendEngine,
				"saved", string(s.config.BackendEngine),
			)
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
			updatedFields = append(updatedFields, "operatingMode")
		}

		// Session 17 (2026-07-27): применяем глобальные настройки llama.cpp.
		// LlamaCpp используется как fallback в 3-tier resolver'е (n_ctx, batch_size,
		// gpu_layers и т.п.) — приоритет: per-request body > per-model profile >
		// per-backend CppWorkerConfig > глобальный LlamaCpp > cppworker default.
		// ВАЖНО: эта секция отвечает за ДЕФОЛТЫ для всех бэкендов, которые не
		// имеют per-backend CppWorkerConfig. Изменения здесь применяются на лету —
		// reload загруженных моделей не требуется (cppworker читает свой config
		// при следующем load).
		//
		// ОГРАНИЧЕНИЯ типов.LlamaCppConfig: эта структура НЕ содержит
		// idleUnloadMinutes / splitMode / metricsRetention (они живут только в
		// cppbackend.Config). Если нужно их менять через balancer — пиши в
		// напрямую cppworker'у через /api/v1/cppworker/config/update (per-backend).
		//
		// Session 17 P.2: PERSISTENCE. В bundled compose config.json монтируется
		// :ro, поэтому s.configSaver() молча падает. Чтобы WebUI-изменения
		// llamaCpp ПЕРСИСТИЛИСЬ, пишем в runtime-override файл в named volume
		// `balancer_data:/app/data/runtime-overrides/llama-cpp.json`. Sidecar-файл
		// содержит ПОЛНУЮ копию LlamaCppConfig (replace semantics, не merge).
		// При старте balancer Load+Apply восстанавливает состояние.
		if req.LlamaCpp != nil {
			ll := req.LlamaCpp
			// Применяем к in-memory config (частичный update).
			applyLlamaCppPartialUpdate(&s.config.LlamaCpp, ll)
			updatedFields = append(updatedFields, "llamaCpp")
			// ПЕРСИСТИРУЕМ в sidecar-файл (если store доступен).
			// Sidecar хранит ПОЛНУЮ копию (replace semantics), поэтому мы
			// читаем текущий override (если есть) и мерджим с partial update,
			// чтобы не потерять поля, которых не было в WebUI-форме.
			if s.overridesStore != nil && s.overridesStore.IsEnabled() {
				if err := s.persistLlamaCppOverride(&s.config.LlamaCpp); err != nil {
					logger.Get().Warnw("clusterConfigHandler: failed to persist llamaCpp override",
						"error", err)
					// Не откатываем in-memory изменение — но сообщаем warning.
					updatedFields = append(updatedFields, "llamaCpp_persist_warning")
				}
			}
			logger.Get().Infow("clusterConfigHandler: llamaCpp global defaults updated",
				"contextLength", s.config.LlamaCpp.ContextLength,
				"kvCacheType", s.config.LlamaCpp.KVCacheType,
				"ropeScalingType", s.config.LlamaCpp.RopeScalingType,
				"numGpuLayers", s.config.LlamaCpp.NumGPULayers,
			)
		}

		// Сохраняем конфигурацию на диск при ЛЮБОМ изменении
		if s.configSaver != nil {
			if err := s.configSaver(); err != nil {
				logger.Get().Warnw("failed to save config after cluster config update", "error", err)
			}
		}

		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"updated": updatedFields,
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
				"backendEngine":       string(s.config.BackendEngine),
				// Session 17: возвращаем текущее состояние LlamaCpp, чтобы WebUI
				// мог сразу отобразить обновлённые значения без дополнительного GET.
				"llamaCpp": s.config.LlamaCpp,
			},
			"message": "Cluster configuration updated successfully",
		})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// configResetHandler — POST /api/v1/config/reset — сброс конфигурации к заводским значениям
// с учётом текущего типа бэкенда (Ollama / llama.cpp).
func (s *Server) configResetHandler(w http.ResponseWriter, r *http.Request) {
	// recover от паники — отдаём 500 с деталями
	defer func() {
		if rec := recover(); rec != nil {
			logger.Get().Errorw("configResetHandler: PANIC recovered", "panic", rec)
			s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"error":   "Internal server error during reset",
			})
		}
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Сохраняем текущий BackendEngine — тип бэкенда не сбрасываем
	currentEngine := s.config.BackendEngine
	logger.Get().Infow("configResetHandler: starting reset",
		"currentEngine", currentEngine,
	)

	// Создаём полностью чистый конфиг и применяем дефолты
	newCfg := &types.LoadBalancerConfig{}
	configDefaults(newCfg)

	// Восстанавливаем BackendEngine и operatingMode согласно типу бэкенда
	newCfg.BackendEngine = currentEngine
	if currentEngine == types.EngineLlamaCPP {
		newCfg.Balancing.OperatingMode = "virtual_router"
		newCfg.Balancing.VirtualModels.Enabled = true
	} else {
		newCfg.Balancing.OperatingMode = "standard"
		// standard — никакие RPC-режимы не включаем
	}

	// Флаг Initialized — false, чтобы Setup Wizard показался при следующем заходе
	newCfg.Initialized = false

	logger.Get().Debugw("configResetHandler: defaults applied",
		"algorithm", newCfg.Balancing.Algorithm,
		"operatingMode", newCfg.Balancing.OperatingMode,
		"backendEngine", newCfg.BackendEngine,
		"initialized", newCfg.Initialized,
	)

	// Заменяем текущий конфиг в Server (сброс в памяти — всегда)
	s.config.LoadBalancer = newCfg.LoadBalancer
	s.config.TLS = newCfg.TLS
	s.config.Auth = newCfg.Auth
	s.config.API = newCfg.API
	s.config.Balancing = newCfg.Balancing
	s.config.Resources = newCfg.Resources
	s.config.Logging = newCfg.Logging
	s.config.Initialized = newCfg.Initialized
	s.config.Backends = nil // явно очищаем
	s.config.BackendEngine = newCfg.BackendEngine

	logger.Get().Infow("configResetHandler: config replaced in memory, saving to disk")

	// Сохраняем на диск — best-effort, не блокируем сброс при ошибке
	var saveWarning string
	if s.configSaver != nil {
		if err := s.configSaver(); err != nil {
			logger.Get().Errorw("configResetHandler: failed to save config to disk (reset applied in memory)", "error", err)
			saveWarning = "Configuration reset applied in memory but could not be saved to disk: " + err.Error()
		}
	} else {
		logger.Get().Warnw("configResetHandler: configSaver is nil — config will NOT be persisted to disk!")
		saveWarning = "Configuration reset applied in memory only (saver not configured)"
	}

	logger.Get().Infow("configResetHandler: configuration reset to defaults",
		"backendEngine", s.config.BackendEngine,
		"operatingMode", s.config.Balancing.OperatingMode,
		"saveWarning", saveWarning,
	)

	// Возвращаем результирующий конфиг
	response := map[string]interface{}{
		"success": true,
		"config": map[string]interface{}{
			"algorithm":           s.config.Balancing.Algorithm,
			"modelAffinity":       s.config.Balancing.ModelAffinity,
			"sessionStickiness":   s.config.Balancing.SessionStickiness,
			"useEnhancedScoring":  s.config.Balancing.UseEnhancedScoring,
			"gpuMaxUsage":         s.config.Resources.GPU.MaxUsagePercent,
			"vramMaxUsage":        s.config.Resources.GPU.MaxVRAMUsagePercent,
			"cpuMaxUsage":         s.config.Resources.CPU.MaxUsagePercent,
			"ramMaxUsage":         s.config.Resources.Memory.MaxUsagePercent,
			"minFreeDisk":         s.config.Resources.Disk.MinFreeMB,
			"operatingMode":       s.config.Balancing.OperatingMode,
			"initialized":         s.config.Initialized,
			"backendEngine":       s.config.BackendEngine,
		},
		"message": "Configuration reset to default values successfully",
	}
	if saveWarning != "" {
		response["warning"] = saveWarning
	}
	s.writeJSON(w, http.StatusOK, response)
}
// applyLlamaCppPartialUpdate применяет non-zero поля из src в dst.
//   - int/float скаляры: применяются только если src > 0 (защита от случайного
//     обнуления при partial-формах WebUI).
//   - bool/string поля: применяются ВСЕГДА (false = явный сброс через WebUI,
//     "" = сброс для KVCacheType/RopeScalingType).
//   - TensorSplit (slice): применяется только если прислали непустой массив
//     ИЛИ если прислали строковое представление.
func applyLlamaCppPartialUpdate(dst *types.LlamaCppConfig, src *types.LlamaCppConfig) {
	if src.NumGPULayers != 0 {
		dst.NumGPULayers = src.NumGPULayers
	}
	if src.ContextLength != 0 {
		dst.ContextLength = src.ContextLength
	}
	if src.BatchSize != 0 {
		dst.BatchSize = src.BatchSize
	}
	// bool-поля — всегда (WebUI шлёт актуальное состояние).
	dst.FlashAttention = src.FlashAttention
	dst.NUMA = src.NUMA
	dst.UseMMap = src.UseMMap
	dst.UseMLock = src.UseMLock
	dst.AutoGpuDistribution = src.AutoGpuDistribution
	dst.NoMemoryMap = src.NoMemoryMap
	dst.NoKVOffload = src.NoKVOffload
	// String-поля — всегда.
	dst.Strategy = src.Strategy
	dst.KVCacheType = src.KVCacheType
	dst.RopeScalingType = src.RopeScalingType
	if src.RopeScalingFactor != 0 {
		dst.RopeScalingFactor = src.RopeScalingFactor
	}
	if src.RopeFreqBase != 0 {
		dst.RopeFreqBase = src.RopeFreqBase
	}
	if src.RopeFreqScale != 0 {
		dst.RopeFreqScale = src.RopeFreqScale
	}
	// YaRN-параметры.
	dst.YarnExtFactor = src.YarnExtFactor
	dst.YarnAttnFactor = src.YarnAttnFactor
	if src.YarnBetaFast != 0 {
		dst.YarnBetaFast = src.YarnBetaFast
	}
	if src.YarnBetaSlow != 0 {
		dst.YarnBetaSlow = src.YarnBetaSlow
	}
	// Multi-GPU.
	if src.MainGPU != 0 {
		dst.MainGPU = src.MainGPU
	}
	dst.RPCBackend = src.RPCBackend
	// TensorSplit.
	if len(src.TensorSplit) > 0 {
		dst.TensorSplit = src.TensorSplit
		dst.TensorSplitStr = src.TensorSplitStr
	} else if src.TensorSplitStr != "" {
		dst.TensorSplitStr = src.TensorSplitStr
	}
	// Performance.
	if src.NThreads != 0 {
		dst.NThreads = src.NThreads
	}
	if src.RMSNormEps != 0 {
		dst.RMSNormEps = src.RMSNormEps
	}
	if src.MaxConcurrentReqs != 0 {
		dst.MaxConcurrentReqs = src.MaxConcurrentReqs
	}
}

// persistLlamaCppOverride сохраняет in-memory LlamaCppConfig в sidecar-файл.
// Sidecar содержит ПОЛНУЮ копию (не partial). При загрузке мы делаем replace,
// поэтому в sidecar-файле должно быть всё, что в данный момент в in-memory.
//
// Если override-файл уже существует, мы берём его как base и применяем к нему
// partial update от текущего in-memory. Это позволяет WebUI-формам, которые
// отправляют только часть полей (например, "Save" вкладки "General"), не
// затирать поля, заданные через другую форму (например, "RoPE & YaRN").
func (s *Server) persistLlamaCppOverride(currentInMemory *types.LlamaCppConfig) error {
	if s.overridesStore == nil || !s.overridesStore.IsEnabled() {
		return nil
	}
	// Загружаем существующий override (если есть) — он может содержать
	// поля, которые не были в WebUI-форме (partial update pattern).
	// exists=false означает "первый save", base = in-memory (уже содержит
	// все поля после partial update).
	existing, exists, err := s.overridesStore.LoadLlamaCpp()
	if err != nil {
		return err
	}
	if !exists {
		existing = &types.LlamaCppConfig{}
	}
	// Копируем — это replace semantics для sidecar-файла.
	final := *currentInMemory
	_ = existing // existing пока не используется — partial update делается
	//              через applyLlamaCppPartialUpdate в handler'е, in-memory уже
	//              содержит merged state. Sidecar хранит целиком.
	return s.overridesStore.SaveLlamaCpp(&final)
}