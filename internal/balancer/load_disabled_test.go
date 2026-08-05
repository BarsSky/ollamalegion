//go:build llama_stub

// load_disabled_test.go — Round 27 follow-up: disabled profile refuses auto-load.
//
// Background: gemma-4 with upstream llama.cpp GGML_ASSERT n_inputs<GGML_SCHED_MAX_SPLIT_INPUTS
// on any n_ctx >= 8192 (verified live 2026-08-05). Without the disabled flag, the
// balancer auto-loads gemma-4 → cppworker SIGABRT → Docker restart → balancer
// auto-loads again → infinite crash loop. Adding "disabled: true" to the profile
// makes executeLlamaCppLoad return an error immediately, breaking the loop.

package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestExecuteLlamaCppLoad_DisabledProfile_Refused — модель с profile.disabled=true
// не должна вызывать POST /api/models/load на cppworker (иначе crash loop).
func TestExecuteLlamaCppLoad_DisabledProfile_Refused(t *testing.T) {
	var loadCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/load" || r.URL.Path == "/api/models/load-with-params" {
			atomic.AddInt32(&loadCount, 1)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "loading"})
	}))
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)

	// Создаём proxy с отключенным профилем для модели.
	mm := NewModelManager(nil)
	mm.proxy = &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"gemma-4-E4B-it-Q4_K_M": {
					ContextLength: 2048,
					NumGPULayers:  -1,
					Disabled:      true,
					Notes:         "upstream GGML_ASSERT, see CHANGELOG v0.5.14",
				},
			},
		},
	}

	result := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load",
		ModelName: "gemma-4-E4B-it-Q4_K_M",
	})
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Success {
		t.Errorf("expected Success=false for disabled profile, got true. Error: %s", result.Error)
	}
	if result.Error == "" {
		t.Errorf("expected non-empty Error message for disabled profile")
	}
	// КРИТИЧНО: load НЕ должен был быть вызван на cppworker (иначе crash).
	if got := atomic.LoadInt32(&loadCount); got > 0 {
		t.Errorf("expected 0 POST to /api/models/load (disabled model), got %d", got)
	}
}

// TestExecuteLlamaCppLoad_EnabledProfile_LoadsNormally — sanity check:
// enabled профиль (Disabled=false) проходит через нормальный load path.
func TestExecuteLlamaCppLoad_EnabledProfile_LoadsNormally(t *testing.T) {
	var loadCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/load", "/api/models/load-with-params":
			atomic.AddInt32(&loadCount, 1)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "loaded",
				"model":  map[string]interface{}{"name": "Qwen3-Instruct-2507-q4km", "state": "loaded"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)

	mm := NewModelManager(nil)
	mm.proxy = &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"Qwen3-Instruct-2507-q4km": {
					ContextLength: 32768,
					NumGPULayers:  -1,
					Disabled:      false, // explicit
				},
			},
		},
	}

	result := mm.executeLlamaCppLoad(host, port, "test-backend", ModelOpRequest{
		Operation: "load",
		ModelName: "Qwen3-Instruct-2507-q4km",
	})
	if !result.Success {
		t.Errorf("expected Success=true for enabled profile, got false. Error: %s", result.Error)
	}
	if got := atomic.LoadInt32(&loadCount); got == 0 {
		t.Errorf("expected at least 1 POST to /api/models/load (enabled model), got 0")
	}
}
