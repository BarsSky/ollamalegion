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
	// R54.9: workload-aware overrides только если есть loaded n_ctx и достаточно samples
	loadedNCtx := p.CurrentContextLength
	if loadedNCtx > 0 {
		if workload.IsLight(loadedNCtx, minSamples) {
			return "f16", fmt.Sprintf("R54.9 workload-light: p95=%d < 25%% of loaded=%d → f16 (high quality)",
				workload.P95NumCtx, loadedNCtx)
		}
		if workload.IsHeavy(loadedNCtx, minSamples) {
			return "q4_0", fmt.Sprintf("R54.9 workload-heavy: p95=%d > 75%% of loaded=%d → q4_0 (max context)",
				workload.P95NumCtx, loadedNCtx)
		}
	}

	// R54.1: model-size-based logic (fallback если workload unknown)
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
	if p.CurrentKVCacheType != "" {
		optimalKV, reasoningKV := computeOptimalKVCacheTypeWithWorkload(p, workload, minSamples)
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

// AutoTuneTracker — Round 54.4 (2026-08-24): per-Proxy tracker для circuit breakers.
// Хранит AutoTuneCircuit для каждой пары (backendID, modelName).
//
// Использование:
//   p.autoTuneTracker.GetCircuit(backendID, modelName) → returns circuit (or new)
//   p.autoTuneTracker.GetConfig() → circuit config
//
// Thread-safe (sync.RWMutex).
type AutoTuneTracker struct {
	mu      sync.RWMutex
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
	Triggered       bool               // true если reload был запущен
	SkippedReason   string             // если Triggered=false, почему
	Plan            *AutoTuneReloadPlan // nil если ничего не нужно менять
	CircuitState    AutoTuneCircuitSnapshot
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
		if m.Name == modelName && m.State == "loaded" {
			loadedModel = m
			break
		}
	}
	if loadedModel.Name == "" {
		res.SkippedReason = "model not in loaded state"
		return res
	}

	// 3) Build profile + analysis
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
		FreeVRAMBytes:        freeVRAM,
		FreeRAMBytes:         freeRAM,
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
	plan := PlanApplyAutoTune(analysis, loadedModel)
	if plan == nil {
		res.SkippedReason = "no actionable recommendations (sub-optimal detected but no plan)"
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
		if p.EventBus() != nil {
			p.EventBus().Publish(types.Event{
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
		}
		return res
	}

	// 6) Trigger async reload via existing nctx_reload infrastructure
	circuit.RecordAttempt()
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
		if p.EventBus() != nil {
			p.EventBus().Publish(types.Event{
				Type:      types.EventAutoTuneReloadTriggered,
				Timestamp: time.Now(),
				BackendID: backendID,
				Model:     modelName,
				Severity:  types.SeverityInfo,
				Source:    "autotune",
				Message:   "AutoTune async reload triggered: " + plan.Reason,
				Data: map[string]interface{}{
					"contextSize":   plan.ContextSize,
					"kvCacheType":   plan.KVCacheType,
					"numGpuLayers":  plan.NumGPULayers,
					"reason":        plan.Reason,
				},
			})
		}

		err := p.executeAutoTuneReload(backendID, modelName, plan, circuit)
		if err != nil {
			circuit.RecordError(err.Error())
			logger.Get().Warnw("autotune: async reload failed",
				"backend", backendID, "model", modelName, "error", err)
			// R54.8: publish failure event
			if p.EventBus() != nil {
				p.EventBus().Publish(types.Event{
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
			}
		} else {
			circuit.RecordSuccess()
			p.autoTuneTracker.ResetCircuit(backendID, modelName)
			logger.Get().Infow("autotune: async reload succeeded",
				"backend", backendID, "model", modelName, "reason", plan.Reason)
			// R54.8: publish success event
			if p.EventBus() != nil {
				p.EventBus().Publish(types.Event{
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

	// Собираем env для cppworker load
	env := make(map[string]string)
	if plan.ContextSize > 0 {
		env["CPPWORKER_CTX_SIZE"] = fmt.Sprintf("%d", plan.ContextSize)
	}
	if plan.KVCacheType != "" {
		env["CPPWORKER_KV_CACHE_TYPE"] = plan.KVCacheType
	}
	if plan.NumGPULayers != 0 {
		env["CPPWORKER_GPU_LAYERS"] = fmt.Sprintf("%d", plan.NumGPULayers)
	}
	if plan.FlashAttn != 0 {
		env["CPPWORKER_FLASH_ATTN_TYPE"] = fmt.Sprintf("%d", plan.FlashAttn)
	}
	if plan.UseMmap != nil {
		if *plan.UseMmap {
			env["CPPWORKER_USE_MMAP"] = "true"
		} else {
			env["CPPWORKER_USE_MMAP"] = "false"
		}
	}
	// UseMmap default from current state if not set
	if _, set := env["CPPWORKER_USE_MMAP"]; !set {
		// keep current — cppworker keeps its own default
	}

	// Выполняем reload через существующую инфраструктуру.
	// Используем modelName как model alias.
	err := p.applyCppWorkerEnvReload(backendID, modelName, env, 300 /* timeoutSec */)
	return err
}

// applyCppWorkerEnvReload — Round 54.4 (2026-08-24): отправляет POST /api/models/reload
// с env-переменными. Использует существующую функцию executeAsyncReload где возможно.
//
// NB: cppworker reload endpoint не поддерживает env-параметры напрямую —
// только n_ctx и use_mmap. Для остальных параметров нужно /api/models/load+unload.
// Здесь делаем best-effort: unload + load с полным набором.
func (p *Proxy) applyCppWorkerEnvReload(backendID, modelName string, env map[string]string, timeoutSec int) error {
	// Используем существующую инфраструктуру ensureModelLoadedOnBackend,
	// которая вызывает /api/models/load с правильными params.
	// Здесь мы только запускаем reload (без n_ctx — это для n_ctx path).
	//
	// NB: для AutoTune мы хотим вызвать /api/models/unload + /api/models/load
	// с обновленными params. Это эквивалентно reset_model_via_unload+load.
	//
	// Через nctx_reload инфраструктуру это сделать сложно (она специфична
	// для n_ctx). Поэтому R54.4 — best-effort: запускаем async reload через
	// текущий cppworker /api/models/load endpoint с env-params.

	// Делегируем существующей логике — executeAsyncReload в nctx_reload.go.
	// Если он не подходит — fallback на /api/models/unload + /api/models/load.

	// TODO: implement proper AutoTune reload path. For now, log the plan.
	logger.Get().Warnw("autotune: env-based reload not fully implemented — apply manually via /api/v1/cppworker/load",
		"backend", backendID, "model", modelName, "env", env)
	return fmt.Errorf("env-based reload not yet implemented; apply plan manually")
}

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
		Operation:  "load",
		ModelName:  plan.ModelName,
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

	result.Success = true
	result.Message = fmt.Sprintf("reloaded with new params: %s", plan.Reason)
	result.FinishedAt = time.Now()
	return result
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
			if rec.RecommendedNumCtx > 0 && rec.RecommendedNumCtx != currentLoaded.ContextLength {
				plan.ContextSize = rec.RecommendedNumCtx
				plan.Reason = fmt.Sprintf("R54.4: n_ctx %d → %d (over-allocation fix)",
					currentLoaded.ContextLength, rec.RecommendedNumCtx)
			}
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
	return plan
}

// _ = strings.Contains — keep import
var _ = strings.Contains
