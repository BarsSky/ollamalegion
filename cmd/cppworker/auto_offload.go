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
	"ollama-loadbalancer/internal/memfit"
	"ollama-loadbalancer/pkg/logger"
	"strings"
)

// kvCacheBytesPerType — байт на KV-элемент для типа кэша.
//
// ВНИМАНИЕ: возвращает ЦЕЛОЕ число байт и потому для квантованных типов теряет
// дробную часть (q8_0 — 34/32 = 1.0625 байта, q4_0 — 18/32 = 0.5625). Для
// планирования раскладки и подсчёта KV используйте kvCacheBytesPerTypeRatio,
// который берёт отношение из memfit (единый источник истины) и не теряет
// точность. Эта функция оставлена только для путей, где нужен грубый «байт на
// пару K+V» (f16 → 4, q8_0 → 2, q4_0 → 1).
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

// kvCacheBytesPerTypeRatio — байт на KV-ЭЛЕМЕНТ с учётом типа (числитель, знаменатель).
//
// Единый источник — memfit.KVBytesPerElement: та же константа, что и в memfit.
// R83 §9.4 шаг 2 (2026-09-27): раньше здесь была своя таблица (2 байта на
// q8_0), и на реальных числах она расходилась с memfit (34/32 против 2). Два
// источника истины для одной константы — ровно тот класс дефекта, который шаг 2
// и устраняет.
//
// ВНИМАНИЕ: это байт на ОДИН элемент (K или V), множитель «2 тензора» считает
// вызывающий. Сложить его сюда — значит завысить KV ровно вдвое (эта ошибка
// была допущена и поймана тестом TestR83_Step2_KVMatchesMemfit).
func kvCacheBytesPerTypeRatio(kvCacheType string) (num, den int64) {
	return memfit.KVBytesPerElement(cppbackend.MemfitKVType(kvCacheType))
}

// calculateOptimalGPULayersForModel вычисляет оптимальное число GPU-слоёв
// для размещения модели в доступной VRAM с учётом KV-cache и типа KV-cache.
//
// R83 §9.4 шаг 1 (2026-09-26): решение принимает memfit — единственный предикат
// computeSplit, тот же, что и в checkVRAMForModel. Прежняя формула (веса ×0.7,
// KV «256 Б/токен») ЗАНИЖАЛА число слоёв: на живом стенде (3070 8 GB +
// Qwen3.8-27B) она давала 18 слоёв при q8_0 и 0 при f16, тогда как memfit — 23
// и 22. Ноль означал загрузку модели целиком на CPU, то есть исторический
// симптом «gpu_layers=0 → непригодно медленно»
// (см. internal/cppbackend/estimator_compare_r83_test.go).
//
// Legacy-формула не удалена, а вызывается ТОЛЬКО как fallback, когда memfit
// судить не о чем: нет глобального backend'а (установка конфига до инициализации,
// тесты) или нет метаданных модели. Это сохраняет прежнее поведение в тех
// случаях, где memfit данных не имеет, — и делает замену проверяемой по шагам.
//
// Параметр kvCacheType принимает "f16"/"q8_0"/"q4_0" или "" (default=f16).
//
// Возвращает:
//   - GPULayers: 0..NLayers (0 = CPU-only через mmap)
//   - -1 (=all layers), если модель целиком влезает в VRAM с запасом
//
// Если auto_offload отключён — возвращает currentConfig.DefaultGPULayers как есть.
func calculateOptimalGPULayersForModel(m cppbackend.ModelInfo, kvCacheType string) int {
	// currentConfig заполняется в main при старте; в тестах/до инициализации его
	// может не быть — тогда судить не о чем, и вызывающий получит -1 (=auto),
	// то есть решение всё равно примет checkVRAMForModel через memfit.
	if currentConfig == nil {
		return -1
	}
	if !*autoOffload {
		return currentConfig.DefaultGPULayers
	}
	if layers, ok := memfitGPULayersForModel(m, kvCacheType); ok {
		return layers
	}
	return legacyCalculateOptimalGPULayersForModel(m, kvCacheType)
}

