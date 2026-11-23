// autotune_analyze.go — R636 (2026-11-19): анализ загруженной модели: AnalyzeLoadedModelPublic, analyzeLoadedModel, analyzeLoadedModelWithWorkload.
//
// ЗАЧЕМ ВЫНЕСЕНО. `autotune.go` держал 1666 строк: типы профиля, правила
// выбора KV-типа и числа слоёв, отчёт, предохранитель, трекер, две перезагрузки
// и планирование применения. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"fmt"
	"ollama-loadbalancer/pkg/types"
)

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
		// R707: раскладка KV (глобальные/оконные слои, ширины, окно) — без неё
		// AutoTune считает KV по всем слоям и советует q4_0 там, где окно влезает.
		applyKVPlanToProfileR707(prof, &m)
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
