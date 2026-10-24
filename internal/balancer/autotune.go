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
	"fmt"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

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
// (free + usage модели, см. autotune_budget_r230.go), из него вычитаются
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

// AnalyzeLoadedModelPublic — R54.6 (2026-08-24): public wrapper для analyzeLoadedModel
// (lowercase) — нужен API handlers в internal/api/.
func AnalyzeLoadedModelPublic(p *ModelProfileInfo) *AutoTuneAnalysis {
	return analyzeLoadedModel(p)
}

// analyzeLoadedModel — Round 54.1: главная функция. Анализирует одну модель
// и возвращает AutoTuneAnalysis с рекомендациями.
//
// Возвращает nil если данных недостаточно.
//
// R54.9: backward-compat wrapper. Без workload → R54.1 logic.
func analyzeLoadedModel(p *ModelProfileInfo) *AutoTuneAnalysis {
	return analyzeLoadedModelWithWorkload(p, WorkloadStats{}, DefaultWorkloadTrackerConfig().MinSamplesForRecommendation)
}

// analyzeLoadedModelWithWorkload — R54.9: workload-aware version.
// Использует WorkloadStats для workload-based KV cache selection.
// minSamples защищает от cold start: без samples fallback к R54.1 logic.
func analyzeLoadedModelWithWorkload(p *ModelProfileInfo, workload WorkloadStats, minSamples int) *AutoTuneAnalysis {
	if p == nil {
		return nil
	}

	analysis := &AutoTuneAnalysis{
		ModelName:       p.Name,
		IsSubOptimal:    false,
		AutoTuneEnabled: p.AutoTuneEnabled,
	}

	// 1) Analyze KV cache type (R54.9: workload-aware)
	// R230: тип KV, который будет применён планом, используется и для расчёта
	// num_gpu_layers — иначе рекомендации противоречат друг другу
	// («q4_0 + -2»/«q8_0 + слои под q4_0»).
	kvForLayers := normalizeKVType(p.CurrentKVCacheType)
	if p.CurrentKVCacheType != "" {
		optimalKV, reasoningKV := computeOptimalKVCacheTypeWithWorkload(p, workload, minSamples)
		if optimalKV != "" && normalizeKVType(optimalKV) != normalizeKVType(p.CurrentKVCacheType) {
			kvForLayers = normalizeKVType(optimalKV)
			// Severity: f16 < q4_0 (closer to f16 = better quality)
			severity := "info"
			if normalizeKVType(p.CurrentKVCacheType) == "q4_0" && normalizeKVType(optimalKV) == "f16" {
				severity = "warning" // significant quality loss
			}
			analysis.Recommendations = append(analysis.Recommendations, AutoTuneRecommendation{
				Severity:           severity,
				Category:           "kv_cache",
				Message:            fmt.Sprintf("KV cache '%s' — sub-optimal, рекомендуется '%s'", p.CurrentKVCacheType, optimalKV),
				Suggestion:         fmt.Sprintf("Перезагрузить с kvCacheType='%s': %s", optimalKV, reasoningKV),
				CurrentKVCache:     p.CurrentKVCacheType,
				RecommendedKVCache: optimalKV,
			})
			analysis.OptimalKVCacheType = optimalKV
		}
	}

	// 2) Analyze num_gpu_layers
	if p.CurrentNumGPULayers != 0 {
		optimalLayers, reasoningLayers := computeOptimalNumGPULayersWithKV(p, kvForLayers)
		if optimalLayers != 0 && optimalLayers != p.CurrentNumGPULayers {
			analysis.Recommendations = append(analysis.Recommendations, AutoTuneRecommendation{
				Severity:                "info",
				Category:                "layers",
				Message:                 fmt.Sprintf("num_gpu_layers=%d — sub-optimal, рекомендуется %d", p.CurrentNumGPULayers, optimalLayers),
				Suggestion:              reasoningLayers,
				CurrentNumGPULayers:     p.CurrentNumGPULayers,
				RecommendedNumGPULayers: optimalLayers,
			})
			analysis.OptimalNumGPULayers = optimalLayers
		}
	}

	// 3) Analyze context length (over-allocation)
	if p.CurrentContextLength > 0 && p.FeasibleMaxContext > 0 {
		// If loaded n_ctx > 1.5x feasibleMaxContext → over-allocated
		if p.CurrentContextLength > int(1.5*float64(p.FeasibleMaxContext)) {
			analysis.Recommendations = append(analysis.Recommendations, AutoTuneRecommendation{
				Severity:          "info",
				Category:          "context",
				Message:           fmt.Sprintf("n_ctx=%d сильно больше feasible_max=%d (over-allocation)", p.CurrentContextLength, p.FeasibleMaxContext),
				Suggestion:        fmt.Sprintf("Перезагрузить с n_ctx=%d для эффективного использования VRAM", p.FeasibleMaxContext),
				CurrentNumCtx:     p.CurrentContextLength,
				RecommendedNumCtx: p.FeasibleMaxContext,
			})
			analysis.OptimalContextLength = p.FeasibleMaxContext
		}
	}

	// Compute overall state
	analysis.IsSubOptimal = len(analysis.Recommendations) > 0
	if analysis.IsSubOptimal {
		analysis.AnalysisReasoning = fmt.Sprintf("Найдено %d проблем(ы) для %s", len(analysis.Recommendations), p.Name)
	}

	return analysis
}

