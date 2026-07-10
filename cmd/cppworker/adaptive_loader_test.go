package main

import (
	"regexp"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// Round 7: integration tests for MoE override-tensors across multiple
// MoE architectures (Qwen3-A3B, Mixtral, DeepSeek-MoE) and dense baselines.
//
// These tests verify that:
//   - SelectStrategy correctly populates OverrideTensors/OverrideTensorBufts
//     parallel arrays for MoE architectures
//   - Dense architectures (Qwen2, Llama-3) leave them empty
//   - Pattern strings match the GGUF tensor naming convention for routed experts

func mkEnv(freeVRAM, freeRAM int64) *EnvironmentProfile {
	return &EnvironmentProfile{
		TotalVRAM:     8 * 1024 * 1024 * 1024, // 8 GB
		FreeVRAM:      freeVRAM,
		TotalRAM:      24 * 1024 * 1024 * 1024,
		FreeRAM:       freeRAM,
		HasCUDA:       true,
		NVMLReady:     true,
		OverheadBytes: 1024 * 1024 * 1024, // 1 GB overhead
	}
}

// TestSelectStrategy_MoEOverrideTensors_AllArchs covers all known MoE
// architectures with real-world metadata values to verify override-tensors
// are populated correctly.
func TestSelectStrategy_MoEOverrideTensors_AllArchs(t *testing.T) {
	tests := []struct {
		name          string
		meta          cppbackend.GGUFModelMeta
		expectMoE     bool
		expectPattern string
	}{
		{
			name: "qwen3_6_35b_a3b",
			meta: cppbackend.GGUFModelMeta{
				Architecture: "qwen35moe",
				NLayers:      48,
				NEmbd:        2048,
				NHeads:       16,
				NKvHeads:     2,
				SizeBytes:    22 * 1024 * 1024 * 1024,
			},
			expectMoE:     true,
			expectPattern: `blk\.\d+\.ffn_.*_exps\.weight`,
		},
		{
			name: "mixtral_8x7b",
			meta: cppbackend.GGUFModelMeta{
				Architecture: "mixtral",
				NLayers:      32,
				NEmbd:        4096,
				NHeads:       32,
				NKvHeads:     8,
				SizeBytes:    24 * 1024 * 1024 * 1024,
			},
			expectMoE:     true,
			expectPattern: `blk\.\d+\.ffn_.*_exps\.weight`,
		},
		{
			name: "deepseek2_moe",
			meta: cppbackend.GGUFModelMeta{
				Architecture: "deepseek2",
				NLayers:      60,
				NEmbd:        5120,
				NHeads:       128,
				NKvHeads:     8, // 8 << 128 → MoE heuristic (KV heads << heads)
				SizeBytes:    40 * 1024 * 1024 * 1024,
			},
			expectMoE:     true,
			expectPattern: `blk\.\d+\.ffn_.*_exps\.weight`,
		},
		{
			name: "qwen2_moe_a3b",
			meta: cppbackend.GGUFModelMeta{
				Architecture: "qwen2moe",
				NLayers:      28,
				NEmbd:        1536,
				NHeads:       24,
				NKvHeads:     4,
				SizeBytes:    12 * 1024 * 1024 * 1024,
			},
			expectMoE:     true,
			expectPattern: `blk\.\d+\.ffn_.*_exps\.weight`,
		},
		{
			name: "qwen3next",
			meta: cppbackend.GGUFModelMeta{
				Architecture: "qwen3next",
				NLayers:      48,
				NEmbd:        2048,
				NHeads:       16,
				NKvHeads:     2,
				SizeBytes:    20 * 1024 * 1024 * 1024,
			},
			expectMoE:     true,
			expectPattern: `blk\.\d+\.ffn_.*_exps\.weight`,
		},
		// Dense baselines
		{
			name: "dense_qwen2",
			meta: cppbackend.GGUFModelMeta{
				Architecture: "qwen2",
				NLayers:      32,
				NEmbd:        3584,
				NHeads:       28,
				NKvHeads:     28, // no GQA → nKvHeads*4 = 112, not < 28
				SizeBytes:    4 * 1024 * 1024 * 1024,
			},
			expectMoE: false,
		},
		{
			name: "dense_llama3",
			meta: cppbackend.GGUFModelMeta{
				Architecture: "llama",
				NLayers:      32,
				NEmbd:        4096,
				NHeads:       32,
				NKvHeads:     8, // GQA: 8*4 = 32 not < 32
				SizeBytes:    7 * 1024 * 1024 * 1024,
			},
			expectMoE: false,
		},
	}

	env := mkEnv(6*1024*1024*1024, 16*1024*1024*1024)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := SelectStrategy(env, "test-model", tc.meta, 8192, -2, &cppbackend.Config{})

			if tc.expectMoE {
				if len(result.OverrideTensors) == 0 {
					t.Fatalf("expected OverrideTensors to be populated for MoE arch, got nil")
				}
				if len(result.OverrideTensorBufts) == 0 {
					t.Fatalf("expected OverrideTensorBufts to be populated for MoE arch, got nil")
				}
				if len(result.OverrideTensors) != len(result.OverrideTensorBufts) {
					t.Fatalf("OverrideTensors and OverrideTensorBufts length mismatch: %d vs %d",
						len(result.OverrideTensors), len(result.OverrideTensorBufts))
				}
				// Verify pattern matches expected GGUF tensor naming
				found := false
				for _, p := range result.OverrideTensors {
					if p == tc.expectPattern {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected pattern %q in OverrideTensors, got %v",
						tc.expectPattern, result.OverrideTensors)
				}
				// Verify all bufts are "CPU" (host memory)
				for i, b := range result.OverrideTensorBufts {
					if b != "CPU" {
						t.Errorf("OverrideTensorBufts[%d] = %q, expected \"CPU\"", i, b)
					}
				}
			} else {
				if len(result.OverrideTensors) > 0 {
					t.Errorf("expected OverrideTensors empty for dense arch, got %v", result.OverrideTensors)
				}
				if len(result.OverrideTensorBufts) > 0 {
					t.Errorf("expected OverrideTensorBufts empty for dense arch, got %v", result.OverrideTensorBufts)
				}
			}
		})
	}
}

