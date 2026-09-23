package balancer

// preflight_profile_hint_r68_test.go — R68 (2026-09-23): profile.contextLength —
// HINT, а не потолок для клиентских запросов.
//
// ЖАЛОБА (R68, реальный клиент Cline): в настройках Cline «Model Context
// Window = 65536», base URL = балансер. Запрос падал с
//   413 {"error":"preflight: prompt + n_predict exceeds n_ctx for this backend",
//        "reason":"requested n_ctx=65536 exceeds model max context=8192"}
// хотя:
//   * GGUF модели держит 131072 (cppworker gguf_max_context),
//   * cppworker сообщал max_vram_n_ctx=66125 (65K помещается),
//   * в логах балансера уже было предупреждение «profile conservative
//     (no auto-adaptation enabled), recommendation: Add contextLengthAuto:true».
//
// ПРИЧИНА: профиль gemma-4-E4B-it-Q4_K_M имеет contextLength=8192 без
// contextLengthAuto → resolveModelMaxContext (Tier 1) возвращал 8192, и preflight
// считал это «model max context», отбивая запрос вместо reload'а. Второй путь
// (transport-level preflightNCtxReload) в той же ситуации честно запускал reload —
// то есть два пути противоречили друг другу.
//
// ФИКС: клиентский потолок = PhysicalMaxContext (min(GGUF, operator cap));
// profile.contextLength участвует только как hint первичной загрузки и
// попадает в текст отказа как profile_hint_n_ctx.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// clineStateR68 — состояние бэкенда как на живом стенде (Cline + gemma-4).
func clineStateR68() *NCtxBackendState {
	return &NCtxBackendState{
		BackendID:          "cppworker-gpu-bundled-agent",
		CurrentNCtx:        8192,
		MaxVRAMNCtx:        66125,
		ModelMaxContext:    8192,   // profile.contextLength (hint)
		PhysicalMaxContext: 131072, // min(gguf 131072, operator cap 131072)
		GGUFMaxContext:     131072,
		ProfileHintNCtx:    8192,
		AutoReloadMaxNCtx:  131072,
	}
}

// TestDecidePreflight_R68_ProfileHintTriggersReload — главный кейс жалобы:
// Cline просит 65536 при profile hint 8192 и физическом потолке 131072 →
// Reload, а НЕ Reject.
func TestDecidePreflight_R68_ProfileHintTriggersReload(t *testing.T) {
	meta := &RequestMeta{
		ModelName:             "gemma-4-E4B-it-Q4_K_M",
		EstimatedPromptTokens: 25507, // Cline: системный промпт + tools
		RequestedNPredict:     0,
		RequestedNCtxOverride: 65536,
		HasTools:              true,
	}
	cfg := NCtxReloadConfig{AutoReloadNCtx: true, PreflightEnabled: true, AutoReloadMaxNCtx: 131072}

	res := DecidePreflight(meta, clineStateR68(), cfg)
	if res.Decision != PreflightReload {
		t.Fatalf("R68: ожидался Reload (модель физически держит 131072, VRAM 66125), получено %v (body=%s)",
			res.Decision, res.RejectBody)
	}
	if res.TargetNCtx != 65536 {
		t.Errorf("TargetNCtx = %d, ожидалось 65536", res.TargetNCtx)
	}
}

// TestDecidePreflight_R68_LegacyStateStillRejects — обратная совместимость:
// если PhysicalMaxContext не заполнен (старые вызовы), потолком остаётся
// ModelMaxContext.
func TestDecidePreflight_R68_LegacyStateStillRejects(t *testing.T) {
	meta := &RequestMeta{
		ModelName:             "gemma-4-E4B-it-Q4_K_M",
		EstimatedPromptTokens: 25507,
		RequestedNCtxOverride: 65536,
	}
	state := clineStateR68()
	state.PhysicalMaxContext = 0 // legacy-вызовы/старые тесты

	res := DecidePreflight(meta, state, NCtxReloadConfig{AutoReloadNCtx: true, PreflightEnabled: true})
	if res.Decision != PreflightReject {
		t.Fatalf("legacy-состояние: ожидался Reject, получено %v", res.Decision)
	}
	if !strings.Contains(res.RejectBody, "exceeds backend context ceiling=8192") {
		t.Errorf("в причине отказа должно быть видно legacy-потолок, получено: %s", res.RejectBody)
	}
}

