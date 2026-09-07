// preflight_nctx_async_test.go — tests for Round 31 #2 async preflight reload.
//
// Async mode (PreflightAsyncReload=true): при decision=PreflightReload balancer
// НЕ блокирует на reload, а сразу возвращает PreflightAsyncReload decision,
// который в preflight_helper.go превращается в HTTP 503 + Retry-After.
// Reload доделывается в фоне, клиент (Cline/OpenWebUI) retry-ит через указанное
// время и получает уже готовую модель с правильным n_ctx.
package balancer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestPreflightAsyncReload_RunPreflightReturnsAsyncDecision — coordinator
// возвращает PreflightAsyncReload (не PreflightReload) при async-режиме.
func TestPreflightAsyncReload_RunPreflightReturnsAsyncDecision(t *testing.T) {
	var reloadCalls int32
	reloadStarted := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/reload" {
			atomic.AddInt32(&reloadCalls, 1)
			select {
			case reloadStarted <- struct{}{}:
			default:
			}
			time.Sleep(50 * time.Millisecond) // имитация долгого reload
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()

	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	coord.SetConfig(NCtxReloadConfig{
		AutoReloadNCtx:              true,
		AutoReloadVRAMSafetyFactor:  0.85,
		PreflightEnabled:            true,
		PreflightAsyncReload:        true,
		PreflightAsyncRetryAfterSec: 7,
	})

	meta := &RequestMeta{
		EstimatedPromptTokens: 50000,
		RequestedNPredict:     1000,
		ModelName:             "gemma-4-E4B-it-Q4_K_M",
		HasTools:              false,
	}
	state := &NCtxBackendState{
		BackendID:       "test-backend",
		CurrentNCtx:     8192,
		MaxVRAMNCtx:     65536,
		ModelMaxContext: 131072,
	}

	loader := &DefaultNCtxReloadHTTPClient{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		APIToken:   "test",
		HeaderName: "X-API-Token",
	}

	ctx := context.Background()
	t0 := time.Now()
	res, err := coord.RunPreflight(ctx, "test-backend", backend.URL, meta, state, loader)
	elapsed := time.Since(t0)

	if err != nil {
		t.Fatalf("RunPreflight error: %v", err)
	}
	if res.Decision != PreflightAsyncReload {
		t.Errorf("expected PreflightAsyncReload, got %v", res.Decision)
	}
	// Async mode должен вернуть СРАЗУ, не дожидаясь reload (50ms в нашем mock)
	if elapsed > 20*time.Millisecond {
		t.Errorf("RunPreflight blocked for %v — should return immediately in async mode", elapsed)
	}
	// TargetNCtx = min(required=51000, MaxVRAMNCtx=65536, ModelMaxContext=131072) = 51000
	// Но DecidePreflight может также учитывать safety factor (0.85 * MaxVRAMNCtx = 55705.6)
	// и round up до 65536. Главное — он > CurrentNCtx (8192) И <= ModelMaxContext (131072).
	if res.TargetNCtx <= 8192 {
		t.Errorf("TargetNCtx should be > current 8192, got %d", res.TargetNCtx)
	}
	if res.TargetNCtx > 131072 {
		t.Errorf("TargetNCtx should be <= model_max 131072, got %d", res.TargetNCtx)
	}
	t.Logf("TargetNCtx=%d (required=51000, max_vram=65536, model_max=131072)", res.TargetNCtx)

	// Подождём чтобы goroutine запустила reload
	select {
	case <-reloadStarted:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("reload was not started within 2s")
	}

	// Подождём завершения goroutine
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(&reloadCalls); got != 1 {
		t.Errorf("expected 1 reload call, got %d", got)
	}
}