// AnalyzeBackend — Round 54.1+R54.2: анализирует все loaded models бэкенда.
// Возвращает AutoTuneReport с recommendations и AutoTuneEnabled flag.
//
// R54.2: AutoTuneEnabled рассчитывается per-model (через IsAutoTuneEnabled),
// учитывает per-model profile override. Если proxy == nil, AutoTuneEnabled=false
// (defensive default).
func AnalyzeBackend(proxy *Proxy, backendID, backendType string, models []types.LlamaCppModel, freeVRAMBytes, freeRAMBytes, totalVRAMBytes uint64) *AutoTuneReport {
	report := &AutoTuneReport{
		BackendID:       backendID,
		BackendType:     backendType,
		Models:          []AutoTuneAnalysis{},
		OverallSeverity: "ok",
		AutoTuneEnabled: proxy != nil && proxy.config.Balancing.AutoTune,
	}

	if len(models) == 0 {
		report.OverallSummary = "no models loaded"
		return report
	}

	for _, m := range models {
		if m.State != "loaded" {
			continue
		}
		// R230: free VRAM/RAM измерены при загруженной модели, а reload её
		// выгружает — бюджет пополняется usage этой модели (иначе анализ видит
		// остаток и советует q4_0/-2). m.VRAMUsage/RAMUsage — в МБ.
		modelFreeVRAM := reloadBudgetBytes(freeVRAMBytes, totalVRAMBytes, m.VRAMUsage)
		modelFreeRAM := reloadBudgetBytes(freeRAMBytes, 0, m.RAMUsage)
		prof := &ModelProfileInfo{
			Name:                 m.Name,
			Architecture:         m.Architecture,
			NLayers:              m.NLayers,
			NEmbd:                m.NEmbd,
			NKvHeads:             m.NKvHeads,
			HeadDimK:             m.HeadDimK,
			SizeBytes:            m.Size,
			GgufMaxContext:       m.MaxContext,
			CurrentContextLength: m.ContextLength,
			CurrentKVCacheType:   m.KvCacheType,
			CurrentNumGPULayers:  m.NumGPULayers,
			CurrentFlashAttn:     0, // not in metrics yet
			CurrentUseMmap:       m.UseMmap,
			FreeVRAMBytes:        modelFreeVRAM,
			FreeRAMBytes:         modelFreeRAM,
			TotalVRAMBytes:       totalVRAMBytes,
			FeasibleMaxContext:   m.FeasibleMaxContext,
			// R54.2: per-model AutoTune switch (after inheritance).
			AutoTuneEnabled: IsAutoTuneEnabled(proxy, m.Name),
		}
		// R54.9: workload-aware analysis (if tracker available)
		var analysis *AutoTuneAnalysis
		if proxy != nil {
			tracker := proxy.WorkloadTracker()
			if tracker != nil {
				workload := tracker.Stats(backendID, m.Name)
				cfg := DefaultWorkloadTrackerConfig()
				analysis = analyzeLoadedModelWithWorkload(prof, workload, cfg.MinSamplesForRecommendation)
			}
		}
		if analysis == nil {
			analysis = analyzeLoadedModel(prof) // fallback
		}
		if analysis == nil {
			continue
		}
		report.Models = append(report.Models, *analysis)

		// Track worst severity
		for _, rec := range analysis.Recommendations {
			if severityRank(rec.Severity) > severityRank(report.OverallSeverity) {
				report.OverallSeverity = rec.Severity
			}
		}
	}

	if report.OverallSeverity != "ok" {
		report.OverallSummary = fmt.Sprintf("%d models, %d sub-optimal", len(report.Models), countSubOptimal(report.Models))
	} else {
		report.OverallSummary = fmt.Sprintf("%d models, all optimal", len(report.Models))
	}

	return report
}

// severityRank — упорядочивает severity для "worst" вычисления.
func severityRank(s string) int {
	switch s {
	case "critical":
		return 4
	case "warning":
		return 3
	case "info":
		return 2
	case "ok":
		return 1
	default:
		return 0
	}
}

func countSubOptimal(models []AutoTuneAnalysis) int {
	n := 0
	for _, m := range models {
		if m.IsSubOptimal {
			n++
		}
	}
	return n
}

// FormatRecommendationShort — краткая однострочная рекомендация для логов.
func FormatRecommendationShort(r AutoTuneRecommendation) string {
	return fmt.Sprintf("[%s/%s] %s", r.Severity, r.Category, r.Message)
}

// IsAutoTuneEnabled — Round 54.2 (2026-08-24): resolves AutoTune master switch
// for a specific model. Returns false if global AutoTune is off OR per-model
// profile has AutoTune=false (explicit manual override).
//
// Resolution order (highest priority first):
//  1. Per-model profile.AutoTune (if non-nil) — explicit override
//  2. BalancingSettings.AutoTune (default true)
//
// Использование:
//
//	p.config.Balancing.AutoTune — global switch (default true)
//	profile.AutoTune = &false — manual override для этой модели
func IsAutoTuneEnabled(p *Proxy, modelName string) bool {
	if p == nil {
		return false
	}
	// 0) R83 (2026-09-29): primary-модель — настройки зафиксированы администратором,
	// авто-оптимизация её не трогает. Иначе AutoTune перезагрузил бы модель по своим
	// рекомендациям и «сдвинул» ровно те параметры, которые оператор зафиксировал.
	if modelName != "" && p.isPrimaryModel(modelName) {
		return false
	}
	// 1) Per-model profile override (highest priority)
	if modelName != "" {
		profile, ok := p.GetModelProfile(modelName)
		if ok && profile.AutoTune != nil {
			return *profile.AutoTune
		}
	}
	// 2) Global default (defensive nil check)
	if p.config == nil {
		return true // default behavior
	}
	return p.config.Balancing.AutoTune
}

// AutoTuneReloadPlan — Round 54.4 (2026-08-24): конкретный план reload'a для
// применения AutoTune рекомендаций. Это то, что передаётся в executeLlamaCppLoad.
//
// Если рекомендация не требует изменения (current = optimal), поля nil/0 →
// reload не выполняется.
//
// КРИТИЧНО: пустой план (все поля = zero value) означает "ничего не делать".
// PlanApplyAutoTune ниже возвращает (nil, false) если план пуст.
type AutoTuneReloadPlan struct {
	ModelName    string
	ContextSize  int    // 0 = no change
	KVCacheType  string // "" = no change
	NumGPULayers int    // 0 = no change (negative values = -1 all, -2 auto)
	FlashAttn    int    // 0 = no change
	UseMmap      *bool  // nil = no change
	Reason       string // for logging (e.g., "R54.4: n_ctx over-allocation")

	// R230: маркер «reload идёт», уже взятый вызывающим. Async trigger берёт его
	// СИНХРОННО (до запуска горутины и до форварда текущего запроса), чтобы
	// параллельные запросы не успели перехватить reload ленивой загрузкой по
	// профилю. Не сериализуется (unexported).
	externalFinish func()
}