// TestDecidePreflight_R68_RejectsBeyondPhysicalCeiling — запрос выше реального
// потолка (GGUF/operator cap) отбивается, и тело отказа объясняет, ПОЧЕМУ:
// profile hint, GGUF max, operator cap, физический потолок.
func TestDecidePreflight_R68_RejectsBeyondPhysicalCeiling(t *testing.T) {
	meta := &RequestMeta{
		ModelName:             "gemma-4-E4B-it-Q4_K_M",
		EstimatedPromptTokens: 25507,
		RequestedNCtxOverride: 262144, // больше GGUF max (131072)
	}
	res := DecidePreflight(meta, clineStateR68(), NCtxReloadConfig{AutoReloadNCtx: true, PreflightEnabled: true})
	if res.Decision != PreflightReject {
		t.Fatalf("ожидался Reject для 262144 > физического потолка 131072, получено %v", res.Decision)
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(res.RejectBody), &body); err != nil {
		t.Fatalf("тело отказа не JSON: %v (%s)", err, res.RejectBody)
	}
	checks := map[string]float64{
		"profile_hint_n_ctx":    8192,
		"gguf_max_context":      131072,
		"auto_reload_max_n_ctx": 131072,
		"physical_max_context":  131072,
		"max_vram_n_ctx":        66125,
	}
	for key, want := range checks {
		got, ok := body[key].(float64)
		if !ok {
			t.Errorf("в теле отказа нет поля %q: %s", key, res.RejectBody)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, ожидалось %v", key, got, want)
		}
	}
	if reason, _ := body["reason"].(string); !strings.Contains(reason, "profile hint=8192") {
		t.Errorf("reason должен ссылаться на hint профиля, получено: %s", reason)
	}
	if res.RejectStatus != http.StatusRequestEntityTooLarge {
		t.Errorf("RejectStatus = %d, ожидался 413", res.RejectStatus)
	}
}

// TestResolvePhysicalMaxContext_R68 — min(GGUF, operator cap) с fallback'ами.
func TestResolvePhysicalMaxContext_R68(t *testing.T) {
	cases := []struct {
		name                           string
		ggufMax, ctxMax, autoReloadMax int
		want                           int
	}{
		{"gguf меньше operator cap", 131072, 262144, 0, 131072},
		{"operator cap меньше gguf", 262144, 131072, 0, 131072},
		{"без contextLengthMax — берём autoReloadMax", 262144, 0, 65536, 65536},
		{"только gguf", 131072, 0, 0, 131072},
		{"только operator cap", 0, 32768, 0, 32768},
		{"ничего неизвестно", 0, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePhysicalMaxContext(tc.ggufMax, tc.ctxMax, tc.autoReloadMax); got != tc.want {
				t.Errorf("resolvePhysicalMaxContext(%d, %d, %d) = %d, want %d",
					tc.ggufMax, tc.ctxMax, tc.autoReloadMax, got, tc.want)
			}
		})
	}
}

// TestNctxPreflightWaitTimeout_R68 — парсинг LB_NCTX_PREFLIGHT_WAIT_SEC.
func TestNctxPreflightWaitTimeout_R68(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", 240 * time.Second},
		{"0", 0},
		{"30", 30 * time.Second},
		{"-1", 30 * time.Minute},
		{"garbage", 240 * time.Second},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv("LB_NCTX_PREFLIGHT_WAIT_SEC", tc.env)
			if got := nctxPreflightWaitTimeout(); got != tc.want {
				t.Errorf("nctxPreflightWaitTimeout() = %v, want %v", got, tc.want)
			}
		})
	}
}

