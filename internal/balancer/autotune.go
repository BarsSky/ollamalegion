// autotune.go — Round 54.1 (2026-08-24): autonomous balancer.
//
// Цель: привести балансер к автономному режиму, который:
//   1. Детектит sub-optimal состояние бэкенда (loaded model с плохими params)
//   2. Вычисляет optimal params на основе model + hardware
//   3. Применяет авто-фикс (async reload) если AutoTune включен
//   4. Сохраняет ручное управление (per-model `autoTune: false`, env overrides)
//
// R54.1 (этот коммит): READ-ONLY детект sub-optimal state.
//   - Модуль AutoTune с методами Detect/Analyze/Recommend
//   - Recommendation field в /api/v1/backends response
//   - Никаких авто-reload (R54.4)
//
// R54.2 (следующий): AutoTune config flag + per-model override.
// R54.3: Manual trigger endpoint /api/v1/admin/autotune.
// R54.4: Autonomous async reload с circuit breaker.
// R54.5: Adaptive streaming (KV cache auto-pick based on request size).

package balancer

import (
	"errors"
	"fmt"
)

// errAutoTuneReloadDeferred — R280 (2026-10-29): перезагрузка ОТЛОЖЕНА, потому
// что на бэкенде идут чужие стримы.
//
// Почему отдельная ошибка, а не nil: нужно отличить «применили» от «не
// применяли» — вызывающий не должен записывать success в circuit breaker за
// операцию, которой не было. И почему не обычная ошибка: в circuit breaker
// попадать тоже нельзя, отложение не является сбоем AutoTune.
var errAutoTuneReloadDeferred = errors.New("autotune reload deferred: active streams on backend")

// AutoTuneRecommendation — рекомендация по оптимизации загруженной модели.
// Поле Severity: "ok" | "info" | "warning" | "critical".
// Поле Suggestion: человекочитаемое объяснение + конкретное действие.
type AutoTuneRecommendation struct {
	Severity   string `json:"severity"`
	Category   string `json:"category"`   // "kv_cache", "context", "layers", "flash_attn"
	Message    string `json:"message"`    // кратко: что не оптимально
	Suggestion string `json:"suggestion"` // действие: что сделать
	// Current → Recommended (если есть конкретный fix)
	CurrentKVCache          string `json:"currentKvCache,omitempty"`
	RecommendedKVCache      string `json:"recommendedKvCache,omitempty"`
	CurrentNumCtx           int    `json:"currentNumCtx,omitempty"`
	RecommendedNumCtx       int    `json:"recommendedNumCtx,omitempty"`
	CurrentNumGPULayers     int    `json:"currentNumGpuLayers,omitempty"`
	RecommendedNumGPULayers int    `json:"recommendedNumGpuLayers,omitempty"`
}

// AutoTuneAnalysis — результат анализа одного loadedModel.
type AutoTuneAnalysis struct {
	ModelName    string `json:"modelName"`
	IsSubOptimal bool   `json:"isSubOptimal"`
	// R54.2: AutoTuneEnabled — per-model switch (after inheritance).
	// false = AutoTune отключён для этой модели (manual control).
	AutoTuneEnabled bool                     `json:"autoTuneEnabled"`
	Recommendations []AutoTuneRecommendation `json:"recommendations"`
	// Optional: computed optimal params (nil если current is already optimal)
	OptimalContextLength int    `json:"optimalContextLength,omitempty"`
	OptimalKVCacheType   string `json:"optimalKvCacheType,omitempty"`
	OptimalNumGPULayers  int    `json:"optimalNumGpuLayers,omitempty"`
	AnalysisReasoning    string `json:"analysisReasoning,omitempty"`
}

