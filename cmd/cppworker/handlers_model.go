package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Model management handlers
// ============================================================

// handleLoadModel ? POST /api/models/load (? /load). ????????? ?????? ? VRAM.
func handleLoadModel(w http.ResponseWriter, r *http.Request) {
	// Round 36 Phase 3: method check per contract Section 9.2.
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req loadModelRequest
	// Round 36 Phase 2: strict JSON decoder.
	if err := types.DecodeJSONRequest(r.Body, types.MaxRequestBodyBytes, &req); err != nil {
		switch {
		case errors.Is(err, types.ErrBodyEmpty):
			writeError(w, http.StatusBadRequest, "empty request body")
		case errors.Is(err, types.ErrBodyTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		default:
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	modelPath := req.Path
	modelName := req.Name

	// Round 32 #14 (2026-08-11): strip .gguf extension from model name.
	// User screenshot showed две одинаковых модели в webui:
	//   gemma-4-E4B-it-Q4_K_M
	//   gemma-4-E4B-it-Q4_K_M.gguf
	// Root cause: balancer (or webui) отправляет name с .gguf, cppworker
	// сохраняет as-is. Fix: TrimSuffix ВСЕГДА — basename без .gguf
	// это canonical model name для inference / dedup / metrics.
	modelName = strings.TrimSuffix(modelName, ".gguf")

	// ???????? ???? ??? HF-????????? ??????? (??????? hf:)
	if strings.HasPrefix(modelName, "hf:") && backend.HFDownloader() != nil {
		parts := strings.TrimPrefix(modelName, "hf:")
		if localPath, err := backend.HFDownloader().GetLocalPath(parts); err == nil && localPath != "" {
			modelPath = localPath
		}
	}

	// ???? ???? ?? ????? ????, ???? ????? ModelManager
	if modelPath == "" {
		modelPath = resolveModelPath(modelName)
	}

	// Round 19 (2026-07-10): fallbacks from currentConfig.Default* so that
	// CPPWORKER_GPU_LAYERS / CPPWORKER_CTX_SIZE / ... env vars are honored
	// by load handlers. Without this, *gpuLayers flag default (-1 = "all layers
	// on GPU") wins over the env value, and 21GB Q4_K_M models OOM on 24GB A10.
	defGPULayers := *gpuLayers
	defCtxSize := *ctxSize
	defBatchSize := *batchSize
	defFlashAttn := *flashAttn
	defNUMA := *numa
	defUseMmap := !*noMmap
	if currentConfig != nil {
		if currentConfig.DefaultGPULayers != 0 || os.Getenv("CPPWORKER_GPU_LAYERS") != "" {
			defGPULayers = currentConfig.DefaultGPULayers
		}
		if currentConfig.DefaultCtxSize != 0 || os.Getenv("CPPWORKER_CTX_SIZE") != "" {
			defCtxSize = currentConfig.DefaultCtxSize
		}
		if currentConfig.DefaultBatchSize != 0 || os.Getenv("CPPWORKER_BATCH_SIZE") != "" {
			defBatchSize = currentConfig.DefaultBatchSize
		}
		if currentConfig.DefaultFlashAttnType != 0 || os.Getenv("CPPWORKER_FLASH_ATTN_TYPE") != "" {
			defFlashAttn = currentConfig.DefaultFlashAttnType
		}
		if currentConfig.DefaultNUMA || os.Getenv("CPPWORKER_NUMA") != "" {
			defNUMA = currentConfig.DefaultNUMA
		}
		if currentConfig.DefaultUseMmap || os.Getenv("CPPWORKER_USE_MMAP") != "" {
			defUseMmap = currentConfig.DefaultUseMmap
		}
	}

	// Build load options (Round 19: env-driven defaults).
	// Phase 8 P.4: TensorSplit + SplitMode defaults from currentConfig
	// (CPPWORKER_TENSOR_SPLIT / CPPWORKER_SPLIT_MODE env vars or config file).
	// Falls back to request value if set explicitly.
	defTensorSplit := req.TensorSplit
	defSplitMode := 0
	if req.SplitMode != nil {
		defSplitMode = *req.SplitMode
	}
	if currentConfig != nil {
		if len(currentConfig.DefaultTensorSplit) > 0 {
			defTensorSplit = currentConfig.DefaultTensorSplit
		}
		if currentConfig.DefaultSplitMode != 0 || os.Getenv("CPPWORKER_SPLIT_MODE") != "" {
			if v := currentConfig.DefaultSplitMode; v >= -1 && v <= 3 {
				if v == -1 {
					defSplitMode = 0 // bridge: 0 = use llama.cpp default
				} else {
					defSplitMode = v
				}
			}
		}
	}
	opts := cppbackend.LoadModelOpts{
		GPULayers:     defaultIntPtr(req.GPULayers, defGPULayers),
		ContextSize:   defaultIntPtr(req.ContextSize, defCtxSize),
		BatchSize:     defaultIntPtr(req.BatchSize, defBatchSize),
		FlashAttnType: defaultIntPtr(req.FlashAttnType, defFlashAttn),
		NUMA:          defaultBoolPtr(req.NUMA, defNUMA),
		UseMmap:       defaultBoolPtr(req.UseMmap, defUseMmap),
		TensorSplit:   defTensorSplit,
		SplitMode:     defSplitMode,
	}
	// Round 44 (2026-08-19) R43 regression fix: apply extended runtime params
	// (kvCacheType, parallel, override-tensors) the same way handleLoadWithParams
	// does. Without this, the legacy /api/models/load endpoint silently drops
	// kvCacheType in the auto-reload path triggered by the balancer.
	if req.KVCacheType != nil && *req.KVCacheType != "" {
		if isValidKVCacheType(*req.KVCacheType) {
			opts.KVCacheType = *req.KVCacheType
		} else {
			logger.Get().Warnw("handleLoadModel: ignoring invalid kvCacheType",
				"name", modelName, "kvCacheType", *req.KVCacheType,
				"validValues", []string{"f16", "q8_0", "q4_0"})
		}
	}
	if req.Parallel != nil && *req.Parallel > 0 {
		opts.Parallel = *req.Parallel
	}
	if req.OverrideTensor != nil && *req.OverrideTensor != "" {
		opts.OverrideTensor = *req.OverrideTensor
	}
	if len(req.OverrideTensors) > 0 && len(req.OverrideTensors) == len(req.OverrideTensorBufts) {
		opts.OverrideTensors = req.OverrideTensors
		opts.OverrideTensorBufts = req.OverrideTensorBufts
		logger.Get().Infow("handleLoadModel: override-tensors (parallel arrays) applied",
			"name", modelName, "count", len(req.OverrideTensors))
	}

	logger.Get().Infow("loading model",
		"name", modelName, "path", modelPath,
		"gpuLayers", opts.GPULayers, "ctxSize", opts.ContextSize,
		"batchSize", opts.BatchSize, "flashAttnType", opts.FlashAttnType,
		"numa", opts.NUMA, "tensorSplit", opts.TensorSplit)

	// ?????????? ?????? ?????? ????????? ??????? ? ???????????? ??? ??????
	// ??????? ???????? ??? concurrent load (??. ????).
	loadStart := time.Now()

	// Round 24 (2026-08-04): parse sync/async load params.
	//   ?wait=true  → блокирующий (legacy Ollama clients), max waitTimeoutMs
	//   ?wait=false → async (default), возврат 202 + Location немедленно
	// Динамическая оценка estimateLoadTimeMs() даёт клиенту реальное время,
	// а не фиксированные 60/120/180s — gemma-4 (5GB) реально загружается
	// 60-90s, что не влезает в дефолтные таймауты curl/OpenWebUI.
	waitSync, waitTimeoutMs := parseLoadWaitParams(r)
	estimatedMs := estimateLoadTimeMs(modelPath, opts.ContextSize)

	// Per-model blocking load
	lockOk, lockErr := backend.TryLockLoad(modelName)
	if lockErr != nil {
		// ?????? ??? ????????? ? ?????????, ????????? ?? ?????????.
		if info, getErr := backend.GetModel(modelName); getErr == nil {
			if info.Path == modelPath && sameLoadOptions(*info, opts) {
				logger.Get().Infow("model already loaded with same parameters", "name", modelName)
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status": "already_loaded",
					"model":  info,
				})
				return
			}
			// ????????? ?????????? ? ????????? ? ?????????????.
			logger.Get().Infow("reloading model because parameters changed",
				"name", modelName, "oldPath", info.Path, "newPath", modelPath)
			if unloadErr := backend.UnloadModel(modelName); unloadErr != nil {
				logger.Get().Errorw("failed to unload model before reload", "name", modelName, "error", unloadErr)
				writeError(w, http.StatusInternalServerError, "unload before reload failed: "+unloadErr.Error())
				return
			}
			lockOk, lockErr = backend.TryLockLoad(modelName)
			if lockErr != nil {
				logger.Get().Errorw("race: model appeared after unload", "name", modelName)
				writeError(w, http.StatusInternalServerError, "concurrent load race after unload")
				return
			}
		} else {
			lockOk = true
		}
	}

	if !lockOk {
		// ?????? ???????? ??? ?????? ??? ??????. ???????? ?????????? ? ???
		// ????????? ?????, ????? ????????/?????? ?????????? ????????????
		// POST /api/models/load ? ?????? ???????? 503, ???? ?????? ???????
		// ?????????? ????? ????????? ??????.
		logger.Get().Infow("model is already being loaded by another request; waiting", "name", modelName)
		if backend.WaitForLoad(modelName) {
			if info, getErr := backend.GetModel(modelName); getErr == nil {
				logger.Get().Infow("handleLoadModel: model loaded by concurrent request",
					"name", modelName, "duration_ms", time.Since(loadStart).Milliseconds())
				if !waitSync {
					// Async mode: even if other request finished, return 202 so client
					// knows to use progressUrl pattern. Model is actually loaded now.
					writeLoadAccepted(w, r, modelName, modelPath,
						int64(info.SizeBytes), estimatedMs, info)
					return
				}
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status":         "loaded_by_other",
					"model":          info,
					"loadDurationMs": time.Since(loadStart).Milliseconds(),
				})
				return
			}
		}
		if !waitSync {
			// Async mode: return 202 with current state (loading in progress).
			lm := cppbackend.ModelInfo{
				Name:  modelName,
				Path:  modelPath,
				State: cppbackend.StateLoading,
			}
			writeLoadAccepted(w, r, modelName, modelPath, 0, estimatedMs, lm)
			return
		}
		writeLoadingResponse(w, modelName, errModelIsLoading)
		return
	}

	// Round 24 (2026-08-04): динамический timeout.
	// В async-режиме (?wait=false, default) возвращаем 202 + Location сразу,
	// реальная загрузка идёт в background goroutine, не привязанной к r.Context().
	// Это решает проблему gemma-4 (5GB, 60-90s load): клиент больше не
	// отваливается по таймауту, т.к. HTTP-запрос завершается за <100ms.
	if !waitSync {
		// Round 32 #8 (2026-08-10): dedup-by-path check ПЕРЕД writeLoadAccepted.
		// Без этого async path возвращал 202 + spawn'ил background goroutine
		// которая делала dedup check — но response уже отправлен, dedup
		// логировался как error в фоне. Пользователь видел "loading" статус
		// в UI, который никогда не становился "loaded" (потому что реально
		// load не шёл). Теперь: если same path уже загружен — return 200 OK
		// с status=already_loaded сразу, без spawn'а background goroutine.
		if existingName, existingInfo, found := backend.GetModelByPath(modelPath); found {
			logger.Get().Infow("handleLoadModel: dedup by path (async path, pre-check)",
				"requested_name", modelName, "existing_name", existingName, "path", modelPath)
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"status":  "already_loaded",
				"model":   existingInfo,
				"dedup":   true,
				"message": fmt.Sprintf("model already loaded as %q (same file path)", existingName),
			})
			return
		}
		var sizeBytes int64
		if fi, statErr := os.Stat(modelPath); statErr == nil {
			sizeBytes = fi.Size()
		}
		lm := cppbackend.ModelInfo{
			Name:             modelName,
			Path:             modelPath,
			State:            cppbackend.StateLoading,
			GPULayers:        opts.GPULayers,
			BatchSize:        opts.BatchSize,
			FlashAttnType:    opts.FlashAttnType,
			NUMA:             opts.NUMA,
			UseMmap:          opts.UseMmap,
			LoadingStartedAt: time.Now(),
			LoadingSizeBytes: sizeBytes,
		}
		writeLoadAccepted(w, r, modelName, modelPath, sizeBytes, estimatedMs, lm)
		// Spawn background load. runAsyncLoad handles UnlockLoad, notifyModelLoaded.
		go runAsyncLoad(modelName, modelPath, opts, balancerReg)
		return
	}

	// Sync mode (?wait=true): блокируем на загрузку, но не дольше waitTimeoutMs.
	// CGo-вызов не отменяется, поэтому при таймауте возвращаем 202 — load
	// продолжится в background и клиент сможет дополлить progress.
	loadDone := make(chan error, 1)
	go func() {
		loadDone <- backend.LoadModelWithOpts(modelName, modelPath, opts)
	}()
	timeout := time.NewTimer(time.Duration(waitTimeoutMs) * time.Millisecond)
	defer timeout.Stop()
	select {
	case loadErr := <-loadDone:
		backend.UnlockLoad(modelName)
		if loadErr != nil {
			// Round 32 #8 (2026-08-10): dedicated dedup-by-path error → success
			// с status=already_loaded. Без этого dedup handler возвращал 500
			// и 2x VRAM был занят (gemma-4 vs gemma-4.gguf).
			var dedupErr *cppbackend.AlreadyLoadedAsError
			if errors.As(loadErr, &dedupErr) {
				logger.Get().Infow("handleLoadModel: model already loaded (dedup by path)",
					"requested_name", modelName, "existing_name", dedupErr.ExistingName, "path", dedupErr.Path)
				model, gErr := backend.GetModel(dedupErr.ExistingName)
				if gErr != nil {
					writeError(w, http.StatusInternalServerError, "dedup: "+gErr.Error())
					return
				}
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status":  "already_loaded",
					"model":   model,
					"dedup":   true,
					"message": fmt.Sprintf("model already loaded as %q (same file path)", dedupErr.ExistingName),
				})
				return
			}
			logger.Get().Errorw("failed to load model (sync)", "name", modelName, "error", loadErr)
			writeError(w, http.StatusInternalServerError, "load failed: "+loadErr.Error())
			return
		}
		loadDuration := time.Since(loadStart)

		model, err := backend.GetModel(modelName)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		if balancerReg != nil {
			// Round 34 (2026-08-12): добавили runtime params (kvCacheType,
			// flashAttnType, useMmap) для profile mismatch detection в balancer.
			balancerReg.notifyModelLoaded(modelName, model.SizeBytes, model.ContextSize, model.GPULayers,
				model.KVCacheType, model.FlashAttnType, model.UseMmap)
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":              "loaded",
			"model":               model,
			"loadDurationMs":      loadDuration.Milliseconds(),
			"estimatedLoadTimeMs": estimatedMs,
		})
	case <-timeout.C:
		// Sync timeout: load still running in background goroutine.
		// It will UnlockLoad when done. Return 202 with current state.
		waitedMs := time.Since(loadStart).Milliseconds()
		var sizeBytes int64
		if fi, statErr := os.Stat(modelPath); statErr == nil {
			sizeBytes = fi.Size()
		}
		lm := cppbackend.ModelInfo{
			Name:             modelName,
			Path:             modelPath,
			State:            cppbackend.StateLoading,
			LoadingStartedAt: loadStart,
			LoadingSizeBytes: sizeBytes,
		}
		writeLoadWaitTimeout(w, r, modelName, modelPath, sizeBytes, waitedMs, estimatedMs, lm)
	}
}

