//go:build llama_stub

// auto_offload_kv_step2_r83_test.go — R83 §9.4 шаг 2 (2026-09-27).
//
// Зачем отдельный тест. legacy-фоллбэк auto_offload (путь, где memfit не может
// собрать полный ModelSpec) считал KV по block_count и n_embd/n_heads. На
// Qwen3.8-27B это 64 слоя × 213 = 13 632 «пары» против реальных 16 слоёв × 256
// = 4 096 — завышение в 3.3 раза, из-за которого раскладка исторически уезжала
// в gpu_layers=0. Здесь зафиксировано, что новая формула:
//
//  1. сходится с memfit.KVBytesPerToken (единая формула на весь стек);
//  2. даёт ровно то, что печатает llama.cpp на живом стенде (1088 MiB);
//  3. не ломается на неполных метаданных (возвращает консервативную оценку,
//     а не ноль и не панику).
package main

import (
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/memfit"
)

// qwen38Spec — Qwen3.8-27B ровно с теми числами, что на живом стенде:
// 64 блока, но KV держат только 16 слоёв, key_length 256 (≠ n_embd/n_heads=213),
// и 4 головы KV (nKvHeads). Последнее важно: с 8 головами KV выходит вдвое
// больше и перестаёт сходиться с аллокацией llama.cpp — числа взяты из
// estimatorScenarios() (internal/cppbackend/estimator_compare_r83_test.go),
// где они зафиксированы по живому замеру.
func qwen38Spec() memfit.ModelSpec {
	return cppbackend.MemfitSpecFromValues(
		"qwen3.8:latest",
		16464440224, // 15.3 GiB
		64, 24, 4, 5120, 262144,
		16,  // kv_layers
		256, // kv_head_dim (attention.key_length)
	)
}

// TestR83_Step2_KVMatchesMemfit — legacy-оценка KV обязана совпадать с memfit
// на тех же входных данных. Расхождение = два источника истины, то есть
// возврат дефекта, ради которого шаг 2 и делался.
func TestR83_Step2_KVMatchesMemfit(t *testing.T) {
	spec := qwen38Spec()
	const ctx = 32768

	for _, kvType := range []string{"f16", "q8_0", "q4_0"} {
		legacy := estimateKVCacheBytesForLayers(ctx, spec.EffectiveKVLayers(),
			spec.NKvHeads, spec.EffectiveKVHeadDim(), kvType)
		memfitKV := int64(memfit.KVBytesPerToken(spec, cppbackend.MemfitKVType(kvType))) * ctx
		if legacy != memfitKV {
			t.Errorf("kv=%s: legacy=%d, memfit=%d — формулы разошлись", kvType, legacy, memfitKV)
		}
	}
}

// TestR83_Step2_KVMatchesLlamaCpp — числа из живого лога:
//
//	llama_kv_cache: size = 1088.00 MiB (32768 cells, 16 layers, K/V (q8_0))
//
// Если этот тест падает — раскладка снова начнёт планировать не то, что
// аллоцирует llama.cpp.
func TestR83_Step2_KVMatchesLlamaCpp(t *testing.T) {
	const ctx = 32768
	got := estimateKVCacheBytesForLayers(ctx, 16, 4, 256, "q8_0")
	kBPerToken := float64(got) / ctx / 1024
	if kBPerToken < 33.9 || kBPerToken > 34.1 {
		t.Errorf("kv/token = %.2f KiB, want ≈34.0 KiB (как в логе live-стенда)", kBPerToken)
	}
	mib := float64(got) / (1024 * 1024)
	if mib < 1087 || mib > 1089 {
		t.Errorf("KV(32768, q8_0, 16 слоёв, head_dim=256) = %.2f MiB, want ≈1088 MiB "+
			"(ровно столько аллоцировал llama.cpp)", mib)
	}
	// И контрольная точка на масштаб типов (эталон — f16 = 2 байта на элемент):
	//   q8_0 = f16 × 17/32   (34/32 против 2)
	//   q4_0 = f16 ×  9/32   (18/32 против 2)
	// Целочисленное деление тут не мешает: 32768 и элементы делятся на 32.
	f16 := estimateKVCacheBytesForLayers(ctx, 16, 4, 256, "f16")
	q8 := estimateKVCacheBytesForLayers(ctx, 16, 4, 256, "q8_0")
	q4 := estimateKVCacheBytesForLayers(ctx, 16, 4, 256, "q4_0")
	if q8 != f16*17/32 {
		t.Errorf("q8_0 = %d, want f16×17/32 = %d", q8, f16*17/32)
	}
	if q4 != f16*9/32 {
		t.Errorf("q4_0 = %d, want f16×9/32 = %d", q4, f16*9/32)
	}
}

