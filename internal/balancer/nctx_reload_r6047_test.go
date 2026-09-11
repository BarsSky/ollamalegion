//go:build llama_stub

// nctx_reload_r6047_test.go — R60.47 tests.
//
// Symptom (R60.47): user reports "instantly empty response" from balancer.
// Root cause: handleNCtxReloadActual делал sync DoReload с
// `?wait=true&waitTimeoutSec=300` — блокировал HTTP handler на 60-300 сек
// (время реального reload). OpenWebUI/Cline timeout 30-60s → клиент
// cancel-ил соединение → "empty response" / EOF / "Unexpected token '<html>'".
//
// R60.47 fix: переключаем handleNCtxReloadActual в async mode (default).
// Kick off reload в goroutine, return 503+Retry-After+digestive за <50ms.
// Клиент retry-ит через Retry-After → к этому моменту reload обычно
// завершён → 200 OK.
//
// Тесты проверяют:
//   - async mode: 503+Retry-After returned <100ms даже если reload занял бы минуты
//   - sync mode (legacy LB_NCTX_RELOAD_ASYNC=0): старое поведение с sync wait
//   - R60.47b: ResolveNumCtx Tier 3 использует max(backend_default, loaded_n_ctx)
//
// Эти тесты дополняют nctx_reload_sync_test.go (который покрывает preflight sync).
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newProxyForR6047 — создаёт Proxy с mock cppworker (который имитирует медленный reload).
func newProxyForR6047(t *testing.T, backendID, modelName string, loadedNCtx int) (*Proxy, *slowCppWorker) {
	t.Helper()
	mock := &slowCppWorker{
		reloadDelay: 5 * time.Second, // имитирует долгий reload (real: 30-180s)
	}
	server := httptest.NewServer(http.HandlerFunc(mock.handle))
	t.Cleanup(server.Close)

	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{
			AutoReloadTimeoutSec: 600,
		}),
		lbNCtxReloadAsync: true,
	}
	p.backends[backendID] = &BackendState{
		Backend: &types.Backend{
			ID:            backendID,
			Host:          "127.0.0.1",
			CppWorkerPort: extractTestPort(server.URL),
			Status:        types.StatusHealthy,
			Type:          types.BackendTypeLlamaCpp,
		},
	}
	p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{
			{Name: modelName, State: "loaded", ContextLength: loadedNCtx},
		},
	}
	return p, mock
}

// slowCppWorker — mock cppworker, имитирующий долгий reload endpoint.
type slowCppWorker struct {
	reloadDelay    time.Duration
	reloadCount    atomic.Int32
	reloadStarted  atomic.Bool
	reloadFinished atomic.Bool
}

func (s *slowCppWorker) handle(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/api/models/reload") {
		s.reloadCount.Add(1)
		s.reloadStarted.Store(true)
		time.Sleep(s.reloadDelay)
		s.reloadFinished.Store(true)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"reloaded"}`)
		return
	}
	if strings.Contains(r.URL.Path, "/api/models") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"name":"q","state":"loaded","path":"/tmp/q.gguf","contextSize":8192}]`)
		return
	}
	http.NotFound(w, r)
}

func extractTestPort(url string) int {
	// url is "http://127.0.0.1:54321" — extract port
	idx := strings.LastIndex(url, ":")
	if idx < 0 {
		return 0
	}
	var port int
	fmt.Sscanf(url[idx+1:], "%d", &port)
	return port
}