// preflightReloadMockR68 — mock cppworker: /api/models/reload отвечает через delay.
func preflightReloadMockR68(t *testing.T, delay time.Duration, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/reload" {
			atomic.AddInt32(calls, 1)
			time.Sleep(delay)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

// TestPreflightAsyncReload_R68_WaitsAndServes — новый контракт: при
// LB_NCTX_PREFLIGHT_WAIT_SEC>0 RunPreflight ДОЖИДАЕТСЯ reload'а и возвращает
// PreflightReload (caller обслуживает первый же запрос), а не AsyncReload.
func TestPreflightAsyncReload_R68_WaitsAndServes(t *testing.T) {
	t.Setenv("LB_NCTX_PREFLIGHT_WAIT_SEC", "5")

	var reloadCalls int32
	const reloadDelay = 150 * time.Millisecond
	backend := preflightReloadMockR68(t, reloadDelay, &reloadCalls)
	defer backend.Close()

	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	coord.SetConfig(NCtxReloadConfig{
		AutoReloadNCtx:             true,
		AutoReloadVRAMSafetyFactor: 0.85,
		PreflightEnabled:           true,
		PreflightAsyncReload:       true, // async-режим, но с ожиданием
		AutoReloadMaxNCtx:          131072,
	})

	meta := &RequestMeta{
		ModelName:             "gemma-4-E4B-it-Q4_K_M",
		EstimatedPromptTokens: 25507,
		RequestedNCtxOverride: 65536,
	}
	loader := &DefaultNCtxReloadHTTPClient{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		APIToken:   "test",
		HeaderName: "X-API-Token",
	}

	t0 := time.Now()
	res, err := coord.RunPreflight(context.Background(), "r68-backend", backend.URL, meta, clineStateR68(), loader)
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("RunPreflight error: %v", err)
	}
	if res.Decision != PreflightReload {
		t.Fatalf("R68: ожидался PreflightReload (дождались и обслуживаем), получено %v", res.Decision)
	}
	if elapsed < reloadDelay {
		t.Errorf("RunPreflight вернулся за %v — reload (%v) не дожидались", elapsed, reloadDelay)
	}
	if res.TargetNCtx != 65536 {
		t.Errorf("TargetNCtx = %d, ожидалось 65536", res.TargetNCtx)
	}
	if got := atomic.LoadInt32(&reloadCalls); got != 1 {
		t.Errorf("reload вызван %d раз, ожидался 1 (дедупликация + одно ожидание)", got)
	}
	if nctx := coord.LastKnownNCtx("r68-backend"); nctx != 65536 {
		t.Errorf("LastKnownNCtx = %d, ожидалось 65536 (обновляется после ожидания)", nctx)
	}
	t.Logf("✅ R68: первый же запрос дождался reload (%v) и обслуживается", elapsed)
}

// TestPreflightAsyncReload_R68_WaitDisabledKeeps503 — LB_NCTX_PREFLIGHT_WAIT_SEC=0
// сохраняет прежнее поведение (сразу AsyncReload → 503/dialog).
func TestPreflightAsyncReload_R68_WaitDisabledKeeps503(t *testing.T) {
	t.Setenv("LB_NCTX_PREFLIGHT_WAIT_SEC", "0")

	var reloadCalls int32
	backend := preflightReloadMockR68(t, 150*time.Millisecond, &reloadCalls)
	defer backend.Close()

	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	coord.SetConfig(NCtxReloadConfig{
		AutoReloadNCtx:             true,
		AutoReloadVRAMSafetyFactor: 0.85,
		PreflightEnabled:           true,
		PreflightAsyncReload:       true,
		AutoReloadMaxNCtx:          131072,
	})

	meta := &RequestMeta{
		ModelName:             "gemma-4-E4B-it-Q4_K_M",
		EstimatedPromptTokens: 25507,
		RequestedNCtxOverride: 65536,
	}
	loader := &DefaultNCtxReloadHTTPClient{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		APIToken:   "test",
		HeaderName: "X-API-Token",
	}

	t0 := time.Now()
	res, err := coord.RunPreflight(context.Background(), "r68-legacy", backend.URL, meta, clineStateR68(), loader)
	elapsed := time.Since(t0)
	if err != nil {
		t.Fatalf("RunPreflight error: %v", err)
	}
	if res.Decision != PreflightAsyncReload {
		t.Fatalf("при выключенном ожидании ожидался AsyncReload, получено %v", res.Decision)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("RunPreflight блокировался %v — в legacy-режиме должен отвечать сразу", elapsed)
	}
}

