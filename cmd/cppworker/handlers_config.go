// handlers_config.go — CppWorker runtime configuration handlers (get, update, reload).
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
	if v, ok := updates["defaultNThreads"]; ok {
		if threads, ok := v.(float64); ok {
			currentConfig.DefaultNThreads = int(threads)
			applied = append(applied, "defaultNThreads")
		}
	}
	envPath := *envFile
	if envPath == "" {
		envPath = "/app/.env"
	}
	if err := currentConfig.SaveConfigToDotEnv(envPath); err != nil {
		logger.Get().Warnw("failed to save config to .env", "path", envPath, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "updated", "applied": applied})
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
	// RAM fallback flags не сохраняются в cppbackend.Config, но при reload
	// из .env мы перечитываем их, чтобы runtime-конфиг соответствовал env.
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
