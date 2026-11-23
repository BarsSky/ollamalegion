// lazyload_calc.go — расчёт оптимальных LoadModelOpts ДО llama.cpp.LoadModel.
//
// Проблема (Issue «qwen3.6 на 20GB VRAM не влезает, на 8GB — да»):
//
//	В lazyload.go:ensureModelLoaded первая загрузка шла с дефолтами:
//	  - ContextSize = *ctxSize (default 32768)
//	  - GPULayers   = *gpuLayers (default 32 или из env)
//	  - autoOffload уменьшал GPULayers грубо (estimatedLayers=80, kvReserve=2GB).
//
//	Для qwen3.6 (~22GB) на 20GB VRAM:
//	  weights + KV-cache (для 32K) ≈ 22 + 10 + 1.5 = 33.5 GB > 20 GB VRAM
//	  → llama.cpp.LoadModel падает с OOM → cppworker возвращает ошибку,
//	    пользователь видит "n_ctx_too_large" или вообще тишину.
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
	AvailableVRAMBytes int64
	AvailableRAMBytes  int64
	MaxViableNCtx      int
	EstimatedKVCacheMB int64
	EstimatedWeightsMB int64

	// R708 (2026-11-23): ЧЕМ посчитан KV CPU-ветки. До этой правки ветка
	// «веса не влезают в VRAM» (Stage 3, gpu_layers=0) брала плоскую формулу
	// «все слои × одна ширина × f16» — для gemma-4-E4B это 3 GiB на 32768
	// токенов против 532 MiB по раскладке, то есть требование к памяти
	// завышалось в 5.8 раза, и n_ctx молча урезался. Теперь KV считается
	// раскладкой (cppbackend.KVPlan → memfit.KVShape), а плоская формула
	// осталась ТОЛЬКО запасным путём, когда раскладки нет.
	//
	// KVPlanSource — cppbackend.KVPlanSourcePlan ("plan") либо
	// cppbackend.KVPlanSourceFlat ("flat"); публикуется в лог и в строку
	// FormatRationale, чтобы вопрос «откуда это число» не требовал чтения кода.
	KVPlanSource    string
	KVPlanExact     bool
	KVLayers        int
	KVGlobalLayers  int
	KVSWALayers     int
	KVSWAWindow     int
	KVPerTokenBytes int64

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

// kvPlanForLazyLoad — раскладка KV по метаданным модели.
//
// R708: переменная, а не прямой вызов, ради ОДНОГО теста — «раскладки нет»
// (старый воркер/битый header/shared_kv съел все слои). На живом стенде такого
// состояния без метаданных не получить: если meta прочиталась, план есть всегда
// (пусть и плоский). Без возможности подставить «плана нет» запасной путь
// оставался бы непроверенным, а он обязан вести себя ровно как до R708.
var kvPlanForLazyLoad = func(modelName string) (cppbackend.KVPlan, bool) {
	if backend == nil {
		return cppbackend.KVPlan{}, false
	}
	return backend.KVPlanForModel(modelName)
}

