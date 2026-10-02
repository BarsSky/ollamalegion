//go:build llama_stub

// per_client_window_end_to_end_r83_test.go — R83-fix (2026-09-30).
//
// Сквозная проверка «окна на клиента» на живых данных cppworker (fake /api/models):
//
//		context_size=32768, context_per_seq=16384, max_slots=2
//
//	 1. поллер обязан разобрать context_per_seq и положить его в кэш метрик;
//	 2. клиент, просящий 65536 «на себя», не должен получить 413: preflight
//	    планирует РОСТ (reload до 65536 × 2 слота = 131072 суммарно) и говорит
//	    «retry», а не «exceeds n_ctx»;
//	 3. значение, уходящее в cppworker для ТЕКУЩЕГО запроса, — окно слота (16384),
//	    а не суммарное (32768) и не будущий таргет (131072): именно из-за подъёма
//	    до суммарного Cline и получал «prompt + n_predict exceeds n_ctx».
//
// Живой контекст: gemma-4-E4B загружена ctx=65536 при max_slots=2 → на клиента
// 32768, длинный промпт Cline не влезал, и клиент видел
//
//	"preflight: prompt + n_predict exceeds n_ctx for this backend".
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

func newPerClientWindowProxy(t *testing.T) *Proxy {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":1,"models":[{"name":"dbg","state":"loaded",`+
			`"context_size":32768,"context_per_seq":16384,"max_slots":2,"parallel":2}]}`)
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["b1"] = &BackendState{Backend: &types.Backend{
		ID: "b1", Host: u.Hostname(), CppWorkerPort: port, Type: types.BackendTypeLlamaCpp,
	}}
	p.llamaCppMetricsPoller = newLlamaCppMetricsPoller(p)
	p.nctxReload = NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	p.client = &http.Client{}
	return p
}

func TestR83_PerClientWindow_PollerParsesContextPerSeq(t *testing.T) {
	p := newPerClientWindowProxy(t)
	p.refreshLoadedWindowNCtx("b1", "dbg")

	if got := p.loadedWindowNCtx("b1", "dbg"); got != 32768 {
		t.Errorf("loadedWindowNCtx = %d, want 32768 (суммарное окно)", got)
	}
	if got := p.loadedPerSeqNCtx("b1", "dbg"); got != 16384 {
		t.Errorf("loadedPerSeqNCtx = %d, want 16384 (окно слота из context_per_seq)", got)
	}
	if got := p.loadedSlotsNCtx("b1", "dbg"); got != 2 {
		t.Errorf("loadedSlotsNCtx = %d, want 2", got)
	}
}

func TestR83_PerClientWindow_PreflightGrowsInsteadOfRejecting(t *testing.T) {
	p := newPerClientWindowProxy(t)
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"dbg": {ContextLength: 16384, ContextLengthMax: 131072},
	}
	body := []byte(`{"model":"dbg","options":{"num_ctx":65536}}`)

	_, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		context.Background(), "b1", "dbg", body, "/api/chat")

	if status == http.StatusRequestEntityTooLarge {
		t.Fatalf("клиент получил 413 вместо роста окна: %q", msg)
	}
	if !p.nctxReload.IsReloadPending("b1", "dbg") {
		t.Fatalf("рост окна не запланирован: needsProxy=%v status=%d msg=%q", needsProxy, status, msg)
	}
}

func TestR83_PerClientWindow_ResolverSendsPerSlotNotTotal(t *testing.T) {
	p := newPerClientWindowProxy(t)
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"dbg": {ContextLength: 16384, ContextLengthMax: 131072},
	}
	body := []byte(`{"model":"dbg","options":{"num_ctx":65536}}`)

	// Preflight наполняет кэш (и планирует рост), резолвер затем считает значение
	// для текущего запроса.
	_, _, _, _ = p.preflightNCtxReloadIfNeeded(context.Background(), "b1", "dbg", body, "/api/chat")

	if got := p.ResolveNumCtx("dbg", body, "b1").Value; got != 16384 {
		t.Errorf("ResolveNumCtx = %d, want 16384 (окно слота); суммарное=%d, "+
			"будущий таргет=%d — отправлять их в cppworker нельзя",
			got, p.loadedWindowNCtx("b1", "dbg"), 131072)
	}
}
