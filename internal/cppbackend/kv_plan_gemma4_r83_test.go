// kv_plan_gemma4_r83_test.go — R83 (2026-10-01): раскладка KV-кэша для моделей
// с KV-sharing и скользящим окном (gemma4/gemma3n).
//
// Тесты фиксируют факты, измеренные на реальном файле
// gemma-4-E4B-it-Q4_K_M.gguf и подтверждённые логом llama.cpp при загрузке:
//
//	gemma4.block_count                        = 42
//	gemma4.attention.shared_kv_layers         = 18   → KV держат 24 слоя из 42
//	gemma4.attention.sliding_window           = 512
//	gemma4.attention.sliding_window_pattern   = 111110 × 7 (35 оконных)
//	gemma4.attention.key_length               = 512  (глобальные слои)
//	gemma4.attention.key_length_swa           = 256  (оконные слои)
//
//	llama_kv_cache: layer 5/11/17/23: dev = CUDA0   → 4 глобальных слоя
//	llama_kv_cache: layer 24..41: does not have KV cache
//
// До этой правки «слоёв с KV» считалось 42 (все), KV на 65536 оценивался в
// 3.0 GiB вместо 294 MiB, и memfit оставлял 7 слоёв на CPU (5-9 tok/s).
package cppbackend

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestKVPlanFor_Gemma4RealNumbers(t *testing.T) {
	pattern := ""
	for i := 0; i < 7; i++ {
		pattern += "111110"
	}
	if len(pattern) != 42 {
		t.Fatalf("паттерн длиной %d, want 42", len(pattern))
	}

	plan := KVPlanFor("gemma4", 42, 0, 0, false, 18, pattern, 512, 256, 512)
	if plan.KVLayers() != 24 {
		t.Errorf("KVLayers = %d, want 24 (42 − 18 shared)", plan.KVLayers())
	}
	if plan.GlobalLayers != 4 {
		t.Errorf("GlobalLayers = %d, want 4 (слои 5/11/17/23)", plan.GlobalLayers)
	}
	if plan.SWALayers != 20 {
		t.Errorf("SWALayers = %d, want 20", plan.SWALayers)
	}
	if plan.SWAWindow != 512 || plan.SWAHeadDim != 256 || plan.GlobalHeadDim != 512 {
		t.Errorf("окно/головы: window=%d swaHeadDim=%d globalHeadDim=%d, want 512/256/512",
			plan.SWAWindow, plan.SWAHeadDim, plan.GlobalHeadDim)
	}
	if !plan.Exact {
		t.Error("раскладка посчитана по правилу llama.cpp — exact должен быть true")
	}
}

// TestKVPlanFor_NoPatternIsUpperBound — без ключа sliding_window_pattern число
// слоёв с кэшем всё равно известно (из KV-sharing), но разделить глобальные и
// оконные нельзя: отдаём верхнюю оценку с exact=false.
func TestKVPlanFor_NoPatternIsUpperBound(t *testing.T) {
	plan := KVPlanFor("gemma4", 42, 0, 0, false, 18, "", 512, 256, 512)
	if plan.KVLayers() != 24 || plan.Exact {
		t.Errorf("без паттерна: layers=%d exact=%v, want 24/false", plan.KVLayers(), plan.Exact)
	}
	if plan.GlobalLayers != 24 || plan.SWALayers != 0 {
		t.Errorf("без паттерна все слои считаются глобальными: global=%d swa=%d",
			plan.GlobalLayers, plan.SWALayers)
	}

	// Без ключа shared_kv_layers — прежняя верхняя оценка (все слои).
	none := KVPlanFor("gemma4", 42, 0, 0, false, 0, "", 0, 0, 512)
	if none.KVLayers() != 42 || none.Exact {
		t.Errorf("без shared_kv_layers: layers=%d exact=%v, want 42/false", none.KVLayers(), none.Exact)
	}
}