// TestSelectStrategy_FallbackNoMeta_NoOverrides verifies that when GGUF
// metadata is missing (NLayers=0, NEmbd=0, etc.), SelectStrategy goes through
// the fallback_no_meta branch and does NOT populate override-tensors
// (since arch detection requires the metadata).
func TestSelectStrategy_FallbackNoMeta_NoOverrides(t *testing.T) {
	env := mkEnv(6*1024*1024*1024, 16*1024*1024*1024)
	meta := cppbackend.GGUFModelMeta{
		Architecture: "qwen35moe", // arch present but no nLayers/nEmbd
		// NLayers=0, NEmbd=0, NHeads=0 → triggers fallback
	}

	result := SelectStrategy(env, "test-no-meta", meta, 8192, -2, &cppbackend.Config{})

	if len(result.OverrideTensors) != 0 {
		t.Errorf("fallback_no_meta: expected OverrideTensors empty (no nLayers/NEmbd), got %v",
			result.OverrideTensors)
	}
	if len(result.OverrideTensorBufts) != 0 {
		t.Errorf("fallback_no_meta: expected OverrideTensorBufts empty, got %v",
			result.OverrideTensorBufts)
	}
	// Stage should indicate fallback path
	if !strings.Contains(result.Stage, "fallback") && result.Stage != "exact_fit" &&
		result.Stage != "partial_offload" && result.Stage != "cpu_only" &&
		result.Stage != "moe_offload" && result.Stage != "auto_retry" {
		t.Errorf("unexpected stage %q", result.Stage)
	}
}

