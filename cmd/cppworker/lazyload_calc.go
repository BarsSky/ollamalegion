// lazyload_calc.go — расчёт оптимальных LoadModelOpts ДО llama.cpp.LoadModel.
//
// Проблема (Issue «qwen3.6 на 20GB VRAM не влезает, на 8GB — да»):
//
//   В lazyload.go:ensureModelLoaded первая загрузка шла с дефолтами:
//     - ContextSize = *ctxSize (default 32768)
//     - GPULayers   = *gpuLayers (default 32 или из env)
//     - autoOffload уменьшал GPULayers грубо (estimatedLayers=80, kvReserve=2GB).
//
//   Для qwen3.6 (~22GB) на 20GB VRAM:
//     weights + KV-cache (для 32K) ≈ 22 + 10 + 1.5 = 33.5 GB > 20 GB VRAM
//     → llama.cpp.LoadModel падает с OOM → cppworker возвращает ошибку,
//       пользователь видит "n_ctx_too_large" или вообще тишину.
//
// Решение: calculateLazyLoadOpts читает РЕАЛЬНЫЕ NLayers/NEmbd/NHeads из GGUF header
// (cppbackend.ReadGGUFHeader), считает maxViableNCtx через существующую
// computeMaxViableNCtx и уменьшает ContextSize/GPULayers до того, как
// память уже некуда будет деть.
//
// Если NLayers/NEmbd неизвестны (старый stub или битый header) — fallback
// на старую логику с estimatedLayers=80, kvReserve=2GB (см. lazyload.go).
package main

