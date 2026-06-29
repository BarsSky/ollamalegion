// auto_offload.go — авто-расчёт числа GPU-слоёв для размещения модели в VRAM.
//
// Проблема: при больших моделях (19GB+) на средних GPU (24GB A10) пользователь
// должен вручную угадать, сколько слоёв поместится вместе с KV-cache для
// requested n_ctx. Неправильное число → OOM на старте LoadModel.
//
// Решение: при `CPPWORKER_AUTO_OFFLOAD=true` cppworker при загрузке/перезагрузке
// вычисляет оптимальное gpu_layers по формуле:
//
//   availableVRAM_safety = availableVRAM * 0.85
//   overhead = 1.5 GB (CUDA + activations + llama.cpp)
//   kvCacheBytes = n_ctx * kv_cache_per_token (зависит от архитектуры)
//   weightsPerLayer = SizeBytes / NLayers
//   gpu_layers = floor((availableVRAM_safety - overhead - kvCacheBytes) / weightsPerLayer)
//
// Файл активируется только при сборке с реальным llama.cpp (не stub).
package main

import (
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// calculateOptimalGPULayersForModel вычисляет оптимальное число GPU-слоёв
// для размещения модели в доступной VRAM с учётом KV-cache.
//
// Возвращает:
//   - GPULayers: 0..NLayers (0 = CPU-only через mmap)
//   - -1 (=all layers), если модель целиком влезает в VRAM с запасом
//
// Если auto_offload отключён — возвращает currentConfig.DefaultGPULayers как есть.
func calculateOptimalGPULayersForModel(m cppbackend.ModelInfo) int {
	if !*autoOffload {
		return currentConfig.DefaultGPULayers
	}
	if m.SizeBytes == 0 || m.NLayers == 0 {
		// Нет метаданных (только что загруженная без path/size) — fallback.
		return currentConfig.DefaultGPULayers
	}
	// Используем FREE VRAM, а не TOTAL VRAM, чтобы не учитывать VRAM,
	// уже занятую другой загруженной моделью (например, gemma-4 5GB на 20GB GPU
	// оставляет ~15GB free).
	// Если freeVRAM недоступен — fallback на availableVRAM с двойным запасом.
	freeVRAM := freeVRAMBytes()
	if freeVRAM <= 0 {
		// freeVRAM не удалось определить — используем availableVRAM с запасом 0.85.
		freeVRAM = int64(float64(availableVRAMBytes()) * 0.85)
	}
	if freeVRAM <= 0 {
		// Не смогли узнать VRAM (CPU-only host, нет nvidia-smi) — fallback.
		return currentConfig.DefaultGPULayers
	}
	safetyFactor := 0.9
	safeVRAM := int64(float64(freeVRAM) * safetyFactor)
	overheadBytes := int64(1536) * 1024 * 1024 // 1.5 GB

	// KV-cache для запрошенного n_ctx (bytes)
	// Формула (f16, 4 байта на токен на KV-pair): 4 * NLayers * headDim
	// где headDim ≈ n_embd (для MHA) или n_embd / n_heads * n_kv_heads (GQA).
	// Упрощённо: 4 * NLayers * NEmbd для MHA, 4 * NLayers * NKvHeads * (NEmbd/NHeads) для GQA.
	nCtx := m.ContextSize
	if nCtx <= 0 {
		nCtx = currentConfig.DefaultCtxSize
	}
	kvCacheBytes := estimateKVCacheBytes(nCtx, m.NLayers, m.NEmbd, m.NHeads, m.NKvHeads)

	availableForWeights := safeVRAM - overheadBytes - kvCacheBytes
	if availableForWeights <= 0 {
		// KV-cache + overhead уже съели всё — модель только в RAM (0 GPU слоёв).
		logger.Get().Warnw("auto_offload: KV-cache + overhead exceed safe VRAM, model CPU-only",
			"model", m.Name,
			"safe_vram_mb", safeVRAM/(1024*1024),
			"kv_cache_mb", kvCacheBytes/(1024*1024),
			"overhead_mb", overheadBytes/(1024*1024))
		return 0
	}
	weightsPerLayer := int64(m.SizeBytes) / int64(m.NLayers)
	if weightsPerLayer == 0 {
		return currentConfig.DefaultGPULayers
	}
	gpuLayers := int(availableForWeights / weightsPerLayer)
	if gpuLayers < 0 {
		gpuLayers = 0
	}
	if gpuLayers > m.NLayers {
		gpuLayers = m.NLayers // не больше, чем есть
	}
	logger.Get().Infow("auto_offload: calculated gpu_layers",
		"model", m.Name,
		"size_bytes", m.SizeBytes,
		"n_layers", m.NLayers,
		"weights_per_layer_mb", weightsPerLayer/(1024*1024),
		"free_vram_mb", freeVRAM/(1024*1024),
		"safe_vram_mb", safeVRAM/(1024*1024),
		"kv_cache_mb", kvCacheBytes/(1024*1024),
		"n_ctx", nCtx,
		"gpu_layers", gpuLayers)
	return gpuLayers
}

// estimateKVCacheBytes — оценка размера KV-cache для n_ctx токенов.
//
// Формула для f16 (4 байта на KV-pair на токен на слой):
//   bytes = 4 * n_ctx * n_layers * head_dim * 2 (K+V)
//
// head_dim для MHA = n_embd
// head_dim для GQA = n_embd / n_heads * n_kv_heads
// Для простоты используем среднее: head_dim ≈ n_embd / n_heads * min(n_heads, n_kv_heads)
func estimateKVCacheBytes(nCtx, nLayers, nEmbd, nHeads, nKvHeads int) int64 {
	if nCtx <= 0 || nLayers <= 0 {
		return 0
	}
	if nEmbd <= 0 || nHeads <= 0 {
		// Fallback: 4 MB на 1K токенов для типичной 7B модели.
		return int64(nCtx) * 4096
	}
	effKVHeads := nKvHeads
	if effKVHeads <= 0 {
		effKVHeads = nHeads
	}
	headDim := nEmbd / nHeads
	if headDim <= 0 {
		headDim = nEmbd
	}
	// 4 (f16 = 2 байта × K+V) × n_ctx × n_layers × effKVHeads × headDim
	return int64(4) * int64(nCtx) * int64(nLayers) * int64(effKVHeads) * int64(headDim)
}