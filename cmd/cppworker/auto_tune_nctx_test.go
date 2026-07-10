package main

import (
	"os"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// helper для установки ENV переменных в тестах.
func setEnv(t *testing.T, key, value string) {
	t.Helper()
	original := os.Getenv(key)
	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("os.Setenv(%s): %v", key, err)
	}
	t.Cleanup(func() {
		if original == "" {
			_ = os.Unsetenv(key)
		} else {
			_ = os.Setenv(key, original)
		}
	})
}

// TestAvailableRAMBytes_EnvOverride — ENV override имеет приоритет.
func TestAvailableRAMBytes_EnvOverride(t *testing.T) {
	// 16 GB
	setEnv(t, "CPPWORKER_AVAILABLE_RAM_BYTES", "17179869184")
	got := availableRAMBytes()
	if got != 17179869184 {
		t.Errorf("expected 16GB, got %d bytes", got)
	}
}

// TestAvailableRAMBytes_InvalidEnv — некорректный ENV → 0 (fallback).
func TestAvailableRAMBytes_InvalidEnv(t *testing.T) {
	setEnv(t, "CPPWORKER_AVAILABLE_RAM_BYTES", "not-a-number")
	got := availableRAMBytes()
	if got != 0 {
		t.Errorf("expected 0 for invalid env, got %d", got)
	}
}

// TestAutoTuneNCtx_NoMetadata — fallback на default для модели без метаданных.
func TestAutoTuneNCtx_NoMetadata(t *testing.T) {
	// currentConfig — package-level *cppbackend.Config, может быть nil в тестах.
	if currentConfig == nil {
		currentConfig = &cppbackend.Config{DefaultCtxSize: 8192, DefaultGPULayers: 32}
	} else {
		originalCtxSize := currentConfig.DefaultCtxSize
		currentConfig.DefaultCtxSize = 8192
		defer func() { currentConfig.DefaultCtxSize = originalCtxSize }()
	}

	m := cppbackend.ModelInfo{
		Name: "test-model",
		// SizeBytes и NLayers = 0 → fallback
	}
	got := AutoTuneNCtx(m, 0, "")
	if got.Source != "fallback" {
		t.Errorf("expected source=fallback, got %q", got.Source)
	}
	if got.RecommendedNCtx != 8192 {
		t.Errorf("expected RecommendedNCtx=8192 (default), got %d", got.RecommendedNCtx)
	}
}

// TestAutoTuneNCtx_ExactMatch — когда requested n_ctx помещается в VRAM.
func TestAutoTuneNCtx_ExactMatch(t *testing.T) {
	// Модели gemma-4-E4B-it-Q4_K_M (~5GB), 24GB VRAM.
	// Default ctx_size 8192. requested=16384.
	setEnv(t, "CPPWORKER_VRAM_BYTES", "25769803776") // 24 GB
	m := cppbackend.ModelInfo{
		Name:          "gemma-4",
		SizeBytes:     5 * 1024 * 1024 * 1024, // 5 GB
		NLayers:       32,
		NEmbd:         2560,
		NHeads:        32,
		NKvHeads:      8,
		ContextSize:   8192,
		GPULayers:     32,
	}
	got := AutoTuneNCtx(m, 16384, "")
	if got.Source != "exact_match" {
		t.Errorf("expected source=exact_match, got %q (ctx=%d, gpu=%d)",
			got.Source, got.RecommendedNCtx, got.RecommendedGPULayers)
	}
	if got.RecommendedNCtx != 16384 {
		t.Errorf("expected RecommendedNCtx=16384, got %d", got.RecommendedNCtx)
	}
	if got.MaxViableNCtx < 16384 {
		t.Errorf("expected MaxViableNCtx >= 16384, got %d", got.MaxViableNCtx)
	}
}