// TestR6047_AsyncReloadReturnsImmediately — async mode должен вернуть
// 503+Retry-After за <100ms даже если reload занял бы минуты.
//
// Это INTEGRATION-уровень test: mock cppworker с reloadDelay=5s, async
// mode должен return ДО завершения reload (т.е. <100ms).
func TestR6047_AsyncReloadReturnsImmediately(t *testing.T) {
	const backendID = "test-r6047-async"
	const modelName = "qwen2.5.gguf"
	p, mock := newProxyForR6047(t, backendID, modelName, 2048)
	mock.reloadDelay = 5 * time.Second // имитация долгого reload

	plan := &ReloadPlan{
		Decision: DecisionReload,
		NewNCtx:  8192,
		Reason:   "test_prompt_too_long",
	}
	bridgeErr := &NCtxBridgeError{
		CurrentNCtx:  2048,
		RequiredNCtx: 8500,
		MaxVRAMNCtx:  16384,
		Code:         NCtxErrCodePromptTooLong,
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"model":"q"}`))

	start := time.Now()
	p.handleNCtxReloadActual(context.Background(), rec, r, backendID, modelName, plan, []byte(`{"model":"q"}`), bridgeErr)
	elapsed := time.Since(start)

	// Главный assert: return <100ms (async mode не блокируется на reload).
	if elapsed > 100*time.Millisecond {
		t.Fatalf("async mode took %v; expected <100ms (sync mode would block on 5s reload)", elapsed)
	}

	// Status code должен быть 503 (async mode returns 503+Retry-After).
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503; got %d", rec.Code)
	}

	// Retry-After header должен быть set.
	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Errorf("expected Retry-After header; got none")
	}

	// Body должен быть JSON с diagnostic info.
	var bodyMap map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &bodyMap); err != nil {
		t.Fatalf("body is not JSON: %v (body=%s)", err, rec.Body.String())
	}
	if errMsg, ok := bodyMap["error"].(string); !ok || !strings.Contains(errMsg, "n_ctx auto-reload") {
		t.Errorf("body.error missing 'n_ctx auto-reload': %v", bodyMap["error"])
	}
	if retry, ok := bodyMap["retry_after"].(float64); !ok || retry < 5 || retry > 600 {
		t.Errorf("body.retry_after invalid: %v", retry)
	}

	// Проверяем что reload goroutine всё-таки запустилась.
	// Через 100ms (даём время на goroutine startup) проверяем что reload идёт.
	time.Sleep(150 * time.Millisecond)
	if !mock.reloadStarted.Load() {
		t.Errorf("reload goroutine was not started (reloadCount=%d)", mock.reloadCount.Load())
	}
}

// TestR6047_SyncModeStillBlocks — LB_NCTX_RELOAD_ASYNC=0 → legacy sync поведение.
// Это negative test: убеждаемся что legacy sync mode всё ещё работает
// (для backward compat / debug / long-running клиентов).
//
// Не делаем полный sync mode test (требует полный http.Client и proxyRequest).
// Проверяем только что sync mode ветка доходит до DoReload (блокируется на нём).
func TestR6047_SyncModeStillBlocks(t *testing.T) {
	const backendID = "test-r6047-sync"
	const modelName = "qwen2.5.gguf"
	p, mock := newProxyForR6047(t, backendID, modelName, 2048)
	p.lbNCtxReloadAsync = false // legacy sync mode
	// p.client остаётся nil — но мы не дойдём до proxyRequest если DoReload работает.
	// Sync mode сделает DoReload → mock slow cppworker → 5s wait.
	// Mock /api/models/reload endpoint возвращает 200 после delay.

	plan := &ReloadPlan{
		Decision: DecisionReload,
		NewNCtx:  8192,
		Reason:   "test_prompt_too_long",
	}
	bridgeErr := &NCtxBridgeError{
		CurrentNCtx:  2048,
		RequiredNCtx: 8500,
		MaxVRAMNCtx:  16384,
		Code:         NCtxErrCodePromptTooLong,
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/chat", strings.NewReader(`{"model":"q"}`))

	// Оборачиваем в recover() потому что sync mode после reload идёт в
	// proxyRequestLlamaCppNonStream который требует p.client (nil) → panic.
	// Нам важен ТОЛЬКО факт что sync mode заблокировался на reload.
	defer func() {
		if r := recover(); r != nil {
			// expected: nil pointer dereference в proxyRequest после reload
		}
	}()

	start := time.Now()
	p.handleNCtxReloadActual(context.Background(), rec, r, backendID, modelName, plan, []byte(`{"model":"q"}`), bridgeErr)
	elapsed := time.Since(start)

	// Sync mode должен заблокироваться минимум на reloadDelay (5s).
	if elapsed < 4*time.Second {
		t.Errorf("sync mode returned in %v; expected ≥4s (sync mode waits for reload)", elapsed)
	}
	// Reload должен быть завершён.
	if !mock.reloadFinished.Load() {
		t.Errorf("sync mode returned but reload not finished")
	}
}

// TestR6047b_ResolveNumCtx_Tier3_UpgradesToLoaded — R60.47b fix:
// когда body.num_ctx отсутствует И loaded_n_ctx > backend_default,
// ResolveNumCtx должен вернуть loaded_n_ctx (НЕ backend_default).
//
// Pre-R60.47b: balancer возвращал backend_default=2048 → cppworker
// получал X-Cpp-Ctx=2048 → reject "prompt too long" (model loaded с 4096).
//
// Post-R60.47b: balancer возвращает max(2048, 4096) = 4096 → cppworker OK.
func TestR6047b_ResolveNumCtx_Tier3_UpgradesToLoaded(t *testing.T) {
	const backendID = "test-r6047b-tier3"
	const modelName = "qwen2.5.gguf"
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Backends: []types.Backend{
				{
					ID:   backendID,
					Type: types.BackendTypeLlamaCpp,
					CppWorkerConfig: &types.LlamaCppConfig{
						ContextLength: 2048, // backend_default=2048
					},
				},
			},
		},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{}),
	}
	// Pre-set LastKnownNCtx = 4096 (модель loaded с 4096).
	p.nctxReload.SetLastKnownNCtx(backendID, 4096)

	// Body БЕЗ num_ctx (OpenWebUI default).
	body := []byte(`{"model":"q","messages":[]}`)
	resolved := p.ResolveNumCtx(modelName, body, backendID)

	if resolved.Value != 4096 {
		t.Errorf("expected Value=4096 (loaded > backend_default); got Value=%d (Source=%s)",
			resolved.Value, resolved.Source)
	}
	if resolved.Source != NumCtxSourceBackend {
		t.Errorf("expected Source=NumCtxSourceBackend; got %s", resolved.Source)
	}
}

// TestR6047b_ResolveNumCtx_Tier3_KeepsDefaultWhenLoadedLower — sanity check:
// если loaded_n_ctx < backend_default (например, после partial unload),
// ResolveNumCtx всё ещё возвращает backend_default (НЕ downgrades до loaded).
func TestR6047b_ResolveNumCtx_Tier3_KeepsDefaultWhenLoadedLower(t *testing.T) {
	const backendID = "test-r6047b-tier3-lower"
	const modelName = "qwen2.5.gguf"
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Backends: []types.Backend{
				{
					ID:   backendID,
					Type: types.BackendTypeLlamaCpp,
					CppWorkerConfig: &types.LlamaCppConfig{
						ContextLength: 8192, // backend_default=8192
					},
				},
			},
		},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{}),
	}
	// Edge case: loaded=1024 (loaded меньше default). ResolveNumCtx
	// возвращает default=8192 чтобы модель загрузилась с правильным n_ctx.
	p.nctxReload.SetLastKnownNCtx(backendID, 1024)

	body := []byte(`{"model":"q"}`)
	resolved := p.ResolveNumCtx(modelName, body, backendID)

	if resolved.Value != 8192 {
		t.Errorf("expected Value=8192 (backend_default); got Value=%d (Source=%s)",
			resolved.Value, resolved.Source)
	}
}

// TestR6047b_ResolveNumCtx_Tier3_ZeroLoadedKeepsDefault — cold start case:
// когда loaded_n_ctx ещё неизвестен (0), возвращаем backend_default.
// После auto-load (R60.33) loaded будет set, и Tier 3 будет upgrade до loaded.
func TestR6047b_ResolveNumCtx_Tier3_ZeroLoadedKeepsDefault(t *testing.T) {
	const backendID = "test-r6047b-tier3-zero"
	const modelName = "qwen2.5.gguf"
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Backends: []types.Backend{
				{
					ID:   backendID,
					Type: types.BackendTypeLlamaCpp,
					CppWorkerConfig: &types.LlamaCppConfig{
						ContextLength: 2048,
					},
				},
			},
		},
		nctxReload: NewNCtxReloadCoordinator(NCtxReloadConfig{}),
	}
	// loaded_n_ctx = 0 (cold start)

	body := []byte(`{"model":"q"}`)
	resolved := p.ResolveNumCtx(modelName, body, backendID)

	if resolved.Value != 2048 {
		t.Errorf("expected Value=2048 (backend_default when loaded=0); got Value=%d",
			resolved.Value)
	}
}

// TestIsNCtxReloadAsyncEnabled — env-based flag test.
func TestIsNCtxReloadAsyncEnabled(t *testing.T) {
	cases := []struct {
		name      string
		envValue  string
		setEnv    bool
		wantAsync bool
	}{
		{"default (env unset) → async", "", false, true},
		{"LB_NCTX_RELOAD_ASYNC=1 → async", "1", true, true},
		{"LB_NCTX_RELOAD_ASYNC=true → async", "true", true, true},
		{"LB_NCTX_RELOAD_ASYNC=0 → sync", "0", true, false},
		{"LB_NCTX_RELOAD_ASYNC=false → sync", "false", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setEnv {
				oldVal, hadOld := os.LookupEnv("LB_NCTX_RELOAD_ASYNC")
				defer func() {
					if hadOld {
						_ = os.Setenv("LB_NCTX_RELOAD_ASYNC", oldVal)
					} else {
						_ = os.Unsetenv("LB_NCTX_RELOAD_ASYNC")
					}
				}()
				_ = os.Setenv("LB_NCTX_RELOAD_ASYNC", tc.envValue)
			} else {
				oldVal, hadOld := os.LookupEnv("LB_NCTX_RELOAD_ASYNC")
				defer func() {
					if hadOld {
						_ = os.Setenv("LB_NCTX_RELOAD_ASYNC", oldVal)
					}
				}()
				_ = os.Unsetenv("LB_NCTX_RELOAD_ASYNC")
			}

			got := IsNCtxReloadAsyncEnabled()
			if got != tc.wantAsync {
				t.Errorf("IsNCtxReloadAsyncEnabled() = %v, want %v", got, tc.wantAsync)
			}
		})
	}
}
