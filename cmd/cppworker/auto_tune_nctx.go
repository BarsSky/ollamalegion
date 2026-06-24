// auto_tune_nctx.go — AutoTuneNCtx: автоподбор n_ctx и gpu_layers при reload.
//
// Проблема (Issue: «при загруженных не хватает места на GPU в видеопамяти,
// ещё остаётся с запасом также много оперативной памяти свободной»):
//
//   С Cline приходит prompt с 53K токенов. Балансер решает auto-reload модели
//   с required_n_ctx=53897. На RTX 3070 8GB (max_vram_n_ctx=32719, RAM=32GB)
//   текущая логика tryRamFallbackReload перезагружает с requestedNCtx=53897
//   и gpu_layers=20 — это не влезает в VRAM (8GB), даже с mmap.
//   Пользователь получает ошибку reload и должен вручную угадывать,
//   какой n_ctx поместится.
//
// Решение: AutoTuneNCtx вычисляет максимальный n_ctx, который помещается
// в VRAM (для weights+KV-cache) + RAM (для оставшихся weights через mmap).
// Стратегия:
//
//  1. Если requestedNCtx помещается в VRAM с current gpu_layers → use as-is.
//  2. Иначе уменьшаем gpu_layers (partial offload) до значения, при котором
//     weights + KV-cache для requestedNCtx помещаются в VRAM.
//  3. Если даже с gpu_layers=0 (CPU-only) weights+KV-cache для requestedNCtx
//     не помещаются в VRAM → уменьшаем n_ctx до максимально допустимого,
//     который помещается с gpu_layers=0 (всё через mmap).
//  4. Если и cpu_layers=0 + уменьшенный n_ctx не помещается → возвращаем ошибку
//     с указанием макс. возможного n_ctx для пользователя.
//
// Файл активируется только при сборке с реальным llama.cpp (не stub).
package main