// handleLoadWithParams ? POST /api/models/load-with-params.
//
// ??????????? ?????? handleLoadModel ? ??????????????? llama.cpp ???????????:
//   - nThreads       ? CPU-?????? (0 = auto)
//   - parallel       ? ???????????? sequences (??????? ?????? KV-cache VRAM)
//   - kvCacheType    ? F16/Q8_0/Q4_0 (Q8_0 ???????? ~50% KV-cache VRAM)
//   - splitMode      ? layer/row ??? multi-GPU tensor split
//   - overrideTensor ? regex-??????? ??? ??????????????? dtype ????????
//
// ?????????????: ??? ??????? ???? ?? loadModelRequest ???? ???????????
// (json-???? ?????????). ???? ??? ??????????? ???? == nil/0 ?
// ????????? ????????? ????????? ? handleLoadModel.
//
// ?????? ????????? handleLoadModel (race-condition handling, parallel
// requests wait, already_loaded detection) ? ??????? ?????? ? ?????
// ??????? ?????? ?????????? ??? LoadModelOpts.
//
// ?????? ????? ?????????????: ??. internal/balancer/llamacpp_handlers_load.go
// (handleLlamaCppLoad ? ?????????? ???????????? ????? ????? mapstructure).
func handleLoadWithParams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req loadWithParamsRequest
	// Round 36 Phase 2: strict JSON decoder.
	if err := types.DecodeJSONRequest(r.Body, types.MaxRequestBodyBytes, &req); err != nil {
		switch {
		case errors.Is(err, types.ErrBodyEmpty):
			writeError(w, http.StatusBadRequest, "empty request body")
		case errors.Is(err, types.ErrBodyTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		default:
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	modelPath := req.Path
	modelName := req.Name

	// Round 32 #14 (2026-08-11): strip .gguf extension from model name.
	// User screenshot showed две одинаковых модели в webui:
	//   gemma-4-E4B-it-Q4_K_M
	//   gemma-4-E4B-it-Q4_K_M.gguf
	// Root cause: balancer (or webui) отправляет name с .gguf, cppworker
	// сохраняет as-is. Fix: TrimSuffix ВСЕГДА — basename без .gguf
	// это canonical model name для inference / dedup / metrics.
	modelName = strings.TrimSuffix(modelName, ".gguf")

	// ???????? ???? ??? HF-????????? ??????? (??????? hf:)
	if strings.HasPrefix(modelName, "hf:") && backend.HFDownloader() != nil {
		parts := strings.TrimPrefix(modelName, "hf:")
		if localPath, err := backend.HFDownloader().GetLocalPath(parts); err == nil && localPath != "" {
			modelPath = localPath
		}
	}
	if modelPath == "" {
		modelPath = resolveModelPath(modelName)
	}

	// Round 19 (2026-07-10): fallbacks from currentConfig.Default* so that
	// CPPWORKER_GPU_LAYERS / CPPWORKER_CTX_SIZE / ... env vars are honored
	// by load handlers. Without this, *gpuLayers flag default (-1 = "all layers
	// on GPU") wins over the env value, and 21GB Q4_K_M models OOM on 24GB A10.
	defGPULayers := *gpuLayers
	defCtxSize := *ctxSize
	defBatchSize := *batchSize
	defFlashAttn := *flashAttn
	defNUMA := *numa
	defUseMmap := !*noMmap
	if currentConfig != nil {
		if currentConfig.DefaultGPULayers != 0 || os.Getenv("CPPWORKER_GPU_LAYERS") != "" {
			defGPULayers = currentConfig.DefaultGPULayers
		}
		if currentConfig.DefaultCtxSize != 0 || os.Getenv("CPPWORKER_CTX_SIZE") != "" {
			defCtxSize = currentConfig.DefaultCtxSize
		}
		if currentConfig.DefaultBatchSize != 0 || os.Getenv("CPPWORKER_BATCH_SIZE") != "" {
			defBatchSize = currentConfig.DefaultBatchSize
		}
		if currentConfig.DefaultFlashAttnType != 0 || os.Getenv("CPPWORKER_FLASH_ATTN_TYPE") != "" {
			defFlashAttn = currentConfig.DefaultFlashAttnType
		}
		if currentConfig.DefaultNUMA || os.Getenv("CPPWORKER_NUMA") != "" {
			defNUMA = currentConfig.DefaultNUMA
		}
		if currentConfig.DefaultUseMmap || os.Getenv("CPPWORKER_USE_MMAP") != "" {
			defUseMmap = currentConfig.DefaultUseMmap
		}
	}

	// Build load options (Round 19: env-driven defaults).
	// Phase 8 P.4: TensorSplit + SplitMode defaults from currentConfig
	// (CPPWORKER_TENSOR_SPLIT / CPPWORKER_SPLIT_MODE env vars or config file).
	// Falls back to request value if set explicitly.
	defTensorSplit2 := req.TensorSplit
	defSplitMode2 := 0
	if req.SplitMode != nil {
		defSplitMode2 = *req.SplitMode
	}
	if currentConfig != nil {
		if len(currentConfig.DefaultTensorSplit) > 0 {
			defTensorSplit2 = currentConfig.DefaultTensorSplit
		}
		if currentConfig.DefaultSplitMode != 0 || os.Getenv("CPPWORKER_SPLIT_MODE") != "" {
			if v := currentConfig.DefaultSplitMode; v >= -1 && v <= 3 {
				if v == -1 {
					defSplitMode2 = 0
				} else {
					defSplitMode2 = v
				}
			}
		}
	}
	opts := cppbackend.LoadModelOpts{
		GPULayers:     defaultIntPtr(req.GPULayers, defGPULayers),
		ContextSize:   defaultIntPtr(req.ContextSize, defCtxSize),
		BatchSize:     defaultIntPtr(req.BatchSize, defBatchSize),
		FlashAttnType: defaultIntPtr(req.FlashAttnType, defFlashAttn),
		NUMA:          defaultBoolPtr(req.NUMA, defNUMA),
		UseMmap:       defaultBoolPtr(req.UseMmap, defUseMmap),
		TensorSplit:   defTensorSplit2,
		SplitMode:     defSplitMode2,
		// Round 15.1: per-model override для batched parallel inference.
		// nil = inherit global cfg.EnableBatchedParallel. non-nil = explicit choice.
		EnableBatchedParallel: req.EnableBatchedParallel,
		// Round 17 (2026-07-31): per-model override для reasoning parser routing.
		// nil = inherit global cfg.DefaultEnableReasoning. non-nil = explicit choice.
		// Решает bug plans/bug-2026-07-31-reasoning-not-routed.md.
		EnableReasoning: req.EnableReasoning,
	}
	if req.NThreads != nil && *req.NThreads > 0 {
		opts.NThreads = *req.NThreads
	}
	if req.Parallel != nil && *req.Parallel > 0 {
		opts.Parallel = *req.Parallel
	}
	if req.KVCacheType != nil && *req.KVCacheType != "" {
		// Session 16: KVCacheType ? ????????? ???????? "f16"/"q8_0"/"q4_0".
		// ?????????? ???????? ????????????? (cppbackend.LoadModelWithOpts
		// fallback'??? ?? default F16 ???? ?????? ?? ??????, ?? ???????????).
		if isValidKVCacheType(*req.KVCacheType) {
			opts.KVCacheType = *req.KVCacheType
		} else {
			logger.Get().Warnw("load-with-params: ignoring invalid kvCacheType",
				"name", modelName, "kvCacheType", *req.KVCacheType,
				"validValues", []string{"f16", "q8_0", "q4_0"})
		}
	}
	if req.SplitMode != nil && *req.SplitMode >= 0 {
		opts.SplitMode = *req.SplitMode
	}
	if req.OverrideTensor != nil && *req.OverrideTensor != "" {
		opts.OverrideTensor = *req.OverrideTensor
	}
	// Round 7: prefer parallel-array OverrideTensors if present and matching length.
	if len(req.OverrideTensors) > 0 && len(req.OverrideTensors) == len(req.OverrideTensorBufts) {
		opts.OverrideTensors = req.OverrideTensors
		opts.OverrideTensorBufts = req.OverrideTensorBufts
		logger.Get().Infow("override-tensors (parallel arrays) applied",
			"name", modelName, "count", len(req.OverrideTensors))
	}

	// Round 26 (2026-08-06): pull-based profile sync from balancer.
	// Применяется ПОСЛЕ env defaults но ДО AutoTuneNCtx — чтобы AutoTuneNCtx
	// мог ещё уменьшить n_ctx/gpu_layers, если профиль задал слишком много
	// (partial offload fallback).
	if profileSyncer != nil {
		if prof := profileSyncer.applyProfileOnLoad(modelName); prof != nil {
			if !applyProfileToLoadRequest(prof, &opts.ContextSize, &opts.BatchSize, &opts.GPULayers, &opts.FlashAttnType, &opts.KVCacheType) {
				logger.Get().Warnw("handleLoadWithParams: profile is disabled, refusing load",
					"name", modelName, "profile", prof)
				writeError(w, http.StatusForbidden, "model "+modelName+" is disabled by profile")
				return
			}
			logger.Get().Infow("handleLoadWithParams: applied profile (pull-based sync from balancer)",
				"name", modelName,
				"profileCtxSize", prof.ContextLength,
				"profileBatchSize", prof.BatchSize,
				"profileGPULayers", prof.NumGPULayers,
				"profileKVCacheType", prof.KVCacheType)
		}
	}

	// Round 27 follow-up (v0.5.14 follow-up #3): apply SelectStrategy (memory auto-tune)
	// BEFORE the actual load. До этого момента AutoTuneNCtx вызывался только в
	// handleReloadModel — при auto-load через handleLoadWithParams (которую
	// использует балансер для executeLlamaCppLoad) AutoTuneNCtx не срабатывал.
	// Это приводило к OOM/crash при загрузке больших моделей с высокими n_ctx.
	//
	// Стратегия:
	//   1. Читаем GGUF header (lazy, уже кэширован в ModelManager).
	//   2. SelectStrategy подбирает n_ctx/gpu_layers под доступную VRAM + RAM.
	//   3. Если клиент явно передал n_ctx и оно влезает → не меняем.
	//   4. Если не влезает → уменьшаем n_ctx (partial offload / cpu-only fallback).
	//
	// Skip если клиент явно задал OverrideTensors (per-tensor routing сложнее
	// пересчитывать, не ломаем opt-in advanced flow).
	if len(req.OverrideTensors) == 0 && autoTuneNCtxOnLoadEnabled && req.ContextSize != nil {
		tunedGPULayers := 0
		if req.GPULayers != nil {
			tunedGPULayers = *req.GPULayers
		}
		tunedOpts, _ := calculateLazyLoadOpts(modelName, *req.ContextSize, tunedGPULayers, currentConfig)
		if tunedOpts.ContextSize > 0 && tunedOpts.ContextSize != *req.ContextSize {
			logger.Get().Infow("handleLoadWithParams: applying AutoTuneNCtx from GGUF meta",
				"name", modelName,
				"requested_n_ctx", *req.ContextSize,
				"tuned_n_ctx", tunedOpts.ContextSize,
				"requested_gpu_layers", tunedGPULayers,
				"tuned_gpu_layers", tunedOpts.GPULayers)
			*req.ContextSize = tunedOpts.ContextSize
			opts.ContextSize = tunedOpts.ContextSize
		}
		if tunedOpts.GPULayers != 0 && tunedOpts.GPULayers != tunedGPULayers {
			if req.GPULayers != nil {
				*req.GPULayers = tunedOpts.GPULayers
			}
			opts.GPULayers = tunedOpts.GPULayers
		}
		if tunedOpts.UseMmap {
			opts.UseMmap = true
		}
	}

	logger.Get().Infow("loading model with extended params",
		"name", modelName, "path", modelPath,
		"gpuLayers", opts.GPULayers, "ctxSize", opts.ContextSize,
		"batchSize", opts.BatchSize, "flashAttnType", opts.FlashAttnType,
		"numa", opts.NUMA, "tensorSplit", opts.TensorSplit,
		"nThreads", opts.NThreads, "parallel", opts.Parallel,
		"kvCacheType", opts.KVCacheType, "splitMode", opts.SplitMode,
		"overrideTensor", opts.OverrideTensor)

	loadStart := time.Now()

	// Round 24 (2026-08-04): parse sync/async load params (same as handleLoadModel).
	//   ?wait=true  → блокирующий (legacy Ollama clients), max waitTimeoutMs
	//   ?wait=false → async (default), возврат 202 + Location немедленно
	waitSync, waitTimeoutMs := parseLoadWaitParams(r)
	estimatedMs := estimateLoadTimeMs(modelPath, opts.ContextSize)

	lockOk, lockErr := backend.TryLockLoad(modelName)
	if lockErr != nil {
		if info, getErr := backend.GetModel(modelName); getErr == nil {
			if info.Path == modelPath && sameLoadOptions(*info, opts) {
				logger.Get().Infow("model already loaded with same parameters", "name", modelName)
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status": "already_loaded",
					"model":  info,
				})
				return
			}
			logger.Get().Infow("reloading model because parameters changed (load-with-params)",
				"name", modelName, "oldPath", info.Path, "newPath", modelPath)
			if unloadErr := backend.UnloadModel(modelName); unloadErr != nil {
				logger.Get().Errorw("failed to unload model before reload", "name", modelName, "error", unloadErr)
				writeError(w, http.StatusInternalServerError, "unload before reload failed: "+unloadErr.Error())
				return
			}
			lockOk, lockErr = backend.TryLockLoad(modelName)
			if lockErr != nil {
				logger.Get().Errorw("race: model appeared after unload", "name", modelName)
				writeError(w, http.StatusInternalServerError, "concurrent load race after unload")
				return
			}
		} else {
			lockOk = true
		}
	}

	if !lockOk {
		logger.Get().Infow("model is already being loaded by another request; waiting", "name", modelName)
		if backend.WaitForLoad(modelName) {
			if info, getErr := backend.GetModel(modelName); getErr == nil {
				logger.Get().Infow("handleLoadWithParams: model loaded by concurrent request",
					"name", modelName, "duration_ms", time.Since(loadStart).Milliseconds())
				if !waitSync {
					writeLoadAccepted(w, r, modelName, modelPath,
						int64(info.SizeBytes), estimatedMs, info)
					return
				}
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status":         "loaded_by_other",
					"model":          info,
					"loadDurationMs": time.Since(loadStart).Milliseconds(),
				})
				return
			}
		}
		if !waitSync {
			lm := cppbackend.ModelInfo{
				Name:  modelName,
				Path:  modelPath,
				State: cppbackend.StateLoading,
			}
			writeLoadAccepted(w, r, modelName, modelPath, 0, estimatedMs, lm)
			return
		}
		writeLoadingResponse(w, modelName, errModelIsLoading)
		return
	}

	// Round 24 (2026-08-04): динамический timeout (async по умолчанию).
	if !waitSync {
		// Round 32 #8 (2026-08-10): dedup-by-path check ПЕРЕД writeLoadAccepted.
		// См. handleLoadModel для деталей.
		if existingName, existingInfo, found := backend.GetModelByPath(modelPath); found {
			logger.Get().Infow("handleLoadWithParams: dedup by path (async path, pre-check)",
				"requested_name", modelName, "existing_name", existingName, "path", modelPath)
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"status":  "already_loaded",
				"model":   existingInfo,
				"dedup":   true,
				"message": fmt.Sprintf("model already loaded as %q (same file path)", existingName),
			})
			return
		}
		var sizeBytes int64
		if fi, statErr := os.Stat(modelPath); statErr == nil {
			sizeBytes = fi.Size()
		}
		lm := cppbackend.ModelInfo{
			Name:             modelName,
			Path:             modelPath,
			State:            cppbackend.StateLoading,
			GPULayers:        opts.GPULayers,
			BatchSize:        opts.BatchSize,
			FlashAttnType:    opts.FlashAttnType,
			NUMA:             opts.NUMA,
			UseMmap:          opts.UseMmap,
			LoadingStartedAt: time.Now(),
			LoadingSizeBytes: sizeBytes,
		}
		writeLoadAccepted(w, r, modelName, modelPath, sizeBytes, estimatedMs, lm)
		go runAsyncLoad(modelName, modelPath, opts, balancerReg)
		return
	}

	// Sync mode: блокируем до waitTimeoutMs, потом 202.
	//
	// Round 52 (2026-08-24) BUGFIX: inline goroutine ОБЯЗАНА вызвать
	// UnlockLoad через defer после LoadModelWithOpts (success OR error).
	// До этого фикса в timeout-ветке lock утекал — request timeout'ился,
	// background goroutine продолжал грузить, channel в b.loading[name]
	// оставался незакрытым. Следующие load-запросы с тем же именем видели
	// `lockOk=false` от TryLockLoad и уходили в "model is already being
	// loaded; waiting" — до полного завершения background goroutine (60+ сек).
	//
	// Async path (выше, `go runAsyncLoad(...)`) не затрагивается —
	// runAsyncLoad сам вызывает UnlockLoad в конце.
	loadDone := make(chan error, 1)
	go func() {
		// defer UnlockLoad здесь, не в select: select может не
		// выполниться полностью (timeout wins), loadErr не придёт
		// в основной goroutine. defer гарантирует освобождение
		// lock когда LoadModelWithOpts ВЕРНЁТСЯ (успех или ошибка).
		defer func() {
			backend.UnlockLoad(modelName)
		}()
		loadDone <- backend.LoadModelWithOpts(modelName, modelPath, opts)
	}()
	timeout := time.NewTimer(time.Duration(waitTimeoutMs) * time.Millisecond)
	defer timeout.Stop()
	select {
	case loadErr := <-loadDone:
		// background goroutine уже вызвал UnlockLoad через defer
		// (см. R52 fix выше). Дополнительный вызов здесь был бы
		// идемпотентен (delete-if-exists) — оставляем однократный вызов.
		if loadErr != nil {
			// Round 32 #8 (2026-08-10): dedup-by-path → success с status=already_loaded.
			var dedupErr *cppbackend.AlreadyLoadedAsError
			if errors.As(loadErr, &dedupErr) {
				logger.Get().Infow("handleLoadWithParams: model already loaded (dedup by path)",
					"requested_name", modelName, "existing_name", dedupErr.ExistingName, "path", dedupErr.Path)
				model, gErr := backend.GetModel(dedupErr.ExistingName)
				if gErr != nil {
					writeError(w, http.StatusInternalServerError, "dedup: "+gErr.Error())
					return
				}
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status":  "already_loaded",
					"model":   model,
					"dedup":   true,
					"message": fmt.Sprintf("model already loaded as %q (same file path)", dedupErr.ExistingName),
				})
				return
			}
			logger.Get().Errorw("failed to load model (load-with-params sync)", "name", modelName, "error", loadErr)
			writeError(w, http.StatusInternalServerError, "load failed: "+loadErr.Error())
			return
		}
		loadDuration := time.Since(loadStart)

		model, err := backend.GetModel(modelName)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		if balancerReg != nil {
			// Round 34 (2026-08-12): добавили runtime params (kvCacheType,
			// flashAttnType, useMmap) для profile mismatch detection в balancer.
			balancerReg.notifyModelLoaded(modelName, model.SizeBytes, model.ContextSize, model.GPULayers,
				model.KVCacheType, model.FlashAttnType, model.UseMmap)
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":              "loaded",
			"model":               model,
			"loadDurationMs":      loadDuration.Milliseconds(),
			"estimatedLoadTimeMs": estimatedMs,
			"appliedOpts":         opts,
		})
	case <-timeout.C:
		// Sync timeout: load still running in background goroutine.
		waitedMs := time.Since(loadStart).Milliseconds()
		var sizeBytes int64
		if fi, statErr := os.Stat(modelPath); statErr == nil {
			sizeBytes = fi.Size()
		}
		lm := cppbackend.ModelInfo{
			Name:             modelName,
			Path:             modelPath,
			State:            cppbackend.StateLoading,
			LoadingStartedAt: loadStart,
			LoadingSizeBytes: sizeBytes,
		}
		writeLoadWaitTimeout(w, r, modelName, modelPath, sizeBytes, waitedMs, estimatedMs, lm)
	}
}

