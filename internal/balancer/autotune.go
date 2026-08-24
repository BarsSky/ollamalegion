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
	CurrentKVCache  string `json:"currentKvCache,omitempty"`
	RecommendedKVCache string `json:"recommendedKvCache,omitempty"`
	CurrentNumCtx   int    `json:"currentNumCtx,omitempty"`
	RecommendedNumCtx int    `json:"recommendedNumCtx,omitempty"`
	CurrentNumGPULayers int    `json:"currentNumGpuLayers,omitempty"`
	RecommendedNumGPULayers int    `json:"recommendedNumGpuLayers,omitempty"`
}

// AutoTuneAnalysis — результат анализа одного loadedModel.
type AutoTuneAnalysis struct {
	ModelName       string                    `json:"modelName"`
	IsSubOptimal    bool                      `json:"isSubOptimal"`
	// R54.2: AutoTuneEnabled — per-model switch (after inheritance).
	// false = AutoTune отключён для этой модели (manual control).
	AutoTuneEnabled bool                      `json:"autoTuneEnabled"`
	Recommendations []AutoTuneRecommendation `json:"recommendations"`
	// Optional: computed optimal params (nil если current is already optimal)
	OptimalContextLength   int    `json:"optimalContextLength,omitempty"`
	OptimalKVCacheType     string `json:"optimalKvCacheType,omitempty"`
	OptimalNumGPULayers    int    `json:"optimalNumGpuLayers,omitempty"`
	AnalysisReasoning      string `json:"analysisReasoning,omitempty"`
}

// AutoTuneReport — отчёт для одного бэкенда.
type AutoTuneReport struct {
	BackendID  string             `json:"backendId"`
	BackendType string            `json:"backendType"`
	// AutoTuneEnabled (R54.2) — глобальный switch. Per-model override
	// отражается в каждой AutoTuneAnalysis.AutoTuneEnabled ниже.
	AutoTuneEnabled bool             `json:"autoTuneEnabled"`
	Models     []AutoTuneAnalysis `json:"models"`
	OverallSeverity string        `json:"overallSeverity"` // worst severity
	OverallSummary string         `json:"overallSummary"`
}

// ModelProfileInfo — минимальные данные о модели для AutoTune.
type ModelProfileInfo struct {
	Name           string
	Quantization   string  // "Q4_K_M", "Q8_0", etc.
	Architecture   string  // "qwen3", "gemma-4", etc.
	NLayers        int
	NEmbd          int
	NKvHeads       int
	HeadDimK       int
	HeadDimV       int
	SizeBytes      uint64
	GgufMaxContext int     // max n_ctx for this model
	// Current loaded state
	CurrentContextLength int
	CurrentKVCacheType   string
	CurrentNumGPULayers  int
	CurrentFlashAttn     int
	CurrentUseMmap       bool
	// Hardware constraints
	FreeVRAMBytes uint64
	FreeRAMBytes  uint64
	TotalVRAMBytes uint64
	FeasibleMaxContext int  // from cppworker
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
	// Estimate KV cache size per token (in bytes)
	// KV cache = 2 (K+V) * NLayers * NKvHeads * HeadDimK * dtype_bytes
	// dtype_bytes: f16=2, q8_0=1, q4_0=0.5
	kvBytesPerTokenF16 := 2 * 2 * p.NLayers * p.NKvHeads * p.HeadDimK
	if kvBytesPerTokenF16 == 0 {
		// Missing architecture data — can't compute, fall back to current
		return p.CurrentKVCacheType, "недостаточно данных для расчёта (NLayers/NKvHeads/HeadDimK не указаны)"
	}

	modelSizeGB := float64(p.SizeBytes) / (1024 * 1024 * 1024)
	freeVRAMGB := float64(p.FreeVRAMBytes) / (1024 * 1024 * 1024)

	// Compute max context with f16 KV cache (in free VRAM)
	maxCtxF16 := int((freeVRAMGB * 1024 * 1024 * 1024 * 0.7) / float64(kvBytesPerTokenF16))
	// 0.7 = safety margin (model weights + activations)

	// For small models (≤4B Q4_K_M, ~2.5GB), f16 KV cache fits well
	if modelSizeGB <= 3.5 && maxCtxF16 >= 16384 {
		return "f16", fmt.Sprintf("model_size=%.1fGB, f16 KV cache fits в %.1fGB freeVRAM (max_ctx=%d)",
			modelSizeGB, freeVRAMGB, maxCtxF16)
	}

	// Medium models (4-8B Q4): q8_0 — баланс качества и контекста
	if modelSizeGB <= 8.0 {
		return "q8_0", fmt.Sprintf("model_size=%.1fGB, q8_0 KV cache (f16 не влезет в %.1fGB)",
			modelSizeGB, freeVRAMGB)
	}

	// Large models (>8B Q4): q4_0 — максимальный контекст
	return "q4_0", fmt.Sprintf("model_size=%.1fGB, q4_0 KV cache нужен для контекста", modelSizeGB)
}