// TestKVPlanFor_Qwen38Unchanged — гибридные модели с рекуррентными слоями не
// затронуты: у них окна нет, число KV-слоёв прежнее.
func TestKVPlanFor_Qwen38Unchanged(t *testing.T) {
	plan := KVPlanFor("qwen35", 65, 1, 0, false, 0, "", 0, 0, 256)
	if plan.KVLayers() != 16 || !plan.Exact || plan.SWALayers != 0 {
		t.Errorf("qwen35: layers=%d exact=%v swa=%d, want 16/true/0", plan.KVLayers(), plan.Exact, plan.SWALayers)
	}
}

// TestReadGGUFHeader_Gemma4KVSharing — парсер обязан прочитать ключи KV-sharing
// и массив sliding_window_pattern: без них раскладку KV посчитать нечем.
func TestReadGGUFHeader_Gemma4KVSharing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gemma4_kvshare.gguf")
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
		must(binary.Write(f, w, uint32(4))) // valType = uint32
		must(binary.Write(f, w, v))
	}
	// Массив флагов: valType=9 (array), elemType=7 (bool), len, затем байты.
	boolArr := func(name, pattern string) {
		str(name)
		must(binary.Write(f, w, uint32(9))) // valType = array
		must(binary.Write(f, w, uint32(7))) // elemType = bool
		must(binary.Write(f, w, uint64(len(pattern))))
		for i := 0; i < len(pattern); i++ {
			b := byte(0)
			if pattern[i] == '1' {
				b = 1
			}
			must(binary.Write(f, w, b))
		}
	}

	pattern := ""
	for i := 0; i < 7; i++ {
		pattern += "111110"
	}

	count := uint64(10) // architecture + 8 скаляров + массив паттерна
	must(binary.Write(f, w, []byte("GGUF")))
	must(binary.Write(f, w, uint32(3)))
	must(binary.Write(f, w, uint64(0))) // tensorCount
	must(binary.Write(f, w, count))     // metadataKvCount

	str("general.architecture")
	must(binary.Write(f, w, uint32(8))) // valType = string
	str("gemma4")

	u32("gemma4.block_count", 42)
	u32("gemma4.attention.head_count", 8)
	u32("gemma4.attention.head_count_kv", 2)
	u32("gemma4.attention.key_length", 512)
	u32("gemma4.attention.value_length", 512)
	u32("gemma4.attention.sliding_window", 512)
	u32("gemma4.attention.shared_kv_layers", 18)
	u32("gemma4.attention.key_length_swa", 256)
	boolArr("gemma4.attention.sliding_window_pattern", pattern)

	info, err := ReadGGUFHeader(path)
	if err != nil {
		t.Fatalf("ReadGGUFHeader: %v", err)
	}
	if info.SharedKVLayers != 18 {
		t.Errorf("shared_kv_layers = %d, want 18", info.SharedKVLayers)
	}
	if info.SlidingWindow != 512 {
		t.Errorf("sliding_window = %d, want 512", info.SlidingWindow)
	}
	if info.SWAKeyLength != 256 {
		t.Errorf("key_length_swa = %d, want 256", info.SWAKeyLength)
	}
	if info.SWAPattern != pattern {
		t.Errorf("sliding_window_pattern = %q, want %q", info.SWAPattern, pattern)
	}

	plan := info.KVPlan()
	if plan.GlobalLayers != 4 || plan.SWALayers != 20 || !plan.Exact {
		t.Errorf("KVPlan: global=%d swa=%d exact=%v, want 4/20/true",
			plan.GlobalLayers, plan.SWALayers, plan.Exact)
	}
	kvLayers, exact := info.KVLayers()
	if kvLayers != 24 || !exact {
		t.Errorf("KVLayers() = (%d, %v), want (24, true)", kvLayers, exact)
	}
	// head_dim KV берётся из key_length (512), а не из n_embd/n_heads (320).
	if got := info.KVHeadDim(); got != 512 {
		t.Errorf("KVHeadDim() = %d, want 512", got)
	}
}
