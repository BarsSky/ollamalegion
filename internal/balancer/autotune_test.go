// autotune_test.go — Round 54.1 (2026-08-24): unit tests for AutoTune analysis.

package balancer

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestAnalyzeLoadedModel_Optimal — sub-optimal detection negative case.
// Current state is already optimal → no recommendations.
func TestAnalyzeLoadedModel_Optimal(t *testing.T) {
	p := &ModelProfileInfo{
		Name:                 "Qwen3-4B",
		SizeBytes:            2_500_000_000, // 2.5GB Q4
		NLayers:              36,
		NEmbd:                2560,
		NKvHeads:             8,
		HeadDimK:             80,
		CurrentContextLength: 32768,
		CurrentKVCacheType:   "f16",
		CurrentNumGPULayers:  36,
		FreeVRAMBytes:        6_000_000_000, // 6GB free
		FreeRAMBytes:         16_000_000_000,
		TotalVRAMBytes:       8_000_000_000, // RTX 3070
		FeasibleMaxContext:   30000,
	}
	analysis := analyzeLoadedModel(p)
	if analysis == nil {
		t.Fatal("expected non-nil analysis")
	}
	if analysis.IsSubOptimal {
		t.Errorf("expected IsSubOptimal=false, got true. Recommendations: %+v", analysis.Recommendations)
	}
	if len(analysis.Recommendations) > 0 {
		t.Errorf("expected 0 recommendations, got %d: %v",
			len(analysis.Recommendations), analysis.Recommendations)
	}
}

// TestAnalyzeLoadedModel_Q4TooAggressive — q4_0 KV cache on small model with
// enough VRAM for f16 → should recommend f16.
func TestAnalyzeLoadedModel_Q4TooAggressive(t *testing.T) {
	p := &ModelProfileInfo{
		Name:                 "Qwen3-4B",
		SizeBytes:            2_500_000_000,
		NLayers:              36,
		NEmbd:                2560,
		NKvHeads:             8,
		HeadDimK:             80,
		CurrentContextLength: 32768,
		CurrentKVCacheType:   "q4_0", // SUB-OPTIMAL: 4B model fits with f16
		CurrentNumGPULayers:  36,
		FreeVRAMBytes:        6_000_000_000,
		FreeRAMBytes:         16_000_000_000,
		TotalVRAMBytes:       8_000_000_000,
		FeasibleMaxContext:   30000,
	}
	analysis := analyzeLoadedModel(p)
	if !analysis.IsSubOptimal {
		t.Fatal("expected IsSubOptimal=true for q4_0 on small model with enough VRAM")
	}
	// Should have at least 1 recommendation about KV cache
	hasKVRec := false
	for _, r := range analysis.Recommendations {
		if r.Category == "kv_cache" {
			hasKVRec = true
			if r.RecommendedKVCache != "f16" {
				t.Errorf("expected recommendation to suggest f16, got %s", r.RecommendedKVCache)
			}
		}
	}
	if !hasKVRec {
		t.Errorf("expected KV cache recommendation, got %+v", analysis.Recommendations)
	}
}

// TestAnalyzeLoadedModel_OverAllocatedContext — current n_ctx >> feasibleMax.
func TestAnalyzeLoadedModel_OverAllocatedContext(t *testing.T) {
	p := &ModelProfileInfo{
		Name:                 "Qwen3-4B",
		SizeBytes:            2_500_000_000,
		NLayers:              36,
		NEmbd:                2560,
		NKvHeads:             8,
		HeadDimK:             80,
		CurrentContextLength: 131072, // 131K
		CurrentKVCacheType:   "f16",
		CurrentNumGPULayers:  36,
		FreeVRAMBytes:        6_000_000_000,
		FreeRAMBytes:         16_000_000_000,
		TotalVRAMBytes:       8_000_000_000,
		FeasibleMaxContext:   10000, // only 10K actually feasible
	}
	analysis := analyzeLoadedModel(p)
	if !analysis.IsSubOptimal {
		t.Fatal("expected IsSubOptimal=true for n_ctx=131072 with feasibleMax=10000")
	}
	hasContextRec := false
	for _, r := range analysis.Recommendations {
		if r.Category == "context" {
			hasContextRec = true
			if r.RecommendedNumCtx > p.FeasibleMaxContext {
				t.Errorf("recommended n_ctx should be ≤ feasibleMax, got %d", r.RecommendedNumCtx)
			}
		}
	}
	if !hasContextRec {
		t.Errorf("expected context recommendation, got %+v", analysis.Recommendations)
	}
}