// handleLoadProgress ? GET /api/models/load/progress?model=<name>
func handleLoadProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("model"))
	if name == "" || name == "*" {
		all := backend.GetLoadingModels()
		out := make([]map[string]interface{}, 0, len(all))
		now := time.Now()
		for _, m := range all {
			elapsed := int64(0)
			if !m.LoadingStartedAt.IsZero() {
				elapsed = now.Sub(m.LoadingStartedAt).Milliseconds()
			}
			out = append(out, map[string]interface{}{
				"name":             m.Name,
				"state":            m.State,
				"loadingStartedAt": m.LoadingStartedAt.UTC().Format(time.RFC3339Nano),
				"loadingSizeBytes": m.LoadingSizeBytes,
				"elapsedMs":        elapsed,
				"error":            m.LoadingError,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"models": out, "count": len(out)})
		return
	}

	m, err := backend.GetModel(name)
	if err != nil {
		for _, lm := range backend.GetLoadingModels() {
			if lm.Name == name {
				elapsed := int64(0)
				if !lm.LoadingStartedAt.IsZero() {
					elapsed = time.Since(lm.LoadingStartedAt).Milliseconds()
				}
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"name": lm.Name, "state": lm.State,
					"loadingStartedAt": lm.LoadingStartedAt.UTC().Format(time.RFC3339Nano),
					"loadingSizeBytes": lm.LoadingSizeBytes, "elapsedMs": elapsed,
					"error": lm.LoadingError,
				})
				return
			}
		}
		writeError(w, http.StatusNotFound, "model not found and not loading: "+name)
		return
	}

	elapsed := int64(0)
	if !m.LoadedAt.IsZero() {
		elapsed = time.Since(m.LoadedAt).Milliseconds()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name": m.Name, "state": m.State,
		"loadedAt":  m.LoadedAt.UTC().Format(time.RFC3339Nano),
		"elapsedMs": elapsed, "sizeBytes": m.SizeBytes, "contextSize": m.ContextSize,
	})
}

