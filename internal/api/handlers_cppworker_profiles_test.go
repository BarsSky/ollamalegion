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