// resolveLazyLoadKVPlan — раскладка KV модели, дозаполненная КОНСЕРВАТИВНО.
//
// Возвращает ok=false, когда раскладки нет или она неполна настолько, что её
// использование ЗАНИЗИЛО бы требование к памяти. Правило направления: любая
// нехватка данных решается в пользу БОЛЬШЕГО KV (плоская формула по модели),
// потому что занижение KV — переподписка VRAM/RAM, а завышение — лишь
// осторожный n_ctx.
//
// Ширины и число KV-голов дозаполняются теми же приёмами, что в
// cppbackend.withFallbackWidthsR707 (для /api/models и ComputeFeasible):
//   - K: <arch>.attention.key_length, иначе n_embd/n_heads;
//   - V: <arch>.attention.value_length, иначе равна K (llama.cpp:
//     value_length опционален, llama-model.cpp:1662-1663);
//   - KV-головы: <arch>.attention.head_count_kv, иначе n_heads (MHA).
//
// Слои с кэшем берутся ТОЛЬКО из плана (KVLayers): у gemma-4 их 24 из 42, у
// Qwen3.8-9B — 8 attention-слоёв из 33; подменять их на block_count нельзя.
func resolveLazyLoadKVPlan(modelName string, meta *cppbackend.GGUFModelMeta) (cppbackend.KVPlan, bool) {
	if meta == nil {
		return cppbackend.KVPlan{}, false
	}
	plan, ok := kvPlanForLazyLoad(modelName)
	if !ok || plan.KVLayers() <= 0 {
		return cppbackend.KVPlan{}, false
	}
	headDimK := plan.GlobalHeadDim
	if headDimK <= 0 {
		headDimK = meta.KVHeadDim()
	}
	if headDimK <= 0 {
		return cppbackend.KVPlan{}, false
	}
	headDimV := plan.GlobalHeadDimV
	if headDimV <= 0 {
		headDimV = meta.KVHeadVDim()
	}
	if headDimV <= 0 {
		headDimV = headDimK
	}
	heads := plan.GlobalKVHeads
	if heads <= 0 {
		heads = meta.NKvHeads
	}
	if heads <= 0 {
		heads = meta.NHeads
	}
	if heads <= 0 {
		return cppbackend.KVPlan{}, false
	}
	plan.GlobalHeadDim, plan.GlobalHeadDimV, plan.GlobalKVHeads = headDimK, headDimV, heads
	// Оконный класс: своя ширина есть только при <arch>.attention.key_length_swa.
	// Нет ключа — считаем шириной глобального класса. Это ВЕРХНЯЯ оценка KV
	// оконных слоёв (gemma-4: 512 вместо реальных 256), то есть безопасная.
	if plan.SWALayers > 0 {
		if plan.SWAHeadDim <= 0 {
			plan.SWAHeadDim = headDimK
		}
		if plan.SWAHeadDimV <= 0 {
			plan.SWAHeadDimV = plan.SWAHeadDim
		}
		if plan.SWAKVHeads <= 0 {
			plan.SWAKVHeads = heads
		}
	}
	return plan, true
}

