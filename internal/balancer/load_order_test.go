//go:build llama_stub

// load_order_test.go — Round 27 follow-up (v0.5.15): order of checks в
// ensureModelLoadedOnBackend. Disabled-profile проверка должна быть РАНЬШЕ
// circuit breaker, иначе breaker открывается на repeated requests к disabled
// модели (gemma-4 upstream GGML_ASSERT → cppworker crash → restart → balancer
// auto-loads again → crash loop).
//
// До фикса:
//   1. ensureModelLoadedOnBackend → check breaker (3 failures = open)
//   2. → executeLlamaCppLoad → check disabled
//   3. → breaker.recordFailure() на каждом disabled-refusal
// После фикса:
//   1. ensureModelLoadedOnBackend → check disabled (BEFORE breaker)
//   2. → return clean 503
//   3. breaker НЕ трогается, потому что запрос отвергнут ДО breaker check

package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestEnsureModelLoaded_DisabledProfile_BypassesBreaker — disabled-модель
// возвращает disabled-ошибку (НЕ breaker-ошибку) даже если breaker открыт
// от прошлых failures. И breaker НЕ инкрементируется от disabled-refusal.
func TestEnsureModelLoaded_DisabledProfile_BypassesBreaker(t *testing.T) {
	var loadCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models", "/v1/models":
			// Пустой список — модель не загружена → триггерит auto-load path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
		case "/api/models/load", "/api/models/load-with-params":
			atomic.AddInt32(&loadCount, 1)
			// cppworker никогда не должен получать load для disabled модели
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"unexpected load for disabled model"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	mm := NewModelManager(nil)
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"gemma-4-E4B-it-Q4_K_M": {
					ContextLength: 2048,
					NumGPULayers:  -1,
					Disabled:      true,
					Notes:         "upstream GGML_ASSERT",
				},
			},
		},
		backends:     map[string]*BackendState{},
		metricsMgr:   NewMetricsManager(),
		modelManager: mm,
	}
	mm.proxy = p
	p.backends["test-bk"] = &BackendState{
		Backend: &types.Backend{
			ID:            "test-bk",
			Host:          host,
			Status:        types.StatusHealthy,
			Type:          types.BackendTypeLlamaCpp,
			CppWorkerPort: port,
		},
	}
	lr := NewLlamaCppRouter(p)

	// Несколько вызовов подряд — все должны получить disabled-error (НЕ breaker-error)
	for i := 0; i < 5; i++ {
		ok, err := lr.ensureModelLoadedOnBackend("test-bk", "gemma-4-E4B-it-Q4_K_M")
		if ok {
			t.Errorf("call %d: expected ok=false for disabled model", i)
		}
		if err == nil {
			t.Errorf("call %d: expected non-nil error", i)
			continue
		}
		if !strings.Contains(err.Error(), "marked as disabled") {
			t.Errorf("call %d: expected disabled error, got: %v", i, err)
		}
		if strings.Contains(err.Error(), "circuit breaker") {
			t.Errorf("call %d: BUG — disabled model returned breaker error before disabled check: %v", i, err)
		}
	}

	// КРИТИЧНО: cppworker НЕ должен получать /api/models/load запросы
	if got := atomic.LoadInt32(&loadCount); got > 0 {
		t.Errorf("expected 0 load requests to cppworker for disabled model, got %d", got)
	}
}

// TestEnsureModelLoaded_DisabledProfile_DoesNotIncrementBreaker —
// breaker для disabled-модели остаётся в initial state (no failures recorded).
func TestEnsureModelLoaded_DisabledProfile_DoesNotIncrementBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	mm := NewModelManager(nil)
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"gemma-4-broken": {Disabled: true, Notes: "test"},
			},
		},
		backends:     map[string]*BackendState{},
		metricsMgr:   NewMetricsManager(),
		modelManager: mm,
	}
	mm.proxy = p
	p.backends["test-bk"] = &BackendState{
		Backend: &types.Backend{
			ID:            "test-bk",
			Host:          host,
			Status:        types.StatusHealthy,
			Type:          types.BackendTypeLlamaCpp,
			CppWorkerPort: port,
		},
	}
	lr := NewLlamaCppRouter(p)

	// 10 disabled-refusals — ни один не должен инкрементировать breaker
	for i := 0; i < 10; i++ {
		_, _ = lr.ensureModelLoadedOnBackend("test-bk", "gemma-4-broken")
	}

	// Breaker для (test-bk, gemma-4-broken) должен быть в initial state
	if skip, reason, _ := lr.loadBackoff.shouldSkip("test-bk", "gemma-4-broken"); skip {
		t.Errorf("breaker should NOT be open for disabled-model refusals, got skip=true reason=%q", reason)
	}
}

// TestEnsureModelLoaded_EnabledModel_StillUsesBreaker — sanity check: для
// НЕ disabled моделей breaker продолжает работать как раньше.
func TestEnsureModelLoaded_EnabledModel_StillUsesBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models", "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
		case "/api/models/load", "/api/models/load-with-params":
			// Эмулируем upstream crash
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"upstream GGML_ASSERT"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.URL)
	mm := NewModelManager(nil)
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"some-broken-model": {
					// НЕ disabled — breaker должен работать
					ContextLength: 8192,
					NumGPULayers:  -1,
					Disabled:      false,
				},
			},
		},
		backends:     map[string]*BackendState{},
		metricsMgr:   NewMetricsManager(),
		modelManager: mm,
	}
	mm.proxy = p
	p.backends["test-bk"] = &BackendState{
		Backend: &types.Backend{
			ID:            "test-bk",
			Host:          host,
			Status:        types.StatusHealthy,
			Type:          types.BackendTypeLlamaCpp,
			CppWorkerPort: port,
		},
	}
	lr := NewLlamaCppRouter(p)

	// 4 попытки с upstream error → breaker должен открыться после 3-й
	var lastErr error
	for i := 0; i < 4; i++ {
		ok, err := lr.ensureModelLoadedOnBackend("test-bk", "some-broken-model")
		lastErr = err
		_ = ok
	}
	if lastErr == nil {
		t.Fatal("expected non-nil error after repeated failures")
	}
	if !strings.Contains(lastErr.Error(), "circuit breaker") &&
		!strings.Contains(lastErr.Error(), "auto-load failed") {
		t.Errorf("expected breaker/auto-load error after repeated failures, got: %v", lastErr)
	}
}
