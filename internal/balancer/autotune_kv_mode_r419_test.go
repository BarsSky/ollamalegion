//go:build llama_stub

// autotune_kv_mode_r419_test.go — R419 (2026-11-03): AutoTune не меняет
// KV-cache автоматически (по умолчанию), потому что смена KV — выбор качества,
// а не следствие «сколько сейчас свободно VRAM».
package balancer

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func newKVModeTestProxyR419() *Proxy {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	p.backends = map[string]*BackendState{}
	return p
}

// TestR419_DefaultModeIsRecommend — без env политика recommend.
func TestR419_DefaultModeIsRecommend(t *testing.T) {
	t.Setenv("LB_AUTOTUNE_KV_MODE", "")
	if got := autoTuneKVModeR419(); got != AutoTuneKVModeRecommend {
		t.Fatalf("mode = %q, want recommend", got)
	}
	if autoTuneKVAutomaticR419() {
		t.Fatal("автоматическая смена KV включена по умолчанию")
	}
}

// TestR419_EnvAutoEnablesAutomatic — оператор явно включил auto.
func TestR419_EnvAutoEnablesAutomatic(t *testing.T) {
	t.Setenv("LB_AUTOTUNE_KV_MODE", " AUTO ")
	if got := autoTuneKVModeR419(); got != AutoTuneKVModeAuto {
		t.Fatalf("mode = %q, want auto", got)
	}
	if !autoTuneKVAutomaticR419() {
		t.Fatal("auto-режим не включился")
	}
}

// TestR419_InvalidModeFallsBackToRecommend — мусор/неизвестное значение не
// должны включать автоматическую смену качества.
func TestR419_InvalidModeFallsBackToRecommend(t *testing.T) {
	for _, v := range []string{"on", "1", "true", "off", "auto-something"} {
		t.Setenv("LB_AUTOTUNE_KV_MODE", v)
		if got := autoTuneKVModeR419(); got != AutoTuneKVModeRecommend {
			t.Fatalf("value %q: mode = %q, want recommend", v, got)
		}
	}
}

// TestR419_RecommendDropsKVPlan — в режиме recommend KV выпадает из плана, и
// reload ради одного KV не запускается.
func TestR419_RecommendDropsKVPlan(t *testing.T) {
	t.Setenv("LB_AUTOTUNE_KV_MODE", "")
	resetAutoTuneKVGuardR416()
	t.Cleanup(resetAutoTuneKVGuardR416)

	p := newKVModeTestProxyR419()
	plan := &AutoTuneReloadPlan{ModelName: "Qwen3.8-9B", KVCacheType: "f16"}
	dropped := guardAutoTuneKVPlanR416(p, "be1", plan.ModelName, plan)
	if dropped == "" {
		t.Fatal("KV-рекомендация не отклонена в режиме recommend")
	}
	if !strings.Contains(dropped, "LB_AUTOTUNE_KV_MODE") {
		t.Fatalf("причина не называет режим: %q", dropped)
	}
	if plan.KVCacheType != "" {
		t.Fatalf("KVCacheType = %q, want пусто", plan.KVCacheType)
	}
	if plan.NeedsReload() {
		t.Fatal("план без KV не должен требовать reload")
	}
}

// TestR419_AutoKeepsKVPlan — в auto KV остаётся (профиль не фиксирует).
func TestR419_AutoKeepsKVPlan(t *testing.T) {
	t.Setenv("LB_AUTOTUNE_KV_MODE", "auto")
	resetAutoTuneKVGuardR416()
	t.Cleanup(resetAutoTuneKVGuardR416)

	p := newKVModeTestProxyR419()
	plan := &AutoTuneReloadPlan{ModelName: "m", KVCacheType: "f16"}
	if dropped := guardAutoTuneKVPlanR416(p, "be1", plan.ModelName, plan); dropped != "" {
		t.Fatalf("auto: KV отклонён (%s)", dropped)
	}
	if plan.KVCacheType != "f16" {
		t.Fatalf("auto: KVCacheType = %q, want f16", plan.KVCacheType)
	}
}
