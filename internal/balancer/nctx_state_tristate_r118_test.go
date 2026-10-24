//go:build llama_stub

// nctx_state_tristate_r118_test.go — R118 (2026-10-14).
//
// ЖИВОЕ ВОСПРОИЗВЕДЕНИЕ (стенд, cppworker gpu-rpc86-r116c, balancer r118-persist4).
// Оператор запустил загрузку gemma-4-E4B-it-Q4_K_M с ctx=32768; пока она шла
// (state=loading, context_size=0), пришёл клиентский запрос с num_ctx=8192:
//
//	22:02:49.455 cppworker: loading model ctxSize=32768 gpuLayers=-1
//	22:02:51.978 balancer:  detected n_ctx mismatch, scheduling async reload
//	                        loaded_n_ctx=0 requested_n_ctx=8192
//	                        reload_target_total=8192 loaded_n_ctx_is_zero=true
//	22:02:51.980 cppworker: loading model ctxSize=8192 gpuLayers=-2
//	                        "reloading model because parameters changed"
//	                        model unloaded            ← подготовленная модель выгружена
//	22:03:40     cppworker: runAsyncLoad: background load failed:
//	                        load cancelled: model load aborted by user (71 c впустую)
//	22:03:11     balancer → клиенту: 503 «model is being reloaded to n_ctx=8192
//	                        (loaded=0); retry in 30s»
//
// ПРИЧИНА: `loaded_n_ctx=0` означало ЧЕТЫРЕ разных состояния сразу — «модели нет»,
// «модель грузится», «опрос бэкенда не прошёл» и «кэш ещё не наполнен». В каждом
// из них preflight планировал загрузку, то есть по недостоверным данным прерывал
// уже идущую загрузку и терял окно, выставленное оператором.
//
// ФИКС: resolveLoadedWindowNCtx разделяет состояния; «грузится» → ждём (второй
// загрузки нет), «неизвестно» → разрушительных решений нет (запрос идёт как есть).
package balancer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// newProxyWithCustomCppWorker — прокси с ПУСТЫМ кэшем метрик и подставным
// cppworker, чей ответ на /api/models задаёт тест.
//
// Пустой кэш — это ровно то состояние, в котором балансер принимал решение в
// живом воспроизведении: поллер опрашивает бэкенды раз в 30 c, а загрузка
// началась секундой раньше.
func newProxyWithCustomCppWorker(t *testing.T, backendID string, handler http.HandlerFunc) *Proxy {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())

	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends[backendID] = &BackendState{
		Backend: &types.Backend{
			ID:            backendID,
			Host:          u.Hostname(),
			CppWorkerPort: port,
			Status:        types.StatusHealthy,
			Type:          types.BackendTypeLlamaCpp,
		},
	}
	p.nctxReload = NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	p.client = &http.Client{}
	p.llamaCppMetricsPoller = newLlamaCppMetricsPoller(p)
	return p
}

// TestR118_LoadingModel_SecondLoadNotStarted — модель ГРУЗИТСЯ (state=loading,
// окна ещё нет). Балансер обязан ждать эту загрузку, а не запускать свою: вторая
// загрузка выгружает первую («reloading model because parameters changed»), и
// оператор теряет подготовленное окно (живой лог в шапке файла).
func TestR118_LoadingModel_SecondLoadNotStarted(t *testing.T) {
	const backendID = "load-cpp-1"
	const model = "gemma-4-E4B-it-Q4_K_M"

	p := newProxyWithCustomCppWorker(t, backendID, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Ровно то, что отдавал cppworker во время живой загрузки:
		// state=loading, context_size=0.
		_, _ = fmt.Fprintf(w, `{"count":1,"models":[{"name":%q,"state":"loading",`+
			`"context_size":0,"context_per_seq":0,"max_slots":1}]}`, model)
	})

	body := []byte(`{"model":"` + model + `","options":{"num_ctx":8192}}`)
	_, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		context.Background(), backendID, model, body, "/api/chat")

	if p.nctxReload.IsReloadPending(backendID, model) {
		t.Fatal("балансер запустил ВТОРУЮ загрузку, пока модель уже грузится — " +
			"это и есть живой дефект: cppworker выгружает первую загрузку " +
			"(«model load aborted by user») и подготовленное окно теряется")
	}
	if needsProxy {
		t.Fatalf("во время загрузки запрос нельзя проксировать как готовый: needsProxy=true, status=%d", status)
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (сигнал «жди текущую загрузку»)", status)
	}
	if msg == "" {
		t.Error("пустое сообщение: вызывающий по нему решает ждать, а не отдавать 503 молча")
	}
}

