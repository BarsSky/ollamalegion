//go:build llama_stub

package balancer

// R82: failover запроса, выбранного на реплику, которая умерла между выбором и
// запросом.
//
// Замер P4 (R81) на стенде из двух копий: `docker stop` одной реплики, затем
// запрос сразу после падения, выбранный на неё ротацией:
//   - async-загрузка провалилась за 30 мс (DNS «no such host»),
//   - но запрос ждал ПОЛНЫЙ LB_AUTO_LOAD_WAIT_SEC (300 с в стенде) и отвалился
//     по клиентскому таймауту (120 с), хотя живая копия модели была доступна.
//
// Здесь проверяем три части фикса:
//   1. waitForModelLoad падает сразу, когда async-загрузка уже провалилась;
//   2. waitForModelLoad падает сразу, когда бэкенд выпал из пула;
//   3. ensureModelLoadedWithFailover переезжает на живую копию вместо 503.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newFailoverHarnessR82 — прокси с «мёртвым» (закрытый порт) и «живым»
// (stub-сервер с загруженной моделью) бэкендами.
func newFailoverHarnessR82(t *testing.T) (*LlamaCppRouter, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"m","state":"loaded","context_size":4096}]}`))
		case "/api/models/load/progress":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"state":"loaded","name":"m"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)
	deadPort := freeTestPorts(1)[0]

	mm := NewModelManager(nil)
	p := &Proxy{
		config:          &types.LoadBalancerConfig{},
		backends:        map[string]*BackendState{},
		metricsMgr:      NewMetricsManager(),
		modelManager:    mm,
		lbAutoLoadAsync: true,
		lbAutoLoadWait:  300 * time.Millisecond,
	}
	mm.proxy = p
	p.backends["dead-bk"] = &BackendState{Backend: &types.Backend{
		ID: "dead-bk", Host: "127.0.0.1", OllamaPort: deadPort, CppWorkerPort: deadPort,
		Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp,
	}}
	p.backends["live-bk"] = &BackendState{Backend: &types.Backend{
		ID: "live-bk", Host: host, OllamaPort: port, CppWorkerPort: port,
		Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp,
	}}
	// Метрики: у обеих копий модель числится загруженной (у «мёртвой» — это
	// устаревший снапшот, как в живом замере: агент ещё слал метрики).
	for _, id := range []string{"dead-bk", "live-bk"} {
		p.metricsMgr.metrics[id] = &types.BackendMetrics{
			GPU: types.GPUMetrics{MemoryTotal: 8192, MemoryFree: 1024},
			LlamaCpp: types.LlamaCppMetrics{
				LoadedModels: []types.LlamaCppModel{{
					Name:          "m",
					ContextLength: 4096,
					State:         string(types.ModelStateLoaded),
				}},
			},
		}
	}
	return NewLlamaCppRouter(p), srv
}

func TestWaitForModelLoad_FailsFastOnLoadFailure_R82(t *testing.T) {
	lr, _ := newAutoloadWaitHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/models/load/progress" {
			_, _ = w.Write([]byte(`{"state":"loading","name":"m"}`))
			return
		}
		_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
	}, 10*time.Second)

	// Async-загрузка уже провалилась (так сигналит горутина загрузки).
	loadDone := make(chan error, 1)
	loadDone <- errFakeLoadFailedR82("lookup cpp-b: no such host")

	start := time.Now()
	err := lr.waitForModelLoad(context.Background(), "test-bk", "m", 10*time.Second, loadDone)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ожидалась ошибка загрузки, получено nil")
	}
	if !strings.Contains(err.Error(), "no such host") {
		t.Errorf("ошибка не содержит причину загрузки: %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("ждали %s — должны были упасть сразу после провала загрузки", elapsed)
	}
}

func TestWaitForModelLoad_BailsWhenBackendUnavailable_R82(t *testing.T) {
	lr, _ := newAutoloadWaitHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/models/load/progress" {
			_, _ = w.Write([]byte(`{"state":"loading","name":"m"}`))
			return
		}
		_, _ = w.Write([]byte(`{"count":0,"models":[]}`))
	}, 10*time.Second)

	// Бэкенд выпал из пула (health-check пометил недоступным).
	lr.proxy.backends["test-bk"].Backend.Status = types.StatusUnhealthy

	start := time.Now()
	err := lr.waitForModelLoad(context.Background(), "test-bk", "m", 10*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ожидалась ошибка доступности бэкенда, получено nil")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("ждали %s — должны были упасть сразу (бэкенд нездоров)", elapsed)
	}
}

func TestEnsureModelLoadedWithFailover_MovesToLiveCopy_R82(t *testing.T) {
	lr, _ := newFailoverHarnessR82(t)

	// "dead-bk" — закрытый порт: загрузка падает на connection refused.
	got, err := lr.ensureModelLoadedWithFailover("dead-bk", "m", warmupOptions{})
	if err != nil {
		t.Fatalf("переезд не сработал, ошибка: %v", err)
	}
	if got != "live-bk" {
		t.Fatalf("выбран бэкенд %q, ожидался live-bk (живая копия)", got)
	}
}

func TestEnsureModelLoadedWithFailover_KeepsHealthyBackend_R82(t *testing.T) {
	lr, _ := newFailoverHarnessR82(t)

	// Живой бэкенд с загруженной моделью — переезд не нужен.
	got, err := lr.ensureModelLoadedWithFailover("live-bk", "m", warmupOptions{})
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if got != "live-bk" {
		t.Errorf("выбран бэкенд %q, ожидался live-bk", got)
	}
}

func TestBackendUnavailableR82_Statuses(t *testing.T) {
	lr, _ := newFailoverHarnessR82(t)

	cases := []struct {
		status types.BackendStatus
		want   bool
	}{
		{types.StatusHealthy, false},
		{types.StatusUnhealthy, true},
		{types.StatusOllamaUnavailable, true},
		{types.StatusOffline, true},
		{types.StatusDraining, true},
		{types.StatusStarting, true},
	}
	for _, tc := range cases {
		lr.proxy.backends["live-bk"].Backend.Status = tc.status
		if got := lr.backendUnavailable("live-bk"); got != tc.want {
			t.Errorf("статус %q → backendUnavailable=%v, ожидалось %v", tc.status, got, tc.want)
		}
	}
	if !lr.backendUnavailable("no-such-backend") {
		t.Error("неизвестный бэкенд должен считаться недоступным")
	}
}

// errFakeLoadFailedR82 — ошибка «загрузка провалилась» без сетевого стека.
func errFakeLoadFailedR82(msg string) error { return &fakeLoadErrR82{msg: msg} }

type fakeLoadErrR82 struct{ msg string }

func (e *fakeLoadErrR82) Error() string { return e.msg }