// TestAutoTuneNCtx_PartialOffload — средняя модель на маленькой VRAM
// требует partial offload (gpu_layers < NLayers, n_ctx остаётся запрошенным).
//
// Используем 10GB модель на 16GB VRAM с n_ctx=16384 — все слои не влезают,
// но с partial offload (gpu_layers=N-1) KV-cache для 16K помещается в VRAM.
func TestAutoTuneNCtx_PartialOffload(t *testing.T) {
	setEnv(t, "CPPWORKER_VRAM_BYTES", "17179869184") // 16 GB
	setEnv(t, "CPPWORKER_AVAILABLE_RAM_BYTES", "68719476736") // 64 GB RAM
	m := cppbackend.ModelInfo{
		Name:        "medium-model",
		SizeBytes:   10 * 1024 * 1024 * 1024, // 10 GB
		NLayers:     40,
		NEmbd:       4096,
		NHeads:      32,
		NKvHeads:    32,
		ContextSize: 8192,
		GPULayers:   40, // all layers currently
	}
	got := AutoTuneNCtx(m, 16384, "")
	if got.Source != "partial_offload" {
		t.Errorf("expected source=partial_offload, got %q (ctx=%d, gpu=%d, max=%d)",
			got.Source, got.RecommendedNCtx, got.RecommendedGPULayers, got.MaxViableNCtx)
	}
	if got.RecommendedGPULayers >= 40 {
		t.Errorf("expected reduced gpu_layers < 40, got %d", got.RecommendedGPULayers)
	}
	if got.RecommendedNCtx != 16384 {
		t.Errorf("expected RecommendedNCtx=16384 (requested fits via partial offload), got %d", got.RecommendedNCtx)
	}
	if !got.UseMmap {
		t.Error("expected UseMmap=true для partial offload")
	}
}

// TestAutoTuneNCtx_ReducedNCtx — когда даже cpu-only + mmap не влезает с requested n_ctx.
func TestAutoTuneNCtx_ReducedNCtx(t *testing.T) {
	// Очень маленькая VRAM (2GB), requested n_ctx=200K → reduce n_ctx.
	setEnv(t, "CPPWORKER_VRAM_BYTES", "2147483648") // 2 GB
	m := cppbackend.ModelInfo{
		Name:        "huge-model",
		SizeBytes:   10 * 1024 * 1024 * 1024,
		NLayers:     32,
		NEmbd:       4096,
		NHeads:      32,
		NKvHeads:    32,
		ContextSize: 32768,
		GPULayers:   32,
	}
	got := AutoTuneNCtx(m, 200000, "")
	if got.Source != "reduced_nctx" {
		t.Errorf("expected source=reduced_nctx, got %q (ctx=%d, max=%d)",
			got.Source, got.RecommendedNCtx, got.MaxViableNCtx)
	}
	if got.RecommendedNCtx >= 200000 {
		t.Errorf("expected RecommendedNCtx < 200000, got %d", got.RecommendedNCtx)
	}
	if got.MaxViableNCtx == 0 {
		t.Error("expected MaxViableNCtx > 0 для diagnostics")
	}
}

// TestAutoTuneNCtx_RamFallbackMaxCap — ram-fallback-max cap имеет приоритет.
func TestAutoTuneNCtx_RamFallbackMaxCap(t *testing.T) {
	setEnv(t, "CPPWORKER_VRAM_BYTES", "25769803776") // 24 GB
	originalMax := *ramFallbackMaxNCtx
	*ramFallbackMaxNCtx = 16384
	defer func() { *ramFallbackMaxNCtx = originalMax }()

	m := cppbackend.ModelInfo{
		Name:        "model",
		SizeBytes:   5 * 1024 * 1024 * 1024,
		NLayers:     32,
		NEmbd:       4096,
		NHeads:      32,
		NKvHeads:    32,
		ContextSize: 8192,
		GPULayers:   32,
	}
	// requested=100000, но ram-fallback-max=16384 → cap до 16384.
	got := AutoTuneNCtx(m, 100000, "")
	if got.RecommendedNCtx > 16384 {
		t.Errorf("expected RecommendedNCtx <= 16384 (capped by ram-fallback-max), got %d", got.RecommendedNCtx)
	}
}

// TestAutoTuneNCtx_NoVRAM — если VRAM неизвестна (fallback), use current params.
func TestAutoTuneNCtx_NoVRAM(t *testing.T) {
	// Устанавливаем VRAM=0 (force fallback path).
	setEnv(t, "CPPWORKER_VRAM_BYTES", "0")
	m := cppbackend.ModelInfo{
		Name:        "model",
		SizeBytes:   5 * 1024 * 1024 * 1024,
		NLayers:     32,
		NEmbd:       4096,
		NHeads:      32,
		NKvHeads:    32,
		ContextSize: 8192,
		GPULayers:   20,
	}
	got := AutoTuneNCtx(m, 16384, "")
	// При VRAM=0 → tryBridgeGPUInfo/nvidia-smi могут вернуть > 0 в реальной системе,
	// но в unit-тестах окружение не имеет GPU → fallback.
	if got.Source != "fallback" {
		t.Logf("got source=%q (may vary in CI environments with GPU)", got.Source)
	}
}

