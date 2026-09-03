// backend_state_test.go — unit tests for (*Proxy).isLlamaCppBackend and
// related ApiStyle routing decisions.
//
// R56 (2026-09-03): test-first design for migrating from
// `backend.Type == BackendTypeLlamaCpp` to
// `backend.EffectiveAPIStyle() == APIStyleOpenAICompatible`.
//
// The first two sub-cases are bit-identical to the pre-R56 behavior
// (Type-based fallback matches the R50 default). The third and fourth
// (operator-explicit ApiStyle override) FAIL on the pre-R56 code —
// that is the gap R51.2 left open and R56 closes.
//
// Reference: docs/superpowers/specs/2026-09-03-routing-apistyle-migration.md
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestProxy_IsLlamaCppBackend_UsesApiStyle — TDD: validates that the
// routing helper honors the explicit ApiStyle field on Backend.
//
// R56 (2026-09-03): pre-R56 implementation only checks backend.Type,
// so the third and fourth sub-cases fail. Post-R56 they pass.
func TestProxy_IsLlamaCppBackend_UsesApiStyle(t *testing.T) {
	p := &Proxy{} // isLlamaCppBackend is a pure function — no setup needed

	cases := []struct {
		name         string
		backend      *types.Backend
		wantLlamaCpp bool
	}{
		{
			// R50 default: Type=llama_cpp with no explicit ApiStyle →
			// EffectiveAPIStyle() returns APIStyleOpenAICompatible.
			name:         "Type=llama_cpp, ApiStyle empty (R50 default)",
			backend:      &types.Backend{Type: types.BackendTypeLlamaCpp},
			wantLlamaCpp: true,
		},
		{
			// R50 default: Type=ollama with no explicit ApiStyle →
			// EffectiveAPIStyle() returns APIStyleOllamaNative.
			name:         "Type=ollama, ApiStyle empty (R50 default)",
			backend:      &types.Backend{Type: types.BackendTypeOllama},
			wantLlamaCpp: false,
		},
		{
			// R56 NEW: operator overrides ApiStyle=ollama-native on a
			// cppworker backend (legitimate config: cppworker speaking
			// native Ollama API). Pre-R56 this returns true (wrong).
			// Post-R56 it returns false (correct — goes through Ollama path).
			name:         "Type=llama_cpp, ApiStyle=ollama-native (operator override)",
			backend:      &types.Backend{Type: types.BackendTypeLlamaCpp, ApiStyle: types.APIStyleOllamaNative},
			wantLlamaCpp: false,
		},
		{
			// R56 NEW: operator overrides ApiStyle=openai-compatible on an
			// Ollama backend (e.g. Ollama with OpenAI-compat shim).
			// Pre-R56 this returns false. Post-R56 it returns true.
			name:         "Type=ollama, ApiStyle=openai-compatible (operator override)",
			backend:      &types.Backend{Type: types.BackendTypeOllama, ApiStyle: types.APIStyleOpenAICompatible},
			wantLlamaCpp: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := p.isLlamaCppBackend(c.backend)
			if got != c.wantLlamaCpp {
				t.Errorf("isLlamaCppBackend() = %v, want %v (backend: %+v)",
					got, c.wantLlamaCpp, c.backend)
			}
		})
	}
}

// TestModelManager_IsLlamaCppBackend_UsesApiStyle — same coverage for
// the (*ModelManager) receiver. Same 4 sub-cases.
func TestModelManager_IsLlamaCppBackend_UsesApiStyle(t *testing.T) {
	// ModelManager.isLlamaCppBackend uses ResolveEngine(Engine, Type),
	// which itself only consults Type. This is a separate migration —
	// for R56 we keep the ModelManager behavior unchanged (call sites
	// in model_management.go don't yet use EffectiveAPIStyle).
	//
	// The test below documents the CURRENT behavior. The ModelManager
	// migration is a follow-up (R56.1) because it affects
	// model load/unload dispatch and needs its own design doc.
	mm := &ModelManager{}

	cases := []struct {
		name         string
		backend      *types.Backend
		wantLlamaCpp bool
	}{
		{
			name:         "Type=llama_cpp, Engine empty (current behavior)",
			backend:      &types.Backend{Type: types.BackendTypeLlamaCpp},
			wantLlamaCpp: true,
		},
		{
			name:         "Type=ollama, Engine empty (current behavior)",
			backend:      &types.Backend{Type: types.BackendTypeOllama},
			wantLlamaCpp: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mm.isLlamaCppBackend(c.backend)
			if got != c.wantLlamaCpp {
				t.Errorf("ModelManager.isLlamaCppBackend() = %v, want %v (backend: %+v)",
					got, c.wantLlamaCpp, c.backend)
			}
		})
	}
}