// AutoTuneReport — отчёт для одного бэкенда.
type AutoTuneReport struct {
	BackendID   string `json:"backendId"`
	BackendType string `json:"backendType"`
	// AutoTuneEnabled (R54.2) — глобальный switch. Per-model override
	// отражается в каждой AutoTuneAnalysis.AutoTuneEnabled ниже.
	AutoTuneEnabled bool               `json:"autoTuneEnabled"`
	Models          []AutoTuneAnalysis `json:"models"`
	OverallSeverity string             `json:"overallSeverity"` // worst severity
	OverallSummary  string             `json:"overallSummary"`
}

// ModelProfileInfo — минимальные данные о модели для AutoTune.
type ModelProfileInfo struct {
	Name           string
	Quantization   string // "Q4_K_M", "Q8_0", etc.
	Architecture   string // "qwen3", "gemma-4", etc.
	NLayers        int
	NEmbd          int
	NKvHeads       int
	HeadDimK       int
	HeadDimV       int
	SizeBytes      uint64
	GgufMaxContext int // max n_ctx for this model
	// R707 (2026-11-25): раскладка KV по слоям (из /api/models).
	//
	// Без неё KV считается как NLayers × NKvHeads × HeadDimK × n_ctx. Это верно
	// только для моделей, где кэш живёт во всех слоях: у gemma-4-E4B его держат
	// 24 слоя из 42 (4 глобальных + 20 оконных с окном 512), у Qwen3.8-9B —
	// только attention-слои (8 из 33). Плоская формула завышала KV в 3-7 раз:
	// R659 отказывал в загрузке на свободный узел, AutoTune советовал q4_0.
	//
	// KVPlanSource == "plan" → считать по классам (KVGlobalLayers/KVSWALayers);
	// пусто или "flat" → прежняя плоская формула (старый cppworker).
	KVPlanSource   string
	KVGlobalLayers int
	KVSWALayers    int
	KVSWAWindow    int
	HeadDimSWA     int
	HeadDimSWAV    int
	NKvHeadsSWA    int
	// Current loaded state
	CurrentContextLength int
	CurrentKVCacheType   string
	CurrentNumGPULayers  int
	CurrentFlashAttn     int
	CurrentUseMmap       bool
	// Hardware constraints
	FreeVRAMBytes      uint64
	FreeRAMBytes       uint64
	TotalVRAMBytes     uint64
	FeasibleMaxContext int // from cppworker
	// R54.2: AutoTune switch (после per-model override resolution).
	// UI показывает это как badge: "AutoTune ON" / "AutoTune OFF (manual)".
	AutoTuneEnabled bool
}

// computeOptimalKVCacheType — Round 54.1: вычисляет optimal KV cache type.
//
// Логика (conservative, для 4B-class моделей на RTX 3070 8GB):
//   - Если model size < 3.5GB (4B Q4_K_M) И freeVRAM > 4GB → f16 (high quality)
//   - Иначе если model size < 6GB → q8_0 (compromise)
//   - Иначе → q4_0 (max context)
//
// Возвращает рекомендацию и булеан "isBetter" (true если отличается от текущего).
func computeOptimalKVCacheType(p *ModelProfileInfo) (recommendation string, reasoning string) {
	return computeOptimalKVCacheTypeWithWorkload(p, WorkloadStats{}, DefaultWorkloadTrackerConfig().MinSamplesForRecommendation)
}