func handleUnloadModel(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name query parameter is required")
		return
	}

	// Round 51.6 (2026-08-20): safety check — refuse unload если есть активные
	// inference-запросы (use-after-free в C-bridge: in-flight generate вызов
	// продолжает использовать handle который UnloadModel уже освободил).
	//
	// Без этого check (предыдущее поведение) unload посреди генерации мог
	// вызвать:
	//   - crash в C-bridge (SIGABRT) — handle double-free
	//   - 502 connection refused для клиента посреди SSE стрима
	//   - WebUI показывал "успешная выгрузка" хотя клиент получил EOF
	//
	// Теперь возвращаем 409 Conflict с деталями, либо принимаем ?force=true
	// override (логируется как WARN для последующего postmortem).
	force := r.URL.Query().Get("force") == "true"
	if !force {
		if inflight := backend.InFlight(); inflight != nil {
			if n := inflight.Get(name); n > 0 {
				// Собираем детали активных запросов для user_id list
				var userIDs []string
				if gen := backend.ActiveGenerations(); gen != nil {
					for _, e := range gen.Snapshot() {
						if e.Model == name {
							userIDs = append(userIDs, e.UserID)
						}
					}
				}
				logger.Get().Warnw("unload refused: model has active requests",
					"name", name, "active_queries", n, "user_ids", userIDs)
				writeJSON(w, http.StatusConflict, map[string]interface{}{
					"error":          "model is busy with active inference requests",
					"name":           name,
					"active_queries": n,
					"user_ids":       userIDs,
					"hint":           "wait for in-flight requests to complete, or retry with ?force=true to cancel them (will disconnect active clients)",
				})
				return
			}
		}
	} else {
		// force=true: отменяем все активные генерации для этой модели
		if gen := backend.ActiveGenerations(); gen != nil {
			cancelled := gen.CancelByModel("", name)
			if cancelled > 0 {
				logger.Get().Warnw("unload force=true: cancelled active generations",
					"name", name, "cancelled", cancelled)
			}
		}
	}

	logger.Get().Infow("unloading model", "name", name, "force", force)
	if err := backend.UnloadModel(name); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// Round 34 (2026-08-12) Phase 3: notify balancer что модель unloaded.
	// Без этого lastKnownNCtx в NCtxReloadCoordinator остаётся прежним (после
	// `idle_unload_after` 10m), preflight думает модель загружена с большим
	// n_ctx, не триггерит reload → пользователь получает 502 connection refused
	// (модель не загружена). Cline-сессии ломаются.
	if balancerReg != nil {
		balancerReg.notifyModelUnloaded(name)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unloaded", "name": name})
}

// handleListModels ? GET /api/models.
//
// ???????? 2026-06-26: ?????? ?????????? top-level ???? ? ???????? VRAM/RAM
// ? max n_ctx ??? ?????????????? (preflight_nctx). ?? ???? ???? ????????
// ????? max_vram_n_ctx=0 ? model_max_context=0 ??? ???? ???????
// (env_log.txt ?????? 121, 133) ? ?? ??? ????????? ??????? n_ctx.
//
// ?????? ????????? ??? ?????? ??????????? ??????. ???? ??????? ?????????,
// ????? ??????????? MaxVRAMNCtx (?.?. ?????? VRAM ??????? ????? ????????).
// ???? ??????? ??? ? ?????? ???????????? ??? ????????? ?????? ?? ModelManager.
//
// NB: ??? ???? ? ??????? cppworker'?, ? ?? ?????????? ??????. ??? ??????
// ?????? ???????? ????? ????????? /api/v1/cluster/models/{name}/reload ?
// ????? contextSize ? ? ???? ?????? ?? ??? ???????? n_ctx.
func handleListModels(w http.ResponseWriter, r *http.Request) {
	models := backend.ListModels()
	mm := backend.ModelManager()

	// 1. ???????? VRAM/RAM ?????? ??? ???? ??????????? ???????.
	// ???????? ???????? ??????????? max_vram_n_ctx, ?????? ??? ????????? ???????
	// ???????????? ???? ? VRAM ?? ????? ? ????? ????? ??????? ????? ????.
	var totalVRAMMB, availableVRAMMB, totalRAMMB, availableRAMMB uint64
	maxVRAMNCtx := 0
	maxRAMNCtx := 0
	modelMaxContext := 0

	// Round 37 (2026-08-18): per-model feasible tracking for conservative-profile detection.
	type feasibleAgg struct {
		feasibleMax int
		ggufMax     int
	}
	perModelFeasible := make(map[string]feasibleAgg, len(models))

	for _, m := range models {
		if m.State != cppbackend.StateLoaded {
			continue
		}
		limits := backend.CalculateResourceLimits(m.Name)
		if limits.TotalVRAMMB > totalVRAMMB {
			totalVRAMMB = limits.TotalVRAMMB
		}
		if limits.AvailableVRAMMB > availableVRAMMB {
			availableVRAMMB = limits.AvailableVRAMMB
		}
		if limits.TotalRAMMB > totalRAMMB {
			totalRAMMB = limits.TotalRAMMB
		}
		if limits.AvailableRAMMB > availableRAMMB {
			availableRAMMB = limits.AvailableRAMMB
		}
		if maxVRAMNCtx == 0 || limits.MaxVRAMNCtx < maxVRAMNCtx {
			maxVRAMNCtx = limits.MaxVRAMNCtx
		}
		if maxRAMNCtx == 0 || limits.MaxRAMNCtx < maxRAMNCtx {
			maxRAMNCtx = limits.MaxRAMNCtx
		}
		if modelMaxContext == 0 || limits.ModelMaxContext > modelMaxContext {
			modelMaxContext = limits.ModelMaxContext
		}

		// Round 37: per-model feasible calc. feasible_i = min(VRAM ctx, RAM ctx, GGUFMax).
		feasibleI := limits.MaxVRAMNCtx
		if limits.MaxRAMNCtx > 0 && (feasibleI == 0 || limits.MaxRAMNCtx < feasibleI) {
			feasibleI = limits.MaxRAMNCtx
		}
		if m.GGUFContextLength > 0 && (feasibleI == 0 || m.GGUFContextLength < feasibleI) {
			feasibleI = m.GGUFContextLength
		}
		perModelFeasible[m.Name] = feasibleAgg{
			feasibleMax: feasibleI,
			ggufMax:     m.GGUFContextLength,
		}
	}

	// 2. Fallback: ???? ?? ???? ?????? ?? ????????? ? ????? ?????? ??
	// GGUFModelMeta ?? ????? name (????? ?????? header, ?? ?????? ? llama.cpp).
	// GGUFModelMeta ?? ???????? ContextLength ? ??? ??????? ??????? ?????
	// ??????? cppbackend.ReadGGUFHeader(m.Path) ????????. ????? ?????????
	// modelMaxContext=0, ???????? ????? ?????????? ????????? ????????.
	if maxVRAMNCtx == 0 && mm != nil {
		_ = mm // ?????????: ????? ?????? ???????? 0, preflight ??? ????? ??????????
	}

	// 2a. Fallback: if no models loaded, query bridge directly for live VRAM/RAM.
	// Fixes /api/models returning available_vram_mb:0 when no model is loaded yet.
	if totalVRAMMB == 0 || availableVRAMMB == 0 {
		if dev, err := bridge.GetGPUInfo(0); err == nil && dev != nil {
			totalVRAMMB = uint64(dev.VRAMTotalMB)
			if availableVRAMMB == 0 {
				availableVRAMMB = uint64(dev.VRAMFreeMB)
			}
		}
	}
	if totalRAMMB == 0 || availableRAMMB == 0 {
		if ramGB := cppbackend.GetSystemRAMGB(); ramGB > 0 {
			totalRAMMB = uint64(ramGB) * 1024
			if availableRAMMB == 0 {
				availableRAMMB = totalRAMMB - 4096
			}
		}
	}

	// 3. ???? ?????? ???????? 0 (??? ??????????? ??????, ??? VRAM), ???????
	// ??????? ????? ?????? GGUF ? ???????? ? ?? ??? cppworker ??? ???????
	// ???????? ??????? ????, ? ??? ????????? (preflight ????? ????????).
	// Round 37 (2026-08-18): build per-model enriched list with feasible_max_context.
	enrichedModels := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		entry := map[string]interface{}{
			"name":                m.Name,
			"path":                m.Path,
			"state":               m.State,
			"architecture":        m.Architecture,
			"n_layers":            m.NLayers,
			"n_embd":              m.NEmbd,
			"n_vocab":             m.NVocab,
			"context_size":        m.ContextSize,
			"gguf_context_length": m.GGUFContextLength,
			"size_bytes":          m.SizeBytes,
			"loaded_at":           m.LoadedAt,
			"gpu_count":           m.GPUCount,
			"kv_cache_type":       m.KVCacheType,
		}
		// Round 51.6 (2026-08-20): per-model active_queries counter. WebUI
		// показывает "Generating N requests" badge чтобы оператор не выгружал
		// модель посреди inference (R51.6: handleUnloadModel теперь возвращает
		// 409 если active_queries > 0, см. handleUnloadModel).
		if inflight := backend.InFlight(); inflight != nil {
			entry["active_queries"] = inflight.Get(m.Name)
		}
		if fa, ok := perModelFeasible[m.Name]; ok {
			entry["gguf_max_context"] = fa.ggufMax
			entry["feasible_max_context"] = fa.feasibleMax
		} else {
			entry["gguf_max_context"] = m.GGUFContextLength
			entry["feasible_max_context"] = 0
		}
		enrichedModels = append(enrichedModels, entry)
	}

	// Round 37 (2026-08-18): top-level feasible/GGUF — min across loaded models.
	topFeasible := 0
	topGGUF := 0
	for _, fa := range perModelFeasible {
		if topFeasible == 0 || (fa.feasibleMax > 0 && fa.feasibleMax < topFeasible) {
			topFeasible = fa.feasibleMax
		}
		if fa.ggufMax > topGGUF {
			topGGUF = fa.ggufMax
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"models": enrichedModels,
		"count":  len(models),
		"max_vram_n_ctx":    maxVRAMNCtx,
		"max_ram_n_ctx":     maxRAMNCtx,
		"available_vram_mb": availableVRAMMB,
		"total_vram_mb":     totalVRAMMB,
		"available_ram_mb":  availableRAMMB,
		"total_ram_mb":      totalRAMMB,
		"model_max_context": modelMaxContext,
		"gpu_count":         backend.GetGPUCount(),
		// Round 37 (2026-08-18): auto-adapt fields.
		// feasible_max_context = min(VRAM, RAM, GGUF) — recommended max n_ctx.
		// gguf_max_context     = max GGUF training context — hard upper bound.
		"feasible_max_context": topFeasible,
		"gguf_max_context":     topGGUF,
	})
}

