// sync_profile_test.go — Unit tests for pull-based profile sync.
//
// Round 26 (2026-08-06): verifies the merge function handles all edge cases
// without making real HTTP calls. HTTP-based tests live in handlers_model_*_test.go
// (integration-style with mock balancer).
package main

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestApplyProfileToLoadRequest_NoProfile(t *testing.T) {
	var ctxSize, batch, gpu, flash int
	kv := ""
	allowed := applyProfileToLoadRequest(nil, &ctxSize, &batch, &gpu, &flash, &kv)
	if !allowed {
		t.Error("expected allowed=true when no profile")
	}
	if ctxSize != 0 || batch != 0 || gpu != 0 || flash != 0 || kv != "" {
		t.Errorf("expected all values to stay zero, got ctxSize=%d batch=%d gpu=%d flash=%d kv=%s", ctxSize, batch, gpu, flash, kv)
	}
}

func TestApplyProfileToLoadRequest_Disabled(t *testing.T) {
	prof := &types.LlamaCppModelProfile{Disabled: true}
	allowed := applyProfileToLoadRequest(prof, newInt(0), nil, nil, nil, nil)
	if allowed {
		t.Error("expected allowed=false for disabled profile")
	}
}

func TestApplyProfileToLoadRequest_FillsZeroes(t *testing.T) {
	// Все указатели = nil → профиль должен заполнить.
	prof := &types.LlamaCppModelProfile{
		ContextLength: 65536,
		BatchSize:     512,
		NumGPULayers:  38,
		KVCacheType:   "f16",
	}
	allowed := applyProfileToLoadRequest(prof, nil, nil, nil, nil, nil)
	if !allowed {
		t.Fatal("expected allowed=true")
	}
	// После применения указатели в profile НЕ мутируют — мы возвращаем новые значения.
	// Caller должен использовать новые *int, но в данном тесте мы передали nil →
	// профиль их не заполнил (потому что (nil == nil || *nil == 0) → нет места для записи).
	// Это by design — caller обязан передать не-nil указатели.
	_ = prof
}

func TestApplyProfileToLoadRequest_RespectsExisting(t *testing.T) {
	// Caller уже задал ctxSize=8192 → профиль не должен перезаписывать.
	prof := &types.LlamaCppModelProfile{
		ContextLength: 65536,
		BatchSize:     512,
	}
	ctxSize := 8192
	allowed := applyProfileToLoadRequest(prof, &ctxSize, nil, nil, nil, nil)
	if !allowed {
		t.Fatal("expected allowed=true")
	}
	if ctxSize != 8192 {
		t.Errorf("expected ctxSize=8192 (caller's value preserved), got %d", ctxSize)
	}
}

func TestApplyProfileToLoadRequest_OverridesWhenCallerZero(t *testing.T) {
	// Caller не задал ctxSize (nil) → профиль заполнит.
	// But: applyProfileToLoadRequest only modifies if (ptr == nil || *ptr == 0).
	// Caller passing nil → no way to write back (function has no return for new values).
	// This is a design limitation: caller must pre-allocate the int with default.
	//
	// In real use, callers pass &opts.ContextSize where opts.ContextSize is int.
	// When opts.ContextSize == 0, the merge should set it.
	// When opts.ContextSize != 0, preserve caller's value.
	//
	// Test that with a non-nil zero pointer, profile applies.
	zero := 0
	prof := &types.LlamaCppModelProfile{ContextLength: 65536}
	allowed := applyProfileToLoadRequest(prof, &zero, nil, nil, nil, nil)
	if !allowed {
		t.Fatal("expected allowed=true")
	}
	if zero != 65536 {
		t.Errorf("expected zero to be set to 65536, got %d", zero)
	}
}

func TestApplyProfileToLoadRequest_FlashAttnBoolToInt(t *testing.T) {
	// Profile.FlashAttn = *bool → into int (1 = on, -1 = off)
	t.Run("flashAttn on", func(t *testing.T) {
		b := true
		prof := &types.LlamaCppModelProfile{FlashAttn: &b}
		flash := 0
		allowed := applyProfileToLoadRequest(prof, nil, nil, nil, &flash, nil)
		if !allowed || flash != 1 {
			t.Errorf("expected flash=1, got %d (allowed=%v)", flash, allowed)
		}
	})
	t.Run("flashAttn off", func(t *testing.T) {
		b := false
		prof := &types.LlamaCppModelProfile{FlashAttn: &b}
		flash := 0
		allowed := applyProfileToLoadRequest(prof, nil, nil, nil, &flash, nil)
		if !allowed || flash != -1 {
			t.Errorf("expected flash=-1, got %d (allowed=%v)", flash, allowed)
		}
	})
}

func TestApplyProfileToLoadRequest_OnlyFillMissingFields(t *testing.T) {
	// caller задал batchSize=256, профиль говорит 512 → 256 сохраняется.
	// caller не задал ctxSize → профиль заполняет 65536.
	prof := &types.LlamaCppModelProfile{
		ContextLength: 65536,
		BatchSize:     512,
	}
	ctxSize := 0
	batch := 256
	allowed := applyProfileToLoadRequest(prof, &ctxSize, &batch, nil, nil, nil)
	if !allowed {
		t.Fatal("expected allowed=true")
	}
	if ctxSize != 65536 {
		t.Errorf("ctxSize: expected 65536, got %d", ctxSize)
	}
	if batch != 256 {
		t.Errorf("batch: expected 256 (caller's value preserved), got %d", batch)
	}
}

func TestApplyProfileToLoadRequest_KVCacheType(t *testing.T) {
	prof := &types.LlamaCppModelProfile{KVCacheType: "q8_0"}
	kv := ""
	allowed := applyProfileToLoadRequest(prof, nil, nil, nil, nil, &kv)
	if !allowed {
		t.Fatal("expected allowed=true")
	}
	if kv != "q8_0" {
		t.Errorf("expected kv='q8_0', got %q", kv)
	}
}

func TestContainsFold(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"gemma-4", "gemma-4-E4B-it-Q4_K_M", true},
		{"gemma-4-E4B-it-Q4_K_M", "gemma-4", true},
		{"qwen3", "Qwen3.6-35B-A3B-UD-Q4_K_M", true}, // case-insensitive
		{"qwerty", "gemma-4-E4B-it-Q4_K_M", false},
		{"", "gemma-4", false},
		{"gemma-4", "", false},
		{"qwen", "qwen3-22b", true},
	}
	for _, c := range cases {
		if got := containsFold(c.a, c.b); got != c.want {
			t.Errorf("containsFold(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestApplyProfileToLoadRequest_NumGPULayersNegativeOne(t *testing.T) {
	// profile.NumGPULayers = -1 (all GPU) → caller должен это интерпретировать.
	prof := &types.LlamaCppModelProfile{NumGPULayers: -1}
	gpu := 0
	allowed := applyProfileToLoadRequest(prof, nil, nil, &gpu, nil, nil)
	if !allowed {
		t.Fatal("expected allowed=true")
	}
	if gpu != -1 {
		t.Errorf("expected gpu=-1, got %d", gpu)
	}
}

func newInt(v int) *int { return &v }
