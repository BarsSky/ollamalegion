//go:build llama_stub

// autotune_kv_guard_r416_test.go — R416 (2026-11-03): KV-cache не переключается
// туда-обратно и не спорит с профилем модели.
//
// ЖИВОЙ ДЕФЕКТ. После загрузки Qwen3.8-9B балансер на каждом запросе делал
// reload с чередованием «q8_0 → q4_0» / «q4_0 → q8_0»: анализ строит
// «оптимальный» KV по свободной VRAM, а после каждой перезагрузки она меняется —
// оба состояния неоптимальны друг относительно друга. Каждый reload = unload +
// load (~80 c), клиент видит «модель тормозит».
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func newKVGuardTestProxyR416(kv string) *Proxy {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	p.backends = map[string]*BackendState{}
	if kv != "" {
		p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
			"Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M": {ContextLength: 16384, KVCacheType: kv},
		}
	}
	return p
}

// TestR416_ProfilePinnedKVWins — оператор задал kvCacheType в профиле:
// рекомендация AutoTune его не трогает.
func TestR416_ProfilePinnedKVWins(t *testing.T) {
	p := newKVGuardTestProxyR416("q8_0")
	plan := &AutoTuneReloadPlan{ModelName: "Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M", KVCacheType: "q4_0"}
	if dropped := guardAutoTuneKVPlanR416(p, "be1", plan.ModelName, plan); dropped == "" {
		t.Fatalf("рекомендация q4_0 при профиле q8_0 не была отклонена")
	}
	if plan.KVCacheType != "" {
		t.Fatalf("KVCacheType = %q, ожидалось пусто (профиль зафиксировал q8_0)", plan.KVCacheType)
	}
	// Если в плане не осталось ничего, кроме KV, — reload не нужен.
	if plan.NeedsReload() {
		t.Fatalf("план без KV-изменения не должен требовать reload")
	}
}

// TestR416_ProfileMatchesRecommendation — рекомендация совпадает с профилем:
// план не трогаем.
func TestR416_ProfileMatchesRecommendation(t *testing.T) {
	p := newKVGuardTestProxyR416("q8_0")
	plan := &AutoTuneReloadPlan{ModelName: "Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M", KVCacheType: "q8_0"}
	if dropped := guardAutoTuneKVPlanR416(p, "be1", plan.ModelName, plan); dropped != "" {
		t.Fatalf("совпадающая с профилем рекомендация отклонена: %s", dropped)
	}
	if plan.KVCacheType != "q8_0" {
		t.Fatalf("KVCacheType = %q, ожидалось q8_0", plan.KVCacheType)
	}
}

// TestR416_FlipCooldown — без профиля первая смена KV разрешена, обратная в
// пределах cooldown — нет; после cooldown снова разрешена.
func TestR416_FlipCooldown(t *testing.T) {
	resetAutoTuneKVGuardR416()
	t.Cleanup(resetAutoTuneKVGuardR416)

	oldCooldown := autoTuneKVFlipCooldownR416
	autoTuneKVFlipCooldownR416 = 50 * time.Millisecond
	t.Cleanup(func() { autoTuneKVFlipCooldownR416 = oldCooldown })

	p := newKVGuardTestProxyR416("")
	model := "Qwen3.8-27B-UD-Q4_K_M"

	// 1) Первая смена разрешена и запоминается.
	plan := &AutoTuneReloadPlan{ModelName: model, KVCacheType: "q8_0"}
	if dropped := guardAutoTuneKVPlanR416(p, "be1", model, plan); dropped != "" {
		t.Fatalf("первая смена KV отклонена: %s", dropped)
	}
	recordAutoTuneKVAppliedR416("be1", model, "q8_0")

	// 2) Немедленно обратно — отклонено гистерезисом.
	plan = &AutoTuneReloadPlan{ModelName: model, KVCacheType: "q4_0"}
	if dropped := guardAutoTuneKVPlanR416(p, "be1", model, plan); dropped == "" {
		t.Fatalf("обратная смена KV внутри cooldown не отклонена (пинг-понг вернулся)")
	}
	if plan.KVCacheType != "" {
		t.Fatalf("KVCacheType = %q, ожидалось пусто", plan.KVCacheType)
	}

	// 3) Другой бэкенд — своя запись, cooldown не наследуется.
	plan = &AutoTuneReloadPlan{ModelName: model, KVCacheType: "q4_0"}
	if dropped := guardAutoTuneKVPlanR416(p, "be2", model, plan); dropped != "" {
		t.Fatalf("другой бэкенд не должен попадать под чужой cooldown: %s", dropped)
	}

	// 4) После cooldown — снова можно.
	time.Sleep(60 * time.Millisecond)
	plan = &AutoTuneReloadPlan{ModelName: model, KVCacheType: "q4_0"}
	if dropped := guardAutoTuneKVPlanR416(p, "be1", model, plan); dropped != "" {
		t.Fatalf("после cooldown смена KV должна быть разрешена: %s", dropped)
	}
}

// TestR416_NonKVPlanNotTouched — n_ctx/слои по-прежнему оптимизируются: гард
// относится только к KV.
func TestR416_NonKVPlanNotTouched(t *testing.T) {
	p := newKVGuardTestProxyR416("q8_0")
	plan := &AutoTuneReloadPlan{
		ModelName:    "Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M",
		ContextSize:  16384,
		NumGPULayers: -2,
		KVCacheType:  "q4_0",
	}
	if dropped := guardAutoTuneKVPlanR416(p, "be1", plan.ModelName, plan); dropped == "" {
		t.Fatalf("KV-часть не отклонена")
	}
	if !plan.NeedsReload() {
		t.Fatalf("план с n_ctx/слоями обязан остаться применимым после отклонения KV")
	}
}

// TestR416d_PreservedContextAloneIsNoChange — после гардов в плане остался только
// сохранённый контекст (== фактическому): перезагружать нечего.
func TestR416d_PreservedContextAloneIsNoChange(t *testing.T) {
	plan := &AutoTuneReloadPlan{ModelName: "m", ContextSize: 16384}
	if autoTunePlanHasRealChangeR416d(plan, 16384) {
		t.Fatalf("сохранённый контекст (== текущему) принят за изменение — reload после каждого ответа")
	}
}

// TestR416d_RealContextGrowthIsChange — рекомендация реально меняет окно.
func TestR416d_RealContextGrowthIsChange(t *testing.T) {
	plan := &AutoTuneReloadPlan{ModelName: "m", ContextSize: 16384}
	if !autoTunePlanHasRealChangeR416d(plan, 8192) {
		t.Fatalf("настоящий рост окна не признан изменением")
	}
}

// TestR416d_KVLayersAreChange — KV/слои по-прежнему считаются изменением.
func TestR416d_KVLayersAreChange(t *testing.T) {
	if !autoTunePlanHasRealChangeR416d(&AutoTuneReloadPlan{KVCacheType: "q8_0"}, 16384) {
		t.Fatal("KV-изменение не признано")
	}
	if !autoTunePlanHasRealChangeR416d(&AutoTuneReloadPlan{NumGPULayers: 28}, 16384) {
		t.Fatal("изменение слоёв не признано")
	}
	if autoTunePlanHasRealChangeR416d(&AutoTuneReloadPlan{}, 16384) {
		t.Fatal("пустой план признан изменением")
	}
}