// TestAnalyzeLoadedModel_NotLoaded — model not in "loaded" state → no analysis.
func TestAnalyzeLoadedModel_NotLoaded(t *testing.T) {
	p := &ModelProfileInfo{
		Name: "Loading-model",
		// no current params
	}
	analysis := analyzeLoadedModel(p)
	if analysis == nil {
		t.Fatal("expected non-nil analysis (always emit something for inspection)")
	}
	if analysis.IsSubOptimal {
		t.Errorf("empty profile should not be sub-optimal")
	}
}

// TestAnalyzeBackend_MultipleModels — multiple models, mixed state.
func TestAnalyzeBackend_MultipleModels(t *testing.T) {
	models := []types.LlamaCppModel{
		{
			Name:               "good-model",
			State:              "loaded",
			ContextLength:      32768,
			KvCacheType:        "f16",
			NumGPULayers:       36,
			Size:               2_500_000_000,
			Architecture:       "qwen3",
			NLayers:            36,
			NEmbd:              2560,
			NKvHeads:           8,
			HeadDimK:           80,
			MaxContext:         131072,
			FeasibleMaxContext: 30000,
		},
		{
			Name:               "suboptimal-model",
			State:              "loaded",
			ContextLength:      32768,
			KvCacheType:        "q4_0", // sub-optimal
			NumGPULayers:       36,
			Size:               2_500_000_000,
			Architecture:       "qwen3",
			NLayers:            36,
			NEmbd:              2560,
			NKvHeads:           8,
			HeadDimK:           80,
			MaxContext:         131072,
			FeasibleMaxContext: 30000,
		},
		{
			Name:               "loading-model",
			State:              "loading", // should be skipped
			ContextLength:      0,
		},
	}
	report := AnalyzeBackend("test-backend", "llama_cpp", models, 6_000_000_000, 16_000_000_000, 8_000_000_000)
	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if len(report.Models) != 2 {
		t.Errorf("expected 2 loaded models analyzed, got %d", len(report.Models))
	}
	if report.OverallSeverity == "ok" {
		t.Errorf("expected overall severity != ok (suboptimal-model has q4_0), got %s", report.OverallSeverity)
	}
	if !strings.Contains(report.OverallSummary, "sub-optimal") {
		t.Errorf("summary should mention sub-optimal: %s", report.OverallSummary)
	}
}

// TestSeverityRank — proper ordering.
func TestSeverityRank(t *testing.T) {
	if severityRank("ok") >= severityRank("info") {
		t.Error("ok should be less than info")
	}
	if severityRank("info") >= severityRank("warning") {
		t.Error("info should be less than warning")
	}
	if severityRank("warning") >= severityRank("critical") {
		t.Error("warning should be less than critical")
	}
}

// TestFormatRecommendationShort — short format for logs.
func TestFormatRecommendationShort(t *testing.T) {
	r := AutoTuneRecommendation{
		Severity: "warning",
		Category: "kv_cache",
		Message:  "test message",
	}
	s := FormatRecommendationShort(r)
	if !strings.Contains(s, "[warning/kv_cache]") {
		t.Errorf("expected prefix in %s", s)
	}
	if !strings.Contains(s, "test message") {
		t.Errorf("expected message in %s", s)
	}
}
