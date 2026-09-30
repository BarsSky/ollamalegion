//go:build llama_stub

// stale_cache_live_refresh_r83_test.go — R83-fix (2026-09-30).
//
// ЖИВОЕ ВОСПРОИЗВЕДЕНИЕ (стенд, balancer v28 + cppworker v27):
//
//	подготовили модель: cppworker /api/models → context_size=31974
//	клиент прислал запрос с options.num_ctx=8192
//	балансер:
//	  preflightNCtxReload: detected n_ctx mismatch, scheduling async reload
//	    loaded_n_ctx=0, requested_n_ctx=8192, loaded_n_ctx_is_zero=true
//	  preflightNCtxReload(async): starting reload ... target_n_ctx=8192
//	  preflightNCtxReload(async): reload returned non-OK status 500
//	ответ клиенту: обрыв (curl CODE:000) — в Cline это выглядит как
//	  "Cannot read properties of undefined (reading 'content')"
//
// ПРИЧИНА: загрузку сделали МИМО балансера (WebUI / прямой вызов cppworker), и
// кэш метрик был пуст (поллер опрашивает раз в 30 c). Балансер считал модель
// незагруженной и планировал перезагрузку на КЛИЕНТСКОЕ (меньшее) окно —
// «подготовленная модель выгружена в пользу меньшего окна от клиента».
//
// ФИКС: если кэш говорит «не загружена», а запрос уже идёт на бэкенд — один раз
// спрашиваем cppworker (/api/models) и только потом решаем про reload.
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

// newProxyWithLiveCppWorker — прокси, у которого есть «живой» cppworker с
// загруженной моделью, но ПУСТОЙ кэш метрик (имитация загрузки мимо балансера).
func newProxyWithLiveCppWorker(t *testing.T, backendID, modelName string, loadedCtx int) (*Proxy, *httptest.Server) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"count":1,"models":[{"name":%q,"state":"loaded",`+
				`"context_size":%d,"context_per_seq":%d,"max_slots":2,"parallel":2}]}`,
				modelName, loadedCtx, loadedCtx/2)
		default:
			http.NotFound(w, r)
		}
	}))
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
	// Кэш метрик ПУСТ — как сразу после загрузки мимо балансера.
	p.nctxReload = NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	p.client = &http.Client{}
	p.metricsHTTPDoer = &stubMetricsDoer{loadedNCtx: loadedCtx}
	p.llamaCppMetricsPoller = newLlamaCppMetricsPoller(p)
	return p, srv
}

// TestR83_StaleCache_LiveWindowPreventsSpuriousReload — клиент просит МЕНЬШЕ
// загруженного: перезагрузки быть не должно, ответ обычный.
func TestR83_StaleCache_LiveWindowPreventsSpuriousReload(t *testing.T) {
	const backendID = "live-cpp-1"
	const model = "Qwen3-Instruct-2507-q4km"

	p, _ := newProxyWithLiveCppWorker(t, backendID, model, 32768)

	body := []byte(`{"model":"` + model + `","options":{"num_ctx":8192}}`)
	_, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		context.Background(), backendID, model, body, "/api/chat")

	if !needsProxy || status != http.StatusOK {
		t.Fatalf("ожидался обычный проксинг (200), получено needsProxy=%v status=%d msg=%q — "+
			"это и есть баг «модель выгружается в пользу меньшего окна от клиента»",
			needsProxy, status, msg)
	}
	if got := loadedNCtxOf(p, backendID); got != 32768 {
		t.Errorf("после принудительного обновления кэш показывает n_ctx=%d, want 32768 "+
			"(иначе следующие запросы снова увидят loaded=0)", got)
	}
	if p.nctxReload.IsReloadPending(backendID, model) {
		t.Error("для модели запланирован reload, хотя она уже загружена")
	}
}

// TestR83_StaleCache_ResolverUsesLiveWindow — резолвер тоже должен видеть
// живое окно: клиент, просящий больше, не получает «потолок профиля».
func TestR83_StaleCache_ResolverUsesLiveWindow(t *testing.T) {
	const backendID = "live-cpp-2"
	const model = "m"

	p, _ := newProxyWithLiveCppWorker(t, backendID, model, 32768)
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		// Потолок профиля ВЫШЕ живого окна — именно так рождался
		// n_ctx_override=32768 при фактических 31974.
		model: {ContextLength: 16384, ContextLengthMax: 65536},
	}

	body := []byte(`{"model":"` + model + `","options":{"num_ctx":65536}}`)
	got := p.ResolveNumCtx(model, body, backendID)

	// Живой cppworker отдаёт: context_size=32768, context_per_seq=16384, max_slots=2.
	// Ограничиваем ОКНОМ СЛОТА (16384), а не потолком профиля (65536) и не
	// суммарным окном (32768): слот с двумя слотами столько не вмещает.
	if got.Value > 16384 {
		t.Errorf("ResolveNumCtx вернул %d при живом окне слота 16384 — cppworker "+
			"примет запрос, который слот не обслужит", got.Value)
	}
	if got.Value != 16384 {
		t.Errorf("ResolveNumCtx = %d, want 16384 (живое окно слота)", got.Value)
	}
}