func handleGetModel(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name query parameter is required")
		return
	}
	model, err := backend.GetModel(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, model)
}

// handleListModelsDir ? GET /api/models/files: ?????? .gguf ?????? ? modelsDir.
// === R60.3 (2026-09-04): per-file meta ===
// До R60.3 API возвращал только {name, sizeBytes, modifiedAt} — webui рисовал
// карточку модели с пустыми полями «Размер: -» и «Квантизация: -», потому что
// gguf-renderer-detail.js:232-234 ожидает `m.size` и `m.quantization`. Теперь
// добавляем алиас `size` (равно sizeBytes, для обратной совместимости с webui)
// и `quantization` (парсится из имени файла: `q4km` → `Q4_K_M`, `Q8_0` → `Q8_0`).
// Cppworker's /api/models/files теперь содержит ВСЮ meta для UI, webui больше
// не нужен fallback на filename parsing.
func handleListModelsDir(w http.ResponseWriter, r *http.Request) {
	mm := backend.ModelManager()
	if mm != nil {
		models := mm.ListModels()
		files := make([]map[string]interface{}, 0, len(models))
		for _, m := range models {
			files = append(files, ggufFileMeta(m.Filename, m.SizeBytes, m.ModifiedAt))
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"files": files, "dir": mm.GetModelsDir(), "count": len(files),
		})
		return
	}
	var files []map[string]interface{}
	entries, err := os.ReadDir(*modelsDir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"files": []interface{}{}, "dir": *modelsDir,
		})
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, ggufFileMeta(name, info.Size(), info.ModTime()))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"files": files, "dir": *modelsDir, "count": len(files),
	})
}

// ggufFileMeta — собирает meta для одной записи /api/models/files.
// Используется в обоих циклах handleListModelsDir (через ModelManager и через
// прямой ReadDir). Возвращает map с алиасами: name + size (alias sizeBytes) +
// quantization (parse из имени) + modifiedAt.
func ggufFileMeta(filename string, size int64, modifiedAt time.Time) map[string]interface{} {
	return map[string]interface{}{
		"name":        filename,
		"size":        size,        // alias for sizeBytes — webui reads m.size
		"sizeBytes":   size,        // canonical name, kept for API stability
		"quantization": parseQuantization(filename),
		"modifiedAt":  modifiedAt.Format(time.RFC3339),
	}
}

