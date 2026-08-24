// autotune_test.go — Round 54.1 (2026-08-24): unit tests for AutoTune analysis.

package balancer

import (
	"strings"
	"testing"
	"time"

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
	report := AnalyzeBackend(nil, "test-backend", "llama_cpp", models, 6_000_000_000, 16_000_000_000, 8_000_000_000)
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

// TestIsAutoTuneEnabled_GlobalDefault — R54.2: without per-model override,
// global BalancingSettings.AutoTune is used.
func TestIsAutoTuneEnabled_GlobalDefault(t *testing.T) {
	// Test 1: global ON, no profile override → ON
	pp := &Proxy{}
	pp.config = &types.LoadBalancerConfig{}
	pp.config.Balancing.AutoTune = true
	if !IsAutoTuneEnabled(pp, "test-model") {
		t.Error("expected AutoTune enabled when global=true")
	}

	// Test 2: global OFF, no profile override → OFF
	pp.config.Balancing.AutoTune = false
	if IsAutoTuneEnabled(pp, "test-model") {
		t.Error("expected AutoTune disabled when global=false")
	}

	// Test 3: empty model name → fall back to global
	pp.config.Balancing.AutoTune = true
	if !IsAutoTuneEnabled(pp, "") {
		t.Error("expected AutoTune enabled with empty model name (global=true)")
	}

	// Test 4: nil config → defaults to true
	ppNilConfig := &Proxy{}
	if !IsAutoTuneEnabled(ppNilConfig, "test-model") {
		t.Error("expected default true when config is nil")
	}
}

// TestIsAutoTuneEnabled_NilProxy — defensive: nil proxy → false (no panics).
func TestIsAutoTuneEnabled_NilProxy(t *testing.T) {
	if IsAutoTuneEnabled(nil, "test") {
		t.Error("expected false for nil proxy")
	}
	if IsAutoTuneEnabled(nil, "") {
		t.Error("expected false for nil proxy with empty model")
	}
}

// === R54.4 (2026-08-24): AutoTuneReloadPlan + CircuitBreaker tests ===

// TestAutoTuneReloadPlan_NeedsReload — verifies NeedsReload() boolean logic.
func TestAutoTuneReloadPlan_NeedsReload(t *testing.T) {
	// Empty plan → no reload
	p := &AutoTuneReloadPlan{}
	if p.NeedsReload() {
		t.Error("empty plan should not need reload")
	}

	// Only ContextSize
	p.ContextSize = 4096
	if !p.NeedsReload() {
		t.Error("plan with ContextSize should need reload")
	}

	// Only KVCacheType
	p2 := &AutoTuneReloadPlan{KVCacheType: "f16"}
	if !p2.NeedsReload() {
		t.Error("plan with KVCacheType should need reload")
	}

	// Only NumGPULayers (positive)
	p3 := &AutoTuneReloadPlan{NumGPULayers: 36}
	if !p3.NeedsReload() {
		t.Error("plan with NumGPULayers=36 should need reload")
	}

	// Only NumGPULayers (negative — semantically means "all" or "auto")
	p4 := &AutoTuneReloadPlan{NumGPULayers: -1}
	if !p4.NeedsReload() {
		t.Error("plan with NumGPULayers=-1 should need reload")
	}

	// UseMmap via pointer
	b := true
	p5 := &AutoTuneReloadPlan{UseMmap: &b}
	if !p5.NeedsReload() {
		t.Error("plan with UseMmap pointer should need reload")
	}
}

// TestPlanApplyAutoTune_NoAction — returns nil when no actionable recommendations.
func TestPlanApplyAutoTune_NoAction(t *testing.T) {
	// Optimal model → no plan
	a := &AutoTuneAnalysis{IsSubOptimal: false}
	plan := PlanApplyAutoTune(a, types.LlamaCppModel{Name: "test"})
	if plan != nil {
		t.Errorf("optimal model should return nil plan, got %+v", plan)
	}

	// Sub-optimal but recommendations don't suggest changes
	a2 := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			// Current == recommended, no change needed
			{Category: "context", CurrentNumCtx: 4096, RecommendedNumCtx: 4096},
		},
	}
	plan2 := PlanApplyAutoTune(a2, types.LlamaCppModel{Name: "test", ContextLength: 4096})
	if plan2 != nil {
		t.Errorf("no-op recommendation should return nil plan, got %+v", plan2)
	}
}

