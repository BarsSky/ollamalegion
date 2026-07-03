// handlers_config.go ? CppWorker runtime configuration handlers (get, update, reload).
package main

import (
	"encoding/json"
	"flag"
	"net/http"
	"os"
	"strconv"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// CppWorker Config handlers
// ============================================================

var currentConfig *cppbackend.Config

func handleCppWorkerGetConfig(w http.ResponseWriter, r *http.Request) {
	if currentConfig == nil {
		writeError(w, http.StatusServiceUnavailable, "backend not initialized yet")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"config":      currentConfig,
		"nodeName":    os.Getenv("NODE_NAME"),
		"nodeLabels":  os.Getenv("NODE_LABELS"),
		"balancerUrl": os.Getenv("BALANCER_URL"),
		"uptime":      time.Since(uptimeStart).String(),
	})
}

// handleCppWorkerRuntimeConfig ? GET /api/v1/cppworker/config/runtime.
//
// ?????????? ?????? ??????? ??????????? ??????? ? ?? ???????????? ???????????
// (ContextSize, GPULayers, BatchSize, FlashAttnType, NUMA, UseMmap, SizeBytes,
// NLayers, NEmbd, NKvHeads, GGUFContextLength). ??? ??, ??? WebUI ??????????
// ????? ? default-?????????? ?? /api/v1/cppworker/config, ????? ????????
// ????? "default: 8192, runtime: 32768 (gemma-4)" ? ??? ????????? ?????????.
//
// Thread-safe: ?????????? backend.ListModels() ? RLock.
func handleCppWorkerRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	if backend == nil {
		writeError(w, http.StatusServiceUnavailable, "backend not initialized yet")
		return
	}
	loaded := backend.ListModels()

	// ??????????? ModelInfo ? JSON-????????????? ????? ? ????????? ??????? ?????
	// (snake_case ??? ????????????? ? WebUI/balancer).
	models := make([]map[string]interface{}, 0, len(loaded))
	for _, m := range loaded {
		models = append(models, map[string]interface{}{
			"name":               m.Name,
			"path":               m.Path,
			"state":              m.State,
			"architecture":       m.Architecture,
			"n_layers":           m.NLayers,
			"n_heads":            m.NHeads,
			"n_kv_heads":         m.NKvHeads,
			"n_embd":             m.NEmbd,
			"n_vocab":            m.NVocab,
			"context_size":       m.ContextSize,
			"gguf_context_length": m.GGUFContextLength,
			"size_bytes":         m.SizeBytes,
			"loaded_at":          m.LoadedAt,
			"gpu_count":          m.GPUCount,
			"gpu_layers":         m.GPULayers,
			"tensor_split":       m.TensorSplit,
			"batch_size":         m.BatchSize,
			"flash_attn_type":    m.FlashAttnType,
			"numa":               m.NUMA,
			"use_mmap":           m.UseMmap,
			"active_queries":     m.ActiveQueries,
			"total_queries":      m.TotalQueries,
			"last_used_at":       m.LastUsedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"loaded_models": models,
		"count":         len(models),
	})
}

func handleCppWorkerUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "use PUT")
		return
	}
	if currentConfig == nil {
		writeError(w, http.StatusServiceUnavailable, "backend not initialized yet")
		return
	}
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	applied := []string{}
	if v, ok := updates["defaultCtxSize"]; ok {
		if ctx, ok := v.(float64); ok && ctx >= 256 {
			currentConfig.DefaultCtxSize = int(ctx)
			applied = append(applied, "defaultCtxSize")
		}
	}
	if v, ok := updates["defaultBatchSize"]; ok {
		if batch, ok := v.(float64); ok && batch >= 1 {
			currentConfig.DefaultBatchSize = int(batch)
			applied = append(applied, "defaultBatchSize")
		}
	}
	if v, ok := updates["defaultGpuLayers"]; ok {
		if gpu, ok := v.(float64); ok {
			currentConfig.DefaultGPULayers = int(gpu)
			applied = append(applied, "defaultGpuLayers")
		}
	}
	if v, ok := updates["defaultFlashAttnType"]; ok {
		if fa, ok := v.(float64); ok {
			currentConfig.DefaultFlashAttnType = int(fa)
			applied = append(applied, "defaultFlashAttnType")
		}
	}
	if v, ok := updates["defaultNuma"]; ok {
		if numa, ok := v.(bool); ok {
			currentConfig.DefaultNUMA = numa
			applied = append(applied, "defaultNuma")
		}
	}
	if v, ok := updates["defaultUseMmap"]; ok {
		if mmap, ok := v.(bool); ok {
			currentConfig.DefaultUseMmap = mmap
			applied = append(applied, "defaultUseMmap")
		}
	}
	if v, ok := updates["defaultNThreads"]; ok {
		if threads, ok := v.(float64); ok {
			currentConfig.DefaultNThreads = int(threads)
			applied = append(applied, "defaultNThreads")
		}
	}
	// ????????? ? config/cppworker-defaults.json ? ?????? ???????? ??????.
	// ?????? ? .env ???????: .env ?? ??????????? ? bundled compose, ?
	// ???????????? ???????? ????? JSON ? .env ???????? ? ???????????.
	if err := saveConfigToDefaultsFile(currentConfig); err != nil {
		logger.Get().Warnw("failed to save config to cppworker-defaults.json", "error", err)
	} else {
		logger.Get().Infow("config saved to cppworker-defaults.json",
			"defaultCtxSize", currentConfig.DefaultCtxSize,
			"defaultGpuLayers", currentConfig.DefaultGPULayers,
			"defaultUseMmap", currentConfig.DefaultUseMmap)
	}

	// ???? 1: auto-reload ??????????? ??????? ? ?????? defaults, ???? ??????????
	// ????????? ????????. ??? ????????? ????????? ????? WebUI ????????? n_ctx=32768
	// ? ??? ??????????? ?????? ????? ?????? ? ??? ??????? POST /api/models/reload.
	reloadStarted := []string{}
	reloadFailed := []string{}
	if backend != nil && hasReloadedDefaults(applied) {
		reloadStarted, reloadFailed = reloadAllLoadedWithDefaults()
		// ?????????? RAM fallback attempts ? ????????? ??????????, ???? ?????? ??????????.
		resetAllReloadAttempts()
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "updated",
		"applied":        applied,
		"reload_started": reloadStarted,
		"reload_failed":  reloadFailed,
	})
}

// hasReloadedDefaults ? true, ???? ????? ??????????? ????? ???? ?????????,
// ??????? ?????? ?? ???????? ? ??????? reload ??????.
func hasReloadedDefaults(applied []string) bool {
	for _, k := range applied {
		switch k {
		case "defaultCtxSize", "defaultBatchSize", "defaultGpuLayers",
			"defaultFlashAttnType", "defaultNuma", "defaultUseMmap", "defaultNThreads":
			return true
		}
	}
	return false
}

// reloadAllLoadedWithDefaults ? ????????????? ?????? ??????????? ??????
// ? ?????? ?????????? ??????????? (DefaultCtxSize, DefaultBatchSize ? ?.?.).
// ????????? reload ? ????????? ??????????? (?????? ?????? ??????????)
// ? ?????????? ????? ??????? ?????????? ? ???????.
//
// ?????????? backend.LoadModelWithOpts ? reload ????? Unload + LoadModelWithOpts.
// ???? ?????? ??? ????????? ? ???? ?? ??????????? ? reload ????????????
// (backend.LoadModelWithOpts ?????? "already_loaded" ??? ??????????? ??????).
func reloadAllLoadedWithDefaults() ([]string, []string) {
	if backend == nil || currentConfig == nil {
		return nil, []string{"backend or config not initialized"}
	}
	loaded := backend.ListModels()
	started := make([]string, 0, len(loaded))
	failed := make([]string, 0)
	for _, m := range loaded {
		if m.State != cppbackend.StateLoaded {
			continue
		}
		opts := cppbackend.LoadModelOpts{
			GPULayers:     currentConfig.DefaultGPULayers,
			ContextSize:   currentConfig.DefaultCtxSize,
			BatchSize:     currentConfig.DefaultBatchSize,
			FlashAttnType: currentConfig.DefaultFlashAttnType,
			NUMA:          currentConfig.DefaultNUMA,
			UseMmap:       currentConfig.DefaultUseMmap,
			TensorSplit:   m.TensorSplit,
		}
		// Use adaptive SelectStrategy if available (tries f16→q8_0→q4_0)
		if *autoOffload {
			if globalEnv != nil {
				env := globalEnv.Get()
				ggufMeta := cppbackend.GGUFModelMeta{
					Architecture: m.Architecture,
					NLayers:      m.NLayers,
					NEmbd:        m.NEmbd,
					NHeads:       m.NHeads,
					NKvHeads:     m.NKvHeads,
					SizeBytes:    int64(m.SizeBytes),
				}
				strategy := SelectStrategy(&env, "", ggufMeta, m.ContextSize, -2, currentConfig)
				opts.GPULayers = strategy.GPULayers
				opts.KVCacheType = strategy.KVCacheType
				opts.UseMmap = strategy.UseMmap
			} else {
				opts.GPULayers = calculateOptimalGPULayersForModel(m, "")
			}
		}
		// ????????? reload ? ???????? ? ?? ????????? ????? WebUI ?? ??????.
		go func(name string, modelPath string, loadOpts cppbackend.LoadModelOpts) {
			_, cancel := contextWithTimeout(120 * time.Second)
			defer cancel()
			if err := backend.UnloadModel(name); err != nil {
				logger.Get().Warnw("handleCppWorkerUpdateConfig: unload failed",
					"model", name, "error", err)
				return
			}
			if err := backend.LoadModelWithOpts(name, modelPath, loadOpts); err != nil {
				logger.Get().Warnw("handleCppWorkerUpdateConfig: reload with new defaults failed",
					"model", name, "error", err)
			} else {
				logger.Get().Infow("handleCppWorkerUpdateConfig: reloaded with new defaults",
					"model", name,
					"context_size", loadOpts.ContextSize,
					"gpu_layers", loadOpts.GPULayers)
			}
		}(m.Name, m.Path, opts)
		started = append(started, m.Name)
	}
	return started, failed
}

