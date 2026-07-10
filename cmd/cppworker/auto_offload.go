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
//   kvCacheBytes = n_ctx * kv_cache_per_token (зависит от архитектуры и kvCacheType)
//   weightsPerLayer = SizeBytes / NLayers
//   gpu_layers = floor((availableVRAM_safety - overhead - kvCacheBytes) / weightsPerLayer)
//
// kvCacheType влияет на размер KV-cache: f16=4B/token, q8_0=2B/token, q4_0=1B/token.
// Если не указан — используется f16 (консервативная оценка).
//
// Файл активируется только при сборке с реальным llama.cpp (не stub).
package main

import (
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
	"os/exec"
	"strings"
	"strconv"
)

// kvCacheBytesPerType возвращает количество байт на KV-pair в зависимости от типа KV-cache.
//   "f16" или ""  → 4 (2 байта K + 2 байта V = f16)
//   "q8_0"       → 2 (1 байт K + 1 байт V = 8-bit quant)
//   "q4_0"       → 1 (0.5 байта K + 0.5 байта V = 4-bit quant)
func kvCacheBytesPerType(kvCacheType string) int64 {
	switch strings.ToLower(kvCacheType) {
	case "q8_0":
		return 2
	case "q4_0":
		return 1
	default:
		return 4 // f16
	}
}

// calculateOptimalGPULayersForModel вычисляет оптимальное число GPU-слоёв
// для размещения модели в доступной VRAM с учётом KV-cache и типа KV-cache.
//
// Параметр kvCacheType принимает "f16"/"q8_0"/"q4_0" или "" (default=f16).
//
// Возвращает:
//   - GPULayers: 0..NLayers (0 = CPU-only через mmap)
//   - -1 (=all layers), если модель целиком влезает в VRAM с запасом
//
// Если auto_offload отключён — возвращает currentConfig.DefaultGPULayers как есть.
func calculateOptimalGPULayersForModel(m cppbackend.ModelInfo, kvCacheType string) int {
	if !*autoOffload {
		return currentConfig.DefaultGPULayers
	}
	if m.SizeBytes == 0 || m.NLayers == 0 {
		// Нет метаданных (только что загруженная без path/size) — fallback.
		return currentConfig.DefaultGPULayers
	}
	availableVRAM := availableVRAMBytes()
	if availableVRAM <= 0 {
		// Fallback: пробуем через nvidia-smi
		availableVRAM = nvidiaSmiVRAMBytes()
	}
	if availableVRAM <= 0 {
		// Не смогли узнать VRAM — fallback.
		logger.Get().Warnw("auto_offload: cannot determine available VRAM (NVML + nvidia-smi both failed), using DefaultGPULayers",
			"model", m.Name)
		return currentConfig.DefaultGPULayers
	}
	safetyFactor := 0.85
	safeVRAM := int64(float64(availableVRAM) * safetyFactor)
	overheadBytes := int64(1536) * 1024 * 1024 // 1.5 GB

	// KV-cache для запрошенного n_ctx (bytes)
	// Формула: bytesPerType(v) * n_ctx * n_layers * effKVHeads * headDim
	// где headDim ≈ n_embd (для MHA) или n_embd / n_heads * n_kv_heads (GQA).
	nCtx := m.ContextSize
	if nCtx <= 0 {
		nCtx = currentConfig.DefaultCtxSize
	}
	kvCacheBytes := estimateKVCacheBytes(nCtx, m.NLayers, m.NEmbd, m.NHeads, m.NKvHeads, kvCacheType)

	availableForWeights := safeVRAM - overheadBytes - kvCacheBytes
	if availableForWeights <= 0 {
		// KV-cache + overhead уже съели всё — модель только в RAM (0 GPU слоёв).
		logger.Get().Warnw("auto_offload: KV-cache + overhead exceed safe VRAM, model CPU-only",
			"model", m.Name,
			"safe_vram_mb", safeVRAM/(1024*1024),
			"kv_cache_mb", kvCacheBytes/(1024*1024),
			"kv_cache_type", kvCacheType,
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
		"available_vram_mb", availableVRAM/(1024*1024),
		"safe_vram_mb", safeVRAM/(1024*1024),
		"kv_cache_mb", kvCacheBytes/(1024*1024),
		"kv_cache_type", kvCacheType,
		"n_ctx", nCtx,
		"gpu_layers", gpuLayers)
	return gpuLayers
}

// nvidiaSmiVRAMBytes — fallback для availableVRAMBytes через nvidia-smi.
// Используется когда NVML недоступен. Возвращает свободную VRAM в байтах.
func nvidiaSmiVRAMBytes() int64 {
	cmd := exec.Command("nvidia-smi", "--query-gpu=memory.free", "--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return 0
	}
	// Берём первую строку (первая GPU), отрезаем единицы
	parts := strings.SplitN(line, "\n", 2)
	vramStr := strings.TrimSpace(parts[0])
	vramMiB, err := strconv.ParseInt(vramStr, 10, 64)
	if err != nil {
		return 0
	}
	return vramMiB * 1024 * 1024
}

// estimateKVCacheBytes — оценка размера KV-cache для n_ctx токенов.
//
// Формула для f16 (4 байта на KV-pair на токен на слой):
//   bytes = bytesPerType * n_ctx * n_layers * head_dim * 2 (K+V)
//
// head_dim для MHA = n_embd
// head_dim для GQA = n_embd / n_heads * n_kv_heads
// Для простоты используем среднее: head_dim ≈ n_embd / n_heads * min(n_heads, n_kv_heads)
//
// Параметр kvCacheType влияет на множитель bytesPerType:
//   "f16" или "" → 4
//   "q8_0"      → 2
//   "q4_0"      → 1
func estimateKVCacheBytes(nCtx, nLayers, nEmbd, nHeads, nKvHeads int, kvCacheType string) int64 {
	if nCtx <= 0 || nLayers <= 0 {
		return 0
	}
	if nEmbd <= 0 || nHeads <= 0 {
		// Fallback: 4 MB на 1K токенов для типичной 7B модели.
		return int64(nCtx) * 4096
	}
	effKVHeads := nKvHeads
	if effKVHeads <= 0 {
		// Fallback to MHA (nHeads) when n_kv_heads is unknown.
		// Using MHA is the safe upper bound for VRAM estimation:
		//   - actual GQA models use LESS KV-cache (smaller than estimate)
		//   - actual MHA models match the estimate exactly
		// Going with nHeads/4 (1:4 GQA ratio) would underestimate VRAM for
		// MHA models and cause false OOM. The conservative MHA fallback ensures
		// we never claim more n_ctx than the worst-case model can handle.
		effKVHeads = nHeads
	}
	if effKVHeads <= 0 {
		effKVHeads = 1
	}
	headDim := nEmbd / nHeads
	if headDim <= 0 {
		headDim = nEmbd
	}
	bytesPerType := kvCacheBytesPerType(kvCacheType)
	return bytesPerType * int64(nCtx) * int64(nLayers) * int64(effKVHeads) * int64(headDim)
}