// TestWaitForBackendNCtx_R68 — ожидание нужного n_ctx у cppworker:
// сначала отдаётся старый n_ctx, потом новый → wait возвращает true;
// если n_ctx не растёт → false (таймаут).
func TestWaitForBackendNCtx_R68(t *testing.T) {
	var nctx atomic.Int64
	nctx.Store(8192)
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Со второго опроса «модель перезагружена» на 65536.
		if polls.Add(1) >= 2 {
			nctx.Store(65536)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"gemma","state":"loaded","context_size":` +
			strconv.FormatInt(nctx.Load(), 10) + `}]}`))
	}))
	defer srv.Close()

	p := admissionTestProxy(t, 4)
	p.mu.RLock()
	st := p.backends["llama_adm"]
	p.mu.RUnlock()
	// Переводим бэкенд на mock-cppworker.
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, portStr, _ := strings.Cut(hostPort, ":")
	port, _ := strconv.Atoi(portStr)
	st.mu.Lock()
	st.Backend.Host = host
	st.Backend.CppWorkerPort = port
	st.mu.Unlock()

	if !p.waitForBackendNCtx(context.Background(), "llama_adm", "gemma", 65536, 5*time.Second) {
		t.Fatal("waitForBackendNCtx не дождался n_ctx=65536 (мок отдаёт его со второго опроса)")
	}
	if p.waitForBackendNCtx(context.Background(), "llama_adm", "gemma", 131072, 300*time.Millisecond) {
		t.Error("waitForBackendNCtx вернул true для недостижимого n_ctx")
	}
}

// TestPreflightNCtxReload_R68_WaitsAndServes — transport-level путь
// (preflightNCtxReloadIfNeeded): при LB_NCTX_PREFLIGHT_WAIT_SEC>0 запрос ждёт
// reload и проксируется (ok=true), а не получает 503 «retry in 30s».
func TestPreflightNCtxReload_R68_WaitsAndServes(t *testing.T) {
	t.Setenv("LB_NCTX_PREFLIGHT_WAIT_SEC", "5")

	const modelName = "test-model"
	cppWorker, _, _ := makeMockCppWorkerWithReload(t, modelName, 8192)
	p := buildProxyWithMockInitialCtx(t, cppWorker.URL, modelName, 8192)

	body := []byte(`{"model":"` + modelName + `","messages":[{"role":"user","content":"опиши проект"}],"options":{"num_ctx":65536,"num_predict":32}}`)

	start := time.Now()
	newBody, ok, msg, status := p.preflightNCtxReloadIfNeeded(nil, "test-backend", modelName, body, "/api/chat")
	elapsed := time.Since(start)

	if !ok || status != http.StatusOK {
		t.Fatalf("R68: ожидалось ok=true/200 (дождались reload и обслуживаем), получено ok=%v status=%d msg=%q",
			ok, status, msg)
	}
	if len(newBody) == 0 {
		t.Error("body запроса потерян после ожидания reload")
	}
	t.Logf("✅ R68 transport-level: reload дождались за %v, запрос проксируется", elapsed)
}

// TestCacheLoadedContextLength_R68 — кэш n_ctx обновляется сразу после reload,
// иначе следующий preflight-чек запустил бы второй reload.
func TestCacheLoadedContextLength_R68(t *testing.T) {
	p := &Proxy{metricsMgr: NewMetricsManager()}
	p.cacheLoadedContextLength("b1", "gemma", 65536)
	if got := p.getLoadedNCtxFromMetrics("b1", "gemma"); got != 65536 {
		t.Fatalf("getLoadedNCtxFromMetrics = %d, ожидалось 65536", got)
	}
	// Обновление существующей записи (без дубликатов).
	p.cacheLoadedContextLength("b1", "gemma", 8192)
	if got := p.getLoadedNCtxFromMetrics("b1", "gemma"); got != 8192 {
		t.Fatalf("после повторного вызова = %d, ожидалось 8192", got)
	}
	p.metricsMgr.mu.RLock()
	n := 0
	if lm := p.metricsMgr.llamaMetrics["b1"]; lm != nil {
		n = len(lm.LoadedModels)
	}
	p.metricsMgr.mu.RUnlock()
	if n != 1 {
		t.Errorf("в кэше %d записей о модели, ожидалась 1", n)
	}
}