// computeOptimalKVCacheTypeWithWorkload — Round 54.9: workload-aware KV cache selection.
//
// Логика поверх R54.1:
//   - Если workload.IsLight(loadedNCtx, minSamples) → f16 (high quality, low mem usage)
//     Типичный случай: 4B модель с n_ctx=65536, но реально используется p95=4096 → f16
//     сохраняет memory и улучшает качество (precision loss = 0).
//   - Если workload.IsHeavy(loadedNCtx, minSamples) → q4_0 (max context)
//     Типичный случай: burst workloads (long RAG context), p95 близко к loaded n_ctx.
//   - Иначе → R54.1 logic (model size + free VRAM).
//
// minSamples защищает от cold start: без samples workload неопределён,
// fallback к R54.1 logic.
func computeOptimalKVCacheTypeWithWorkload(p *ModelProfileInfo, workload WorkloadStats, minSamples int) (recommendation string, reasoning string) {
	if p == nil {
		return "", "нет профиля модели"
	}
	if p.NLayers <= 0 || p.NKvHeads <= 0 || p.HeadDimK <= 0 {
		// Missing architecture data — can't compute, fall back to current
		return p.CurrentKVCacheType, "недостаточно данных для расчёта (NLayers/NKvHeads/HeadDimK не указаны)"
	}
	if p.FreeVRAMBytes == 0 {
		// R230: пустая VRAM в профиле = неполные метрики. Раньше это давало
		// «свободно 0 GB» и совет q4_0; правильнее не трогать конфигурацию.
		return p.CurrentKVCacheType, "нет данных о свободной VRAM (метрики неполные) — оставляем текущий KV cache"
	}

	// R230: бюджет = free (уже reload-бюджет: free + usage модели, см.
	// AnalyzeBackend/triggerAutoTuneReload). Считаем в байтах, не в «GB».
	budgetGB := bytesToGB(p.FreeVRAMBytes)
	reserve := autotuneReserveBytes(p.TotalVRAMBytes)
	reserveGB := bytesToGB(reserve)
	modelSizeGB := bytesToGB(p.SizeBytes)

	availForKV := int64(0)
	if int64(p.FreeVRAMBytes) > int64(reserve)+int64(p.SizeBytes) {
		availForKV = int64(p.FreeVRAMBytes) - int64(reserve) - int64(p.SizeBytes)
	}
	availKVGB := bytesToGB(uint64(availForKV))

	fits := func(kvType string) bool {
		need := kvCacheBytesForContext(p, kvType, p.CurrentContextLength)
		return need > 0 && int64(need) <= availForKV
	}

	// R54.9: workload-aware overrides — но только физически исполнимая рекомендация
	// (R230: f16 не советуем, если KV для загруженного n_ctx не влезает в бюджет).
	loadedNCtx := p.CurrentContextLength
	if loadedNCtx > 0 {
		if workload.IsLight(loadedNCtx, minSamples) && fits("f16") {
			return "f16", fmt.Sprintf(
				"R54.9 workload-light: p95=%d < 25%% от loaded=%d, f16 KV (%.1fGB) влезает в бюджет %.1fGB → f16 (high quality)",
				workload.P95NumCtx, loadedNCtx,
				bytesToGB(kvCacheBytesForContext(p, "f16", loadedNCtx)), budgetGB)
		}
		if workload.IsHeavy(loadedNCtx, minSamples) && fits("q4_0") {
			return "q4_0", fmt.Sprintf(
				"R54.9 workload-heavy: p95=%d > 75%% от loaded=%d, q4_0 KV (%.1fGB) влезает в бюджет %.1fGB → q4_0 (max context)",
				workload.P95NumCtx, loadedNCtx,
				bytesToGB(kvCacheBytesForContext(p, "q4_0", loadedNCtx)), budgetGB)
		}

		// R230: fit-aware выбор типа KV для фактического n_ctx.
		switch {
		case fits("f16"):
			return "f16", fmt.Sprintf(
				"f16 KV (%.1fGB) для n_ctx=%d влезает в бюджет %.1fGB (free+usage модели, model=%.1fGB, резерв=%.1fGB) → f16 (лучшее качество)",
				bytesToGB(kvCacheBytesForContext(p, "f16", loadedNCtx)), loadedNCtx,
				budgetGB, modelSizeGB, reserveGB)
		case fits("q8_0"):
			return "q8_0", fmt.Sprintf(
				"f16 KV (%.1fGB) не влезает в доступные %.1fGB, q8_0 KV (%.1fGB) влезает → q8_0 (компромисс качество/контекст)",
				bytesToGB(kvCacheBytesForContext(p, "f16", loadedNCtx)), availKVGB,
				bytesToGB(kvCacheBytesForContext(p, "q8_0", loadedNCtx)))
		case fits("q4_0"):
			return "q4_0", fmt.Sprintf(
				"f16 (%.1fGB) и q8_0 (%.1fGB) KV не влезают в доступные %.1fGB, q4_0 (%.1fGB) влезает → q4_0",
				bytesToGB(kvCacheBytesForContext(p, "f16", loadedNCtx)),
				bytesToGB(kvCacheBytesForContext(p, "q8_0", loadedNCtx)),
				availKVGB,
				bytesToGB(kvCacheBytesForContext(p, "q4_0", loadedNCtx)))
		default:
			// R230: ни один тип KV не влезает — перезагрузка не даст выигрыша,
			// поэтому не рекомендуем смену (иначе reload ради деградации).
			return p.CurrentKVCacheType, fmt.Sprintf(
				"ни один тип KV не влезает в доступные %.1fGB (f16=%.1fGB, q8_0=%.1fGB, q4_0=%.1fGB) — оставляем текущий",
				availKVGB,
				bytesToGB(kvCacheBytesForContext(p, "f16", loadedNCtx)),
				bytesToGB(kvCacheBytesForContext(p, "q8_0", loadedNCtx)),
				bytesToGB(kvCacheBytesForContext(p, "q4_0", loadedNCtx)))
		}
	}

	// ctx неизвестен (модель без загруженного контекста): эвристика R54.1 по
	// размеру модели. Единицы и бюджет — корректные (R230).
	if modelSizeGB <= 3.5 {
		return "f16", fmt.Sprintf("n_ctx неизвестен, model=%.1fGB ≤ 3.5GB и бюджет %.1fGB позволяет f16 KV", modelSizeGB, budgetGB)
	}
	if modelSizeGB <= 8.0 {
		return "q8_0", fmt.Sprintf("n_ctx неизвестен, model=%.1fGB ≤ 8GB → q8_0 KV (бюджет %.1fGB)", modelSizeGB, budgetGB)
	}
	return "q4_0", fmt.Sprintf("n_ctx неизвестен, model=%.1fGB > 8GB → q4_0 KV для максимального контекста (бюджет %.1fGB)", modelSizeGB, budgetGB)
}

