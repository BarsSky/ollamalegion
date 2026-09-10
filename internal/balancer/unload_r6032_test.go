//go:build llama_stub

// unload_r6032_test.go — R60.32 test: state invalidation on unload.
//
// Symptom (R60.31 bug): R60.31 stickiness check (loaded_n_ctx покрывает
// required → NoOp) опирается на state.CurrentNCtx. Но после unload
// через balancer state.CurrentNCtx не обновляется (stale) —
// NCtxReloadCoordinator.lastKnownNCtx остаётся прежним. R60.31
// думает "модель загружена с n_ctx=X", не триггерит reload →
// пользователь получает 502 от cppworker (модель не загружена).
//
// R60.32 fix: executeLlamaCppUnload после успешного cppworker
// ответа вызывает:
//   - mm.proxy.GetNCtxReloadCoordinator().SetLastKnownNCtx(backendID, 0)
//   - mm.proxy.GetMetricsManager().UpdateLlamaCppModelUnloaded(backendID, model)
// Это инвалидирует state так что R60.31 stickiness знает что
// модель реально НЕ загружена, и следующий запрос триггерит reload.

package balancer

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestExecuteLlamaCppUnload_InvalidatesState — после unload
// state.CurrentNCtx должен быть 0, чтобы preflight (R60.31)
// не считал loaded покрывает required.
func TestExecuteLlamaCppUnload_InvalidatesState(t *testing.T) {
	var unloadCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/unload" && r.URL.Query().Get("name") == "test-model" {
			atomic.AddInt32(&unloadCount, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"unloaded"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)

	// Set up proxy with NCtxReloadCoordinator + MetricsManager
	// и предварительно "loaded" модель с n_ctx=4096 (state=stale)
	mm := NewModelManager(nil)
	coord := NewNCtxReloadCoordinator(NCtxReloadConfig{})
	mm.proxy = &Proxy{
		config:    &types.LoadBalancerConfig{},
		metricsMgr: NewMetricsManager(),
		nctxReload: coord,
	}
	// Pretend model was loaded with n_ctx=4096 (typical pre-unload state)
	coord.SetLastKnownNCtx("test-backend", 4096)
	if got := coord.LastKnownNCtx("test-backend"); got != 4096 {
		t.Fatalf("setup: lastKnownNCtx=%d, want 4096", got)
	}

	// Unload the model
	result := mm.executeLlamaCppUnload(host, port, "test-backend", ModelOpRequest{
		Operation: "unload",
		ModelName: "test-model",
	})
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.Success {
		t.Fatalf("expected Success=true, got false. Error: %s", result.Error)
	}
	if atomic.LoadInt32(&unloadCount) == 0 {
		t.Fatal("expected unload call to cppworker")
	}

	// R60.32 fix: state should be invalidated (CurrentNCtx=0)
	// so R60.31 stickiness knows model is NOT loaded.
	got := coord.LastKnownNCtx("test-backend")
	if got != 0 {
		t.Errorf("after unload: lastKnownNCtx=%d, want 0 (R60.32 invalidation fix)", got)
	}
}