// === R60.3: parseQuantization — extract quantization level from filename ===
// Поддерживает llama.cpp naming conventions:
//   - Q4_K_M, Q4_K_S, Q4_0, Q4_1, Q5_K_M, Q8_0, Q2_K, F16, F32, BF16
//   - compressed aliases: q4km → Q4_K_M, q4ks → Q4_K_S, q5_1, q8_0
//   - I-quants (llama.cpp IQ series): IQ1_S, IQ2_XXS, IQ3_S, IQ4_XS
// Returns "" if no recognizable quantization level found.
//
// Strategy: try multiple "candidates" derived from the last dash-separated
// segments of the filename. Filenames like "Qwen3-Instruct-2507-q4km" have
// `q4km` as a single trailing token. "Llama-3-8B-Q4_K_M" has `Q4_K_M` split
// into 3 trailing tokens. We try single-token, 2-token-joined, 3-token-joined
// variants, each against three regexes (standard underscore, compressed, F-quant).
func parseQuantization(filename string) string {
	base := filename
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
	if idx := strings.LastIndex(base, `\`); idx >= 0 {
		base = base[idx+1:]
	}
	base = strings.TrimSuffix(base, ".gguf")
	base = strings.TrimSuffix(base, ".GGUF")
	if base == "" {
		return ""
	}
	parts := strings.Split(base, "-")
	n := len(parts)

	// Build candidates: try longest match first.
	candidates := make([]string, 0, 5)
	if n >= 3 {
		candidates = append(candidates, parts[n-3]+"_"+parts[n-2]+"_"+parts[n-1]) // "Q4_K_M"
		candidates = append(candidates, parts[n-3]+parts[n-2]+parts[n-1])          // "Q4KM"
	}
	if n >= 2 {
		candidates = append(candidates, parts[n-2]+"_"+parts[n-1]) // "Q6_K"
		candidates = append(candidates, parts[n-2]+parts[n-1])     // "Q6K"
	}
	candidates = append(candidates, parts[n-1]) // "q4km", "F16", etc.

	// Standard: Q4_K_M, Q4_0, IQ4_XS, Q4 (kind optional).
	stdRe := regexp.MustCompile(`^(I?Q)(\d+)(_(K_[SML]|K|0|1|S|M|L|XS|XXS))?$`)
	// Compressed: Q4KM, Q6K, Q4K (no underscore between bits and kind).
	cmpRe := regexp.MustCompile(`^(I?Q)(\d+)(KM|KS|KL|KM|KS|KL|0|1|S|M|L|XS|XXS|K)$`)
	// F-quant: F16, BF16, FP16.
	fRe := regexp.MustCompile(`^(F|BF|FP)(\d+)$`)

	for _, cand := range candidates {
		up := strings.ToUpper(cand)
		if m := fRe.FindStringSubmatch(up); m != nil {
			if m[1] == "FP" {
				return "F" + m[2]
			}
			return m[1] + m[2] // F16, BF16
		}
		if m := stdRe.FindStringSubmatch(up); m != nil {
			prefix := m[1] // "Q" or "IQ"
			bits := m[2]
			kind := m[3]
			if kind == "" {
				return prefix + bits
			}
			return prefix + bits + kind
		}
		if m := cmpRe.FindStringSubmatch(up); m != nil {
			prefix := m[1]
			bits := m[2]
			suffix := m[3]
			// Normalize compressed → canonical form.
			switch suffix {
			case "KM":
				return prefix + bits + "_K_M"
			case "KS":
				return prefix + bits + "_K_S"
			case "KL":
				return prefix + bits + "_K_L"
			case "K":
				return prefix + bits + "_K"
			case "XS":
				return prefix + bits + "_XS"
			case "XXS":
				return prefix + bits + "_XXS"
			default:
				return prefix + bits + "_" + suffix
			}
		}
	}
	return ""
}

// handleDeleteModel ? ???????? .gguf ????? ? ????? (DELETE /api/models/delete, /api/delete).
func handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "use POST or DELETE")
		return
	}

	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			name = strings.TrimSpace(req.Name)
		}
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required (JSON body or ?name= query param)")
		return
	}

	filename := name
	if !strings.HasSuffix(strings.ToLower(filename), ".gguf") {
		filename = filename + ".gguf"
	}

	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusServiceUnavailable, "model manager not available")
		return
	}

	modelPath, err := mm.FindModelByPath(filename)
	if err != nil {
		writeError(w, http.StatusNotFound, "model not found: "+err.Error())
		return
	}

	modelName := strings.TrimSuffix(filepath.Base(modelPath), ".gguf")
	if _, getErr := backend.GetModel(modelName); getErr == nil {
		logger.Get().Infow("delete: unloading model from memory before file removal", "name", modelName)
		if unloadErr := backend.UnloadModel(modelName); unloadErr != nil {
			logger.Get().Warnw("delete: unload before delete failed", "name", modelName, "error", unloadErr)
		}
	}

	logger.Get().Infow("deleting model file", "name", modelName, "path", modelPath)
	if err := os.Remove(modelPath); err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "model file does not exist: "+modelPath)
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete model: "+err.Error())
		return
	}

	if _, scanErr := mm.ScanModels(); scanErr != nil {
		logger.Get().Warnw("delete: model rescan failed", "error", scanErr)
	}

	logger.Get().Infow("model deleted", "name", modelName, "path", modelPath)
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "deleted", "name": modelName, "filename": filepath.Base(modelPath),
	})
}

// handleReloadModel ? POST /api/models/reload.
func handleReloadModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req reloadModelRequest
	// Round 36 Phase 2: strict JSON decoder.
	if err := types.DecodeJSONRequest(r.Body, types.MaxRequestBodyBytes, &req); err != nil {
		switch {
		case errors.Is(err, types.ErrBodyEmpty):
			writeError(w, http.StatusBadRequest, "empty request body")
		case errors.Is(err, types.ErrBodyTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		default:
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	// Round 24 (2026-08-04): async reload (?wait=false default) — Bug #1 fix.
	// Reload с новыми n_ctx/gpu_layers инициирует UnloadModel + LoadModelWithOpts.
	// Для gemma-4 (5GB) reload занимает 30-60+ сек, что превышает default
	// curl/JS timeout. Default async: возвращаем 202 + Location, реальный
	// reload идёт в background. Клиент polls /api/models/load/progress.
	waitSync, waitTimeoutMs := parseLoadWaitParams(r)

	current, err := backend.GetModel(req.Name)
	if err != nil {
		writeError(w, http.StatusNotFound, "model not currently loaded: "+err.Error()+
			" ? use /api/models/load to load it first")
		return
	}

	modelPath := current.Path
	if modelPath == "" {
		if mm := backend.ModelManager(); mm != nil {
			if foundPath, ferr := mm.FindModelByPath(req.Name); ferr == nil {
				modelPath = foundPath
			}
		}
	}
	if modelPath == "" {
		writeError(w, http.StatusInternalServerError, "could not resolve model path for "+req.Name)
		return
	}

	// Compute estimated time early so we can return it in 202 response.
	estimatedMs := estimateLoadTimeMs(modelPath, current.ContextSize)

	if req.ContextSize != nil {
		if *req.ContextSize < 256 || *req.ContextSize > 262144 {
			writeError(w, http.StatusBadRequest, "contextSize must be in [256, 262144], got "+fmt.Sprintf("%d", *req.ContextSize))
			return
		}
	}
	if req.BatchSize != nil && *req.BatchSize < 1 {
		writeError(w, http.StatusBadRequest, "batchSize must be >= 1")
		return
	}
	if req.GPULayers != nil && *req.GPULayers < -2 {
		writeError(w, http.StatusBadRequest, "gpuLayers must be >= -2 (-1 = all layers, -2 = AUTO)")
		return
	}

	opts := cppbackend.LoadModelOpts{
		GPULayers:     defaultIntPtr(req.GPULayers, current.GPULayers),
		ContextSize:   defaultIntPtr(req.ContextSize, current.ContextSize),
		BatchSize:     defaultIntPtr(req.BatchSize, current.BatchSize),
		FlashAttnType: defaultIntPtr(req.FlashAttn, current.FlashAttnType),
		NUMA:          defaultBoolPtr(req.NUMA, current.NUMA),
		UseMmap:       defaultBoolPtr(req.UseMmap, current.UseMmap),
		TensorSplit:   current.TensorSplit,
		// Session 16 (2026-06-27): Parallel + KVCacheType ????? /api/models/reload
		// ??? ?????????? Per-Model Profile (parallel + kvCacheType).
		// ?? ????????? (nil ?? ???????) ? inherit ?? ??????? ????????.
		Parallel:    current.Parallel,
		KVCacheType: current.KVCacheType,
	}
	// Round 7: forward MoE override-tensors (parallel arrays) from request
	// body to LoadModelOpts so the bridge applies them to llama_model_params.
	if len(req.OverrideTensors) > 0 && len(req.OverrideTensors) == len(req.OverrideTensorBufts) {
		opts.OverrideTensors = req.OverrideTensors
		opts.OverrideTensorBufts = req.OverrideTensorBufts
	}
	if req.Parallel != nil {
		opts.Parallel = *req.Parallel
	}
	if req.KVCacheType != nil && *req.KVCacheType != "" {
		// ?????????? ? ?????????? ?????????? ???????? (cppbackend fallback'???
		// ?? default F16, ?? ????? ???????????? ???????????? ? ?????).
		if isValidKVCacheType(*req.KVCacheType) {
			opts.KVCacheType = *req.KVCacheType
		} else {
			logger.Get().Warnw("reload: ignoring invalid kvCacheType",
				"name", req.Name, "kvCacheType", *req.KVCacheType,
				"validValues", []string{"f16", "q8_0", "q4_0"})
		}
	}

	// Round 26 (2026-08-06): pull-based profile sync from balancer.
	// При reload запрошенные параметры имеют приоритет над профилем (явный вызов),
	// но если reload идёт без параметров (пустой body) — профиль задаёт дефолты.
	if profileSyncer != nil {
		if prof := profileSyncer.applyProfileOnLoad(req.Name); prof != nil {
			// For reload: apply profile to fields that are still zero-value
			// (i.e. user didn't override them via body).
			if !applyProfileToLoadRequest(prof, &opts.ContextSize, &opts.BatchSize, &opts.GPULayers, &opts.FlashAttnType, &opts.KVCacheType) {
				logger.Get().Warnw("handleReloadModel: profile is disabled, refusing reload",
					"name", req.Name, "profile", prof)
				writeError(w, http.StatusForbidden, "model "+req.Name+" is disabled by profile")
				return
			}
			logger.Get().Infow("handleReloadModel: applied profile (pull-based sync from balancer)",
				"name", req.Name,
				"profileCtxSize", prof.ContextLength,
				"profileBatchSize", prof.BatchSize,
				"profileGPULayers", prof.NumGPULayers,
				"profileKVCacheType", prof.KVCacheType)
		}
	}

	// === Auto-offload: ???? GPULayers=-2 (AUTO) ??? auto_offload ??????? ?
	// ???????????? ??????????? ????? GPU-????? ??? ???????????? n_ctx.
	// ??? ?????? ????????: ??????? ?????? (~18GB) ?? 20GB VRAM ???
	// gpu_layers=-1 ???????? ??? VRAM ? KV-cache ?? ?????????? ?
	// max_vram_n_ctx ????????? ? ????????????? reject'?? ??????.
	// ??? auto_offload gpu_layers ???????????, ????? ????? ?????? ? RAM
	// (mmap), ?????????? VRAM ??? KV-cache ???????? ???????.
	if opts.GPULayers == -2 || (*autoOffload && opts.GPULayers == current.GPULayers) {
		// ??????? ????????? ModelInfo ? ??????????? n_ctx ??? ???????.
		m := *current
		m.ContextSize = opts.ContextSize
		var calculated int
		// Use adaptive SelectStrategy which tries all kvCacheTypes (f16->q8_0->q4_0)
		// and handles MoE, dynamic overhead, and VRAM/RAM limits.
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
			strategy := SelectStrategy(&env, req.Name, ggufMeta, m.ContextSize, -2, currentConfig)
			if strategy.GPULayers >= 0 || strategy.Stage == "fallback_no_meta" {
				// For fallback_no_meta (GGUF header not parsed, e.g. gemma4),
				// keep gpuLayers=-2 (AUTO) — backend's checkVRAMForModel will
				// calculate proper layers with the KV-cache fallback we added.
				// Previously this fell through to calculated=0 → gpuLayers=0 (CPU-only),
				// which made inference unusably slow.
				if strategy.Stage == "fallback_no_meta" {
					calculated = -2
				} else {
					calculated = strategy.GPULayers
					// Round 40 #2 (2026-08-18): respect user-explicit kvCacheType
					// (from reload body or profile). Previously this was an
					// unconditional override `opts.KVCacheType = strategy.KVCacheType`
					// which silently discarded user q4_0/q8_0 choices. See
					// reload_kv_cache_override.go for the full precedence contract.
					applyStrategyKVCacheOverride(&opts, strategy, req.Name)
					opts.UseMmap = strategy.UseMmap
				}
				// Round 7: apply MoE override-tensors from strategy. For Qwen3-A3B
				// (and other MoE) this routes routed-expert tensors to CPU, keeping
				// attention on GPU regardless of gpu_layers.
				if len(strategy.OverrideTensors) > 0 && len(strategy.OverrideTensors) == len(strategy.OverrideTensorBufts) {
					opts.OverrideTensors = strategy.OverrideTensors
					opts.OverrideTensorBufts = strategy.OverrideTensorBufts
					logger.Get().Infow("reload: override-tensors from strategy applied",
						"name", req.Name,
						"count", len(strategy.OverrideTensors))
				}
				logger.Get().Infow("reload: adaptive SelectStrategy applied",
					"name", req.Name,
					"stage", strategy.Stage,
					"gpu_layers", strategy.GPULayers,
					"kv_cache_type", strategy.KVCacheType,
					"n_ctx", strategy.NCtx)
			}
		} else {
			calculated = calculateOptimalGPULayersForModel(m, opts.KVCacheType)
		}
		if calculated > 0 && calculated != opts.GPULayers {
			logger.Get().Infow("reload: auto-offload recalculated gpu_layers",
				"name", req.Name,
				"old_gpu_layers", opts.GPULayers,
				"new_gpu_layers", calculated,
				"n_ctx", opts.ContextSize,
				"model_size_mb", current.SizeBytes/(1024*1024))
			opts.GPULayers = calculated
			opts.UseMmap = true // partial offload ??????? mmap
		}
	}

	// === AutoTuneNCtx: ???? auto_tune_nctx ??????? ? ????????? ???????????
	// n_ctx ? gpu_layers ? ?????? ???????? ????????? VRAM ? RAM.
	// ??? ????????? ??? reload ????????????? ??????? partial offload,
	// ???? ??????????? n_ctx ?? ?????????? ? VRAM ? ???????? gpu_layers.
	if *autoTuneNCtx && opts.ContextSize > current.ContextSize {
		tuned := AutoTuneNCtx(*current, opts.ContextSize, "")
		if tuned.RecommendedNCtx > 0 && tuned.Source != "fallback" {
			logger.Get().Infow("reload: AutoTuneNCtx applied",
				"name", req.Name,
				"requested_n_ctx", opts.ContextSize,
				"tuned_n_ctx", tuned.RecommendedNCtx,
				"tuned_gpu_layers", tuned.RecommendedGPULayers,
				"old_gpu_layers", opts.GPULayers,
				"source", tuned.Source,
				"max_viable_n_ctx", tuned.MaxViableNCtx)
			opts.ContextSize = tuned.RecommendedNCtx
			opts.GPULayers = tuned.RecommendedGPULayers
			opts.UseMmap = tuned.UseMmap
		}
	}

	logger.Get().Infow("reloading model with new params",
		"name", req.Name, "path", modelPath,
		"old_ctx", current.ContextSize, "new_ctx", opts.ContextSize,
		"old_batch", current.BatchSize, "new_batch", opts.BatchSize,
		"old_gpu_layers", current.GPULayers, "new_gpu_layers", opts.GPULayers,
		"use_mmap", opts.UseMmap)

	// ???? force=true ? ?????????? ???????? "params already sufficient".
	// ??? ?????, ????? C-bridge ?? ????? ??????? ???????????? ??????????? n_ctx
	// (effective n_ctx < GGUF native context_length), ? ????????????? ???? force,
	// ????? ????????????? ????????????? ?????? ? ?????? ???????????.
	forceReload := req.Force != nil && *req.Force

	if !forceReload &&
		opts.ContextSize <= current.ContextSize &&
		opts.BatchSize <= current.BatchSize &&
		opts.GPULayers <= current.GPULayers &&
		opts.FlashAttnType == current.FlashAttnType &&
		opts.NUMA == current.NUMA &&
		opts.UseMmap == current.UseMmap {
		logger.Get().Infow("reload: skipping ? current params already sufficient",
			"name", req.Name,
			"current_ctx", current.ContextSize, "requested_ctx", opts.ContextSize)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":              "already_loaded",
			"model":               current,
			"estimatedLoadTimeMs": estimatedMs,
		})
		return
	}

	// Round 24 (2026-08-04) Bug #1 fix: async reload by default.
	// Если wait=false (default), возвращаем 202 + Location и запускаем
	// реальный reload в background goroutine. WebUI settings UI больше
	// не зависает на 30-60+ сек при reload'е reasoning-модели.
	if !waitSync {
		var sizeBytes int64
		if fi, statErr := os.Stat(modelPath); statErr == nil {
			sizeBytes = fi.Size()
		}
		lm := cppbackend.ModelInfo{
			Name:             req.Name,
			Path:             modelPath,
			State:            cppbackend.StateLoading,
			GPULayers:        opts.GPULayers,
			BatchSize:        opts.BatchSize,
			FlashAttnType:    opts.FlashAttnType,
			NUMA:             opts.NUMA,
			UseMmap:          opts.UseMmap,
			LoadingStartedAt: time.Now(),
			LoadingSizeBytes: sizeBytes,
		}
		writeLoadAccepted(w, r, req.Name, modelPath, sizeBytes, estimatedMs, lm)
		// Background reload: UnloadModel + LoadModelWithOpts + notify.
		go runAsyncReload(req.Name, modelPath, opts, current, balancerReg)
		return
	}

	lockOk, lockErr := backend.TryLockReload(req.Name)
	if lockErr != nil {
		logger.Get().Errorw("reload: TryLockReload returned unexpected error",
			"name", req.Name, "error", lockErr)
		writeError(w, http.StatusInternalServerError, "reload lock error: "+lockErr.Error())
		return
	}

	if !lockOk {
		logger.Get().Infow("reload: another goroutine is already loading this model, waiting",
			"name", req.Name)
		if backend.WaitForLoad(req.Name) {
			if model, getErr := backend.GetModel(req.Name); getErr == nil {
				writeJSON(w, http.StatusOK, map[string]interface{}{
					"status": "reloaded_by_other", "model": model,
				})
				return
			}
		}
		writeLoadingResponse(w, req.Name, errModelIsLoading)
		return
	}
	defer backend.UnlockLoad(req.Name)

	// Round 24 (2026-08-04): sync reload (?wait=true) — bound by waitTimeoutMs.
	// Если не успели за waitTimeoutMs — возвращаем 202 + Location, reload продолжится
	// в background (defer UnlockLoad разблокирует после настоящего завершения).
	_ = waitTimeoutMs // explicitly noted: sync path keeps existing blocking behavior
	// для обратной совместимости. CGo не отменяется, поэтому таймаут в этой
	// ветке не режет load — он лишь говорит клиенту "я устал ждать, полли сам".
	// Future enhancement: добавить goroutine + select, как в handleLoadModel.

	// 2026-06-24: graceful reload ??? ?????? ???????? ??????????.
	// ????? UnloadModel ???? ?????????? ???? ???????? inference-????????
	// ? ???? ?????? (InFlight counter, ??. internal/cppbackend/inflight.go).
	// ??? ????? cppworker ???????? HTTP-?????????? ???????? (EOF), ?
	// ?????????????, polling'?????? /api/models ? ???? ??????, ????? RST.
	//
	// ??????? ?? ?????? ? ???? ??????? ?? ??????????? (???????), ?????
	// reload ??????? ? context.WithTimeout ??????? (??. reload watchdog).
	// ??? InFlight.WaitZero ?????????? 100ms polling.
	if inflight := backend.InFlight(); inflight != nil {
		if n := inflight.Get(req.Name); n > 0 {
			logger.Get().Infow("reload: waiting for in-flight inference requests to drain",
				"name", req.Name, "in_flight", n)
		}
		inflight.WaitZero(req.Name, 0) // 0 = ??? ??????
	}

	// ????????????? reload_pending heartbeat ? ????????????? ?????? ???
	// ???? ?? /api/metrics ? ?? ???????? ??????? LoadModel API, ????
	// reload ?? ????????. ??? ????? ? race condition: ?????????????
	// polling'?? ???????????? state=loading, ?????? LoadModel, ? ???
	// ?????? ????????? ? TryLockLoad ???? ?????.
	backend.SetReloadPending(req.Name)
	defer backend.SetReloadPending("")

	unloadStart := time.Now()
	if err := backend.UnloadModel(req.Name); err != nil {
		writeError(w, http.StatusInternalServerError, "unload failed: "+err.Error())
		return
	}
	logger.Get().Infow("reload: model unloaded",
		"name", req.Name, "unload_ms", time.Since(unloadStart).Milliseconds())

	loadStart := time.Now()
	if err := backend.LoadModelWithOpts(req.Name, modelPath, opts); err != nil {
		logger.Get().Errorw("reload: load with new params failed",
			"name", req.Name, "error", err)
		oldOpts := cppbackend.LoadModelOpts{
			GPULayers:     current.GPULayers,
			ContextSize:   current.ContextSize,
			BatchSize:     current.BatchSize,
			FlashAttnType: current.FlashAttnType,
			NUMA:          current.NUMA,
			UseMmap:       current.UseMmap,
			TensorSplit:   current.TensorSplit,
		}
		if rollbackErr := backend.LoadModelWithOpts(req.Name, modelPath, oldOpts); rollbackErr != nil {
			logger.Get().Errorw("reload rollback failed (model is no longer loaded!)",
				"name", req.Name, "rollback_error", rollbackErr)
		}
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	logger.Get().Infow("reload: model loaded with new params",
		"name", req.Name, "load_ms", time.Since(loadStart).Milliseconds())

	model, err := backend.GetModel(req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "reloaded", "name": req.Name,
		"contextSize": opts.ContextSize, "batchSize": opts.BatchSize,
		"gpuLayers": opts.GPULayers, "flashAttnType": opts.FlashAttnType,
		"reloadDurationMs": time.Since(unloadStart).Milliseconds(),
		"model":            model,
	})
}

// ============================================================
// Ollama-compatible model info & management handlers
// ============================================================

func handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	modelMap := make(map[string]map[string]interface{})

	mm := backend.ModelManager()
	if mm != nil {
		fsModels := mm.ListModels()
		for _, fm := range fsModels {
			modelName := fm.Filename
			if strings.HasSuffix(strings.ToLower(modelName), ".gguf") {
				modelName = modelName[:len(modelName)-5]
			}
			digest := fmt.Sprintf("sha256:%x", fm.SizeBytes)
			modelMap[modelName] = map[string]interface{}{
				"name": modelName, "model": modelName,
				"modified_at": fm.ModifiedAt.Format(time.RFC3339),
				"size":        fm.SizeBytes, "digest": digest,
				"details": map[string]interface{}{
					"format": "gguf", "family": fm.Architecture,
					"parameter_size": "unknown", "quantization_level": fm.FileType,
				},
			}
		}
	}

	loadedModels := backend.ListModels()
	for _, m := range loadedModels {
		sizeBytes := m.SizeBytes
		if sizeBytes == 0 {
			if fsEntry, ok := modelMap[m.Name]; ok {
				if fsSize, ok2 := fsEntry["size"].(int64); ok2 && fsSize > 0 {
					sizeBytes = uint64(fsSize)
				}
			}
		}
		digest := fmt.Sprintf("sha256:%x", sizeBytes)
		modelMap[m.Name] = map[string]interface{}{
			"name": m.Name, "model": m.Name,
			"modified_at": m.LoadedAt.Format(time.RFC3339),
			"size":        sizeBytes, "digest": digest,
			"details": map[string]interface{}{
				"format": "gguf", "family": m.Architecture,
				"parameter_size":     fmt.Sprintf("%.1fB", float64(m.NLayers*m.NEmbd)/1e9),
				"quantization_level": "unknown",
			},
		}
	}

	ollamaModels := make([]map[string]interface{}, 0, len(modelMap))
	for _, v := range modelMap {
		ollamaModels = append(ollamaModels, v)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"models": ollamaModels})
}

// handleOllamaPS — Ollama /api/ps (running models in memory).
// Round 21 hotfix (2026-08-03): раньше не было реализовано, balancer возвращал
// phantom 200 с `{"models":null}`. Теперь возвращает реальный список загруженных
// моделей в формате Ollama, с size_vram / expires_at / size, как ожидает клиент.
func handleOllamaPS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	loadedModels := backend.ListModels()
	psModels := make([]map[string]interface{}, 0, len(loadedModels))

	for _, m := range loadedModels {
		// Ollama-формат: name, model, size, size_vram, digest, expires_at, details.
		// size_vram = сколько занимает в VRAM (если есть), size = общий размер файла.
		// expires_at — через сколько модель выгрузится (idle TTL). У нас пока
		// нет per-model idle TTL, поэтому ставим дефолт 5 минут как в Ollama.
		sizeBytes := m.SizeBytes
		if sizeBytes == 0 && m.LoadingSizeBytes > 0 {
			sizeBytes = uint64(m.LoadingSizeBytes)
		}
		digest := fmt.Sprintf("sha256:%x", sizeBytes)
		psModels = append(psModels, map[string]interface{}{
			"name":       m.Name,
			"model":      m.Name,
			"size":       sizeBytes,
			"size_vram":  uint64(0), // cppworker не отслеживает per-model VRAM пока; 0 = неизвестно
			"digest":     digest,
			"expires_at": time.Now().Add(5 * time.Minute).Format(time.RFC3339),
			"details": map[string]interface{}{
				"format":             "gguf",
				"family":             m.Architecture,
				"parameter_size":     fmt.Sprintf("%.1fB", float64(m.NLayers*m.NEmbd)/1e9),
				"quantization_level": "unknown",
			},
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"models": psModels})
}

// handleOllamaShow ? Ollama /api/show.
//
// Round 44 (2026-08-19) variant 4 structural fix: when the requested model is
// NOT yet loaded into memory, return metadata directly from the GGUF header
// instead of triggering a full llama.cpp model load.
//
// Why this matters
//
// R31-R43 retained the historical "lazy-load on /api/show" behavior for
// compatibility with Ollama tooling that polls /api/show to discover
// capabilities. The hidden cost is that a 22GB model (Qwen3.6-35B) takes
// 6+ minutes to cold-load on RTX 3070 8GB VRAM, while 5GB gemma-4 takes
// 77s. Real Cline/Codegen clients with hard 5-minute HTTP timeouts
// (claude/claude#12829 reference; Cline CLI OLLAMA_DEFAULT_TIMEOUT_MS=300000)
// cancel the request mid-load. With the old code, cancellation left the
// cppworker still holding the single-flight load slot, blocking every
// subsequent /api/chat for the same backend (the R44 stuck-state symptom).
//
// New flow
//
//   1. If the model is already loaded in memory → return full info from
//      backend.GetModel (state="loaded"; n_vocab/gpu_layers present).
//   2. Otherwise, look up the on-disk path via ModelManager and read
//      metadata-only via mm.GetModelMeta (which parses the GGUF header in
//      <100ms, no model load). Return header-derived fields with
//      state="unloaded" and context_size=0 (load is required to populate
//      runtime-only fields like n_vocab and gpu_layers).
//   3. If neither loaded nor on disk → 404.
//
// Behavior preserved
//   - `/api/show` still answers in well under 1s even for the largest model.
//   - `state` field always set: "loaded" / "unloaded" / "not loaded".
//   - Ollama / OpenWebUI render correctly: they only inspect the family and
//     parameter_size fields for UI hints, both available from the header.
//   - The lazy-load path (ensureModelLoaded) is no longer triggered by
//     /api/show. Clients that actually need inference hit /api/chat or
//     /api/generate, which DO load the model (this is unchanged).
func handleOllamaShow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := req.Name
	if name == "" {
		name = req.Model
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "name or model is required")
		return
	}

	// Path 1: model already in memory → return full info (state=loaded).
	if info, err := backend.GetModel(name); err == nil {
		writeOllamaShowLoadedResponse(w, info)
		return
	}

	// Path 2: model NOT loaded → metadata-only via GGUF header (no load).
	// This is the R44 structural fix.
	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusServiceUnavailable, "model manager not available")
		return
	}
	meta, metaErr := mm.GetModelMeta(name)
	if metaErr != nil || meta == nil {
		// Fallback: try with .gguf suffix (Ollama clients often send basename).
		if meta2, err2 := mm.GetModelMeta(name + ".gguf"); err2 == nil && meta2 != nil {
			meta = meta2
			metaErr = nil
		}
	}
	if metaErr == nil && meta != nil && meta.Path != "" {
		writeOllamaShowMetadataResponse(w, meta)
		return
	}

	// Path 3: not on disk either → 404 (not changed from previous behavior).
	path, err := mm.FindModelByPath(strings.TrimSuffix(name, ".gguf") + ".gguf")
	if err != nil {
		writeError(w, http.StatusNotFound, "model not found: "+name)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to stat model file: "+err.Error())
		return
	}
	modelName := strings.TrimSuffix(filepath.Base(path), ".gguf")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"license": "unknown", "modelfile": "", "parameters": "unknown",
		"template": "", "system": "",
		"details": map[string]interface{}{
			"parent_model": "", "format": "gguf", "family": "unknown",
			"families": []string{}, "parameter_size": "unknown",
			"quantization_level": "unknown",
		},
		"model_info": map[string]interface{}{
			"name": modelName, "filename": filepath.Base(path),
			"size_bytes": info.Size(), "modified_at": info.ModTime().Format(time.RFC3339),
			"state": "not loaded", "architecture": "unknown",
		},
	})
}

// writeOllamaShowLoadedResponse — Path 1 of /api/show: model is in memory.
// Same response shape as R43 and earlier; n_vocab and gpu_layers present,
// state=loaded.
func writeOllamaShowLoadedResponse(w http.ResponseWriter, info *cppbackend.ModelInfo) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"license": "unknown", "modelfile": "",
		"parameters": fmt.Sprintf("%.1fB", float64(info.NLayers*info.NEmbd)/1e9),
		"template":   "", "system": "",
		"details": map[string]interface{}{
			"parent_model": "", "format": "gguf", "family": info.Architecture,
			"families":           []string{info.Architecture},
			"parameter_size":     fmt.Sprintf("%.1fB", float64(info.NLayers*info.NEmbd)/1e9),
			"quantization_level": "unknown",
		},
		"model_info": map[string]interface{}{
			"architecture": info.Architecture, "n_layers": info.NLayers,
			"n_heads": info.NHeads, "n_embd": info.NEmbd,
			"n_kv_heads": info.NKvHeads, "head_dim_k": info.HeadDimK, "head_dim_v": info.HeadDimV,
			"n_vocab": info.NVocab, "context_size": info.ContextSize,
			"gpu_layers": info.GPULayers, "kv_cache_type": info.KVCacheType, "state": info.State,
		},
	})
}

// writeOllamaShowMetadataResponse — Path 2 of /api/show: header-only metadata
// (model NOT in memory; no load triggered).
//
// R44 (2026-08-19) NEW: this is the variant-4 fix. The response shape is
// compatible with Ollama clients that only inspect family / parameter_size
// for UI rendering. n_vocab and gpu_layers are runtime-only fields and are
// zeroed; the state field is "unloaded" so the client knows the model is
// not yet ready for inference (it should call /api/chat or /api/generate
// to actually load and use it).
func writeOllamaShowMetadataResponse(w http.ResponseWriter, meta *cppbackend.GGUFModelMeta) {
	// GGUF header may be missing NEmbd/NLayers for non-llama.cpp-arch files
	// or when ReadGGUFHeader failed silently. Use safe zero defaults.
	nLayers := meta.NLayers
	nEmbd := meta.NEmbd
	paramSize := "unknown"
	if nLayers > 0 && nEmbd > 0 {
		paramSize = fmt.Sprintf("%.1fB", float64(nLayers*nEmbd)/1e9)
	}
	family := meta.Architecture
	if family == "" {
		family = "unknown"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"license": "unknown", "modelfile": "",
		"parameters": paramSize,
		"template":   "", "system": "",
		"details": map[string]interface{}{
			"parent_model": "", "format": "gguf", "family": family,
			"families":           []string{family},
			"parameter_size":     paramSize,
			"quantization_level": "unknown",
		},
		"model_info": map[string]interface{}{
			"architecture":  family,
			"n_layers":      nLayers,
			"n_heads":       meta.NHeads,
			"n_embd":        nEmbd,
			"n_kv_heads":    meta.NKvHeads,
			"head_dim_k":    0, // не из GGUF header
			"head_dim_v":    0, // не из GGUF header
			"n_vocab":       0, // runtime-only (load required)
			"context_size":  0, // runtime-only (load required)
			"gpu_layers":    0, // runtime-only (load required)
			"kv_cache_type": "",
			"state":         "unloaded",
		},
	})
}

// handleOllamaCopy ? Ollama /api/copy.
func handleOllamaCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Source == "" || req.Destination == "" {
		writeError(w, http.StatusBadRequest, "source and destination are required")
		return
	}

	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusServiceUnavailable, "model manager not available")
		return
	}

	srcName := req.Source
	if !strings.HasSuffix(strings.ToLower(srcName), ".gguf") {
		srcName = srcName + ".gguf"
	}
	dstName := req.Destination
	if !strings.HasSuffix(strings.ToLower(dstName), ".gguf") {
		dstName = dstName + ".gguf"
	}

	srcPath, err := mm.FindModelByPath(srcName)
	if err != nil {
		writeError(w, http.StatusNotFound, "source model not found: "+err.Error())
		return
	}
	dstPath := filepath.Join(filepath.Dir(srcPath), dstName)

	srcFile, err := os.Open(srcPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to open source model: "+err.Error())
		return
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dstPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create destination model: "+err.Error())
		return
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to copy model: "+err.Error())
		return
	}

	if _, scanErr := mm.ScanModels(); scanErr != nil {
		logger.Get().Warnw("copy: model rescan failed", "error", scanErr)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "copied", "source": req.Source, "destination": req.Destination,
	})
}

// handleOllamaCreate ? Ollama /api/create.
func handleOllamaCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Name      string `json:"name"`
		ModelFile string `json:"modelfile"`
		From      string `json:"from"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusServiceUnavailable, "model manager not available")
		return
	}

	sourceName := req.From
	if sourceName == "" && req.ModelFile != "" {
		lines := strings.Split(req.ModelFile, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(strings.ToUpper(line), "FROM ") {
				sourceName = strings.TrimSpace(line[5:])
				break
			}
		}
	}
	if sourceName == "" {
		writeError(w, http.StatusBadRequest, "source model is required (from field or FROM line in modelfile)")
		return
	}
	if !strings.HasSuffix(strings.ToLower(sourceName), ".gguf") {
		sourceName = sourceName + ".gguf"
	}

	srcPath, err := mm.FindModelByPath(sourceName)
	if err != nil {
		writeError(w, http.StatusNotFound, "source model not found: "+err.Error())
		return
	}

	modelName := req.Name
	if strings.HasSuffix(strings.ToLower(modelName), ".gguf") {
		modelName = modelName[:len(modelName)-5]
	}
	dir := filepath.Dir(srcPath)
	aliasPath := filepath.Join(dir, modelName+".gguf.json")

	alias := map[string]interface{}{
		"name": modelName, "source": sourceName,
		"created_at": time.Now().Format(time.RFC3339), "modelfile": req.ModelFile,
	}
	aliasJSON, err := json.MarshalIndent(alias, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to marshal alias: "+err.Error())
		return
	}
	if err := os.WriteFile(aliasPath, aliasJSON, 0644); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to write alias file: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "created", "name": modelName})
}

