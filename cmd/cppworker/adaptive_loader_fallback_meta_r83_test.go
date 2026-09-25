//go:build llama_stub

// adaptive_loader_fallback_meta_r83_test.go — R83 (2026-09-25).
//
// C2 из плана. В ветке fallback_no_meta (GGUF header не дал архитектуры:
// NLayers/NEmbd/NHeads = 0) оценка KV и выбор типа KV-cache опирались на
// ХАРДКОД:
//
//	KV-cache estimate: 42 layers, n_kv_heads=2, head_dim=128
//	modelBytes := 5120 MB   // «conservative model estimate»
//	nKvHeads := 2; if modelBytes > 10GB { nKvHeads = 4 }   // оценка по размеру
//
// Для Qwen3.8-27B (15.3 ГБ, 64 слоя, 8 kv-heads) это занижало KV-cache в разы,
// а maxViableNCtx — число, которое уходит балансеру и в UI как feasible —
// оказывался завышенным, то есть «влезает больше, чем реально».
//
// Тест фиксирует: n_kv_heads из меты используется, а не угадывается по размеру.
package main

import (
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

const r83GiB = int64(1024 * 1024 * 1024)

func r83Env() *EnvironmentProfile {
	return &EnvironmentProfile{
		TotalVRAM:     24 * r83GiB,
		FreeVRAM:      20 * r83GiB,
		TotalRAM:      64 * r83GiB,
		FreeRAM:       48 * r83GiB,
		HasCUDA:       true,
		NVMLReady:     true,
		OverheadBytes: 1 * r83GiB,
	}
}

// TestR83_FallbackNoMeta_UsesNKvHeadsFromMeta — главный регресс C2.
func TestR83_FallbackNoMeta_UsesNKvHeadsFromMeta(t *testing.T) {
	env := r83Env()
	cfg := &cppbackend.Config{DefaultCtxSize: 32768, DefaultGPULayers: 32}

	// NLayers/NEmbd/NHeads = 0 → именно ветка fallback_no_meta.
	base := cppbackend.GGUFModelMeta{
		Architecture: "qwen3",
		SizeBytes:    16 * r83GiB,
	}

	meta4 := base
	meta4.NKvHeads = 4
	meta8 := base
	meta8.NKvHeads = 8

	r4 := SelectStrategyWithKV(env, "Qwen3.8-27B-UD-Q4_K_M", meta4, 65536, -1, cfg, "")
	r8 := SelectStrategyWithKV(env, "Qwen3.8-27B-UD-Q4_K_M", meta8, 65536, -1, cfg, "")

	if r4.Stage != "fallback_no_meta" || r8.Stage != "fallback_no_meta" {
		t.Fatalf("ожидалась ветка fallback_no_meta, получено %q / %q", r4.Stage, r8.Stage)
	}
	if r4.MaxViableNCtx <= 0 || r8.MaxViableNCtx <= 0 {
		t.Fatalf("maxViableNCtx не посчитан: 4→%d, 8→%d", r4.MaxViableNCtx, r8.MaxViableNCtx)
	}

	// KV на токен линейно зависит от n_kv_heads: вдвое больше heads → примерно
	// вдвое меньше максимальный контекст. На старом коде (nKvHeads по размеру,
	// meta.NKvHeads игнорировался) оба значения совпадали → тест падал.
	if r8.MaxViableNCtx >= r4.MaxViableNCtx {
		t.Errorf("maxViableNCtx не зависит от n_kv_heads из меты: 4→%d, 8→%d (ожидалось 8→меньше)",
			r4.MaxViableNCtx, r8.MaxViableNCtx)
	}
	ratio := float64(r4.MaxViableNCtx) / float64(r8.MaxViableNCtx)
	if ratio < 1.8 || ratio > 2.2 {
		t.Errorf("отношение maxViableNCtx = %.2f, ожидалось ≈2 (KV линейно по n_kv_heads)", ratio)
	}
}

// TestR83_FallbackNoMeta_ZeroNKvHeadsFallsBackToSizeGuess — когда мета не дала
// n_kv_heads (0), оценка по размеру остаётся: без неё KV посчитался бы по
// 2 heads и дал бы завышенный maxViableNCtx для крупных моделей.
func TestR83_FallbackNoMeta_ZeroNKvHeadsFallsBackToSizeGuess(t *testing.T) {
	env := r83Env()
	cfg := &cppbackend.Config{DefaultCtxSize: 32768, DefaultGPULayers: 32}

	meta := cppbackend.GGUFModelMeta{
		Architecture: "qwen3",
		SizeBytes:    16 * r83GiB,
		NKvHeads:     0, // мета не дала
	}
	unknown := SelectStrategyWithKV(env, "m", meta, 65536, -1, cfg, "")

	meta8 := meta
	meta8.NKvHeads = 8
	known := SelectStrategyWithKV(env, "m", meta8, 65536, -1, cfg, "")

	if unknown.MaxViableNCtx <= 0 {
		t.Fatal("maxViableNCtx не посчитан при NKvHeads=0")
	}
	// 16 ГБ попадает в класс «>10 ГБ» → оценка 4 heads, то есть между 2 и 8.
	if unknown.MaxViableNCtx <= known.MaxViableNCtx {
		t.Errorf("оценка по размеру (4 heads) дала %d, а точные 8 heads — %d: оценка должна быть оптимистичнее",
			unknown.MaxViableNCtx, known.MaxViableNCtx)
	}
}