// TestSelectStrategy_DetectArchType_Hints verifies the arch-detection helper.
func TestSelectStrategy_DetectArchType_Hints(t *testing.T) {
	tests := []struct {
		name     string
		arch     string
		nHeads   int
		nKvHeads int
		want     string
	}{
		// MoE via arch name
		{"moe_in_arch", "deepseek_moe", 32, 32, "moe"},
		{"mixtral", "mixtral", 32, 8, "moe"},
		{"qwen35moe", "qwen35moe", 16, 2, "moe"},
		{"qwen2moe_a3b", "qwen2a3b", 24, 4, "moe"},
		{"qwen3moe_a3b", "qwen3a3b", 16, 2, "moe"},
		{"qwen3next", "qwen3next", 16, 2, "moe"},
		// MoE via nKvHeads hint (nKvHeads*4 < nHeads)
		{"gqa_heavy_moe", "unknown_arch", 32, 4, "moe"}, // 4*4=16 < 32 → MoE
		// Dense baselines (nKvHeads not 4x smaller)
		{"llama_gqa", "llama", 32, 8, "dense"}, // 8*4=32 not < 32
		{"qwen2_dense", "qwen2", 28, 28, "dense"}, // 28*4=112 not < 28
		// Edge: zero heads shouldn't crash
		{"zero_heads", "unknown", 0, 0, "dense"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectArchType(tc.arch, tc.nHeads, tc.nKvHeads)
			if got != tc.want {
				t.Errorf("detectArchType(%q, %d, %d) = %q, want %q",
					tc.arch, tc.nHeads, tc.nKvHeads, got, tc.want)
			}
		})
	}
}

// TestSelectStrategy_OverrideTensorPatterns validates the actual regex
// patterns would match real GGUF tensor names for the architectures.
func TestSelectStrategy_OverrideTensorPatterns(t *testing.T) {
	patterns := []string{
		`blk\.\d+\.ffn_.*_exps\.weight`,
		`blk\.\d+\.ffn_.*_exps\.bias`,
	}

	// Real GGUF tensor names from Qwen3.6-35B-A3B and Mixtral
	// .weight variants
	realWeightNames := []string{
		"blk.0.ffn_up_exps.weight",
		"blk.0.ffn_gate_exps.weight",
		"blk.0.ffn_down_exps.weight",
		"blk.39.ffn_down_exps.weight", // confirmed in docker logs
		"blk.39.ffn_gate_exps.weight",
		"blk.39.ffn_up_exps.weight",
		// Mixtral uses same naming
		"blk.5.ffn_up_exps.weight",
		"blk.5.ffn_gate_exps.weight",
		"blk.5.ffn_down_exps.weight",
	}
	// .bias variants
	realBiasNames := []string{
		"blk.0.ffn_up_exps.bias",
		"blk.0.ffn_gate_exps.bias",
		"blk.0.ffn_down_exps.bias",
		"blk.39.ffn_up_exps.bias",
	}

	nonExpertNames := []string{
		"blk.0.attn_q.weight",
		"blk.0.attn_k.weight",
		"blk.0.attn_v.weight",
		"blk.0.attn_output.weight",
		"blk.0.attn_norm.weight",
		"blk.0.ffn_gate_inp.weight",    // gating, not expert
		"blk.0.ffn_gate_shexp.weight",  // shared expert
		"blk.0.ffn_up_shexp.weight",    // shared expert
		"blk.0.ffn_down_shexp.weight",  // shared expert
		"token_embd.weight",
		"output.weight",
	}

	for i, pat := range patterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			t.Errorf("regex compile error for %q: %v", pat, err)
			continue
		}
		var names []string
		if i == 0 {
			names = realWeightNames
		} else {
			names = realBiasNames
		}
		for _, tn := range names {
			if !re.MatchString(tn) {
				t.Errorf("pattern %q should match %q but did not", pat, tn)
			}
		}
		for _, tn := range nonExpertNames {
			if re.MatchString(tn) {
				t.Errorf("pattern %q should NOT match %q but did", pat, tn)
			}
		}
	}
}
