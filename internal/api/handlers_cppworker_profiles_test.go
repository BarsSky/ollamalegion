package api

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Session 16 (2026-06-27): Per-Model Profiles — Parallel + KVCacheType
// ============================================================
//
// Тесты для валидации и merge новых полей Per-Model Profile:
//   - Parallel ∈ [0, 8]  (cppworker не поддерживает > 8 параллельных слотов)
//   - KVCacheType ∈ {"", "f16", "q8_0", "q4_0"}
//
// Также проверяем mergeModelProfile с partial updates для новых полей:
//   - если в update Parallel=0 (zero-value), то existing.Parallel сохраняется
//   - если в update KVCacheType="", то existing.KVCacheType сохраняется

// TestValidateModelProfile_ParallelBounds — Parallel ∈ [0, 8].
func TestValidateModelProfile_ParallelBounds(t *testing.T) {
	tests := []struct {
		name     string
		parallel int
		wantErr  bool
	}{
		{"parallel=0 (default = no batching)", 0, false},
		{"parallel=1 (single slot)", 1, false},
		{"parallel=4 (typical OpenWebUI)", 4, false},
		{"parallel=8 (max allowed)", 8, false},
		{"parallel=9 (too high)", 9, true},
		{"parallel=100 (way too high)", 100, true},
		{"parallel=-1 (negative)", -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := types.LlamaCppModelProfile{
				ContextLength: 8192,
				Parallel:      tt.parallel,
			}
			err := validateModelProfile(p)
			if tt.wantErr && err == nil {
				t.Errorf("validateModelProfile(parallel=%d): expected error, got nil", tt.parallel)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateModelProfile(parallel=%d): unexpected error: %v", tt.parallel, err)
			}
		})
	}
}

// TestValidateModelProfile_KVCacheTypeValid — KVCacheType ∈ {"", "f16", "q8_0", "q4_0"}.
func TestValidateModelProfile_KVCacheTypeValid(t *testing.T) {
	tests := []struct {
		name       string
		kvCacheType string
		wantErr    bool
	}{
		{"empty (default F16)", "", false},
		{"f16 (default precision)", "f16", false},
		{"q8_0 (-50% VRAM)", "q8_0", false},
		{"q4_0 (-75% VRAM)", "q4_0", false},
		{"invalid: fp8", "fp8", true},
		{"invalid: Q8_0 (uppercase)", "Q8_0", true},
		{"invalid: garbage", "garbage", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := types.LlamaCppModelProfile{
				ContextLength: 8192,
				KVCacheType:   tt.kvCacheType,
			}
			err := validateModelProfile(p)
			if tt.wantErr && err == nil {
				t.Errorf("validateModelProfile(kvCacheType=%q): expected error, got nil", tt.kvCacheType)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validateModelProfile(kvCacheType=%q): unexpected error: %v", tt.kvCacheType, err)
			}
		})
	}
}

// TestMergeModelProfile_PartialUpdate_Parallel — partial update of Parallel.
// Если update.Parallel=0, existing.Parallel должен сохраниться (PATCH semantics).
func TestMergeModelProfile_PartialUpdate_Parallel(t *testing.T) {
	existing := types.LlamaCppModelProfile{
		ContextLength: 8192,
		Parallel:      4,
		KVCacheType:   "q8_0",
	}

	// partial update: только Notes, Parallel=0 (zero-value) — не должен перезаписывать.
	update := types.LlamaCppModelProfile{
		ContextLength: 16384, // меняем n_ctx
		Parallel:      0,      // zero-value — не трогаем existing
		Notes:         "extended context for long prompts",
	}
	out := mergeModelProfile(existing, update)

	if out.ContextLength != 16384 {
		t.Errorf("ContextLength: expected 16384, got %d", out.ContextLength)
	}
	if out.Parallel != 4 {
		t.Errorf("Parallel: expected 4 (preserved from existing), got %d", out.Parallel)
	}
	if out.KVCacheType != "q8_0" {
		t.Errorf("KVCacheType: expected 'q8_0' (preserved from existing), got %q", out.KVCacheType)
	}
	if out.Notes != "extended context for long prompts" {
		t.Errorf("Notes: expected updated, got %q", out.Notes)
	}
}

// TestMergeModelProfile_PartialUpdate_KVCacheType — partial update of KVCacheType.
// Если update.KVCacheType="", existing.KVCacheType должен сохраниться.
func TestMergeModelProfile_PartialUpdate_KVCacheType(t *testing.T) {
	existing := types.LlamaCppModelProfile{
		ContextLength: 8192,
		Parallel:      2,
		KVCacheType:   "q4_0",
	}

	// partial update: меняем Parallel на 8, KVCacheType="" (не трогать).
	update := types.LlamaCppModelProfile{
		ContextLength: 0, // zero-value — не трогаем
		Parallel:      8,
		KVCacheType:   "", // zero-value — не трогаем existing
	}
	out := mergeModelProfile(existing, update)

	if out.ContextLength != 8192 {
		t.Errorf("ContextLength: expected 8192 (preserved), got %d", out.ContextLength)
	}
	if out.Parallel != 8 {
		t.Errorf("Parallel: expected 8 (updated), got %d", out.Parallel)
	}
	if out.KVCacheType != "q4_0" {
		t.Errorf("KVCacheType: expected 'q4_0' (preserved), got %q", out.KVCacheType)
	}
}

