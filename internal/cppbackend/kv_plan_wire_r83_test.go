// kv_plan_wire_r83_test.go — R83/v52 (2026-10-01): раскладка KV обязана доехать
// до memfit из метаданных модели.
//
// Живой дефект, поймавший этот тест. checkVRAMForModel собирал ModelSpec вручную
// и брал из метаданных только «число слоёв с кэшем» и head_dim. Для gemma-4 этого
// мало (24 слоя с кэшем, из них 20 оконных), поэтому memfit по-прежнему считал
// KV как «все слои × n_ctx»: 3.17 GB вместо 294 MiB на 65536, вердикт
// partial_offload/35 слоёв, 7 слоёв на CPU и генерация 5-9 tok/s на стенде.
package cppbackend

import (
	"testing"

	"ollama-loadbalancer/internal/memfit"
)

// gemma4MetaForPlan — метаданные gemma-4-E4B с реальными числами GGUF.
func gemma4MetaForPlan() *GGUFModelMeta {
	pattern := "111110111110111110111110111110111110111110"
	return &GGUFModelMeta{
		Filename:       "gemma-4-E4B-it-Q4_K_M.gguf",
		SizeBytes:      4215695776,
		Architecture:   "gemma4",
		NLayers:        42,
		NHeads:         8,
		NKvHeads:       2,
		NEmbd:          2560,
		ContextLength:  131072,
		KeyLength:      512,
		ValueLength:    512,
		SharedKVLayers: 18,
		SlidingWindow:  512,
		SWAKeyLength:   256,
		SWAPattern:     pattern,
	}
}

func TestApplyKVPlanToSpec_Gemma4(t *testing.T) {
	spec := MemfitSpecFromValues("gemma-4-E4B-it-Q4_K_M", 4215695776, 42, 8, 2, 2560, 0, 0, 0)
	applyKVPlanToSpec(&spec, gemma4MetaForPlan())

	if spec.KVLayers != 24 {
		t.Errorf("KVLayers = %d, want 24 (42 − 18 shared)", spec.KVLayers)
	}
	if spec.KVSWALayers != 20 {
		t.Errorf("KVSWALayers = %d, want 20 — без этого поля оконные слои снова "+
			"считаются растущими с n_ctx", spec.KVSWALayers)
	}
	if spec.SWAWindow != 512 || spec.SWAHeadDim != 256 {
		t.Errorf("окно/голова SWA = %d/%d, want 512/256", spec.SWAWindow, spec.SWAHeadDim)
	}
	if spec.KVHeadDim != 512 {
		t.Errorf("KVHeadDim = %d, want 512 (key_length глобальных слоёв)", spec.KVHeadDim)
	}
}

// TestApplyKVPlanToSpec_AllowsFullOffloadOn3070 — проверка на живых числах
// стенда: с раскладкой KV модель целиком уходит на GPU, без неё — нет.
func TestApplyKVPlanToSpec_AllowsFullOffloadOn3070(t *testing.T) {
	budget := memfit.Budget{
		VRAMFree:    memfit.MiBOf(7015),
		VRAMTotal:   memfit.MiBOf(8192),
		VRAMKnown:   true,
		RAMAvail:    memfit.MiBOf(18490),
		RAMTotal:    memfit.MiBOf(25042),
		RAMKnown:    true,
		VRAMReserve: memfit.MiBOf(2048),
		RAMReserve:  memfit.MiBOf(4096),
	}
	policy := MemfitPolicy()

	withPlan := MemfitSpecFromValues("gemma-4-E4B-it-Q4_K_M", 4215695776, 42, 8, 2, 2560, 0, 0, 0)
	applyKVPlanToSpec(&withPlan, gemma4MetaForPlan())
	v := memfit.Evaluate(withPlan, memfit.Request{Ctx: 65536, KVType: memfit.KVQ4, GPULayers: -1}, budget, policy)
	if v.Stage != memfit.StageExactFit || v.GPULayers != 42 {
		t.Errorf("с раскладкой KV: stage=%s layers=%d, want exact_fit/42\n  %s", v.Stage, v.GPULayers, v.String())
	}

	without := MemfitSpecFromValues("gemma-4-E4B-it-Q4_K_M", 4215695776, 42, 8, 2, 2560, 0, 0, 0)
	v2 := memfit.Evaluate(without, memfit.Request{Ctx: 65536, KVType: memfit.KVQ4, GPULayers: -1}, budget, policy)
	if v2.Stage == memfit.StageExactFit {
		t.Errorf("без раскладки KV полного оффлоада быть не должно: %s", v2.String())
	}
}