// computeOptimalNumGPULayers — Round 54.1 + R230: optimal num_gpu_layers.
//
// R230: рекомендация обязана быть физически исполнимой. Бюджет — reload-бюджет
// (free + usage модели, см. autotune_budget.go), из него вычитаются
// резерв на activations и KV cache для загруженного n_ctx. Раньше freeVRAM
// приезжал в МБ как байты (≈0 GB) и на любой модели выдавалось -2.
//
// Семантика значений:
//   - -1 = все слои на GPU
//   - -2 = auto offload (cppworker решает сколько влезает)
//   - N (positive) = явно N слоёв на GPU
//
// Политика R230: рекомендовать УМЕНЬШЕНИЕ слоёв нельзя (текущая конфигурация
// могла быть подобрана cppworker'ом с KV в RAM; «оптимизация» вниз = лишний
// reload и деградация). Рекомендуем только -1, когда всё влезает, или больше
// слоёв, когда расчёт показывает запас.
func computeOptimalNumGPULayers(p *ModelProfileInfo) (recommendation int, reasoning string) {
	return computeOptimalNumGPULayersWithKV(p, p.CurrentKVCacheType)
}

// computeOptimalNumGPULayersWithKV — R230: та же логика, но явно для KV-типа,
// который будет применён планом (например, рекомендованный q4_0).
func computeOptimalNumGPULayersWithKV(p *ModelProfileInfo, kvType string) (recommendation int, reasoning string) {
	if p.SizeBytes == 0 || p.FreeVRAMBytes == 0 || p.NLayers == 0 {
		return p.CurrentNumGPULayers, "недостаточно данных"
	}

	budgetGB := bytesToGB(p.FreeVRAMBytes)
	modelGB := bytesToGB(p.SizeBytes)
	reserve := autotuneReserveBytes(p.TotalVRAMBytes)
	reserveGB := bytesToGB(reserve)
	kvBytes := kvCacheBytesForContext(p, kvType, p.CurrentContextLength)
	kvGB := bytesToGB(kvBytes)

	availForWeights := int64(0)
	if int64(p.FreeVRAMBytes) > int64(reserve)+int64(kvBytes) {
		availForWeights = int64(p.FreeVRAMBytes) - int64(reserve) - int64(kvBytes)
	}
	bytesPerLayer := float64(p.SizeBytes) / float64(p.NLayers)

	current := p.CurrentNumGPULayers
	currentMeansAll := current == -1 || current == p.NLayers

	// R230: без n_kv_heads/head_dim_k KV не посчитать, а значит нельзя
	// доказать, что слои влезут. Не трогаем раскладку: «оптимизация» вслепую —
	// ровно то, на что была жалоба (16 → -2 → хуже начальной загрузки).
	if p.CurrentContextLength > 0 && normalizeKVType(kvType) != "" && kvBytes == 0 {
		return current, "нет параметров архитектуры (n_kv_heads/head_dim_k) для расчёта KV — num_gpu_layers не меняем"
	}

	// 1) Все слои вместе с KV влезают в бюджет → -1 (all).
	if availForWeights >= int64(p.SizeBytes) {
		if currentMeansAll {
			return current, "current = all layers, optimal = all layers"
		}
		return -1, fmt.Sprintf(
			"model=%.1fGB + KV_%s(%.1fGB) + резерв(%.1fGB) влезают в бюджет %.1fGB — рекомендуем -1 (all layers)",
			modelGB, normalizeKVType(kvType), kvGB, reserveGB, budgetGB)
	}

	// 2) Не всё влезает: сколько слоёв помещается по весам (KV может быть в RAM).
	layersThatFit := int(float64(availForWeights) * autotuneLayerSafetyFactor / bytesPerLayer)
	if layersThatFit >= p.NLayers {
		layersThatFit = p.NLayers - 1
	}
	if layersThatFit < 1 {
		return current, fmt.Sprintf(
			"model=%.1fGB не влезает в бюджет %.1fGB даже без KV; оставляем num_gpu_layers=%d (cppworker балансирует сам)",
			modelGB, budgetGB, current)
	}

	// 3) Auto offload уже выбран — cppworker сам решает, не трогаем (-2 не
	// попадает в reload-план с R229, поэтому и не рекомендуем менять).
	if current == -2 {
		return current, fmt.Sprintf(
			"current = auto offload (-2): по бюджету %.1fGB влезает ≈%d из %d слоёв, cppworker выберет сам",
			budgetGB, layersThatFit, p.NLayers)
	}

	// 4) current=-1 (все слои), но по бюджету не влезает → конкретное число.
	if currentMeansAll {
		return layersThatFit, fmt.Sprintf(
			"model=%.1fGB + KV_%s(%.1fGB) не влезают в бюджет %.1fGB: влезает ≈%d из %d слоёв — рекомендуем %d вместо -1",
			modelGB, normalizeKVType(kvType), kvGB, budgetGB, layersThatFit, p.NLayers, layersThatFit)
	}

	// 5) Есть запас по слоям → поднимаем. Меньшать не советуем никогда.
	if layersThatFit > current {
		return layersThatFit, fmt.Sprintf(
			"model=%.1fGB, бюджет %.1fGB, KV_%s(%.1fGB): влезает ≈%d из %d слоёв (сейчас %d) — есть смысл поднять num_gpu_layers до %d",
			modelGB, budgetGB, normalizeKVType(kvType), kvGB, layersThatFit, p.NLayers, current, layersThatFit)
	}

	return current, fmt.Sprintf(
		"num_gpu_layers=%d уже ≥ расчётного %d для бюджета %.1fGB — не трогаем (KV может быть частично в RAM)",
		current, layersThatFit, budgetGB)
}