// TestR118_UnknownBackendState_NoDestructiveReload — состояние бэкенда узнать не
// удалось (опрос /api/models не прошёл). По недостоверным данным выгружать
// подготовленную модель нельзя: запрос идёт как есть, reload не планируется.
func TestR118_UnknownBackendState_NoDestructiveReload(t *testing.T) {
	const backendID = "down-cpp-1"
	const model = "Qwen3.8-27B-UD-Q4_K_M"

	p := newProxyWithCustomCppWorker(t, backendID, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "cppworker busy", http.StatusInternalServerError)
	})

	body := []byte(`{"model":"` + model + `","options":{"num_ctx":8192}}`)
	_, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		context.Background(), backendID, model, body, "/api/chat")

	if p.nctxReload.IsReloadPending(backendID, model) {
		t.Fatal("reload запланирован по недостоверным данным (опрос /api/models не прошёл) — " +
			"так балансер выгружал уже загруженную модель")
	}
	if !needsProxy || status != http.StatusOK {
		t.Fatalf("ожидался обычный проксинг (200): needsProxy=%v status=%d msg=%q",
			needsProxy, status, msg)
	}
}

// TestR118_LoadedWindowFromCache_States — таблица состояний кэша метрик.
// Ключевое различие: «записи о модели нет» (состояние неизвестно) и «запись есть
// без окна» (модель грузится либо загрузка упала) — это разные решения.
func TestR118_LoadedWindowFromCache_States(t *testing.T) {
	const backendID = "state-cpp-1"
	const model = "m"

	cases := []struct {
		name       string
		models     []types.LlamaCppModel
		wantNCtx   int
		wantState  loadedWindowState
		skipModels bool
	}{
		{
			name:       "пустой кэш (поллер ещё не проходил) — неизвестно",
			skipModels: true,
			wantState:  loadedWindowUnknown,
		},
		{
			name:      "список пуст, но опрос прошёл — это «absent» решает resolver, не кэш",
			models:    nil,
			wantState: loadedWindowUnknown,
		},
		{
			name:      "state=loading без окна — грузится",
			models:    []types.LlamaCppModel{{Name: model, State: "loading", ContextLength: 0}},
			wantState: loadedWindowLoading,
		},
		{
			name:      "state=error без окна — загрузка нужна",
			models:    []types.LlamaCppModel{{Name: model, State: "error", ContextLength: 0}},
			wantState: loadedWindowAbsent,
		},
		{
			name:      "окно известно — загружена",
			models:    []types.LlamaCppModel{{Name: model, State: "loaded", ContextLength: 32768}},
			wantNCtx:  32768,
			wantState: loadedWindowLoaded,
		},
		{
			name: "две записи, одна с окном — берём окно",
			models: []types.LlamaCppModel{
				{Name: model, State: "loading", ContextLength: 0},
				{Name: model, State: "loaded", ContextLength: 16384},
			},
			wantNCtx:  16384,
			wantState: loadedWindowLoaded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{metricsMgr: NewMetricsManager()}
			if !tc.skipModels {
				p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{LoadedModels: tc.models}
			}
			n, st := p.loadedWindowFromCache(backendID, model)
			if st != tc.wantState {
				t.Errorf("state = %s, want %s", st, tc.wantState)
			}
			if n != tc.wantNCtx {
				t.Errorf("n_ctx = %d, want %d", n, tc.wantNCtx)
			}
		})
	}
}

// TestR118_ResolveLoadedWindow_AbsentAfterSuccessfulPoll — «нет записи» после
// УСПЕШНОГО опроса означает «модели на бэкенде нет» (загрузка нужна), а не
// «неизвестно». Иначе балансер вообще перестал бы грузить модели.
func TestR118_ResolveLoadedWindow_AbsentAfterSuccessfulPoll(t *testing.T) {
	const backendID = "absent-cpp-1"
	const model = "m"

	p := newProxyWithCustomCppWorker(t, backendID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
	})

	n, st := p.resolveLoadedWindowNCtx(backendID, model, true)
	if st != loadedWindowAbsent {
		t.Fatalf("state = %s, want absent (опрос прошёл, модели нет)", st)
	}
	if n != 0 {
		t.Errorf("n_ctx = %d, want 0", n)
	}
}

// TestR118_ResolveLoadedWindow_LoadingAfterPoll — тот же путь, но бэкенд отдаёт
// state=loading: решение «ждать», а не «грузить».
func TestR118_ResolveLoadedWindow_LoadingAfterPoll(t *testing.T) {
	const backendID = "loading-cpp-1"
	const model = "m"

	p := newProxyWithCustomCppWorker(t, backendID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":1,"models":[{"name":%q,"state":"loading","context_size":0}]}`, model)
	})

	if n, st := p.resolveLoadedWindowNCtx(backendID, model, true); st != loadedWindowLoading || n != 0 {
		t.Fatalf("resolveLoadedWindowNCtx = (%d, %s), want (0, loading)", n, st)
	}
}
