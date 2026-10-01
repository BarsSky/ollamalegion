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
	"encoding/binary"
	"os"
	"path/filepath"
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

// TestGetModelMetaResolved_ClientNameFindsGGUF — второй дефект того же дня:
// checkVRAMForModel звал GetModelMeta("gemma-4-E4B-it-Q4_K_M"), а в кэше ключ —
// имя файла с «.gguf». Прямой lookup возвращал ошибку, метаданные молча
// терялись, и KV снова считался по всем слоям. Здесь проверяется резолв по
// внешнему имени на настоящем (синтетическом) GGUF в каталоге моделей.
func TestGetModelMetaResolved_ClientNameFindsGGUF(t *testing.T) {
	dir := t.TempDir()
	writeGemma4GGUF(t, filepath.Join(dir, "gemma-4-E4B-it-Q4_K_M.gguf"))

	mm := NewModelManager(dir, Config{ModelsDir: dir})
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	// Прямой lookup по имени клиента (без .gguf) метаданных не даёт — именно так
	// и терялась раскладка KV.
	if meta, err := mm.GetModelMeta("gemma-4-E4B-it-Q4_K_M"); err == nil && meta != nil {
		t.Log("прямой GetModelMeta нашёл метаданные — резолв не нужен (ок)")
	}

	meta, err := mm.GetModelMetaResolved("gemma-4-E4B-it-Q4_K_M")
	if err != nil || meta == nil {
		t.Fatalf("GetModelMetaResolved: %v", err)
	}
	if meta.SharedKVLayers != 18 || meta.SlidingWindow != 512 || meta.SWAKeyLength != 256 {
		t.Errorf("метаданные KV-sharing не прочитаны: shared=%d window=%d swaKeyLen=%d",
			meta.SharedKVLayers, meta.SlidingWindow, meta.SWAKeyLength)
	}
	plan := meta.KVPlan()
	if plan.GlobalLayers != 4 || plan.SWALayers != 20 || !plan.Exact {
		t.Errorf("KVPlan: global=%d swa=%d exact=%v, want 4/20/true",
			plan.GlobalLayers, plan.SWALayers, plan.Exact)
	}

	spec := MemfitSpecFromValues("gemma-4-E4B-it-Q4_K_M", meta.SizeBytes, meta.NLayers,
		meta.NHeads, meta.NKvHeads, meta.NEmbd, 0, 0, 0)
	applyKVPlanToSpec(&spec, meta)
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
	v := memfit.Evaluate(spec, memfit.Request{Ctx: 65536, KVType: memfit.KVQ4, GPULayers: -1}, budget, MemfitPolicy())
	if v.Stage != memfit.StageExactFit || v.GPULayers != 42 {
		t.Errorf("на числах стенда должен быть полный оффлоад: stage=%s layers=%d\n  %s",
			v.Stage, v.GPULayers, v.String())
	}
}

// writeGemma4GGUF пишет минимальный валидный GGUF с метаданными gemma-4-E4B
// (те же числа, что в реальном файле стенда).
func writeGemma4GGUF(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := binary.LittleEndian
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	str := func(s string) {
		must(binary.Write(f, w, uint64(len(s))))
		_, err := f.Write([]byte(s))
		must(err)
	}
	u32 := func(name string, v uint32) {
		str(name)
		must(binary.Write(f, w, uint32(4)))
		must(binary.Write(f, w, v))
	}
	pattern := "111110111110111110111110111110111110111110"

	must(binary.Write(f, w, []byte("GGUF")))
	must(binary.Write(f, w, uint32(3)))
	must(binary.Write(f, w, uint64(0)))
	must(binary.Write(f, w, uint64(10)))

	str("general.architecture")
	must(binary.Write(f, w, uint32(8)))
	str("gemma4")

	u32("gemma4.block_count", 42)
	u32("gemma4.attention.head_count", 8)
	u32("gemma4.attention.head_count_kv", 2)
	u32("gemma4.attention.key_length", 512)
	u32("gemma4.attention.value_length", 512)
	u32("gemma4.attention.sliding_window", 512)
	u32("gemma4.attention.shared_kv_layers", 18)
	u32("gemma4.attention.key_length_swa", 256)
	str("gemma4.attention.sliding_window_pattern")
	must(binary.Write(f, w, uint32(9))) // array
	must(binary.Write(f, w, uint32(7))) // bool
	must(binary.Write(f, w, uint64(len(pattern))))
	for i := 0; i < len(pattern); i++ {
		b := byte(0)
		if pattern[i] == '1' {
			b = 1
		}
		must(binary.Write(f, w, b))
	}
	if _, err := f.Write(make([]byte, 1024)); err != nil {
		t.Fatal(err)
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
