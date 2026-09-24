// adaptive_loader_r70_test.go — R70 (2026-09-24): хинт kvCacheType для
// адаптивной стратегии.
//
// ПРОБЛЕМА (живой стенд, Cline 65K на RTX 3070 8 GB): балансер запрашивал
// стратегию без указания KV-типа, cppworker считал по f16 (KV на 65K ≈ 7 GB) и
// отвечал
//
//	stage=cpu_only, gpuLayers=0, kvCacheType=f16,
//	recommendation="for 8GB VRAM, use n_ctx <= 65536 with kvCacheType=q4_0"
//
// то есть его же рекомендация противоречила его же ответу. Балансер применял
// gpuLayers=0 (CPU-only) — reload 2 минуты и медленная генерация, хотя с q4_0
// та же модель влезает на GPU (в живом стеке профиль q4_0 и даёт 42/43 слоя).
//
// ФИКС: балансер передаёт известный рабочий тип (?kv_cache_type=…), стратегия
// начинает перебор с него (kvCacheOrderWithHint).
package main

import (
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// gemmaMetaR70 — метаданные gemma-4-E4B-it-Q4_K_M (как в живом стеке).
func gemmaMetaR70() cppbackend.GGUFModelMeta {
	return cppbackend.GGUFModelMeta{
		Architecture: "gemma4",
		NLayers:      42,
		NEmbd:        2560,
		NHeads:       8,
		NKvHeads:     2,
		SizeBytes:    4215695776,
	}
}

// envR70 — 8 GB VRAM (3 GB свободно) + 20 GB RAM, как на живом стенде.
func envR70() *EnvironmentProfile {
	return &EnvironmentProfile{
		GPUCount:      1,
		TotalVRAM:     8 * 1024 * 1024 * 1024,
		FreeVRAM:      3 * 1024 * 1024 * 1024,
		TotalRAM:      24 * 1024 * 1024 * 1024,
		FreeRAM:       20 * 1024 * 1024 * 1024,
		OverheadBytes: 512 * 1024 * 1024,
	}
}

// TestSelectStrategyWithKV_R70_HintKeepsModelOnGPU — с хинтом q4_0 стратегия не
// сваливается в cpu_only и остаётся на этом же типе KV.
func TestSelectStrategyWithKV_R70_HintKeepsModelOnGPU(t *testing.T) {
	env := envR70()
	meta := gemmaMetaR70()
	cfg := &cppbackend.Config{}

	withHint := SelectStrategyWithKV(env, "gemma-4-E4B-it-Q4_K_M", meta, 65536, -2, cfg, "q4_0")
	if withHint.KVCacheType != "q4_0" {
		t.Errorf("с хинтом q4_0 ожидался kvCacheType=q4_0, получено %q (stage=%s)",
			withHint.KVCacheType, withHint.Stage)
	}
	if withHint.Stage == "cpu_only" {
		t.Errorf("с хинтом q4_0 модель не должна уходить в cpu_only: %s", withHint.Explanation)
	}
	if withHint.GPULayers <= 0 {
		t.Errorf("с хинтом q4_0 ожидались слои на GPU, получено gpuLayers=%d (stage=%s)",
			withHint.GPULayers, withHint.Stage)
	}
	t.Logf("hint=q4_0 → stage=%s gpuLayers=%d kv=%s maxViable=%d",
		withHint.Stage, withHint.GPULayers, withHint.KVCacheType, withHint.MaxViableNCtx)
}

// TestSelectStrategyWithKV_R70_NoHintKeepsLegacy — без хинта поведение прежнее
// (перебор начинается с f16), то есть существующие вызовы не изменились.
func TestSelectStrategyWithKV_R70_NoHintKeepsLegacy(t *testing.T) {
	env := envR70()
	meta := gemmaMetaR70()
	cfg := &cppbackend.Config{}

	legacy := SelectStrategy(env, "gemma-4-E4B-it-Q4_K_M", meta, 65536, -2, cfg)
	explicitEmpty := SelectStrategyWithKV(env, "gemma-4-E4B-it-Q4_K_M", meta, 65536, -2, cfg, "")

	if legacy.KVCacheType != explicitEmpty.KVCacheType || legacy.Stage != explicitEmpty.Stage ||
		legacy.GPULayers != explicitEmpty.GPULayers {
		t.Errorf("SelectStrategy и SelectStrategyWithKV(\"\") должны совпадать: %+v vs %+v", legacy, explicitEmpty)
	}
	t.Logf("без хинта → stage=%s gpuLayers=%d kv=%s maxViable=%d",
		legacy.Stage, legacy.GPULayers, legacy.KVCacheType, legacy.MaxViableNCtx)
}

// TestKVCacheOrderWithHint_R70 — порядок перебора: хинт первым, остальные как
// fallback; невалидный/пустой хинт → прежний порядок.
func TestKVCacheOrderWithHint_R70(t *testing.T) {
	order := kvCacheOrderWithHint("q4_0")
	if len(order) != len(KVCacheTypeOrder) {
		t.Fatalf("ожидался тот же набор типов, получено %v", order)
	}
	if order[0] != "q4_0" {
		t.Errorf("хинт должен быть первым, получено %v", order)
	}
	seen := map[string]bool{}
	for _, kv := range order {
		if seen[kv] {
			t.Errorf("дубликат в порядке: %v", order)
		}
		seen[kv] = true
	}
	if got := kvCacheOrderWithHint(""); len(got) != len(KVCacheTypeOrder) || got[0] != KVCacheTypeOrder[0] {
		t.Errorf("пустой хинт должен давать прежний порядок, получено %v", got)
	}
	if got := kvCacheOrderWithHint("bogus"); len(got) != len(KVCacheTypeOrder) || got[0] != KVCacheTypeOrder[0] {
		t.Errorf("невалидный хинт должен игнорироваться, получено %v", got)
	}
}