// TestPlanApplyAutoTune_ContextReload — returns plan with new ContextSize.
func TestPlanApplyAutoTune_ContextReload(t *testing.T) {
	a := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "context", CurrentNumCtx: 65536, RecommendedNumCtx: 24337},
		},
	}
	loaded := types.LlamaCppModel{Name: "test", ContextLength: 65536}
	plan := PlanApplyAutoTune(a, loaded)
	if plan == nil {
		t.Fatal("expected non-nil plan")
	}
	if plan.ContextSize != 24337 {
		t.Errorf("expected ContextSize=24337, got %d", plan.ContextSize)
	}
	if !plan.NeedsReload() {
		t.Error("plan should need reload")
	}
	if plan.Reason == "" {
		t.Error("expected Reason to be set")
	}
}

// TestPlanApplyAutoTune_KVCacheReload — returns plan with new KVCacheType.
func TestPlanApplyAutoTune_KVCacheReload(t *testing.T) {
	a := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "kv_cache", CurrentKVCache: "q4_0", RecommendedKVCache: "f16"},
		},
	}
	loaded := types.LlamaCppModel{Name: "test", KvCacheType: "q4_0"}
	plan := PlanApplyAutoTune(a, loaded)
	if plan == nil {
		t.Fatal("expected non-nil plan")
	}
	if plan.KVCacheType != "f16" {
		t.Errorf("expected KVCacheType=f16, got %s", plan.KVCacheType)
	}
}

// TestAutoTuneCircuit_CoolDown — circuit blocks reloads during cool-down.
func TestAutoTuneCircuit_CoolDown(t *testing.T) {
	cfg := AutoTuneCircuitConfig{CooldownAfterErrorSec: 60, CooldownAfterSuccessSec: 300, MaxRetries: 3}
	c := &AutoTuneCircuit{}

	// Fresh circuit — should allow reload
	if ok, _ := c.CanReload(cfg); !ok {
		t.Error("fresh circuit should allow reload")
	}

	// After successful reload — should block during stable period
	c.RecordAttempt()
	c.RecordSuccess()
	if ok, reason := c.CanReload(cfg); ok {
		t.Errorf("circuit should be stable after success, got ok=true (reason: %s)", reason)
	}

	// After error — should block during cool-down
	c2 := &AutoTuneCircuit{}
	c2.RecordAttempt()
	c2.RecordError("test error")
	if ok, reason := c2.CanReload(cfg); ok {
		t.Errorf("circuit should be cooling down after error, got ok=true (reason: %s)", reason)
	}
}

// TestAutoTuneTracker_GetCircuit — returns same circuit for same key.
func TestAutoTuneTracker_GetCircuit(t *testing.T) {
	tr := NewAutoTuneTracker(DefaultAutoTuneCircuitConfig())
	c1 := tr.GetCircuit("backend1", "model1")
	c2 := tr.GetCircuit("backend1", "model1")
	if c1 != c2 {
		t.Error("GetCircuit should return same instance for same key")
	}
	c3 := tr.GetCircuit("backend1", "model2")
	if c1 == c3 {
		t.Error("different models should have different circuits")
	}
}

// TestAutoTuneTracker_ResetCircuit — clears state.
func TestAutoTuneTracker_ResetCircuit(t *testing.T) {
	tr := NewAutoTuneTracker(DefaultAutoTuneCircuitConfig())
	c := tr.GetCircuit("b1", "m1")
	c.RecordAttempt()
	c.RecordError("err")
	c.RecordSuccess()

	tr.ResetCircuit("b1", "m1")

	if !c.LastAttempt.IsZero() || !c.LastSuccess.IsZero() || c.LastError != "" {
		t.Errorf("ResetCircuit should clear all state, got: %+v", c)
	}
}

// === R54.8 (2026-08-24): AutoTune event publishing tests ===