// computeOptimalNumGPULayers — Round 54.1: optimal num_gpu_layers для модели.
//
// Логика:
//   - Если model помещается в VRAM с GPU layers = -1 (all): рекомендуем -1
//   - Иначе если есть auto_offload в cppworker: рекомендуем -2 (auto)
//   - Иначе: estimate layers that fit
//
// Семантика значений:
//   - -1 = все слои на GPU (auto-detect by cppworker)
//   - -2 = auto offload (cppworker решает сколько влезает)
//   - N (positive) = явно N слоёв на GPU
//
// Current "all layers" = N (where N == NLayers). Если current = -1, current
// is also "all layers" (cppworker's auto-detect). Need to compare semantically.
func computeOptimalNumGPULayers(p *ModelProfileInfo) (recommendation int, reasoning string) {
	if p.SizeBytes == 0 || p.FreeVRAMBytes == 0 || p.NLayers == 0 {
		return p.CurrentNumGPULayers, "недостаточно данных"
	}

	modelGB := float64(p.SizeBytes) / (1024 * 1024 * 1024)
	freeGB := float64(p.FreeVRAMBytes) / (1024 * 1024 * 1024)

	// Estimate per-layer size
	bytesPerLayer := float64(p.SizeBytes) / float64(p.NLayers)
	layersThatFit := int(freeGB * 1024 * 1024 * 1024 * 0.85 / bytesPerLayer)

	var optimalLayers int
	var optimalReasoning string
	if layersThatFit >= p.NLayers {
		optimalLayers = -1 // all layers
		optimalReasoning = fmt.Sprintf("model=%.1fGB, free=%.1fGB, все %d слоёв влезут (≈%.1fGB) — рекомендуем -1 (all)",
			modelGB, freeGB, p.NLayers, float64(p.NLayers)*bytesPerLayer/(1024*1024*1024))
	} else {
		optimalLayers = -2 // auto offload
		optimalReasoning = fmt.Sprintf("model=%.1fGB не влезает в free=%.1fGB, нужен auto offload (cppworker решает)",
			modelGB, freeGB)
	}

	// Compare semantically: if current == NLayers or current == -1, it's "all layers"
	currentMeansAll := p.CurrentNumGPULayers == p.NLayers || p.CurrentNumGPULayers == -1
	optimalMeansAll := optimalLayers == -1

	if currentMeansAll && optimalMeansAll {
		// Both are "all layers" — no recommendation
		return p.CurrentNumGPULayers, "current = all layers, optimal = all layers"
	}

	// If both are "auto offload" (-2), no recommendation
	if p.CurrentNumGPULayers == -2 && optimalLayers == -2 {
		return p.CurrentNumGPULayers, "current = auto offload, optimal = auto offload"
	}

	return optimalLayers, optimalReasoning
}

// analyzeLoadedModel — Round 54.1: главная функция. Анализирует одну модель
// и возвращает AutoTuneAnalysis с рекомендациями.
//
// Возвращает nil если данных недостаточно.
func analyzeLoadedModel(p *ModelProfileInfo) *AutoTuneAnalysis {
	if p == nil {
		return nil
	}

	analysis := &AutoTuneAnalysis{
		ModelName:       p.Name,
		IsSubOptimal:    false,
		AutoTuneEnabled: p.AutoTuneEnabled,
	}

	// 1) Analyze KV cache type
	if p.CurrentKVCacheType != "" {
		optimalKV, reasoningKV := computeOptimalKVCacheType(p)
		if optimalKV != "" && optimalKV != p.CurrentKVCacheType {
			// Severity: f16 < q4_0 (closer to f16 = better quality)
			severity := "info"
			if p.CurrentKVCacheType == "q4_0" && optimalKV == "f16" {
				severity = "warning" // significant quality loss
			}
			analysis.Recommendations = append(analysis.Recommendations, AutoTuneRecommendation{
				Severity:            severity,
				Category:            "kv_cache",
				Message:             fmt.Sprintf("KV cache '%s' — sub-optimal, рекомендуется '%s'", p.CurrentKVCacheType, optimalKV),
				Suggestion:          fmt.Sprintf("Перезагрузить с kvCacheType='%s': %s", optimalKV, reasoningKV),
				CurrentKVCache:      p.CurrentKVCacheType,
				RecommendedKVCache:  optimalKV,
			})
			analysis.OptimalKVCacheType = optimalKV
		}
	}

	// 2) Analyze num_gpu_layers
	if p.CurrentNumGPULayers != 0 {
		optimalLayers, reasoningLayers := computeOptimalNumGPULayers(p)
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
				Severity:           "info",
				Category:           "context",
				Message:            fmt.Sprintf("n_ctx=%d сильно больше feasible_max=%d (over-allocation)", p.CurrentContextLength, p.FeasibleMaxContext),
				Suggestion:         fmt.Sprintf("Перезагрузить с n_ctx=%d для эффективного использования VRAM", p.FeasibleMaxContext),
				CurrentNumCtx:      p.CurrentContextLength,
				RecommendedNumCtx:  p.FeasibleMaxContext,
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
		BackendID:      backendID,
		BackendType:    backendType,
		Models:         []AutoTuneAnalysis{},
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
			FreeVRAMBytes:        freeVRAMBytes,
			FreeRAMBytes:         freeRAMBytes,
			TotalVRAMBytes:       totalVRAMBytes,
			FeasibleMaxContext:   m.FeasibleMaxContext,
			// R54.2: per-model AutoTune switch (after inheritance).
			AutoTuneEnabled: IsAutoTuneEnabled(proxy, m.Name),
		}
		analysis := analyzeLoadedModel(prof)
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
//   p.config.Balancing.AutoTune — global switch (default true)
//   profile.AutoTune = &false — manual override для этой модели
func IsAutoTuneEnabled(p *Proxy, modelName string) bool {
	if p == nil {
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

// _ = strings.Contains — keep import
var _ = strings.Contains