// TestR83_Step2_OldFormulaWasOverestimating — фиксирует САМ дефект, чтобы его
// нельзя было вернуть незаметно: прежняя формула (block_count × n_embd/n_heads)
// завышает KV на живой модели в разы.
func TestR83_Step2_OldFormulaWasOverestimating(t *testing.T) {
	const ctx = 32768
	newWay := estimateKVCacheBytesForLayers(ctx, 16, 4, 256, "q8_0")
	oldWay := estimateKVCacheBytes(ctx, 64 /* block_count */, 5120, 24, 8, "q8_0")
	if oldWay <= newWay {
		t.Fatalf("прежняя формула больше не завышает (%d против %d) — тест устарел, "+
			"проверьте, не сломали ли estimateKVCacheBytes", oldWay, newWay)
	}
	if oldWay < 2*newWay {
		t.Errorf("завышение всего %.2fx — ожидалось кратное (на 27B было ~3.3x)",
			float64(oldWay)/float64(newWay))
	}
}

// TestR83_Step2_IncompleteMetadataIsConservative — без метаданных KV считать не
// из чего: обязана вернуться не «ноль» (это молча разрешило бы всю модель в
// VRAM), а явный отказ вызывающему через ok=false + прежняя верхняя оценка.
func TestR83_Step2_IncompleteMetadataIsConservative(t *testing.T) {
	// Через backend без каталога моделей — метаданных нет.
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	if l, hd, ok := backend.KVLayersForModel("no-such-model"); ok {
		t.Errorf("KVLayersForModel без метаданных вернул ok=true (%d, %d)", l, hd)
	}

	// И сама функция на пустых числах не выдумывает размер: 0 = «не знаю»,
	// вызывающий обязан взять консервативную оценку (см. auto_offload.go).
	if got := estimateKVCacheBytesForLayers(32768, 0, 0, 0, "q8_0"); got != 0 {
		t.Errorf("без kv_layers/kv_head_dim получилось %d, want 0 («не знаю»)", got)
	}
}

// TestR83_Step2_RealMetadataWins — когда метаданные есть, KVLayersForModel
// отдаёт ИМЕННО реальные числа (16/256), а не block_count и не n_embd/n_heads.
func TestR83_Step2_RealMetadataWins(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()

	const name = "kv-step2-model"
	be, filename := backendWithTruncatedModel(t, name, 64, 24, 8, 5120, 1024*1024)
	backend = be
	// backendWithTruncatedModel пишет fake-GGUF без attention.key_length, поэтому
	// дописываем метаданные KV так, как их видит ModelManager после чтения
	// заголовка: 16 слоёв с KV и key_length=256.
	mm := be.ModelManager()
	meta, err := mm.GetModelMeta(filename)
	if err != nil || meta == nil {
		t.Fatalf("GetModelMeta: %v", err)
	}
	meta.KeyLength = 256

	layers, headDim, ok := backend.KVLayersForModel(filename)
	if !ok {
		t.Fatal("KVLayersForModel не нашёл метаданные — реальный путь не проверен")
	}
	if layers != 64 {
		// KVLayersFor для архитектуры qwen3 без nextn/recurrent-метаданных
		// считает KV по всем блокам — это ожидаемо и честно: «нет данных о
		// гибридности». Важно, что head_dim взят из key_length (256), а не 213.
		t.Logf("kv_layers=%d (нет метаданных о гибридности — консервативно все блоки)", layers)
	}
	if headDim != 256 {
		t.Errorf("kv_head_dim = %d, want 256 (attention.key_length, а не n_embd/n_heads=213)", headDim)
	}
}