// TestTriggerAutoTuneReload_CircuitCoolDown_PublishesEvent — когда circuit
// блокирует reload из-за cool-down после ошибки, должен быть опубликован
// EventAutoTuneCircuitOpen в EventBus чтобы WebUI показал toast.
func TestTriggerAutoTuneReload_CircuitCoolDown_PublishesEvent(t *testing.T) {
	// Arrange: прокси с AutoTune enabled и подоптимальной моделью в метриках.
	config := createTestConfig()
	config.Balancing.AutoTune = true
	proxy := newProxyWithCleanup(t, config)

	// Подоптимальная модель: Q4 KV cache при 6GB free VRAM (f16 рекомендован).
	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.llamaMetrics["backend-1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{
			{
				Name:              "qwen3-4b",
				State:             "loaded",
				ContextLength:     65536, // over-allocated
				KvCacheType:       "q4_0", // sub-optimal
				NumGPULayers:      36,
				NLayers:           36,
				NEmbd:             2560,
				NKvHeads:          8,
				HeadDimK:          80,
				Size:              2_500_000_000,
				MaxContext:        131072,
				FeasibleMaxContext: 30000, // way below loaded 65536
			},
		},
	}
	proxy.metricsMgr.mu.Unlock()

	// Pre-poison the circuit: имитация прошлой неудачи → cool-down активен.
	proxy.autoTuneTracker = NewAutoTuneTracker(DefaultAutoTuneCircuitConfig())
	circuit := proxy.autoTuneTracker.GetCircuit("backend-1", "qwen3-4b")
	circuit.RecordAttempt()
	circuit.RecordError("previous reload failed: OOM")

	// Subscribe to EventBus to capture published events.
	subID, eventCh := proxy.EventBus().Subscribe()
	defer proxy.EventBus().Unsubscribe(subID)

	// Act: вызываем triggerAutoTuneReload — должен задетектить cool-down
	// и опубликовать EventAutoTuneCircuitOpen.
	res := proxy.triggerAutoTuneReload("backend-1", "qwen3-4b",
		6_000_000_000, 16_000_000_000, 8_000_000_000)

	// Assert: результат содержит "skipped" reason
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	if res.SkippedReason == "" {
		t.Error("expected SkippedReason to be set on cool-down")
	}
	if !strings.Contains(res.SkippedReason, "circuit-cooling-down") &&
		!strings.Contains(res.SkippedReason, "stable period") {
		t.Errorf("expected circuit-related skip reason, got: %s", res.SkippedReason)
	}

	// Проверяем что событие EventAutoTuneCircuitOpen было опубликовано.
	// EventBus неблокирующий (drop-on-full), поэтому event приходит сразу
	// или не приходит если канал переполнен. Используем polling с timeout.
	var circuitEvent *types.Event
	deadline := time.After(2 * time.Second)
	for circuitEvent == nil {
		select {
		case ev := <-eventCh:
			if ev.Type == types.EventAutoTuneCircuitOpen {
				circuitEvent = &ev
			}
		case <-deadline:
			t.Fatal("timeout waiting for EventAutoTuneCircuitOpen event")
		}
	}

	if circuitEvent.BackendID != "backend-1" {
		t.Errorf("expected backendId=backend-1, got %s", circuitEvent.BackendID)
	}
	if circuitEvent.Model != "qwen3-4b" {
		t.Errorf("expected model=qwen3-4b, got %s", circuitEvent.Model)
	}
	if circuitEvent.Severity != types.SeverityWarning {
		t.Errorf("expected severity=warning, got %s", circuitEvent.Severity)
	}
	if !strings.Contains(circuitEvent.Message, "circuit") {
		t.Errorf("expected message to mention 'circuit', got: %s", circuitEvent.Message)
	}
	if circuitEvent.Source != "autotune" {
		t.Errorf("expected source=autotune, got %s", circuitEvent.Source)
	}
}

// TestTriggerAutoTuneReload_Disabled_NoEvent — если AutoTune выключен,
// EventAutoTuneCircuitOpen не должен публиковаться.
func TestTriggerAutoTuneReload_Disabled_NoCircuitEvent(t *testing.T) {
	config := createTestConfig()
	config.Balancing.AutoTune = false
	proxy := newProxyWithCleanup(t, config)

	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.llamaMetrics["backend-1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{
			{
				Name:              "qwen3-4b",
				State:             "loaded",
				ContextLength:     65536,
				KvCacheType:       "q4_0",
				NumGPULayers:      36,
				NLayers:           36,
				NEmbd:             2560,
				NKvHeads:          8,
				HeadDimK:          80,
				Size:              2_500_000_000,
				MaxContext:        131072,
				FeasibleMaxContext: 30000,
			},
		},
	}
	proxy.metricsMgr.mu.Unlock()

	subID, eventCh := proxy.EventBus().Subscribe()
	defer proxy.EventBus().Unsubscribe(subID)

	// AutoTune disabled → должно вернуть "disabled" и НЕ публиковать circuit event.
	res := proxy.triggerAutoTuneReload("backend-1", "qwen3-4b",
		6_000_000_000, 16_000_000_000, 8_000_000_000)
	if res == nil || !strings.Contains(res.SkippedReason, "disabled") {
		t.Errorf("expected 'disabled' SkippedReason, got: %+v", res)
	}

	// Drain any events briefly — verify NO circuit event arrives.
	deadline := time.After(200 * time.Millisecond)
drain:
	for {
		select {
		case ev := <-eventCh:
			if ev.Type == types.EventAutoTuneCircuitOpen {
				t.Errorf("did not expect circuit event when AutoTune disabled")
			}
		case <-deadline:
			break drain
		}
	}
}