// cpuOnlyKVBudgetR708 — KV CPU-ветки по раскладке (plan) либо плоской формулой
// (flat). Возвращает KV на токен, потолок n_ctx и ЧЕМ считали.
//
// Оконные слои насыщаются окном, поэтому потолок считается НЕ делением бюджета
// на «KV на токен»: после окна растут только глобальные слои, и обратная
// формула живёт в memfit.KVShape.MaxCtxForBudget (одна на весь стек). На
// gemma-4-E4B прежнее деление давало ~11.5K токенов, обратная формула — ~67.5K:
// плоская оценка видела рост всех 24 слоёв там, где 20 из них уже не растут.
//
// kvType — пустой: тип KV-cache до загрузки неизвестен, f16 (наибольший KV) —
// единственное безопасное допущение, ровно как было в прежней формуле.
func cpuOnlyKVBudgetR708(plan cppbackend.KVPlan, planOK bool, meta *cppbackend.GGUFModelMeta,
	budgetBytes int64, flatKVBytes int64, requestedNCtx int) (kvPerToken int64, maxCtx int, source string) {

	if planOK {
		kvPerToken = plan.KVPerTokenBytes("")
		if kvPerToken > 0 {
			maxCtx = plan.MaxCtxForBudget("", budgetBytes, 0)
			// Источник берём у САМОГО плана: плоский план (все слои глобальные)
			// — это прежняя формула, и называть его "plan" значило бы врать.
			source = plan.Source
			if source == "" {
				source = cppbackend.KVPlanSourceFlat
			}
			return kvPerToken, maxCtx, source
		}
	}
	// Прежний путь: KV на токен из плоской оценки на запрошенном окне.
	if requestedNCtx > 0 && flatKVBytes > 0 {
		kvPerToken = flatKVBytes / int64(requestedNCtx)
	}
	if kvPerToken <= 0 && meta != nil && meta.NLayers > 0 && meta.NEmbd > 0 && meta.NHeads > 0 {
		// Прежний запасной расчёт «средней ширины головы» (был здесь же до R708).
		kvPerToken = int64(meta.NLayers) * int64(meta.NEmbd) / int64(meta.NHeads) * 4 * 2
	}
	if kvPerToken <= 0 {
		kvPerToken = 4096 // прежний fallback «4 KB на токен»
	}
	if budgetBytes > 0 {
		maxCtx = int(budgetBytes / kvPerToken)
	}
	return kvPerToken, maxCtx, cppbackend.KVPlanSourceFlat
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

	// R708 (2026-11-23): РАСКЛАДКА KV — до решений о памяти.
	//
	// Зачем до решений. CPU-ветка (Stage 3) обязана считать KV по классам слоёв:
	// у gemma-4-E4B кэш держат 24 слоя из 42, причём 20 из них живут в окне 512
	// токенов и шириной 256, а не 512. Плоская формула даёт на 32768 токенах
	// 3 GiB против 532 MiB по раскладке, поэтому окно молча урезалось, а на
	// 8 GB карте модель уходила в CPU-only. Stage 1/2 оставлены на плоской
	// оценке НАМЕРЕННО: она завышает KV, то есть решение остаётся осторожным.
	kvPlan, kvPlanOK := resolveLazyLoadKVPlan(modelName, meta)
	if kvPlanOK {
		logger.Get().Debugw("calculateLazyLoadOpts: KV plan resolved",
			"model", modelName,
			"kv_source", kvPlan.Source,
			"kv_exact", kvPlan.Exact,
			"kv_layers", kvPlan.KVLayers(),
			"kv_global_layers", kvPlan.GlobalLayers,
			"kv_swa_layers", kvPlan.SWALayers,
			"kv_swa_window", kvPlan.SWAWindow,
			"kv_per_token_bytes", kvPlan.KVPerTokenBytes(""))
	} else {
		logger.Get().Debugw("calculateLazyLoadOpts: KV plan unavailable, flat formula", "model", modelName)
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
	//
	// R83 §9.4 шаг 2, продолжение (2026-09-27): по реальным параметрам кэша из
	// метаданных (kv_layers/attention.key_length), если они есть — иначе прежняя
	// консервативная оценка. Число идёт в rationale и в текст отказа клиенту,
	// поэтому завышение здесь видел оператор.
	kvBytes := estimateKVCacheBytes(requestedNCtx, meta.NLayers, meta.NEmbd, meta.NHeads, meta.NKvHeads, "")
	if kvLayers, _ := meta.KVLayers(); kvLayers > 0 {
		if headDim := meta.KVHeadDim(); headDim > 0 {
			kvBytes = estimateKVCacheBytesForLayers(requestedNCtx, kvLayers, meta.NKvHeads, headDim, "")
		}
	}
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
	safetyFactor := 0.85
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
	//
	// R708: KV считается ПО РАСКЛАДКЕ (KVPlan → memfit.KVShape), если она есть.
	// Прежний путь — плоская формула — остаётся запасным и помечается "flat".
	budgetCPUOnly := safeVRAM - overheadBytes
	kvPerToken, maxNCtxCPUOnly, kvSource := cpuOnlyKVBudgetR708(kvPlan, kvPlanOK, meta,
		budgetCPUOnly, kvBytes, requestedNCtx)
	if maxNCtxCPUOnly < 0 {
		maxNCtxCPUOnly = 0
	}

	// Что реально использовано — и в rationale, и в лог. Без этого «KV урезал
	// окно» снова превращалось бы в догадку: число есть, источника не видно.
	rationale.KVPlanSource = kvSource
	rationale.KVPerTokenBytes = kvPerToken
	if kvPlanOK && kvSource == cppbackend.KVPlanSourcePlan {
		rationale.KVPlanExact = kvPlan.Exact
		rationale.KVLayers = kvPlan.KVLayers()
		rationale.KVGlobalLayers = kvPlan.GlobalLayers
		rationale.KVSWALayers = kvPlan.SWALayers
		rationale.KVSWAWindow = kvPlan.SWAWindow
		// KV на запрошенном окне — то же число, которое уйдёт в отказ клиенту
		// (InsufficientResourcesError.KVCacheRequiredMB), поэтому считаем его
		// по раскладке, а не плоской формулой.
		if planKV := kvPlan.KVBytesAtCtx("", requestedNCtx); planKV > 0 {
			rationale.EstimatedKVCacheMB = planKV / (1024 * 1024)
		}
	} else {
		if l, _ := meta.KVLayers(); l > 0 {
			rationale.KVLayers = l
		}
	}

	finalNCtx := requestedNCtx
	if int64(requestedNCtx) > int64(maxNCtxCPUOnly) {
		finalNCtx = maxNCtxCPUOnly
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
		// R83 §9.3 (2026-09-26): Source="fallback_no_fit" — это НЕ «попробуем
		// всё равно», а «не влезает даже в CPU-only». Возвращаем opts как есть
		// (поведение calculateLazyLoadOpts не меняем), но вызывающие обязаны
		// отказать: см. insufficientResourcesFromFallbackNoFit() и её вызовы
		// в lazyload.go (ensureModelLoaded) и handlers_model.go
		// (handleLoadWithParams). Иначе llama.cpp получает заведомо
		// непомещающийся план и падает OOM (или грузит 16 GB в RAM, вытесняя
		// всё остальное).
		rationale.Source = "fallback_no_fit"
		rationale.MaxViableNCtx = maxNCtxCPUOnly
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
	rationale.MaxViableNCtx = maxNCtxCPUOnly
	if requestedNCtx > 0 {
		rationale.NCtxReductionPct = float64(requestedNCtx-finalNCtx) / float64(requestedNCtx) * 100.0
	}

	logger.Get().Infow("calculateLazyLoadOpts: reducing n_ctx for cpu-only offload",
		"model", modelName,
		"requested_n_ctx", requestedNCtx,
		"reduced_n_ctx", finalNCtx,
		"max_viable_n_ctx_cpu_only", maxNCtxCPUOnly,
		"vram_safe_mb", safeVRAM/(1024*1024),
		"kv_source", kvSource,
		"kv_layers", rationale.KVLayers,
		"kv_global_layers", rationale.KVGlobalLayers,
		"kv_swa_layers", rationale.KVSWALayers,
		"kv_swa_window", rationale.KVSWAWindow,
		"kv_per_token", kvPerToken,
		"kv_cache_mb_at_requested_n_ctx", rationale.EstimatedKVCacheMB,
		"reason", "VRAM insufficient for weights+KV-cache at requested n_ctx")

	return cppbackend.LoadModelOpts{
		GPULayers:   0,
		ContextSize: finalNCtx,
		BatchSize:   *batchSize,
		UseMmap:     true,
	}, rationale
}

// insufficientResourcesFromFallbackNoFit — R83 §9.3 (2026-09-26).
//
// Строит отказ для случая «модель не влезает даже в CPU-only». Раньше этот
// случай (Source="fallback_no_fit") только писал error в лог, и загрузка
// продолжалась: 16.4 GB модель уходила в RAM целиком, вытесняя всё остальное, а
// клиент узнавал о проблеме по таймауту или OOM от llama.cpp.
//
// Возвращает nil, если rationale относится к другому случаю. Форма ошибки —
// *InsufficientResourcesError: её понимает handleInferenceError (HTTP 413 +
// structured JSON с code=6), и балансер проксирует ответ клиенту без изменений.
func insufficientResourcesFromFallbackNoFit(modelName string, r LazyLoadRationale) error {
	// В адаптивном пути (lazyload.go: SelectStrategy) Source заполняется из
	// strategy.Stage, то есть тем же значением "fallback_no_fit".
	if r.Source != "fallback_no_fit" {
		return nil
	}
	return &InsufficientResourcesError{
		Model:              modelName,
		RequestedNCtx:      r.RequestedNCtx,
		MaxViableNCtx:      r.MaxViableNCtx,
		AvailableVRAMMB:    r.AvailableVRAMBytes / (1024 * 1024),
		AvailableRAMMB:     r.AvailableRAMBytes / (1024 * 1024),
		ModelSizeBytes:     r.ModelSize,
		KVCacheRequiredMB:  r.EstimatedKVCacheMB,
		GPULayersAttempted: r.AppliedGPULayers,
	}
}

// FormatRationale — обоснование для логирования.
func (r LazyLoadRationale) FormatRationale() string {
	// R708: kv_source/kv_layers/kv_per_token — ЧЕМ посчитан KV и на чём он
	// стоит. До этой правки оператор видел только «reduction_pct»: вопрос
	// «почему окно урезано на gemma-4» требовал чтения кода.
	kv := "kv_source=" + r.KVPlanSource
	if r.KVLayers > 0 {
		kv += fmt.Sprintf(" kv_layers=%d kv_global_layers=%d kv_swa_layers=%d kv_swa_window=%d",
			r.KVLayers, r.KVGlobalLayers, r.KVSWALayers, r.KVSWAWindow)
	}
	if r.KVPerTokenBytes > 0 {
		kv += fmt.Sprintf(" kv_per_token=%d", r.KVPerTokenBytes)
	}
	if r.KVPlanExact {
		kv += " kv_exact=true"
	}
	return fmt.Sprintf(
		"source=%s requested_nctx=%d applied_nctx=%d reduction_pct=%.1f%% requested_gpu=%d applied_gpu=%d gpu_reduced=%v arch=%s nlayers=%d nembd=%d size_mb=%d available_vram_mb=%d max_viable_nctx=%d %s",
		r.Source, r.RequestedNCtx, r.AppliedNCtx, r.NCtxReductionPct,
		r.RequestedGPULayers, r.AppliedGPULayers, r.GPULayersReduced,
		r.ArchName, r.NLayers, r.NEmbd,
		r.ModelSize/(1024*1024),
		r.AvailableVRAMBytes/(1024*1024),
		r.MaxViableNCtx,
		kv,
	)
}