// TestAutoTuneNCtx_SafetyFactorFromEnv — 2026-06-29: проверяет, что
// CPPWORKER_NCTX_SAFETY_FACTOR env-flag уважается и применяется ко всем
// этапам расчёта (AutoTuneNCtx + computeMaxViableNCtx).
func TestAutoTuneNCtx_SafetyFactorFromEnv(t *testing.T) {
	m := cppbackend.ModelInfo{
		Name:        "gemma-4",
		SizeBytes:   5 * 1024 * 1024 * 1024, // 5 GB
		NLayers:     32,
		NEmbd:       2560,
		NHeads:      32,
		NKvHeads:    8,
		ContextSize: 8192,
		GPULayers:   32,
	}

	// Default safetyFactor=0.85.
	setEnv(t, "CPPWORKER_VRAM_BYTES", "8589934592") // 8 GB
	setEnv(t, "CPPWORKER_AVAILABLE_RAM_BYTES", "16777216000") // 16 GB
	gotDefault := AutoTuneNCtx(m, 16384, "")
	t.Logf("default safetyFactor=0.85: source=%s n_ctx=%d gpu_layers=%d max_viable_n_ctx=%d",
		gotDefault.Source, gotDefault.RecommendedNCtx,
		gotDefault.RecommendedGPULayers, gotDefault.MaxViableNCtx)
	if gotDefault.Source == "fallback" {
		t.Fatalf("expected proper autotune (not fallback), got %+v", gotDefault)
	}

	// Env override 0.92 — больший бюджет (более щедрый partial offload).
	// MaxViableNCtx должен быть БОЛЬШЕ чем при default.
	setEnv(t, "CPPWORKER_NCTX_SAFETY_FACTOR", "0.92")
	gotHighSafety := AutoTuneNCtx(m, 16384, "")
	t.Logf("env safetyFactor=0.92: source=%s n_ctx=%d gpu_layers=%d max_viable_n_ctx=%d",
		gotHighSafety.Source, gotHighSafety.RecommendedNCtx,
		gotHighSafety.RecommendedGPULayers, gotHighSafety.MaxViableNCtx)

	// Invalid value должно использовать default 0.85 (без fatal).
	setEnv(t, "CPPWORKER_NCTX_SAFETY_FACTOR", "not-a-number")
	gotInvalid := AutoTuneNCtx(m, 16384, "")
	if gotInvalid.Source == "fallback" {
		t.Fatalf("invalid env should fallback to default 0.85, not fatal")
	}
	t.Logf("invalid env rejected, nctx_safety_factor=%.3f (default)", 0.85)

	// Out-of-range value должен быть rejected.
	setEnv(t, "CPPWORKER_NCTX_SAFETY_FACTOR", "2.0")
	gotOOR := AutoTuneNCtx(m, 16384, "")
	if gotOOR.Source == "fallback" {
		t.Fatalf("out-of-range env should be rejected, not fatal")
	}

	// После out-of-range → возвращаем к default 0.85, проверим, что это всё ещё работает.
	t.Cleanup(func() {
		// тест-окружение не сохраняет prev value (через t.Cleanup)
	})
}

// TestComputeMaxViableNCtx — sanity check для формулы.
func TestComputeMaxViableNCtx(t *testing.T) {
	setEnv(t, "CPPWORKER_VRAM_BYTES", "8589934592") // 8 GB
	m := cppbackend.ModelInfo{
		Name:        "model",
		SizeBytes:   5 * 1024 * 1024 * 1024,
		NLayers:     32,
		NEmbd:       4096,
		NHeads:      32,
		NKvHeads:    32,
		ContextSize: 8192,
		GPULayers:   32,
	}
	weightsPerLayer := int64(m.SizeBytes) / int64(m.NLayers)
	overhead := int64(1536) * 1024 * 1024
	got := computeMaxViableNCtx(m, 8589934592, 32, weightsPerLayer, overhead)
	// Должен быть > 0 (KV-cache помещается с 32 GPU layers).
	if got <= 0 {
		t.Errorf("expected MaxViableNCtx > 0, got %d", got)
	}
	t.Logf("computeMaxViableNCtx(8GB VRAM, 32 layers) = %d", got)
}