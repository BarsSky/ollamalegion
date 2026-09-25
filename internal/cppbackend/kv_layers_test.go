// kv_layers_test.go — R83 (2026-09-25): число слоёв с KV-кэшем.
//
// Тесты фиксируют факты, установленные на реальном файле
// Qwen3.8-27B-UD-Q4_K_M.gguf независимым парсером метаданных и по исходникам
// llama.cpp (см. kv_layers.go). Именно здесь ловится регресс «в одном логе 64, в
// другом 65, а KV считают 16»:
//
//	qwen35.block_count           = 65   → n_layer_all
//	qwen35.nextn_predict_layers  = 1    → n_layer() = 64 (llama_model_n_layer)
//	attention.recurrent_layers   — ключа НЕТ → правило по умолчанию, интервал 4
//	attention.key_length         = 256  → head_dim KV (не n_embd/n_heads = 213)
//	⇒ KV хранят 16 слоёв, KV = 65 536 Б/токен (f16) вместо наивных 221 520
package cppbackend

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestKVLayersFor_Qwen38RealNumbers — на числах реального файла.
//
// Эталон здесь — не расчёт, а измерение: llama.cpp при загрузке напечатал
// «llama_kv_cache: size = 144.00 MiB (8192 cells, 16 layers, …)», и 16 совпадает
// с 2×16×4×256×18/32×8192 = 150 994 944 Б = 144.00 MiB.
func TestKVLayersFor_Qwen38RealNumbers(t *testing.T) {
	// Ключа full_attention_interval в файле нет → интервал задан как 0 и должен
	// смениться дефолтом llama.cpp (4). MTP-слой (индекс 64) в KV-кэш не входит,
	// поэтому считаются только слои [0, 64) → 16 attention-слоёв.
	got, exact := KVLayersFor("qwen35", 65, 1, 0, false)
	if got != 16 || !exact {
		t.Errorf("KVLayersFor(qwen35, 65, nextn=1, interval=default) = (%d, exact=%v), want (16, true)", got, exact)
	}

	// Явный интервал 4 даёт то же самое.
	if got, exact := KVLayersFor("qwen35", 65, 1, 4, false); got != 16 || !exact {
		t.Errorf("явный интервал 4: (%d, %v), want (16, true)", got, exact)
	}

	// Интервал 2: attention-слои — каждый второй из 64 → 32.
	if got, _ := KVLayersFor("qwen35", 65, 1, 2, false); got != 32 {
		t.Errorf("интервал 2: %d, want 32", got)
	}

	// Без MTP-слоя (nextn=0) результат тот же: KV считают 16 слоёв из 64.
	if got, _ := KVLayersFor("qwen35", 64, 0, 4, false); got != 16 {
		t.Errorf("nextn=0: %d, want 16", got)
	}
}

// TestKVLayersFor_GuardsAgainstGuessing — если правило неизвестно, число не
// выдумывается: возвращается верхняя оценка с exact=false, чтобы вызывающий код
// мог либо осознанно её взять, либо отказаться считать.
func TestKVLayersFor_GuardsAgainstGuessing(t *testing.T) {
	// Архитектура вне проверенного набора (gemma-4: KV общий для части слоёв).
	if got, exact := KVLayersFor("gemma4", 42, 0, 0, false); got != 42 || exact {
		t.Errorf("gemma4: (%d, exact=%v), want (42, false) — верхняя оценка", got, exact)
	}
	// В файле есть явный список рекуррентных слоёв, который мы не разбираем.
	if got, exact := KVLayersFor("qwen35", 65, 1, 0, true); got != 65 || exact {
		t.Errorf("явный recurrent_layers: (%d, exact=%v), want (65, false)", got, exact)
	}
	// Нет данных о числе блоков.
	if got, exact := KVLayersFor("qwen35", 0, 0, 0, false); got != 0 || exact {
		t.Errorf("без block_count: (%d, %v), want (0, false)", got, exact)
	}
}

// TestKVHeadDimFor — head_dim KV берётся из attention.key_length.
func TestKVHeadDimFor(t *testing.T) {
	if got := KVHeadDimFor(256, 5120, 24); got != 256 {
		t.Errorf("key_length=256: %d, want 256", got)
	}
	if got := KVHeadDimFor(0, 5120, 24); got != 213 {
		t.Errorf("без key_length: %d, want 213 (n_embd/n_heads)", got)
	}
	if got := KVHeadDimFor(0, 0, 0); got != 0 {
		t.Errorf("без данных: %d, want 0", got)
	}
}

// TestReadGGUFHeader_KVParams — парсер обязан ЧИТАТЬ ключи, от которых зависит
// реальный KV (до этой правки читались только block_count/head_count/…, поэтому
// рассчитать KV правильно было физически нечем).
func TestReadGGUFHeader_KVParams(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "qwen35_kvparams.gguf")
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

	type kv struct {
		name string
		val  uint32
	}
	keys := []kv{
		{"qwen35.block_count", 65},
		{"qwen35.nextn_predict_layers", 1},
		{"qwen35.attention.key_length", 256},
		{"qwen35.attention.value_length", 256},
		{"qwen35.attention.head_count", 24},
		{"qwen35.attention.head_count_kv", 4},
		{"qwen35.embedding_length", 5120},
		{"qwen35.context_length", 262144},
	}

	must(binary.Write(f, w, []byte("GGUF")))
	must(binary.Write(f, w, uint32(3)))
	must(binary.Write(f, w, uint64(0)))           // tensorCount
	must(binary.Write(f, w, uint64(len(keys)+1))) // metadataKvCount
	str("general.architecture")
	must(binary.Write(f, w, uint32(8))) // valType = string
	str("qwen35")
	for _, k := range keys {
		u32(k.name, k.val)
	}

	info, err := ReadGGUFHeader(path)
	if err != nil {
		t.Fatalf("ReadGGUFHeader: %v", err)
	}
	if info.Architecture != "qwen35" {
		t.Errorf("Architecture = %q", info.Architecture)
	}
	if info.KeyLength != 256 || info.ValueLength != 256 {
		t.Errorf("key_length/value_length = %d/%d, want 256/256", info.KeyLength, info.ValueLength)
	}
	if info.NextNPredictLayers != 1 {
		t.Errorf("nextn_predict_layers = %d, want 1", info.NextNPredictLayers)
	}
	if info.HasRecurrentLayersKey {
		t.Errorf("HasRecurrentLayersKey = true, хотя ключа в файле нет")
	}
	kvLayers, exact := info.KVLayers()
	if kvLayers != 16 || !exact {
		t.Errorf("info.KVLayers() = (%d, exact=%v), want (16, true)", kvLayers, exact)
	}
	if got := info.KVHeadDim(); got != 256 {
		t.Errorf("info.KVHeadDim() = %d, want 256", got)
	}
}