// TestPreflightAsyncReload_SyncModeStillBlocks — backward compat:
// PreflightAsyncReload=false → sync reload (старое поведение).
func TestPreflightAsyncReload_SyncModeStillBlocks(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/reload" {
			time.Sleep(50 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()

	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	coord.SetConfig(NCtxReloadConfig{
		AutoReloadNCtx:             true,
		AutoReloadVRAMSafetyFactor: 0.85,
		PreflightEnabled:           true,
		PreflightAsyncReload:       false, // SYNC mode (default)
	})

	meta := &RequestMeta{
		EstimatedPromptTokens: 50000,
		RequestedNPredict:     1000,
		ModelName:             "gemma-4",
	}
	state := &NCtxBackendState{
		BackendID:       "test-sync",
		CurrentNCtx:     8192,
		MaxVRAMNCtx:     65536,
		ModelMaxContext: 131072,
	}
	loader := &DefaultNCtxReloadHTTPClient{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		APIToken:   "test",
		HeaderName: "X-API-Token",
	}

	t0 := time.Now()
	res, err := coord.RunPreflight(context.Background(), "test-sync", backend.URL, meta, state, loader)
	elapsed := time.Since(t0)

	if err != nil {
		t.Fatalf("RunPreflight: %v", err)
	}
	if res.Decision != PreflightReload {
		t.Errorf("expected PreflightReload (sync), got %v", res.Decision)
	}
	// Sync mode ДОЛЖЕН блокировать на reload
	if elapsed < 40*time.Millisecond {
		t.Errorf("sync mode returned too fast (%v) — should block on reload", elapsed)
	}
}

// TestNCtxReloadConfig_DefaultAsyncValues — defaults должны быть backward compat.
func TestNCtxReloadConfig_DefaultAsyncValues(t *testing.T) {
	cfg := DefaultNCtxReloadConfig()
	if cfg.PreflightAsyncReload {
		t.Error("default PreflightAsyncReload should be false (backward compat)")
	}
	if cfg.PreflightAsyncRetryAfterSec != 5 {
		t.Errorf("default PreflightAsyncRetryAfterSec should be 5, got %d", cfg.PreflightAsyncRetryAfterSec)
	}
}

// TestNCtxReloadConfig_EffectiveAsyncRetryAfter — clamp в [2, PreflightAsyncRetryAfterMaxSec].
//
// R60.6 (2026-09-07): max clamp 30 → 120s (default). 30s было слишком мало
// для realistic hardware (5GB+131072 n_ctx load = 60-180s на RTX 3070 8GB).
func TestNCtxReloadConfig_EffectiveAsyncRetryAfter(t *testing.T) {
	tests := []struct {
		in             int
		maxOverride    int
		want           int
	}{
		{0, 0, 5},    // default
		{-1, 0, 5},   // default
		{1, 0, 2},    // clamp min
		{2, 0, 2},    // min
		{5, 0, 5},    // normal
		{30, 0, 30},  // old max
		{50, 0, 50},  // R60.6: under new max=120, no clamp
		{120, 0, 120}, // R60.6: new max (default)
		{200, 0, 120}, // R60.6: clamp at new max
		{50, 30, 30},  // R60.6: operator override max=30 (backward compat)
		{200, 200, 200}, // R60.6: operator override max=200 (custom)
	}
	for _, tt := range tests {
		cfg := NCtxReloadConfig{
			PreflightAsyncRetryAfterSec:    tt.in,
			PreflightAsyncRetryAfterMaxSec: tt.maxOverride,
		}
		if got := cfg.effectiveAsyncRetryAfter(); got != tt.want {
			t.Errorf("effectiveAsyncRetryAfter(in=%d, maxOverride=%d) = %d, want %d",
				tt.in, tt.maxOverride, got, tt.want)
		}
	}
}

// R60.6 (2026-09-07): TestEstimateReloadTimeMs — verify formula matches
// cppworker's estimateLoadTimeMs (cmd/cppworker/handlers_model_async.go:110).
// Formula: max(1000, size_MB / 100 + ctx/4K*500 + 2000) ms.
// Critical: 5GB model + 131072 n_ctx должно дать ~60-90s (достаточно для
// realistic 8GB GPU hardware без cascading retries).
func TestEstimateReloadTimeMs(t *testing.T) {
	tests := []struct {
		name        string
		modelSize   int64
		targetNCtx  int
		minExpected int64 // ms
		maxExpected int64 // ms
	}{
		{"empty", 0, 0, 0, 0},  // unknown → 0
		{"size_only_5GB", 5 * 1024 * 1024 * 1024, 0, 47000, 55000},  // 5GB/100MB/s + 2000ms overhead
		{"size_only_2GB", 2 * 1024 * 1024 * 1024, 0, 22000, 24000},
		{"ctx_only_131K", 0, 131072, 14000, 20000},                  // 131072/4K*500 + 2000 + 1000 (baseMin)
		{"ctx_only_32K", 0, 32768, 4000, 7000},
		{"qwen3_5GB_131K", 5 * 1024 * 1024 * 1024, 131072, 62000, 90000},  // realistic: 60-90s
		{"qwen3_5GB_32K", 5 * 1024 * 1024 * 1024, 32768, 51000, 60000},
		{"tiny_1MB_4K", 1024 * 1024, 4096, 1000, 4000},  // baseMin
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateReloadTimeMs(tt.modelSize, tt.targetNCtx)
			if tt.minExpected == 0 && tt.maxExpected == 0 {
				if got != 0 {
					t.Errorf("expected 0 (unknown), got %d", got)
				}
				return
			}
			if got < tt.minExpected || got > tt.maxExpected {
				t.Errorf("EstimateReloadTimeMs(size=%d, ctx=%d) = %d, want [%d, %d]",
					tt.modelSize, tt.targetNCtx, got, tt.minExpected, tt.maxExpected)
			}
		})
	}
}
