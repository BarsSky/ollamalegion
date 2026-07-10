package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// Ensure Config from pkg/types compiles in test context.
var _ = (*types.LoadBalancerConfig)(nil)

// TestResolveOverrideTensors verifies the priority order of override-tensors
// resolution: explicit request > saved profile > none.
//
// Round 7 (2026-07-09): MoE override-tensors feature wiring.
func TestResolveOverrideTensors(t *testing.T) {
	// Build proxy with a profile carrying override-tensors.
	profileOverride := []string{`blk\.\d+\.ffn_.*_exps\.weight`}
	profileBufts := []string{"CPU"}

	profiles := map[string]types.LlamaCppModelProfile{
		"qwen3-a3b": {
			ContextLength:      16384,
			OverrideTensors:    profileOverride,
			OverrideTensorBufts: profileBufts,
		},
		"gemma-4": {
			ContextLength: 8192, // no override-tensors
		},
	}

	proxy := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: profiles,
		},
	}
	mm := &ModelManager{proxy: proxy}

	tests := []struct {
		name        string
		req         ModelOpRequest
		wantPat     string
		wantBuft    string
		wantEmpty   bool
	}{
		{
			name: "explicit_request_overrides_profile",
			req: ModelOpRequest{
				ModelName: "qwen3-a3b",
				OverrideTensors: []string{`blk\.\d+\.attn_.*\.weight`},
				OverrideTensorBufts: []string{"CUDA0"},
			},
			wantPat:  `blk\.\d+\.attn_.*\.weight`,
			wantBuft: "CUDA0",
		},
		{
			name: "profile_used_when_no_explicit",
			req: ModelOpRequest{
				ModelName: "qwen3-a3b",
			},
			wantPat:  `blk\.\d+\.ffn_.*_exps\.weight`,
			wantBuft: "CPU",
		},
		{
			name: "no_override_when_profile_missing_field",
			req: ModelOpRequest{
				ModelName: "gemma-4",
			},
			wantEmpty: true,
		},
		{
			name: "no_override_when_model_unknown",
			req: ModelOpRequest{
				ModelName: "llama-3-70b",
			},
			wantEmpty: true,
		},
		{
			name: "empty_profile_arrays_means_no_override",
			req: ModelOpRequest{
				ModelName: "gemma-4",
				// No explicit fields → empty profile too → no override.
			},
			wantEmpty: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotPats, gotBufts := mm.resolveOverrideTensors(tc.req)

			if tc.wantEmpty {
				if len(gotPats) > 0 || len(gotBufts) > 0 {
					t.Errorf("expected no override, got pats=%v bufts=%v", gotPats, gotBufts)
				}
				return
			}

			if len(gotPats) != 1 || gotPats[0] != tc.wantPat {
				t.Errorf("pats = %v, want [%s]", gotPats, tc.wantPat)
			}
			if len(gotBufts) != 1 || gotBufts[0] != tc.wantBuft {
				t.Errorf("bufts = %v, want [%s]", gotBufts, tc.wantBuft)
			}
		})
	}
}

// TestResolveOverrideTensors_NilProxy verifies safe handling when proxy is nil.
func TestResolveOverrideTensors_NilProxy(t *testing.T) {
	mm := &ModelManager{proxy: nil}
	pats, bufts := mm.resolveOverrideTensors(ModelOpRequest{ModelName: "any"})
	if len(pats) > 0 || len(bufts) > 0 {
		t.Errorf("nil proxy should return no override, got pats=%v bufts=%v", pats, bufts)
	}
}

// TestResolveOverrideTensors_MismatchedExplicitFallbackToProfile verifies that
// mismatched-length explicit request falls back to profile (not error).
func TestResolveOverrideTensors_MismatchedExplicitFallbackToProfile(t *testing.T) {
	profiles := map[string]types.LlamaCppModelProfile{
		"qwen3-a3b": {
			ContextLength:       16384,
			OverrideTensors:     []string{`blk\.\d+\.ffn_.*_exps\.weight`},
			OverrideTensorBufts: []string{"CPU"},
		},
	}
	proxy := &Proxy{config: &types.LoadBalancerConfig{LlamaCppModelProfiles: profiles}}
	mm := &ModelManager{proxy: proxy}

	// Explicit mismatched (2 pats, 1 buft) — resolveOverrideTensors returns
	// only if len(pats) == len(bufts), so this should fall through to profile.
	req := ModelOpRequest{
		ModelName:          "qwen3-a3b",
		OverrideTensors:    []string{"a", "b"},
		OverrideTensorBufts: []string{"CPU"},
	}
	pats, bufts := mm.resolveOverrideTensors(req)
	if len(pats) != 1 || pats[0] != `blk\.\d+\.ffn_.*_exps\.weight` {
		t.Errorf("expected fallback to profile, got pats=%v", pats)
	}
	if len(bufts) != 1 || bufts[0] != "CPU" {
		t.Errorf("expected fallback to profile bufts, got bufts=%v", bufts)
	}
}