// memfitPlanForModel — раскладка через memfit БЕЗ потери вердикта.
//
// ok=false означает «судить не о чем» (нет backend'а, модели в каталоге или
// метаданных) — вызывающий обязан уйти в legacy-fallback, а не выдумывать числа.
//
// R83 §9.4 шаг 1б (2026-09-26): вызывающим нужен не только GPULayers, но и
// Stage/Fits/MaxHardCtx — чтобы отличить «влезает частично» (работает, хотя и
// медленнее) от «не влезает суммарно» (загрузка заведомо провалится) и назвать
// потолок в подсказке отказа.
func memfitPlanForModel(m cppbackend.ModelInfo, kvCacheType string) (memfit.Verdict, bool) {
	if backend == nil || m.Name == "" {
		return memfit.Verdict{}, false
	}
	spec, ok := backend.MemfitSpec(m.Name)
	if !ok || !spec.Complete() {
		return memfit.Verdict{}, false
	}
	nCtx := m.ContextSize
	if nCtx <= 0 {
		nCtx = currentConfig.DefaultCtxSize
	}
	return memfit.Evaluate(spec, memfit.Request{
		Ctx:    nCtx,
		KVType: cppbackend.MemfitKVType(kvCacheType),
	}, backend.MemfitBudget(), cppbackend.MemfitPolicy()), true
}

// memfitGPULayersForModel — число GPU-слоёв по вердикту memfit (обёртка над
// memfitPlanForModel). ok=false — «судить не о чем», см. выше.
func memfitGPULayersForModel(m cppbackend.ModelInfo, kvCacheType string) (int, bool) {
	v, ok := memfitPlanForModel(m, kvCacheType)
	if !ok {
		return 0, false
	}
	layers := v.GPULayers
	if layers < 0 {
		layers = 0
	}
	if v.TotalLayers > 0 && layers > v.TotalLayers {
		layers = v.TotalLayers
	}
	logger.Get().Infow("auto_offload: gpu_layers from memfit (R83 §9.4)",
		"model", m.Name,
		"n_ctx", m.ContextSize,
		"kv_cache_type", kvCacheType,
		"stage", string(v.Stage),
		"gpu_layers", layers,
		"total_layers", v.TotalLayers,
		"vram_used_mb", (v.GPUWeights + v.GPUKV).MiB(),
		"usable_vram_mb", v.UsableVRAM.MiB(),
		"usable_ram_mb", v.UsableRAM.MiB(),
		"max_exact_fit_n_ctx", v.MaxExactFitCtx,
		"max_hard_n_ctx", v.MaxHardCtx,
		"suggestion", v.Suggestion)
	return layers, true
}