// Round 7 (2026-07-09): tests for override-tensors validation and merge.
func TestValidateModelProfile_OverrideTensors(t *testing.T) {
	tests := []struct {
		name      string
		profile   types.LlamaCppModelProfile
		wantError bool
	}{
		{
			name: "valid_qwen3_a3b_cpu",
			profile: types.LlamaCppModelProfile{
				ContextLength:       16384,
				OverrideTensors:     []string{`blk\.\d+\.ffn_.*_exps\.weight`},
				OverrideTensorBufts: []string{"CPU"},
			},
			wantError: false,
		},
		{
			name: "valid_multiple",
			profile: types.LlamaCppModelProfile{
				ContextLength: 16384,
				OverrideTensors: []string{
					`blk\.\d+\.ffn_.*_exps\.weight`,
					`blk\.\d+\.ffn_.*_exps\.bias`,
				},
				OverrideTensorBufts: []string{"CPU", "CUDA0"},
			},
			wantError: false,
		},
		{
			name: "empty_arrays_allowed",
			profile: types.LlamaCppModelProfile{
				ContextLength: 8192,
			},
			wantError: false,
		},
		{
			name: "mismatched_length_rejected",
			profile: types.LlamaCppModelProfile{
				ContextLength:       16384,
				OverrideTensors:     []string{"a", "b"},
				OverrideTensorBufts: []string{"CPU"}, // only 1
			},
			wantError: true,
		},
		{
			name: "invalid_buft_value_rejected",
			profile: types.LlamaCppModelProfile{
				ContextLength:       16384,
				OverrideTensors:     []string{"x"},
				OverrideTensorBufts: []string{"DISK0"}, // not CPU or CUDA<n>
			},
			wantError: true,
		},
		{
			name: "invalid_buft_in_array_rejected",
			profile: types.LlamaCppModelProfile{
				ContextLength:       16384,
				OverrideTensors:     []string{"a", "b"},
				OverrideTensorBufts: []string{"CPU", "RAM0"},
			},
			wantError: true,
		},
		{
			name: "one_side_only_rejected",
			profile: types.LlamaCppModelProfile{
				ContextLength:   16384,
				OverrideTensors: []string{"a"}, // bufts missing → length 0 vs 1 → mismatch
			},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateModelProfile(tt.profile)
			if tt.wantError && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestMergeModelProfile_PartialUpdate_OverrideTensors verifies:
//   - existing overrides preserved when update.OverrideTensors == nil
//   - explicit empty slice clears them
//   - non-nil matching arrays replace them
//   - mismatched-length update is rejected (existing preserved)
func TestMergeModelProfile_PartialUpdate_OverrideTensors(t *testing.T) {
	t.Run("nil_preserves_existing", func(t *testing.T) {
		existing := types.LlamaCppModelProfile{
			ContextLength:       16384,
			OverrideTensors:     []string{`blk\.\d+\.ffn_.*_exps\.weight`},
			OverrideTensorBufts: []string{"CPU"},
		}
		update := types.LlamaCppModelProfile{
			ContextLength: 0,
			// OverrideTensors nil → keep existing
		}
		out := mergeModelProfile(existing, update)
		if len(out.OverrideTensors) != 1 {
			t.Errorf("expected existing OverrideTensors preserved, got %v", out.OverrideTensors)
		}
		if len(out.OverrideTensorBufts) != 1 || out.OverrideTensorBufts[0] != "CPU" {
			t.Errorf("expected existing OverrideTensorBufts preserved, got %v", out.OverrideTensorBufts)
		}
	})

	t.Run("non_nil_replaces", func(t *testing.T) {
		existing := types.LlamaCppModelProfile{
			ContextLength: 16384,
			// Empty existing.
		}
		update := types.LlamaCppModelProfile{
			OverrideTensors:     []string{`blk\.\d+\.ffn_.*_exps\.weight`},
			OverrideTensorBufts: []string{"CUDA0"},
		}
		out := mergeModelProfile(existing, update)
		if len(out.OverrideTensors) != 1 || out.OverrideTensors[0] != `blk\.\d+\.ffn_.*_exps\.weight` {
			t.Errorf("OverrideTensors not replaced: %v", out.OverrideTensors)
		}
		if len(out.OverrideTensorBufts) != 1 || out.OverrideTensorBufts[0] != "CUDA0" {
			t.Errorf("OverrideTensorBufts not replaced: %v", out.OverrideTensorBufts)
		}
	})

	t.Run("mismatched_length_keeps_existing", func(t *testing.T) {
		existing := types.LlamaCppModelProfile{
			ContextLength:       16384,
			OverrideTensors:     []string{`blk\.\d+\.ffn_.*_exps\.weight`},
			OverrideTensorBufts: []string{"CPU"},
		}
		update := types.LlamaCppModelProfile{
			OverrideTensors:     []string{"a", "b"},
			OverrideTensorBufts: []string{"CPU"}, // mismatch
		}
		out := mergeModelProfile(existing, update)
		// Existing should be preserved.
		if len(out.OverrideTensors) != 1 || out.OverrideTensors[0] != `blk\.\d+\.ffn_.*_exps\.weight` {
			t.Errorf("mismatch should preserve existing, got %v", out.OverrideTensors)
		}
	})

	t.Run("explicit_clear", func(t *testing.T) {
		existing := types.LlamaCppModelProfile{
			ContextLength:       16384,
			OverrideTensors:     []string{`blk\.\d+\.ffn_.*_exps\.weight`},
			OverrideTensorBufts: []string{"CPU"},
		}
		update := types.LlamaCppModelProfile{
			OverrideTensors:     []string{}, // explicit empty
			OverrideTensorBufts: []string{},
		}
		out := mergeModelProfile(existing, update)
		// Both arrays now empty.
		if len(out.OverrideTensors) != 0 {
			t.Errorf("explicit empty should clear, got %v", out.OverrideTensors)
		}
		if len(out.OverrideTensorBufts) != 0 {
			t.Errorf("explicit empty should clear, got %v", out.OverrideTensorBufts)
		}
	})
}