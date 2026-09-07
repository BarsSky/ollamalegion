// adaptive_loader_r60_13_test.go — R60.13 (2026-09-07): tests for
// smart-skip safety clamp in SelectStrategy cpu_only branch.
//
// Pre-R60.13: when the requested n_ctx exceeded VRAM (with partial
// offload), SelectStrategy picked cpu_only with theoretical maxNCtx
// up to 200K (safeVRAM + safeRAM). The model would load with KV-cache
// in RAM but take 60-180s (touching 48GB of RAM).
//
// R60.13: cap cpu_only n_ctx at a practical limit (default 65536).
// Override via env CPPWORKER_CPU_ONLY_MAX_NCTX.
package main

import (
	"os"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestSelectStrategy_CpuOnlyClamp_Default — R60.13: with no env var,
// cpu_only n_ctx is clamped to 65536.
func TestSelectStrategy_CpuOnlyClamp_Default(t *testing.T) {
	// Clear any env override.
	oldVal := os.Getenv("CPPWORKER_CPU_ONLY_MAX_NCTX")
	os.Unsetenv("CPPWORKER_CPU_ONLY_MAX_NCTX")
	defer func() {
		if oldVal != "" {
			os.Setenv("CPPWORKER_CPU_ONLY_MAX_NCTX", oldVal)
		}
	}()

	// 8GB VRAM + 16GB RAM (typical RTX 3070 setup).
	env := mkEnvBig()
	// 5GB Qwen3-4B, requested n_ctx=131072 (would otherwise take 60-180s in cpu_only).
	meta := mkQwen3Meta()
	cfg := &cppbackend.Config{}

	strategy := SelectStrategy(env, "test-qwen3-4b", meta, 131072, -1, cfg)

	// Should be cpu_only (131K exceeds VRAM).
	if strategy.Stage != "cpu_only" {
		t.Fatalf("expected cpu_only for 131K on 8GB VRAM, got %q", strategy.Stage)
	}
	// R60.13: n_ctx should be clamped to <= 65536.
	if strategy.NCtx > 65536 {
		t.Errorf("R60.13: cpu_only n_ctx = %d, want <= 65536 (clamp). Explanation: %s",
			strategy.NCtx, strategy.Explanation)
	}
	// Should still be > 0 (legitimate value, not zeroed out).
	if strategy.NCtx <= 0 {
		t.Errorf("R60.13: cpu_only n_ctx = %d (should be positive)", strategy.NCtx)
	}
	// Explanation should mention the clamp.
	if !contains(strategy.Explanation, "cpu_only_clamped_to") {
		t.Errorf("R60.13: explanation should mention clamp, got: %s", strategy.Explanation)
	}
}

// TestSelectStrategy_CpuOnlyClamp_EnvOverride — R60.13: env var
// CPPWORKER_CPU_ONLY_MAX_NCTX=32768 clamps to 32K instead of 65K.
func TestSelectStrategy_CpuOnlyClamp_EnvOverride(t *testing.T) {
	os.Setenv("CPPWORKER_CPU_ONLY_MAX_NCTX", "32768")
	defer os.Unsetenv("CPPWORKER_CPU_ONLY_MAX_NCTX")

	env := mkEnvBig()
	meta := mkQwen3Meta()
	cfg := &cppbackend.Config{}

	strategy := SelectStrategy(env, "test-qwen3-4b", meta, 131072, -1, cfg)

	if strategy.Stage != "cpu_only" {
		t.Fatalf("expected cpu_only, got %q", strategy.Stage)
	}
	if strategy.NCtx > 32768 {
		t.Errorf("env override: cpu_only n_ctx = %d, want <= 32768", strategy.NCtx)
	}
	if !contains(strategy.Explanation, "cpu_only_clamped_to=32768") {
		t.Errorf("explanation should mention override clamp, got: %s", strategy.Explanation)
	}
}

// TestSelectStrategy_CpuOnlyClamp_Disabled — R60.13: env var set to
// 0 disables clamp (legacy behavior). Use with caution.
func TestSelectStrategy_CpuOnlyClamp_Disabled(t *testing.T) {
	os.Setenv("CPPWORKER_CPU_ONLY_MAX_NCTX", "0")
	defer os.Unsetenv("CPPWORKER_CPU_ONLY_MAX_NCTX")

	env := mkEnvBig()
	meta := mkQwen3Meta()
	cfg := &cppbackend.Config{}

	strategy := SelectStrategy(env, "test-qwen3-4b", meta, 131072, -1, cfg)

	if strategy.Stage != "cpu_only" {
		t.Fatalf("expected cpu_only, got %q", strategy.Stage)
	}
	// 0 means disabled — n_ctx should match theoretical max.
	if strategy.NCtx == 65536 {
		t.Errorf("R60.13: env=0 should disable clamp, but n_ctx = %d (looks like default clamp)",
			strategy.NCtx)
	}
}

// TestSelectStrategy_CpuOnlyClamp_NonMoE_NotAffected — R60.13: clamp
// only applies to cpu_only path. exact_fit / partial_offload are
// unaffected.
func TestSelectStrategy_CpuOnlyClamp_NonMoE_NotAffected(t *testing.T) {
	os.Unsetenv("CPPWORKER_CPU_ONLY_MAX_NCTX")
	// Tiny model + small n_ctx that fits easily in VRAM.
	env := mkEnvBig()
	meta := cppbackend.GGUFModelMeta{
		Architecture: "llama", // non-MoE
		NLayers:      32,
		NEmbd:        2048,
		NHeads:       32,
		NKvHeads:     32,
		SizeBytes:    1 * 1024 * 1024 * 1024, // 1GB
	}
	cfg := &cppbackend.Config{}

	// 4K n_ctx + 1GB model = 1GB + 1GB KV = 2GB, fits in 8GB VRAM.
	strategy := SelectStrategy(env, "test-llama-1b", meta, 4096, -1, cfg)

	// Should NOT be cpu_only.
	if strategy.Stage == "cpu_only" {
		t.Errorf("1B model on 8GB VRAM with 4K n_ctx should not go cpu_only, got %q",
			strategy.Stage)
	}
	// n_ctx should be 4096 (not clamped).
	if strategy.NCtx != 4096 {
		t.Errorf("expected n_ctx=4096 (not clamped), got %d", strategy.NCtx)
	}
}

// TestSelectStrategy_CpuOnlyClamp_EnvInvalid — R60.13: invalid env
// value (e.g. "abc") falls back to default 65536 (not panic, not 0).
func TestSelectStrategy_CpuOnlyClamp_EnvInvalid(t *testing.T) {
	os.Setenv("CPPWORKER_CPU_ONLY_MAX_NCTX", "not-a-number")
	defer os.Unsetenv("CPPWORKER_CPU_ONLY_MAX_NCTX")

	env := mkEnvBig()
	meta := mkQwen3Meta()
	cfg := &cppbackend.Config{}

	// Should not panic, should use default 65536.
	strategy := SelectStrategy(env, "test-qwen3-4b", meta, 131072, -1, cfg)
	if strategy.Stage != "cpu_only" {
		t.Fatalf("expected cpu_only, got %q", strategy.Stage)
	}
	if strategy.NCtx > 65536 {
		t.Errorf("invalid env should fall back to default 65536, got n_ctx=%d",
			strategy.NCtx)
	}
}

// mkEnvBig — 8GB VRAM + 16GB RAM profile (RTX 3070).
func mkEnvBig() *EnvironmentProfile {
	return &EnvironmentProfile{
		TotalVRAM: 8 * 1024 * 1024 * 1024, // 8GB
		FreeVRAM:  7 * 1024 * 1024 * 1024, // 7GB available (1GB used by OS)
		TotalRAM:  32 * 1024 * 1024 * 1024,
		FreeRAM:   16 * 1024 * 1024 * 1024, // 16GB free
		HasCUDA:   true,
		GPUCount:  1,
	}
}

// mkQwen3Meta — 5GB Qwen3-4B model (typical cpu_only case).
func mkQwen3Meta() cppbackend.GGUFModelMeta {
	return cppbackend.GGUFModelMeta{
		Architecture: "qwen3", // dense (not MoE)
		NLayers:      36,
		NEmbd:        2560,
		NHeads:       20,
		NKvHeads:     8, // GQA
		SizeBytes:    5 * 1024 * 1024 * 1024, // 5GB
	}
}