// NeedsReload — true если хотя бы одно поле отличается от current (т.е. есть что менять).
func (p *AutoTuneReloadPlan) NeedsReload() bool {
	return p.ContextSize > 0 || p.KVCacheType != "" || p.NumGPULayers != 0 ||
		p.FlashAttn != 0 || p.UseMmap != nil
}

// AutoTuneCircuitBreaker — Round 54.4 (2026-08-24): защита от reload storm.
//
// Проблема: если AutoTune детектит sub-optimal state, а reload не удаётся
// (cppworker busy / timeout / wrong params), без breaker'а балансер будет
// пытаться reload каждым запросом — нагрузка на cppworker, логи захлёбываются.
//
// Решение: track last_attempt + last_success per (backend, model).
// Пропускаем reload если:
//   - Last attempt < 30s ago AND last attempt failed (cool-down)
//   - Last success < 60s ago (don't re-fix what was just fixed)
//
// Хранится в-memory (per-Proxy). Не персистится — после restart balancer'a
// AutoTune попробует заново, что и хочется.
type AutoTuneCircuit struct {
	LastAttempt time.Time
	LastSuccess time.Time
	LastError   string
}

// AutoTuneCircuitConfig — Round 54.4 (2026-08-24): tunable breakers.
type AutoTuneCircuitConfig struct {
	// CooldownAfterErrorSec: после неуспешного reload не пытаемся N секунд.
	CooldownAfterErrorSec int
	// CooldownAfterSuccessSec: после успешного reload не пытаемся N секунд.
	CooldownAfterSuccessSec int
	// MaxRetries: сколько reloads в одном "burst" (circuit opens after).
	MaxRetries int
}

// DefaultAutoTuneCircuitConfig — sane defaults для 4B-class моделей на 8GB VRAM.
func DefaultAutoTuneCircuitConfig() AutoTuneCircuitConfig {
	return AutoTuneCircuitConfig{
		CooldownAfterErrorSec:   60,  // 1 min cool-down после fail
		CooldownAfterSuccessSec: 300, // 5 min "stable period" после success
		MaxRetries:              3,   // 3 fail → open circuit
	}
}

// CanReload — Round 54.4 (2026-08-24): возвращает true если circuit closed
// и можно делать reload. Возвращает (false, reason) если open.
func (c *AutoTuneCircuit) CanReload(cfg AutoTuneCircuitConfig) (bool, string) {
	now := time.Now()
	if !c.LastAttempt.IsZero() && c.LastError != "" {
		coolDownEnd := c.LastAttempt.Add(time.Duration(cfg.CooldownAfterErrorSec) * time.Second)
		if now.Before(coolDownEnd) {
			return false, fmt.Sprintf("circuit-cooling-down: %.0fs left after error: %s",
				coolDownEnd.Sub(now).Seconds(), c.LastError)
		}
	}
	if !c.LastSuccess.IsZero() {
		stableEnd := c.LastSuccess.Add(time.Duration(cfg.CooldownAfterSuccessSec) * time.Second)
		if now.Before(stableEnd) {
			return false, fmt.Sprintf("circuit-stable: %.0fs left after success",
				stableEnd.Sub(now).Seconds())
		}
	}
	return true, ""
}

// RecordAttempt — фиксирует попытку reload. Call this BEFORE the actual reload.
func (c *AutoTuneCircuit) RecordAttempt() {
	c.LastAttempt = time.Now()
}

// RecordSuccess — фиксирует успешный reload. Reset error.
func (c *AutoTuneCircuit) RecordSuccess() {
	c.LastSuccess = time.Now()
	c.LastError = ""
}

// RecordError — фиксирует failed reload.
func (c *AutoTuneCircuit) RecordError(err string) {
	c.LastError = err
}

// AutoTuneTracker accessor для API handlers (R54.6).
func (p *Proxy) AutoTuneTracker() *AutoTuneTracker {
	if p == nil {
		return nil
	}
	return p.autoTuneTracker
}

// WorkloadTracker accessor для API handlers (R54.9).
// Lazy init с default config — same pattern as AutoTuneTracker.
func (p *Proxy) WorkloadTracker() *WorkloadTracker {
	if p == nil {
		return nil
	}
	if p.workloadTracker == nil {
		p.workloadTracker = NewWorkloadTracker(DefaultWorkloadTrackerConfig())
	}
	return p.workloadTracker
}

// AutoTuneHistory accessor для API handlers (R55.2).
// Lazy init с default config (500 entries max).
// Используется для ring buffer последних AutoTune events.
func (p *Proxy) AutoTuneHistory() *AutoTuneHistory {
	if p == nil {
		return nil
	}
	if p.autoTuneHistory == nil {
		p.autoTuneHistory = NewAutoTuneHistory(500) // R55.2 default
	}
	return p.autoTuneHistory
}

// publishAutoTuneEvent — R55.2: helper для двойной записи event
// (EventBus для live WebSocket + AutoTuneHistory ring buffer для HTTP poll).
// Убирает boilerplate в 4 publish call-sites triggerAutoTuneReload.
func (p *Proxy) publishAutoTuneEvent(ev types.Event) {
	if p == nil {
		return
	}
	if p.EventBus() != nil {
		p.EventBus().Publish(ev)
	}
	if h := p.AutoTuneHistory(); h != nil {
		h.Record(ev)
	}
}

// AutoTuneTracker — Round 54.4 (2026-08-24): per-Proxy tracker для circuit breakers.
// Хранит AutoTuneCircuit для каждой пары (backendID, modelName).
//
// Использование:
//
//	p.autoTuneTracker.GetCircuit(backendID, modelName) → returns circuit (or new)
//	p.autoTuneTracker.GetConfig() → circuit config
//
// Thread-safe (sync.RWMutex).
type AutoTuneTracker struct {
	mu       sync.RWMutex
	circuits map[string]*AutoTuneCircuit // key: "backendID|modelName"
	config   AutoTuneCircuitConfig
}

// NewAutoTuneTracker — Round 54.4 (2026-08-24): constructor.
func NewAutoTuneTracker(cfg AutoTuneCircuitConfig) *AutoTuneTracker {
	return &AutoTuneTracker{
		circuits: make(map[string]*AutoTuneCircuit),
		config:   cfg,
	}
}