// handleOllamaPush ? Ollama /api/push: ?? ?????????????? ??? llama.cpp backend.
func handleOllamaPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	writeError(w, http.StatusNotImplemented, "Ollama registry push is not supported for llama.cpp backends")
}

// resolveModelPath резолвит имя модели в абсолютный путь к .gguf файлу.
//
// Стратегия поиска (от точного к приблизительному):
//  1. ModelManager.FindModelByPath — прямое совпадение имени или пути
//  2. Glob "modelsDir/<name>*.gguf" — wildcards (qwen3-4b → qwen3-4b-it.gguf)
//  3. ModelManager library scan — если в директории РОВНО ОДИН .gguf, берём его
//     (auto-pick the only model — пользователь загрузил одну модель с коротким именем)
//  4. Fallback: "modelsDir/<name>.gguf" (вызовет 500 на cppworker, но с осмысленной ошибкой)
//
// Round 21 hotfix (2026-08-03): добавлен шаг 3. Раньше при `name=qwen3-4b` и единственном
// файле `Qwen3-Instruct-2507-q4km.gguf` в директории — балансер не мог auto-load
// модель (имя не совпадало с файлом), и все inference-эндпоинты после idle-unload
// падали с HTTP 500 "load failed". Теперь single-model dirs auto-pick'аются.
func resolveModelPath(modelName string) string {
	mm := backend.ModelManager()
	if mm != nil {
		if foundPath, err := mm.FindModelByPath(modelName); err == nil {
			return foundPath
		}
		// Шаг 2: glob
		modelPath := filepath.Join(*modelsDir, modelName)
		if !strings.HasSuffix(modelPath, ".gguf") {
			if matches, err := filepath.Glob(modelPath + "*.gguf"); err == nil && len(matches) > 0 {
				return matches[0]
			}
			modelPath += ".gguf"
		}

		// Шаг 3 (Round 21 hotfix): auto-pick единственного .gguf в директории.
		// Типичный случай: пользователь скачал одну модель и переименовал её
		// при load (name=qwen3-4b, file=Qwen3-Instruct-2507-q4km.gguf). При
		// auto-load после idle-unload — балансер не знает правильного path, и
		// ищет models/qwen3-4b.gguf. Если в директории РОВНО ОДИН .gguf файл —
		// логично предположить, что это и есть нужная модель.
		files := mm.ListModels()
		if len(files) == 1 {
			logger.Get().Infow("resolveModelPath: auto-pick single .gguf from modelsDir",
				"requested_name", modelName, "resolved_path", files[0].Path)
			return files[0].Path
		}

		return modelPath
	}
	// Fallback без ModelManager
	modelPath := filepath.Join(*modelsDir, modelName)
	if !strings.HasSuffix(modelPath, ".gguf") {
		if matches, err := filepath.Glob(modelPath + "*.gguf"); err == nil && len(matches) > 0 {
			return matches[0]
		}
		modelPath += ".gguf"
	}
	return modelPath
}