// resetAllReloadAttempts ? ?????????? ??????? RAM fallback attempts ??? ???? ???????.
// ?????????? ????? apply config ? ?????? ????? reload-loop ?????? ???????????.
func resetAllReloadAttempts() {
	// ramFallbackAttempts ? sync.Map ? per-model ??????????.
	// ????? ?????? ??????????? map ????? ???????: ???????????? ??? ?????.
	ramFallbackAttempts.Range(func(key, value interface{}) bool {
		ramFallbackAttempts.Delete(key)
		return true
	})
}

func handleCppWorkerReloadConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	envPath := *envFile
	if envPath == "" {
		envPath = "/app/.env"
	}
	if _, err := os.Stat(envPath); err != nil {
		writeError(w, http.StatusNotFound, "env file not found: "+envPath)
		return
	}
	if err := cppbackend.LoadDotEnvFile(envPath); err != nil {
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	newCfg := cppbackend.LoadConfigFromEnv()
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			newCfg.Port = *port
		case "models-dir":
			newCfg.ModelsDir = *modelsDir
		case "ctx-size":
			newCfg.DefaultCtxSize = *ctxSize
		case "batch-size":
			newCfg.DefaultBatchSize = *batchSize
		case "gpu-layers":
			newCfg.DefaultGPULayers = *gpuLayers
		case "flash-attn":
			newCfg.DefaultFlashAttnType = *flashAttn
		case "numa":
			newCfg.DefaultNUMA = *numa
		case "no-mmap":
			newCfg.DefaultUseMmap = !*noMmap
		}
	})
	// RAM fallback flags ?? ??????????? ? cppbackend.Config, ?? ??? reload
	// ?? .env ?? ???????????? ??, ????? runtime-?????? ?????????????? env.
	if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_N_CTX"); envVal != "" {
		*ramFallbackNCtx = parseBoolEnv(envVal)
	}
	if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_GPU_LAYERS"); envVal != "" {
		if v, err := strconv.Atoi(envVal); err == nil && v >= -1 {
			*ramFallbackGpuLayers = v
		}
	}
	if envVal := os.Getenv("CPPWORKER_RAM_FALLBACK_MAX_N_CTX"); envVal != "" {
		if v, err := strconv.Atoi(envVal); err == nil && v >= 512 {
			*ramFallbackMaxNCtx = v
		}
	}
	*currentConfig = newCfg
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "reloaded", "config": currentConfig})
}

// saveConfigToDefaultsFile ????????? ??????? ???????????? ?
// config/cppworker-defaults.json ? ?????? ???????? ?????? ???
// ????????? ?????????? CppWorker.
func saveConfigToDefaultsFile(cfg *cppbackend.Config) error {
	defaultsPath := "config/cppworker-defaults.json"
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(defaultsPath, data, 0644)
}