// GetConfig — returns current circuit config.
func (t *AutoTuneTracker) GetConfig() AutoTuneCircuitConfig {
	if t == nil {
		return DefaultAutoTuneCircuitConfig()
	}
	return t.config
}

// GetCircuit — returns existing circuit or creates a new one (zero-state = closed).
func (t *AutoTuneTracker) GetCircuit(backendID, modelName string) *AutoTuneCircuit {
	if t == nil {
		return nil
	}
	key := backendID + "|" + modelName
	t.mu.RLock()
	c, ok := t.circuits[key]
	t.mu.RUnlock()
	if ok {
		return c
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Double-check after lock upgrade
	if c, ok := t.circuits[key]; ok {
		return c
	}
	c = &AutoTuneCircuit{}
	t.circuits[key] = c
	return c
}

// ResetCircuit — очистить circuit для (backend, model). Используется при successful reload.
func (t *AutoTuneTracker) ResetCircuit(backendID, modelName string) {
	if t == nil {
		return
	}
	key := backendID + "|" + modelName
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.circuits[key]
	if ok {
		c.LastAttempt = time.Time{}
		c.LastSuccess = time.Time{}
		c.LastError = ""
	}
}

// GetCircuitSnapshot — Round 54.4 (2026-08-24): debug helper для /api/v1/admin/autotune.
// Возвращает копию circuit'а для API response.
func (t *AutoTuneTracker) GetCircuitSnapshot(backendID, modelName string) AutoTuneCircuitSnapshot {
	c := t.GetCircuit(backendID, modelName)
	if c == nil {
		return AutoTuneCircuitSnapshot{}
	}
	return AutoTuneCircuitSnapshot{
		LastAttempt: c.LastAttempt,
		LastSuccess: c.LastSuccess,
		LastError:   c.LastError,
		CanReload:   true, // computed by caller with config
	}
}

// AutoTuneCircuitSnapshot — API-friendly snapshot of circuit state.
type AutoTuneCircuitSnapshot struct {
	LastAttempt time.Time `json:"lastAttempt,omitempty"`
	LastSuccess time.Time `json:"lastSuccess,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	CanReload   bool      `json:"canReload"`
}

// AutoTuneReloadResult — Round 54.4 (2026-08-24): результат triggerAutoTuneReload.
// Используется в transport для логирования и метрик.
type AutoTuneReloadResult struct {
	Triggered     bool                // true если reload был запущен
	SkippedReason string              // если Triggered=false, почему
	Plan          *AutoTuneReloadPlan // nil если ничего не нужно менять
	CircuitState  AutoTuneCircuitSnapshot
}

// triggerAutoTuneReload — Round 54.4 (2026-08-24): main entry point для autonomous reload.
//
// Вызывается из proxyRequestLlamaCpp ПОСЛЕ preflightNCtxReloadIfNeeded.
// Логика:
//  1. Check global + per-model AutoTune switch (IsAutoTuneEnabled).
//  2. Get loaded model from metrics.
//  3. Build ModelProfileInfo + run analyzeLoadedModel.
//  4. If sub-optimal → PlanApplyAutoTune → check circuit → async reload.
//
// Возвращает AutoTuneReloadResult. Не блокирует (reload асинхронный).
func (p *Proxy) triggerAutoTuneReload(backendID, modelName string, freeVRAM, freeRAM, totalVRAM uint64) *AutoTuneReloadResult {
	res := &AutoTuneReloadResult{}

	// 1) AutoTune switch
	if !IsAutoTuneEnabled(p, modelName) {
		res.SkippedReason = "AutoTune disabled (global or per-model)"
		return res
	}

	// 2) Get loaded model from metrics
	if p.metricsMgr == nil {
		res.SkippedReason = "no metrics manager"
		return res
	}
	lm := p.metricsMgr.GetLlamaCppMetrics(backendID)
	if lm == nil {
		res.SkippedReason = "no llamaCpp metrics for backend"
		return res
	}
	var loadedModel types.LlamaCppModel
	for _, m := range lm.LoadedModels {
		// R66d: нормализация .gguf/пути/регистра (cppworker отдаёт имя без расширения).
		if modelNameMatches(m.Name, modelName) && m.State == "loaded" {
			loadedModel = m
			break
		}
	}
	if loadedModel.Name == "" {
		res.SkippedReason = "model not in loaded state"
		return res
	}

	// 3) Build profile + analysis
	// R230: reload выгружает текущую копию модели → бюджет = free + usage этой
	// модели (VRAMUsage/RAMUsage в МБ), иначе анализ видит только остаток.
	prof := &ModelProfileInfo{
		Name:                 loadedModel.Name,
		Architecture:         loadedModel.Architecture,
		NLayers:              loadedModel.NLayers,
		NEmbd:                loadedModel.NEmbd,
		NKvHeads:             loadedModel.NKvHeads,
		HeadDimK:             loadedModel.HeadDimK,
		SizeBytes:            loadedModel.Size,
		GgufMaxContext:       loadedModel.MaxContext,
		CurrentContextLength: loadedModel.ContextLength,
		CurrentKVCacheType:   loadedModel.KvCacheType,
		CurrentNumGPULayers:  loadedModel.NumGPULayers,
		FreeVRAMBytes:        reloadBudgetBytes(freeVRAM, totalVRAM, loadedModel.VRAMUsage),
		FreeRAMBytes:         reloadBudgetBytes(freeRAM, 0, loadedModel.RAMUsage),
		TotalVRAMBytes:       totalVRAM,
		FeasibleMaxContext:   loadedModel.FeasibleMaxContext,
		AutoTuneEnabled:      true,
	}
	// R54.9: workload-aware analysis — если есть WorkloadTracker, используем
	// реальную статистику num_ctx чтобы выбрать KV cache type оптимально.
	var analysis *AutoTuneAnalysis
	if p.workloadTracker != nil {
		workload := p.workloadTracker.Stats(backendID, modelName)
		cfg := DefaultWorkloadTrackerConfig()
		analysis = analyzeLoadedModelWithWorkload(prof, workload, cfg.MinSamplesForRecommendation)
	}
	if analysis == nil {
		analysis = analyzeLoadedModel(prof) // fallback
	}
	if analysis == nil || !analysis.IsSubOptimal {
		res.SkippedReason = "model is optimal"
		return res
	}

	// 4) Plan
	// R69: не «оптимизируем» n_ctx ниже того, что просил клиент — иначе
	// ping-pong reload (65536 → 38385 → 65536 …) и клиент получает 503.
	minContext := 0
	if p.nctxReload != nil {
		minContext = p.nctxReload.DesiredNCtx(backendID, modelName)
	}
	plan := PlanApplyAutoTuneWithMinContext(analysis, loadedModel, minContext)
	if plan == nil {
		if minContext > 0 {
			res.SkippedReason = fmt.Sprintf(
				"client requested n_ctx=%d (within %s): over-allocation downgrade skipped",
				minContext, desiredNCtxTTL)
		} else {
			res.SkippedReason = "no actionable recommendations (sub-optimal detected but no plan)"
		}
		return res
	}
	res.Plan = plan

	// 5) Circuit breaker
	if p.autoTuneTracker == nil {
		p.autoTuneTracker = NewAutoTuneTracker(DefaultAutoTuneCircuitConfig())
	}
	circuit := p.autoTuneTracker.GetCircuit(backendID, modelName)
	res.CircuitState = p.autoTuneTracker.GetCircuitSnapshot(backendID, modelName)
	canReload, reason := circuit.CanReload(p.autoTuneTracker.GetConfig())
	if !canReload {
		res.SkippedReason = reason
		// R54.8 (2026-08-24): publish circuit_cool_down event — оператор видит
		// в WebUI что AutoTune временно приостановлен после серии неудач.
		// Это НЕ новый circuit-open логика (cool-down уже работает), а visibility
		// в UI, чтобы не приходилось лезть в логи чтобы понять почему
		// AutoTune не срабатывает.
		// R55.2: используем publishAutoTuneEvent helper — пишет в EventBus + history log.
		p.publishAutoTuneEvent(types.Event{
			Type:      types.EventAutoTuneCircuitOpen,
			Timestamp: time.Now(),
			BackendID: backendID,
			Model:     modelName,
			Severity:  types.SeverityWarning,
			Source:    "autotune",
			Message:   "AutoTune circuit cooling down: " + reason,
			Data: map[string]interface{}{
				"reason":      reason,
				"lastError":   circuit.LastError,
				"lastAttempt": circuit.LastAttempt,
			},
		})
		return res
	}

	// 6) Trigger async reload via existing nctx_reload infrastructure
	circuit.RecordAttempt()

	// R230: берём маркер «reload идёт» СИНХРОННО, до запуска горутины и до
	// форварда текущего запроса. Иначе запрос успевал уйти в воркер, тот видел
	// выгруженную модель и делал ленивую загрузку по профилю (q4_0/40), а наш
	// load с планом (f16/-1) потом только дожидался чужой загрузки и рапортовал
	// success при неизменной конфигурации. Неудача (reload уже идёт) — план не
	// запускаем вовсе.
	if p.nctxReload != nil {
		finish, started := p.nctxReload.BeginExternalReload(backendID, modelName)
		if !started {
			res.SkippedReason = "reload already in progress for model"
			return res
		}
		plan.externalFinish = finish
	}

	logger.Get().Infow("autotune: triggering async reload",
		"backend", backendID, "model", modelName,
		"reason", plan.Reason,
		"new_context_size", plan.ContextSize,
		"new_kv_cache_type", plan.KVCacheType,
		"new_num_gpu_layers", plan.NumGPULayers,
	)

	// Запускаем async reload в goroutine
	go func() {
		// R54.8 (2026-08-24): publish autotune_reload_triggered event
		// R55.2: publishAutoTuneEvent пишет в EventBus + history log.
		p.publishAutoTuneEvent(types.Event{
			Type:      types.EventAutoTuneReloadTriggered,
			Timestamp: time.Now(),
			BackendID: backendID,
			Model:     modelName,
			Severity:  types.SeverityInfo,
			Source:    "autotune",
			Message:   "AutoTune async reload triggered: " + plan.Reason,
			Data: map[string]interface{}{
				"contextSize":  plan.ContextSize,
				"kvCacheType":  plan.KVCacheType,
				"numGpuLayers": plan.NumGPULayers,
				"reason":       plan.Reason,
			},
		})

		// R83: проверка «reload уже идёт» перенесена ВЫШЕ, в синхронную часть:
		// маркер (BeginExternalReload) берётся до запуска горутины, иначе проверка
		// видела бы собственный маркер и план не запускался бы никогда (R230).

		err := p.executeAutoTuneReload(backendID, modelName, plan, circuit)
		if err != nil {
			circuit.RecordError(err.Error())
			logger.Get().Warnw("autotune: async reload failed",
				"backend", backendID, "model", modelName, "error", err)
			// R54.8: publish failure event (R55.2: also record to history)
			p.publishAutoTuneEvent(types.Event{
				Type:      types.EventAutoTuneReloadFailed,
				Timestamp: time.Now(),
				BackendID: backendID,
				Model:     modelName,
				Severity:  types.SeverityWarning,
				Source:    "autotune",
				Message:   "AutoTune reload failed: " + err.Error(),
				Data: map[string]interface{}{
					"error":         err.Error(),
					"circuitErrors": circuit.LastError,
				},
			})
		} else {
			circuit.RecordSuccess()
			p.autoTuneTracker.ResetCircuit(backendID, modelName)
			logger.Get().Infow("autotune: async reload succeeded",
				"backend", backendID, "model", modelName, "reason", plan.Reason)
			// R54.8: publish success event (R55.2: also record to history)
			p.publishAutoTuneEvent(types.Event{
				Type:      types.EventAutoTuneReloadSucceeded,
				Timestamp: time.Now(),
				BackendID: backendID,
				Model:     modelName,
				Severity:  types.SeverityInfo,
				Source:    "autotune",
				Message:   "AutoTune reload succeeded: " + plan.Reason,
				Data: map[string]interface{}{
					"contextSize":  plan.ContextSize,
					"kvCacheType":  plan.KVCacheType,
					"numGpuLayers": plan.NumGPULayers,
					"reason":       plan.Reason,
				},
			})
		}
	}()

	res.Triggered = true
	res.CircuitState = p.autoTuneTracker.GetCircuitSnapshot(backendID, modelName)
	return res
}

// executeAutoTuneReload — Round 54.4 (2026-08-24): actual reload execution.
// Использует существующую инфраструктуру cppworker (executeAsyncReload).
func (p *Proxy) executeAutoTuneReload(backendID, modelName string, plan *AutoTuneReloadPlan, circuit *AutoTuneCircuit) error {
	if plan == nil {
		return nil
	}
	// Получаем текущий backend
	backends := p.GetAllBackends()
	var backend types.Backend
	var found bool
	for _, b := range backends {
		if b.ID == backendID {
			backend = b
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("backend %s not found", backendID)
	}
	if backend.CppWorkerPort == 0 {
		return fmt.Errorf("backend %s has no CppWorkerPort", backendID)
	}

	// R66c (2026-09-22): применяем план РЕАЛЬНО реализованным путём —
	// ApplyAutoTunePlan (unload + load с новыми параметрами), тем же, что
	// использует ручной endpoint /api/v1/admin/autotune/{id}/apply.
	//
	// Было: собирался env-map и вызывался applyCppWorkerEnvReload, который
	// ВСЕГДА возвращал ошибку «env-based reload not yet implemented; apply plan
	// manually». Последствия: авто-перезагрузка AutoTune не работала никогда,
	// а ошибка записывалась в circuit breaker (RecordError) и в историю событий
	// — оператор в UI видел «AutoTune reload failed» и растущий cool-down вместо
	// применения плана, хотя ручной apply в это же время работал.
	//
	// Параметры плана не теряются: ApplyAutoTunePlan прокидывает ContextSize,
	// NumGPULayers, KVCacheType, UseMmap и FlashAttn (см. ниже) — ровно те же
	// поля, что раньше складывались в env.
	// R230: маркер, взятый синхронно в triggerAutoTuneReload, обязан быть снят
	// на ЛЮБОМ пути (в т.ч. ошибка «backend not found» до ApplyAutoTunePlan),
	// иначе модель осталась бы «reload идёт» до истечения TTL. Повторный вызов
	// идемпотентен (sync.Once внутри).
	if plan.externalFinish != nil {
		defer plan.externalFinish()
	}
	applyRes := p.ApplyAutoTunePlan(backendID, plan)
	if applyRes == nil {
		return fmt.Errorf("autotune: apply plan returned nil result")
	}
	if !applyRes.Success {
		return fmt.Errorf("autotune: apply plan failed: %s", applyRes.Error)
	}
	logger.Get().Infow("autotune: plan applied (unload+load)",
		"backend", backendID, "model", modelName, "reason", plan.Reason,
		"context_size", plan.ContextSize, "kv_cache_type", plan.KVCacheType,
		"gpu_layers", plan.NumGPULayers,
		"duration_ms", applyRes.FinishedAt.Sub(applyRes.StartedAt).Milliseconds())
	return nil
}

// applyCppWorkerEnvReload удалён в R66c (2026-09-22).
//
// Он был заглушкой: всегда логировал «env-based reload not fully implemented»
// и возвращал ошибку «env-based reload not yet implemented; apply plan
// manually». Вызывался ТОЛЬКО из executeAutoTuneReload, поэтому авто-применение
// плана AutoTune гарантированно не работало и вдобавок засоряло circuit
// breaker и историю событий ошибками. Замена — ApplyAutoTunePlan (ниже):
// unload + load с новыми параметрами, тот же путь, что у ручного apply.

// ApplyAutoTunePlan — R54.6 (2026-08-24): применяет AutoTuneReloadPlan к бэкенду.
//
// Алгоритм:
//  1. Unload текущую модель (через cppworker /api/models/unload).
//  2. Load с обновленными параметрами (через ModelManager.executeLlamaCppLoad).
//  3. Возвращает результат (success/error + message).
//
// Используется из /api/v1/admin/autotune/{backendID}/apply endpoint.
//
// NB: circuit breaker не применяется (manual apply — operator override).
// Но circuit state обновляется (RecordSuccess/RecordError) для telemetry.
func (p *Proxy) ApplyAutoTunePlan(backendID string, plan *AutoTuneReloadPlan) *AutoTuneApplyResult {
	result := &AutoTuneApplyResult{
		BackendID: backendID,
		ModelName: plan.ModelName,
		StartedAt: time.Now(),
	}

	if p == nil {
		result.Error = "nil proxy"
		return result
	}
	if plan == nil {
		result.Error = "nil plan"
		return result
	}

	// 1) Find backend
	backends := p.GetAllBackends()
	var backend types.Backend
	var found bool
	for _, b := range backends {
		if b.ID == backendID {
			backend = b
			found = true
			break
		}
	}
	if !found {
		result.Error = fmt.Sprintf("backend %s not found", backendID)
		return result
	}
	if backend.CppWorkerPort == 0 {
		result.Error = fmt.Sprintf("backend %s has no CppWorkerPort", backendID)
		return result
	}

	// R230: пока идёт apply, помечаем reload как идущий. Иначе клиентский
	// запрос, пришедший между unload и load, успевает вызвать ленивую загрузку
	// по профилю — и она выигрывает гонку, оставляя конфигурацию неизменной
	// (см. reloadDedupRegistry.BeginExternalReload).
	//
	// Async trigger (executeAutoTuneReload) уже взял маркер СИНХРОННО — тогда
	// используем его, а не пытаемся взять второй (иначе получили бы «reload уже
	// идёт» на собственном маркере).
	var finishReload func()
	if plan.externalFinish != nil {
		finishReload = plan.externalFinish
	} else if p.nctxReload != nil {
		var started bool
		finishReload, started = p.nctxReload.BeginExternalReload(backendID, plan.ModelName)
		if !started {
			result.Error = "reload already in progress for model"
			result.FinishedAt = time.Now()
			return result
		}
	}
	if finishReload != nil {
		defer finishReload()
	}

	// 2) Unload current model
	unloadResult := p.executeLlamaCppUnload(backend.Host, backend.CppWorkerPort, plan.ModelName)
	if !unloadResult.Success {
		result.Error = fmt.Sprintf("unload failed: %s", unloadResult.Error)
		result.FinishedAt = time.Now()
		return result
	}
	logger.Get().Infow("autotune: unloaded model for AutoTune apply",
		"backend", backendID, "model", plan.ModelName)

	// 3) Build load request
	req := ModelOpRequest{
		Operation: "load",
		ModelName: plan.ModelName,
	}
	if plan.ContextSize > 0 {
		cs := plan.ContextSize
		req.ContextSize = &cs
	}
	if plan.NumGPULayers != 0 {
		gl := plan.NumGPULayers
		req.GPULayers = &gl
	}
	if plan.KVCacheType != "" {
		kt := plan.KVCacheType
		req.KVCacheType = &kt
	}
	if plan.UseMmap != nil {
		req.UseMmap = plan.UseMmap
	}
	if plan.FlashAttn != 0 {
		fa := plan.FlashAttn
		req.FlashAttn = &fa
	}

	// 4) Load with new params (wait for completion)
	mm := p.autotuneModelManager()
	if mm == nil {
		result.Error = "ModelManager not available"
		result.FinishedAt = time.Now()
		return result
	}
	loadResult := mm.executeLlamaCppLoad(backend.Host, backend.CppWorkerPort, backendID, req)
	if !loadResult.Success {
		result.Error = fmt.Sprintf("load failed: %s", loadResult.Error)
		result.FinishedAt = time.Now()
		return result
	}

	// R230: проверяем, что план ДЕЙСТВИТЕЛЬНО применился. Раньше apply отвечал
	// success, даже когда параллельная ленивая загрузка успевала раньше и грузила
	// модель с профильными params (q4_0/40 вместо f16/-1) — AutoTune «применял»
	// неизменную конфигурацию снова и снова, а оператор видел reload без эффекта.
	if actual, ok := p.freshLoadedModel(backendID, backend.Host, backend.CppWorkerPort, plan.ModelName); ok {
		if mismatch := autoTuneAppliedMismatch(plan, actual); mismatch != "" {
			result.Error = fmt.Sprintf("load reported success, но конфигурация не применилась: %s", mismatch)
			result.Message = "plan not applied"
			result.FinishedAt = time.Now()
			return result
		}
	}

	result.Success = true
	result.Message = fmt.Sprintf("reloaded with new params: %s", plan.Reason)
	result.FinishedAt = time.Now()
	return result
}

// freshLoadedModel — R230: принудительно опрашивает /api/models бэкенда и
// возвращает загруженную модель (нужно, чтобы проверить факт применения плана,
// а не ждать следующего тика поллера).
func (p *Proxy) freshLoadedModel(backendID, host string, port int, modelName string) (types.LlamaCppModel, bool) {
	if p == nil || p.metricsMgr == nil {
		return types.LlamaCppModel{}, false
	}
	if poller := p.llamaCppMetricsPoller; poller != nil {
		poller.pollBackend(backendInfo{id: backendID, host: host, port: port})
	}
	lm := p.metricsMgr.GetLlamaCppMetrics(backendID)
	if lm == nil {
		return types.LlamaCppModel{}, false
	}
	for _, m := range lm.LoadedModels {
		if modelNameMatches(m.Name, modelName) && m.State == "loaded" {
			return m, true
		}
	}
	return types.LlamaCppModel{}, false
}

// autoTuneAppliedMismatch — R230: расхождение между планом и фактически
// загруженной моделью. Пустая строка = план применён.
func autoTuneAppliedMismatch(plan *AutoTuneReloadPlan, actual types.LlamaCppModel) string {
	if plan == nil {
		return ""
	}
	var diffs []string
	// Сверяем только то, что воркер реально сообщил: старые сборки/моки могут
	// не отдавать параметры вообще (0/""), и это не повод объявлять провал.
	if plan.ContextSize > 0 && actual.ContextLength > 0 && actual.ContextLength != plan.ContextSize {
		diffs = append(diffs, fmt.Sprintf("n_ctx=%d, ожидалось %d", actual.ContextLength, plan.ContextSize))
	}
	if want := normalizeKVType(plan.KVCacheType); want != "" && actual.KvCacheType != "" {
		if got := normalizeKVType(actual.KvCacheType); got != want {
			diffs = append(diffs, fmt.Sprintf("kv_cache=%s, ожидалось %s", actual.KvCacheType, plan.KVCacheType))
		}
	}
	if plan.NumGPULayers > 0 && actual.NumGPULayers > 0 {
		// -1 (all) в плане не сравниваем: cppworker сообщает фактическое число.
		allLayers := actual.NumGPULayers == actual.NLayers && actual.NLayers > 0
		wantAll := plan.NumGPULayers >= actual.NLayers && actual.NLayers > 0
		if !(allLayers && wantAll) && actual.NumGPULayers != plan.NumGPULayers {
			diffs = append(diffs, fmt.Sprintf("num_gpu_layers=%d, ожидалось %d", actual.NumGPULayers, plan.NumGPULayers))
		}
	}
	return strings.Join(diffs, "; ")
}

// AutoTuneApplyResult — R54.6 (2026-08-24): результат apply AutoTune plan.
type AutoTuneApplyResult struct {
	BackendID  string    `json:"backendId"`
	ModelName  string    `json:"modelName"`
	Success    bool      `json:"success"`
	Error      string    `json:"error,omitempty"`
	Message    string    `json:"message,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

// autotuneModelManager — R54.6: accessor для ModelManager (используем existing p.modelManager).
// Метод переименован чтобы не конфликтовать с полем modelManager.
func (p *Proxy) autotuneModelManager() *ModelManager {
	if p == nil {
		return nil
	}
	return p.modelManager // existing field set in NewProxy
}

// executeLlamaCppUnload — R54.6: helper для unload через cppworker API.
// Использует существующий POST /api/models/unload.
func (p *Proxy) executeLlamaCppUnload(host string, port int, modelName string) *ModelOpResult {
	// Try ModelManager first
	if mm := p.autotuneModelManager(); mm != nil {
		req := ModelOpRequest{Operation: "unload", ModelName: modelName}
		return mm.executeLlamaCppUnload(host, port, modelName, req)
	}
	return &ModelOpResult{
		Success:   false,
		Operation: "unload",
		ModelName: modelName,
		Error:     "ModelManager not available; cannot unload",
	}
}

// PlanApplyAutoTune — Round 54.4 (2026-08-24): строит reload-план из AutoTuneAnalysis.
// Возвращает (nil, false) если ничего менять не нужно.
//
// Логика:
//   - n_ctx over-allocation: recommendedNumCtx = p.FeasibleMaxContext (или optimal)
//   - KV cache mismatch: рекомендуем optimal, но ТОЛЬКО если у нас size_bytes
//     (computeOptimalKVCacheType иначе возвращает "no data")
//   - num_gpu_layers: если -1 (all) и все слои влезают → keep current -1
//
// Параметр currentLoaded — текущее состояние модели (для diff).
func PlanApplyAutoTune(analysis *AutoTuneAnalysis, currentLoaded types.LlamaCppModel) *AutoTuneReloadPlan {
	return PlanApplyAutoTuneWithMinContext(analysis, currentLoaded, 0)
}

// PlanApplyAutoTuneWithMinContext — R69 (2026-09-23): то же, но с учётом
// минимально необходимого n_ctx (сколько просил клиент).
//
// minContext > 0 запрещает «over-allocation fix» вниз: на живом стенде Cline с
// num_ctx=65536 получал reload 65536 → 38385 (AutoTune) → снова 65536 → …,
// каждый запрос завершался 503 «n_ctx auto-reload in progress».
func PlanApplyAutoTuneWithMinContext(
	analysis *AutoTuneAnalysis, currentLoaded types.LlamaCppModel, minContext int,
) *AutoTuneReloadPlan {
	if analysis == nil || !analysis.IsSubOptimal {
		return nil
	}

	plan := &AutoTuneReloadPlan{
		ModelName: currentLoaded.Name,
	}

	for _, rec := range analysis.Recommendations {
		switch rec.Category {
		case "context":
			// R53.2: currentLoaded.ContextLength → recommendedNumCtx
			if rec.RecommendedNumCtx <= 0 || rec.RecommendedNumCtx == currentLoaded.ContextLength {
				continue
			}
			// R69: уменьшать ниже клиентского запроса нельзя — рекомендация
			// построена на текущем feasible и не знает про запросы клиента.
			if minContext > 0 && rec.RecommendedNumCtx < minContext {
				logger.Get().Infow("autotune: skipping n_ctx downgrade below client request",
					"model", currentLoaded.Name,
					"current_n_ctx", currentLoaded.ContextLength,
					"recommended_n_ctx", rec.RecommendedNumCtx,
					"client_requested_n_ctx", minContext)
				continue
			}
			plan.ContextSize = rec.RecommendedNumCtx
			plan.Reason = fmt.Sprintf("R54.4: n_ctx %d → %d (over-allocation fix)",
				currentLoaded.ContextLength, rec.RecommendedNumCtx)
		case "kv_cache":
			// Если recommendedKVCache отличается от current — план
			if rec.RecommendedKVCache != "" && rec.RecommendedKVCache != currentLoaded.KvCacheType {
				plan.KVCacheType = rec.RecommendedKVCache
				if plan.Reason == "" {
					plan.Reason = fmt.Sprintf("R54.4: kv_cache %s → %s (quality fix)",
						currentLoaded.KvCacheType, rec.RecommendedKVCache)
				}
			}
		case "layers":
			// R229 (2026-10-22): -2 («auto offload») — не ЦЕЛЬ reload'а, а режим,
			// в котором воркер сам решает раскладку. После перезагрузки с -2 воркер
			// сообщает ФАКТ (реальное число слоёв, например 20), анализ снова видит
			// «рекомендуется -2, а стоит 20» и выдаёт тот же план — получается серия
			// одинаковых reload'ов (AutoTune «включает авто» снова и снова), пока не
			// сработает circuit breaker. Живой симптом: модель перезагружается без
			// видимой причины, клиент получает 503.
			//
			// Поэтому -2 в план НЕ попадает никогда: auto — это настройка профиля/
			// панели, а не решение автоматики. Остальные рекомендации слоёв
			// (конкретное число) применяются как раньше.
			if rec.RecommendedNumGPULayers == -2 {
				if logger.Get() != nil {
					logger.Get().Infow(i18n.T("balancer.autotune.layers_auto_no_reload_info"),
						"model", currentLoaded.Name,
						"current_gpu_layers", currentLoaded.NumGPULayers,
						"reason", i18n.T("balancer.autotune.layers_auto_no_reload_reason"))
				}
				continue
			}
			if rec.RecommendedNumGPULayers != 0 && rec.RecommendedNumGPULayers != currentLoaded.NumGPULayers {
				plan.NumGPULayers = rec.RecommendedNumGPULayers
				if plan.Reason == "" {
					plan.Reason = fmt.Sprintf("R54.4: gpu_layers %d → %d",
						currentLoaded.NumGPULayers, rec.RecommendedNumGPULayers)
				}
			}
		}
	}

	if !plan.NeedsReload() {
		return nil
	}

	// R83 (2026-09-29): если reload запускается НЕ ради контекста (kv_cache или
	// layers), контекст обязан остаться фактически загруженным.
	//
	// ДЕФЕКТ, который это закрывает (живой лог 2026-09-29): план менял только
	// kv_cache f16 → q4_0, ContextSize оставался 0, ApplyAutoTunePlan при нуле НЕ
	// кладёт contextSize в запрос, и cppworker поднимал модель со своим
	// env-дефолтом (CPPWORKER_CTX_SIZE=32768) вместо загруженных 131072. Оператор
	// видел «после запроса от Cline модель принудительно сброшена до 32768», хотя
	// Cline просил 128000; дальше каждый запрос получал code 2 (n_ctx exceeds).
	//
	// Ставим значение ТОЛЬКО если рекомендация «context» ничего не предложила
	// (ContextSize == 0), и только после проверки NeedsReload, чтобы «план без
	// изменений» по-прежнему возвращал nil (это контракт PlanApplyAutoTune).
	if plan.ContextSize <= 0 {
		plan.ContextSize = currentLoaded.ContextLength
	}
	return plan
}

// _ = strings.Contains — keep import
var _ = strings.Contains