import (
	"fmt"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// TunedNCtxResult — результат AutoTuneNCtx для применения в LoadModelOpts.
type TunedNCtxResult struct {
	// RecommendedNCtx — итоговое n_ctx, которое пройдёт в VRAM (с учётом weights+KV-cache).
	RecommendedNCtx int
	// RecommendedGPULayers — число GPU-слоёв для partial offload.
	// 0 = всё через mmap в RAM; -1 = все слои на GPU (если влезает).
	RecommendedGPULayers int
	// UseMmap — true если часть весов будет в RAM (partial offload).
	UseMmap bool
	// Source — как было принято решение (для логирования).
	Source string // "exact_match" | "partial_offload" | "reduced_nctx" | "fallback"
	// MaxViableNCtx — максимально возможный n_ctx при текущих ресурсах.
	// Если requested > MaxViable → пользователю показывается это значение.
	MaxViableNCtx int
}

// AutoTuneNCtx вычисляет оптимальные n_ctx и gpu_layers для модели m
// с учётом доступной VRAM (через availableVRAMBytes()), requestedNCtx
// и текущих параметров модели.
//
// requestedNCtx — n_ctx, который хочет клиент (из body или X-Cpp-Ctx header).
// Если requestedNCtx <= 0, используется currentInfo.ContextSize или defaultCtxSize.
//
// Алгоритм (Stage 1 — AutoTune для 20GB GPU + свободная RAM):
//   1. availableVRAM := availableVRAMBytes() (если неизвестно → fallback).
//   2. Если requestedNCtx помещается в VRAM (с current gpu_layers) → return as-is.
//   3. Иначе: уменьшаем gpu_layers пропорционально свободной VRAM.
//   4. Если всё ещё не влезает даже при gpu_layers=0 → уменьшаем n_ctx.
//   5. Возвращаем TunedNCtxResult с обоснованием.
func AutoTuneNCtx(m cppbackend.ModelInfo, requestedNCtx int) TunedNCtxResult {
	if m.SizeBytes == 0 || m.NLayers == 0 {
		// Нет метаданных — fallback на текущие значения без изменений.
		ctx := requestedNCtx
		if ctx <= 0 {
			ctx = currentConfig.DefaultCtxSize
		}
		return TunedNCtxResult{
			RecommendedNCtx:      ctx,
			RecommendedGPULayers: currentConfig.DefaultGPULayers,
			UseMmap:              true,
			Source:               "fallback",
			MaxViableNCtx:        ctx,
		}
	}

	availableVRAM := freeVRAMBytes()
	// Fallback: если freeVRAM недоступна (stub, старый bridge), используем total.
	// Это менее точно (double-counting), но лучше, чем 0.
	if availableVRAM <= 0 {
		availableVRAM = availableVRAMBytes()
	}
	// Параметры для расчётов.
	safetyFactor := 0.85
	overheadBytes := int64(1536) * 1024 * 1024 // 1.5 GB CUDA + activations
	weightsPerLayer := int64(m.SizeBytes) / int64(m.NLayers)

	// Default ctx: requested → current → defaultConfig.
	if requestedNCtx <= 0 {
		if m.ContextSize > 0 {
			requestedNCtx = m.ContextSize
		} else {
			requestedNCtx = currentConfig.DefaultCtxSize
		}
	}

	// Cap на максимально допустимый n_ctx.
	if *ramFallbackMaxNCtx > 0 && requestedNCtx > *ramFallbackMaxNCtx {
		logger.Get().Infow("AutoTuneNCtx: requested exceeds ram-fallback-max-n-ctx, capping",
			"model", m.Name,
			"requested_n_ctx", requestedNCtx,
			"max_n_ctx", *ramFallbackMaxNCtx)
		requestedNCtx = *ramFallbackMaxNCtx
	}

	// === STAGE 1: requestedNCtx + current gpu_layers ===
	if availableVRAM > 0 && weightsPerLayer > 0 && m.NLayers > 0 {
		currentGpuLayers := m.GPULayers
		if currentGpuLayers == -1 {
			currentGpuLayers = m.NLayers // all layers
		} else if currentGpuLayers < 0 {
			currentGpuLayers = currentConfig.DefaultGPULayers
		}
		// KV-cache для requestedNCtx.
		kvCacheBytes := estimateKVCacheBytes(requestedNCtx, m.NLayers, m.NEmbd, m.NHeads, m.NKvHeads)
		// Размер weights для currentGpuLayers на GPU.
		gpuWeightsBytes := int64(currentGpuLayers) * weightsPerLayer
		// Размер weights для (NLayers - currentGpuLayers) в RAM (mmap).
		ramWeightsBytes := int64(m.NLayers-currentGpuLayers) * weightsPerLayer

		totalVRAMNeeded := gpuWeightsBytes + kvCacheBytes + overheadBytes
		maxViableNCtx := computeMaxViableNCtx(m, availableVRAM, currentGpuLayers, weightsPerLayer, overheadBytes)

		if int64(float64(availableVRAM)*safetyFactor) >= totalVRAMNeeded {
			// === STAGE 1 PASS: requested влезает в VRAM с current gpu_layers ===
			logger.Get().Infow("AutoTuneNCtx: requested fits with current gpu_layers",
				"model", m.Name,
				"n_ctx", requestedNCtx,
				"gpu_layers", currentGpuLayers,
				"vram_needed_mb", totalVRAMNeeded/(1024*1024),
				"vram_available_mb", int64(float64(availableVRAM)*safetyFactor)/(1024*1024),
				"ram_weights_mb", ramWeightsBytes/(1024*1024))
			return TunedNCtxResult{
				RecommendedNCtx:      requestedNCtx,
				RecommendedGPULayers: currentGpuLayers,
				UseMmap:              ramWeightsBytes > 0,
				Source:               "exact_match",
				MaxViableNCtx:        maxViableNCtx,
			}
		}

		// === STAGE 2: уменьшаем gpu_layers для partial offload ===
		// Цель: gpu_weights + kv_cache + overhead <= safeVRAM
		// gpu_weights = gpuLayers * weightsPerLayer
		// => gpuLayers <= (safeVRAM - overheadBytes - kvCacheBytes) / weightsPerLayer
		safeVRAM := int64(float64(availableVRAM) * safetyFactor)
		availableForGPUWeights := safeVRAM - overheadBytes - kvCacheBytes
		if availableForGPUWeights > 0 {
			reducedGPULayers := int(availableForGPUWeights / weightsPerLayer)
			if reducedGPULayers > m.NLayers {
				reducedGPULayers = m.NLayers
			}
			if reducedGPULayers >= 0 {
				// Проверяем: с reducedGPULayers + mmap веса для остальных слоёв — должно влезть.
				// mmap для RAM — там обычно много места, проверим что у нас есть хотя бы
				// (NLayers - reducedGPULayers) * weightsPerLayer свободной RAM.
				requiredRAM := int64(m.NLayers-reducedGPULayers) * weightsPerLayer
				availableRAM := availableRAMBytes()
				if availableRAM > 0 && requiredRAM > availableRAM {
					logger.Get().Warnw("AutoTuneNCtx: weights don't fit in RAM either",
						"model", m.Name,
						"required_ram_mb", requiredRAM/(1024*1024),
						"available_ram_mb", availableRAM/(1024*1024))
					// Fall through к STAGE 3
				} else {
					logger.Get().Infow("AutoTuneNCtx: partial offload for requested n_ctx",
						"model", m.Name,
						"requested_n_ctx", requestedNCtx,
						"original_gpu_layers", currentGpuLayers,
						"reduced_gpu_layers", reducedGPULayers,
						"n_ctx", requestedNCtx,
						"vram_available_for_weights_mb", availableForGPUWeights/(1024*1024),
						"vram_needed_mb", totalVRAMNeeded/(1024*1024),
						"ram_for_remaining_weights_mb", requiredRAM/(1024*1024))
					return TunedNCtxResult{
						RecommendedNCtx:      requestedNCtx,
						RecommendedGPULayers: reducedGPULayers,
						UseMmap:              true,
						Source:               "partial_offload",
						MaxViableNCtx:        maxViableNCtx,
					}
				}
			}
		}

		// === STAGE 3: cpu-only (gpu_layers=0) + reduced n_ctx ===
		// При gpu_layers=0 нужно только KV-cache в VRAM (weights через mmap в RAM).
		// KV-cache для gpu_layers=0: kvCacheBytes = requestedNCtx * 4 * NLayers * effKVHeads * headDim
		// Нам нужно: kvCacheBytes + overheadBytes <= safeVRAM
		// => requestedNCtx <= (safeVRAM - overheadBytes) / kv_per_token
		kvPerToken := kvCacheBytes / int64(requestedNCtx)
		if kvPerToken <= 0 {
			kvPerToken = 4096 // fallback 4MB/1K tokens
		}
		maxNCtxForCPUOnly := (safeVRAM - overheadBytes) / kvPerToken
		if maxNCtxForCPUOnly < 0 {
			maxNCtxForCPUOnly = 0
		}

		// Если requested > maxNCtxForCPUOnly → уменьшаем n_ctx.
		var finalNCtx int
		if int64(requestedNCtx) > maxNCtxForCPUOnly {
			finalNCtx = int(maxNCtxForCPUOnly)
			// Также проверяем, что weights в RAM есть место.
			requiredRAM := int64(m.SizeBytes)
			availableRAM := availableRAMBytes()
			if availableRAM > 0 && requiredRAM > availableRAM {
				// Даже n_ctx=1 не влезет — пробуем оценить минимально возможный n_ctx,
				// если weights (без KV) тоже не влезают.
				logger.Get().Errorw("AutoTuneNCtx: model weights don't fit in available RAM",
					"model", m.Name,
					"model_size_mb", requiredRAM/(1024*1024),
					"available_ram_mb", availableRAM/(1024*1024))
				finalNCtx = 0 // сигнал ошибки
			}
			logger.Get().Infow("AutoTuneNCtx: reduced n_ctx for cpu-only offload",
				"model", m.Name,
				"requested_n_ctx", requestedNCtx,
				"reduced_n_ctx", finalNCtx,
				"vram_safe_mb", safeVRAM/(1024*1024),
				"max_n_ctx_cpu_only", maxNCtxForCPUOnly,
				"reason", "VRAM insufficient for weights+KV-cache at requested n_ctx")
			source := "reduced_nctx"
			if finalNCtx == 0 {
				source = "fallback"
			}
			return TunedNCtxResult{
				RecommendedNCtx:      finalNCtx,
				RecommendedGPULayers: 0, // CPU-only
				UseMmap:              true,
				Source:               source,
				MaxViableNCtx:        int(maxNCtxForCPUOnly),
			}
		}

		// requested помещается даже в cpu-only mode.
		logger.Get().Infow("AutoTuneNCtx: requested fits with cpu-only offload",
			"model", m.Name,
			"requested_n_ctx", requestedNCtx,
			"gpu_layers", 0,
			"vram_available_mb", safeVRAM/(1024*1024),
			"kv_cache_mb", kvCacheBytes/(1024*1024))
		return TunedNCtxResult{
			RecommendedNCtx:      requestedNCtx,
			RecommendedGPULayers: 0,
			UseMmap:              true,
			Source:               "partial_offload",
			MaxViableNCtx:        int(maxNCtxForCPUOnly),
		}
	}

	// Fallback: VRAM неизвестна (CPU-only host) — используем safe defaults.
	return TunedNCtxResult{
		RecommendedNCtx:      requestedNCtx,
		RecommendedGPULayers: currentConfig.DefaultGPULayers,
		UseMmap:              true,
		Source:               "fallback",
		MaxViableNCtx:        requestedNCtx,
	}
}

// computeMaxViableNCtx вычисляет максимальный n_ctx, который помещается
// в availableVRAM при заданном gpuLayers. Используется для информирования
// пользователя о реальном лимите.
//
// kvPerToken = 4 * NLayers * effKVHeads * headDim
// maxNCtx = (safeVRAM - overheadBytes) / kvPerToken (при gpu_layers=0)
// При gpu_layers > 0: нужно ещё weightsPerLayer * gpuLayers для весов.
func computeMaxViableNCtx(m cppbackend.ModelInfo, availableVRAM int64, gpuLayers int,
	weightsPerLayer int64, overheadBytes int64) int {
	if m.NLayers == 0 || m.NEmbd == 0 {
		return m.ContextSize // fallback
	}
	safetyFactor := 0.85
	safeVRAM := int64(float64(availableVRAM) * safetyFactor)
	// KV-cache для m.ContextSize → kvPerToken.
	kvCacheBytes := estimateKVCacheBytes(m.ContextSize, m.NLayers, m.NEmbd, m.NHeads, m.NKvHeads)
	kvPerToken := kvCacheBytes / int64(m.ContextSize)
	if kvPerToken <= 0 {
		kvPerToken = 4096
	}
	// Доступно для KV-cache.
	availableForKV := safeVRAM - overheadBytes - int64(gpuLayers)*weightsPerLayer
	if availableForKV < 0 {
		return 0
	}
	maxNCtx := availableForKV / kvPerToken
	// Apply ram-fallback-max cap.
	if *ramFallbackMaxNCtx > 0 && int(maxNCtx) > *ramFallbackMaxNCtx {
		maxNCtx = int64(*ramFallbackMaxNCtx)
	}
	return int(maxNCtx)
}

// AutoTuneError — ошибка AutoTuneNCtx когда requested n_ctx не влезает
// даже при cpu-only offload (model size > available RAM).
type AutoTuneError struct {
	Model           string
	RequestedNCtx   int
	MaxViableNCtx   int
	ModelSizeBytes  int64
	AvailableRAMMB  int64
}

func (e *AutoTuneError) Error() string {
	return fmt.Sprintf(
		"AutoTuneNCtx: cannot fit model %q (size=%d MB) with requested n_ctx=%d. "+
			"Max viable n_ctx (cpu-only offload): %d. Available RAM: %d MB. "+
			"Reduce tools/history or save model profile with smaller n_ctx.",
		e.Model, e.ModelSizeBytes/(1024*1024), e.RequestedNCtx, e.MaxViableNCtx, e.AvailableRAMMB)
}