// legacyCalculateOptimalGPULayersForModel — прежняя формула (веса ×0.7,
// KV «256 Б/токен»). Оставлена как fallback без метаданных; удаляется шагом 2
// протокола plans/2026-09-26-estimator-unification-protocol.md.
func legacyCalculateOptimalGPULayersForModel(m cppbackend.ModelInfo, kvCacheType string) int {
	if m.SizeBytes == 0 || m.NLayers == 0 {
		// Нет метаданных (только что загруженная без path/size) — fallback.
		return currentConfig.DefaultGPULayers
	}
	// R83 §9.4 шаг 4 (2026-09-26): спрашиваем СВОБОДНУЮ VRAM, а не полную.
	// Прежний код брал availableVRAMBytes() — это ёмкость карты (bridge отдаёт
	// VRAMTotalMB), на 3070 это 8 GB против ~1 GB фактически свободных. Из-за
	// этого legacy-фоллбэк планировал слои в VRAM, которой уже нет.
	freeVRAM := freeVRAMBytes()
	if freeVRAM <= 0 {
		// Fallback: nvidia-smi --query-gpu=memory.free
		freeVRAM = tryNvidiaSMIFree()
	}
	if freeVRAM <= 0 {
		// Могли не получить free (старый bridge) — берём полную ёмкость как
		// верхнюю границу и предупреждаем: оценка будет оптимистичной.
		freeVRAM = availableVRAMBytes()
		if freeVRAM > 0 {
			logger.Get().Warnw("auto_offload: свободная VRAM неизвестна, используем полную ёмкость карты "+
				"(оценка числа слоёв оптимистична)",
				"model", m.Name, "total_vram_mb", freeVRAM/(1024*1024))
		}
	}
	if freeVRAM <= 0 {
		// Не смогли узнать VRAM — fallback.
		logger.Get().Warnw("auto_offload: cannot determine available VRAM (NVML + nvidia-smi both failed), using DefaultGPULayers",
			"model", m.Name)
		return currentConfig.DefaultGPULayers
	}
	availableVRAM := freeVRAM
	safetyFactor := 0.85
	safeVRAM := int64(float64(availableVRAM) * safetyFactor)
	overheadBytes := int64(1536) * 1024 * 1024 // 1.5 GB

	// KV-cache для запрошенного n_ctx (bytes)
	//
	// R83 §9.4 шаг 2 (2026-09-27): считаем по РЕАЛЬНЫМ параметрам KV-кэша
	// (число слоёв с attention + head_dim KV), а не по block_count и
	// n_embd/n_heads. На Qwen3.8-27B прежняя формула давала 64×213 против
	// реальных 16×256, то есть завышала KV в 3.3 раза — именно это исторически
	// загоняло раскладку в gpu_layers=0. Метаданные берём у backend'а тем же
	// путём, что и memfit (KVLayersForModel); если их нет — прежняя
	// консервативная оценка + предупреждение в лог.
	nCtx := m.ContextSize
	if nCtx <= 0 {
		nCtx = currentConfig.DefaultCtxSize
	}
	// kvLayers/kvHeadDim — из метаданных; nKvHeads известен из ModelInfo всегда,
	// но при нуле берём nHeads (MHA — консервативная верхняя граница).
	kvLayers, kvHeadDim := m.NLayers, 0
	nKvHeads := m.NKvHeads
	if nKvHeads <= 0 {
		nKvHeads = m.NHeads
	}
	if backend != nil {
		if l, hd, kvOK := backend.KVLayersForModel(m.Name); l > 0 && hd > 0 {
			kvLayers, kvHeadDim = l, hd
			logger.Get().Infow("auto_offload: KV посчитан по реальным параметрам кэша (R83 §9.4 шаг 2)",
				"model", m.Name,
				"kv_layers", l,
				"kv_head_dim", hd,
				"n_kv_heads", nKvHeads,
				"block_count", m.NLayers,
				"kv_exact", kvOK)
		}
	}
	kvCacheBytes := estimateKVCacheBytesForLayers(nCtx, kvLayers, nKvHeads, kvHeadDim, kvCacheType)

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

// estimateKVCacheBytesForLayers — KV-cache из РЕАЛЬНЫХ параметров кэша.
//
// R83 §9.4 шаг 2 (2026-09-27). Отличие от estimateKVCacheBytes (ниже) только во
// входных числах: там n_layers = block_count и head_dim ≈ n_embd/n_heads, здесь —
// kv_layers (сколько слоёв реально держат KV) и kv_head_dim (attention.key_length).
//
// Формула ОДНА с memfit.KVBytesPerToken:
//
//	2 (K и V) × kv_layers × n_kv_heads × kv_head_dim × байт-на-элемент
//
// Про n_kv_heads легко забыть, и это не косметика: без множителя KV занижается
// ровно в число голов (на Qwen3.8-27B — в 8 раз), то есть раскладка планирует
// KV в 8 раз меньше реального и уходит в переподписку. Тест
// TestR83_Step2_KVMatchesLlamaCpp фиксирует абсолютное число из лога llama.cpp
// (1088 MiB при 32768 токенах), поэтому пропустить это нельзя.
//
// Живой пример (3070 8 GB, Qwen3.8-27B): 16 слоёв × 8 голов × 256 = 32 768
// элементов на токен; q8_0 → 34/32 байта → 34 816 Б/токен → × 32 768 токенов =
// 1088 MiB, ровно как в логе
// `llama_kv_cache: size = 1088.00 MiB (32768 cells, 16 layers, K/V (q8_0))`.
func estimateKVCacheBytesForLayers(nCtx, kvLayers, nKvHeads, kvHeadDim int, kvCacheType string) int64 {
	if nCtx <= 0 || kvLayers <= 0 || nKvHeads <= 0 || kvHeadDim <= 0 {
		return 0
	}
	// Единая константа с memfit (34/32 для q8_0, 18/32 для q4_0).
	num, den := kvCacheBytesPerTypeRatio(kvCacheType)
	elements := int64(kvLayers) * int64(nKvHeads) * int64(kvHeadDim) * 2
	return int64(nCtx) * elements * num / den
}

// estimateKVCacheBytes — оценка размера KV-cache для n_ctx токенов.
//
// ВНИМАНИЕ: это КОНСЕРВАТИВНАЯ ОЦЕНКА СВЕРХУ для путей, где известны только
// параметры модели (block_count, n_embd, n_heads, n_kv_heads). Она завышает
// KV для гибридных моделей (где KV держат не все слои) и для моделей с
// attention.key_length ≠ n_embd/n_heads. Там, где есть метаданные GGUF,
// используйте estimateKVCacheBytesForLayers — иначе раскладка снова начнёт
// занижать gpu_layers (см. R83 §9.4 шаг 2).
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