import (
	"fmt"
	"os"
	"strconv"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// LazyLoadRationale — обоснование выбора параметров загрузки.
// Возвращается из calculateLazyLoadOpts для логирования и /api/diagnostics.
type LazyLoadRationale struct {
	// Что было запрошено.
	RequestedNCtx      int
	RequestedGPULayers int

	// Что было применено после auto-tune.
	AppliedNCtx      int
	AppliedGPULayers int
	AppliedUseMmap   bool

	// Источник решения.
	Source string // "exact_fit" | "reduced_nctx" | "partial_offload" | "fallback_no_meta"

	// Реальные метрики (если удалось прочитать из GGUF).
	NLayers   int
	NEmbd     int
	NHeads    int
	NKvHeads  int
	ArchName  string
	ModelSize int64

	// VRAM/RAM, использованные в расчётах.
	AvailableVRAMBytes  int64
	AvailableRAMBytes   int64
	MaxViableNCtx       int
	EstimatedKVCacheMB  int64
	EstimatedWeightsMB  int64

	// Если было clamping — на сколько уменьшили.
	NCtxReductionPct float64
	GPULayersReduced bool
}

// autoTuneNCtxOnLoadEnabled — глобальный флаг, управляемый через
// CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD env (default "true").
//
// Если false — calculateLazyLoadOpts возвращает opts как есть (старое поведение).
var autoTuneNCtxOnLoadEnabled = true

func init() {
	if v := os.Getenv("CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD"); v != "" {
		if enabled, err := strconv.ParseBool(v); err == nil {
			autoTuneNCtxOnLoadEnabled = enabled
		}
	}
}

// calculateLazyLoadOpts рассчитывает оптимальные LoadModelOpts для модели
// с учётом доступных VRAM/RAM и архитектурных параметров из GGUF header.
//
// Аргументы:
//   - modelName: имя модели (filename без .gguf).
//   - requestedNCtx: n_ctx из request body / config / defaults.
//   - requestedGPULayers: gpu_layers из request body / config / defaults.
//   - defaults: текущая конфигурация cppworker (DefaultCtxSize и т.д.).
//
// Возвращает:
//   - LoadModelOpts для передачи в backend.LoadModelWithOpts.
//   - LazyLoadRationale для логирования и диагностики.
//
// Если meta пустая или ReadGGUFHeader не сработал — возвращает opts как есть
// и Source="fallback_no_meta" (старое поведение, см. ensureModelLoaded).
func calculateLazyLoadOpts(
	modelName string,
	requestedNCtx int,
	requestedGPULayers int,
	defaults *cppbackend.Config,
) (cppbackend.LoadModelOpts, LazyLoadRationale) {
	rationale := LazyLoadRationale{
		RequestedNCtx:      requestedNCtx,
		RequestedGPULayers: requestedGPULayers,
		AppliedNCtx:        requestedNCtx,
		AppliedGPULayers:   requestedGPULayers,
		AppliedUseMmap:     !*noMmap,
		Source:             "fallback_no_meta",
	}

	// Если auto-tune отключён через ENV — fallback на request body / defaults.
	if !autoTuneNCtxOnLoadEnabled {
		return cppbackend.LoadModelOpts{
			GPULayers:   requestedGPULayers,
			ContextSize: requestedNCtx,
			BatchSize:   *batchSize,
			UseMmap:     !*noMmap,
		}, rationale
	}

	// Шаг 1: получить meta с архитектурой (lazy ReadGGUFHeader при необходимости).
	mm := backend.ModelManager()
	if mm == nil {
		return cppbackend.LoadModelOpts{
			GPULayers:   requestedGPULayers,
			ContextSize: requestedNCtx,
			BatchSize:   *batchSize,
			UseMmap:     !*noMmap,
		}, rationale
	}

	// Сначала ищем с .gguf, потом без.
	meta, err := mm.GetModelMeta(modelName + ".gguf")
	if err != nil {
		meta, err = mm.GetModelMeta(modelName)
		if err != nil {
			logger.Get().Debugw("calculateLazyLoadOpts: meta not found, fallback",
				"model", modelName, "error", err)
			return cppbackend.LoadModelOpts{
				GPULayers:   requestedGPULayers,
				ContextSize: requestedNCtx,
				BatchSize:   *batchSize,
				UseMmap:     !*noMmap,
			}, rationale
		}
	}

	rationale.ModelSize = meta.SizeBytes
	rationale.ArchName = meta.Architecture
	rationale.NLayers = meta.NLayers
	rationale.NEmbd = meta.NEmbd
	rationale.NHeads = meta.NHeads
	rationale.NKvHeads = meta.NKvHeads

	// Шаг 2: если архитектура неизвестна (ReadGGUFHeader не сработал) —
	// fallback на старую логику lazyload.go.
	if meta.NLayers == 0 || meta.NEmbd == 0 || meta.NHeads == 0 {
		logger.Get().Debugw("calculateLazyLoadOpts: GGUF header has no architecture, fallback",
			"model", modelName,
			"nlayers", meta.NLayers,
			"nembd", meta.NEmbd,
			"nheads", meta.NHeads)
		return cppbackend.LoadModelOpts{
			GPULayers:   requestedGPULayers,
			ContextSize: requestedNCtx,
			BatchSize:   *batchSize,
			UseMmap:     !*noMmap,
		}, rationale
	}

	// Шаг 3: реальные метрики доступны. Считаем max viable n_ctx.
	availableVRAM := freeVRAMBytes()
	if availableVRAM <= 0 {
		availableVRAM = availableVRAMBytes()
	}
	rationale.AvailableVRAMBytes = availableVRAM
	rationale.AvailableRAMBytes = availableRAMBytes()

	// Если даже VRAM неизвестна (CPU-only без nvidia-smi) — fallback.
	if availableVRAM <= 0 {
		logger.Get().Debugw("calculateLazyLoadOpts: VRAM unknown, fallback to defaults",
			"model", modelName)
		return cppbackend.LoadModelOpts{
			GPULayers:   requestedGPULayers,
			ContextSize: requestedNCtx,
			BatchSize:   *batchSize,
			UseMmap:     !*noMmap,
		}, rationale
	}

	// Считаем KV-cache для requestedNCtx с реальными параметрами архитектуры.
	kvBytes := estimateKVCacheBytes(requestedNCtx, meta.NLayers, meta.NEmbd, meta.NHeads, meta.NKvHeads)
	rationale.EstimatedKVCacheMB = kvBytes / (1024 * 1024)

	weightsPerLayer := meta.SizeBytes / int64(meta.NLayers)
	if weightsPerLayer == 0 {
		weightsPerLayer = 1
	}
	gpuLayers := requestedGPULayers
	if gpuLayers == 0 && defaults != nil {
		gpuLayers = defaults.DefaultGPULayers
	}
	if gpuLayers == -1 {
		gpuLayers = meta.NLayers
	}
	if gpuLayers < 0 || gpuLayers > meta.NLayers {
		gpuLayers = meta.NLayers
	}

	// === Stage 1: проверяем, влезает ли requestedNCtx + gpuLayers в VRAM ===
	safetyFactor := nctxSafetyFactor // overridable via CPPWORKER_NCTX_SAFETY_FACTOR
	overheadBytes := int64(1536) * 1024 * 1024 // 1.5GB CUDA + activations
	safeVRAM := int64(float64(availableVRAM) * safetyFactor)

	gpuWeightsBytes := int64(gpuLayers) * weightsPerLayer
	ramWeightsBytes := int64(meta.NLayers-gpuLayers) * weightsPerLayer
	rationale.EstimatedWeightsMB = (gpuWeightsBytes + ramWeightsBytes) / (1024 * 1024)

	totalVRAMNeeded := gpuWeightsBytes + kvBytes + overheadBytes
	availForWeights := safeVRAM - overheadBytes - kvBytes

	logger.Get().Debugw("calculateLazyLoadOpts: Stage 1 check",
		"model", modelName,
		"available_vram_mb", availableVRAM/(1024*1024),
		"safe_vram_mb", safeVRAM/(1024*1024),
		"requested_n_ctx", requestedNCtx,
		"kv_cache_mb", kvBytes/(1024*1024),
		"gpu_layers", gpuLayers,
		"weights_per_layer_mb", weightsPerLayer/(1024*1024),
		"total_vram_needed_mb", totalVRAMNeeded/(1024*1024),
		"avail_for_weights_mb", availForWeights/(1024*1024))

	// Stage 1 PASS: всё влезает.
	if totalVRAMNeeded <= safeVRAM {
		rationale.Source = "exact_fit"
		rationale.AppliedNCtx = requestedNCtx
		rationale.AppliedGPULayers = gpuLayers
		rationale.AppliedUseMmap = ramWeightsBytes > 0
		rationale.MaxViableNCtx = requestedNCtx
		return cppbackend.LoadModelOpts{
			GPULayers:   gpuLayers,
			ContextSize: requestedNCtx,
			BatchSize:   *batchSize,
			UseMmap:     ramWeightsBytes > 0,
		}, rationale
	}

	// === Stage 2: уменьшаем gpuLayers (partial offload) ===
	if availForWeights > 0 {
		reducedGPULayers := int(availForWeights / weightsPerLayer)
		if reducedGPULayers > meta.NLayers {
			reducedGPULayers = meta.NLayers
		}
		if reducedGPULayers >= 0 && reducedGPULayers < gpuLayers {
			// Проверяем, что оставшиеся веса (mmap в RAM) влезают в available RAM.
			requiredRAM := int64(meta.NLayers-reducedGPULayers) * weightsPerLayer
			availableRAM := rationale.AvailableRAMBytes

			if availableRAM <= 0 || requiredRAM <= availableRAM {
				logger.Get().Infow("calculateLazyLoadOpts: applying partial offload",
					"model", modelName,
					"requested_gpu_layers", gpuLayers,
					"reduced_gpu_layers", reducedGPULayers,
					"requested_n_ctx", requestedNCtx,
					"kv_cache_mb", kvBytes/(1024*1024),
					"required_ram_mb", requiredRAM/(1024*1024),
					"available_ram_mb", availableRAM/(1024*1024))
				rationale.Source = "partial_offload"
				rationale.AppliedNCtx = requestedNCtx
				rationale.AppliedGPULayers = reducedGPULayers
				rationale.AppliedUseMmap = true
				rationale.GPULayersReduced = true
				rationale.MaxViableNCtx = requestedNCtx
				return cppbackend.LoadModelOpts{
					GPULayers:   reducedGPULayers,
					ContextSize: requestedNCtx,
					BatchSize:   *batchSize,
					UseMmap:     true,
				}, rationale
			}
		}
	}

	// === Stage 3: gpu_layers=0 + reduced n_ctx (cpu-only + mmap) ===
	kvPerToken := kvBytes / int64(requestedNCtx)
	if kvPerToken <= 0 {
		kvPerToken = int64(meta.NLayers) * int64(meta.NEmbd) / int64(meta.NHeads) * 4 * 2
		if kvPerToken <= 0 {
			kvPerToken = 4096 // fallback 4KB/token
		}
	}
	maxNCtxCPUOnly := (safeVRAM - overheadBytes) / kvPerToken
	if maxNCtxCPUOnly < 0 {
		maxNCtxCPUOnly = 0
	}

	finalNCtx := requestedNCtx
	if int64(requestedNCtx) > maxNCtxCPUOnly {
		finalNCtx = int(maxNCtxCPUOnly)
	}

	// Проверяем, что все веса модели влезают в RAM.
	requiredRAMAll := meta.SizeBytes
	availableRAM := rationale.AvailableRAMBytes
	if availableRAM > 0 && requiredRAMAll > availableRAM {
		logger.Get().Errorw("calculateLazyLoadOpts: model doesn't fit even in CPU-only mode",
			"model", modelName,
			"model_size_mb", requiredRAMAll/(1024*1024),
			"available_ram_mb", availableRAM/(1024*1024))
		// Возвращаем opts как есть — пусть llama.cpp попробует и упадёт
		// с понятной ошибкой OOM. Пользователь увидит actionable сообщение.
		rationale.Source = "fallback_no_fit"
		return cppbackend.LoadModelOpts{
			GPULayers:   requestedGPULayers,
			ContextSize: requestedNCtx,
			BatchSize:   *batchSize,
			UseMmap:     true,
		}, rationale
	}

	rationale.Source = "reduced_nctx"
	rationale.AppliedNCtx = finalNCtx
	rationale.AppliedGPULayers = 0
	rationale.AppliedUseMmap = true
	rationale.GPULayersReduced = true
	rationale.MaxViableNCtx = int(maxNCtxCPUOnly)
	if requestedNCtx > 0 {
		rationale.NCtxReductionPct = float64(requestedNCtx-finalNCtx) / float64(requestedNCtx) * 100.0
	}

	logger.Get().Infow("calculateLazyLoadOpts: reducing n_ctx for cpu-only offload",
		"model", modelName,
		"requested_n_ctx", requestedNCtx,
		"reduced_n_ctx", finalNCtx,
		"max_viable_n_ctx_cpu_only", maxNCtxCPUOnly,
		"vram_safe_mb", safeVRAM/(1024*1024),
		"kv_per_token", kvPerToken,
		"reason", "VRAM insufficient for weights+KV-cache at requested n_ctx")

	return cppbackend.LoadModelOpts{
		GPULayers:   0,
		ContextSize: finalNCtx,
		BatchSize:   *batchSize,
		UseMmap:     true,
	}, rationale
}

// FormatRationale — обоснование для логирования.
func (r LazyLoadRationale) FormatRationale() string {
	return fmt.Sprintf(
		"source=%s requested_nctx=%d applied_nctx=%d reduction_pct=%.1f%% requested_gpu=%d applied_gpu=%d gpu_reduced=%v arch=%s nlayers=%d nembd=%d size_mb=%d available_vram_mb=%d max_viable_nctx=%d",
		r.Source, r.RequestedNCtx, r.AppliedNCtx, r.NCtxReductionPct,
		r.RequestedGPULayers, r.AppliedGPULayers, r.GPULayersReduced,
		r.ArchName, r.NLayers, r.NEmbd,
		r.ModelSize/(1024*1024),
		r.AvailableVRAMBytes/(1024*1024),
		r.MaxViableNCtx,
	)
}