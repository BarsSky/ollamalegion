// cancel_client_r229_test.go — R229 (2026-10-22): отмена генерации клиентом.
//
// ЖИВОЙ ДЕФЕКТ (замер ДО правки): POST /api/cancel через балансер не отвечал
// (таймаут 15 с), а стрим продолжался до конца (380 чанков, 62 с) — то есть
// кнопка Stop у клиента не останавливала генерацию, воркер держал слот.
//
// Тесты фиксируют поведение балансера:
//   - отмена уходит ТОЛЬКО на бэкенды, где модель загружена;
//   - модель не загружена нигде — приказ всё равно рассылается (воркер решит);
//   - нет подходящих бэкендов — 200 no_active_generation (а не таймаут);
//   - не-POST — 405, тело не обязано быть валидным;
//   - request_id/user_id клиента доезжают до воркера.
package balancer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

type cancelRecorder struct {
	mu     sync.Mutex
	bodies []map[string]interface{}
}

func (r *cancelRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/cancel" || req.Method != http.MethodPost {
			http.NotFound(w, req)
			return
		}
		raw, _ := io.ReadAll(req.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		if req.Header.Get("X-API-Token") == "" {
			http.Error(w, `{"error":"no token"}`, http.StatusUnauthorized)
			return
		}
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"cancelled"}`))
	}
}

func (r *cancelRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *cancelRecorder) last() map[string]interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		return nil
	}
	return r.bodies[len(r.bodies)-1]
}

// newCancelTestProxy создаёт прокси с n llama.cpp-бэкендами, указывающими на
// подставные воркеры, и возвращает их записыватели.
func newCancelTestProxy(t *testing.T, n int) (*Proxy, []*cancelRecorder) {
	t.Helper()
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	recs := make([]*cancelRecorder, 0, n)
	for i := 0; i < n; i++ {
		rec := &cancelRecorder{}
		srv := httptest.NewServer(rec.handler())
		t.Cleanup(srv.Close)
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatalf("parse url: %v", err)
		}
		port, _ := strconv.Atoi(u.Port())
		id := "backend-" + strconv.Itoa(i+1)
		p.backends[id] = &BackendState{Backend: &types.Backend{
			ID: id, Host: u.Hostname(), CppWorkerPort: port,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
			CppWorkerApiToken: "test-token",
		}}
		recs = append(recs, rec)
	}
	return p, recs
}

func doCancel(t *testing.T, p *Proxy, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/cancel", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.handleClientCancel(rec, req)
	return rec
}

func TestR229_ClientCancelGoesOnlyToBackendWithLoadedModel(t *testing.T) {
	p, recs := newCancelTestProxy(t, 2)
	p.metricsMgr.UpdateLlamaCppMetrics("backend-1", &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: "m1", State: "loaded"}},
	})

	rec := doCancel(t, p, http.MethodPost, `{"model":"m1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if recs[0].count() != 1 {
		t.Errorf("бэкенд с загруженной моделью получил %d отмен, want 1", recs[0].count())
	}
	if recs[1].count() != 0 {
		t.Errorf("бэкенд без модели получил %d отмен, want 0", recs[1].count())
	}
	if got := recs[0].last(); got["model"] != "m1" {
		t.Errorf("в теле отмены model = %v, want m1", got["model"])
	}
	// R229: в ответе операции видны детали воркера (сколько отменено/статус), а не
	// только «кому отправили» — иначе «Stop не сработал» неотличим от «сработал».
	if body := rec.Body.String(); !strings.Contains(body, `"status":"cancelled"`) {
		t.Errorf("в ответе балансера нет статуса воркера: %s", body)
	}
}

func TestR229_ClientCancelFansOutWhenModelNotLoadedAnywhere(t *testing.T) {
	p, recs := newCancelTestProxy(t, 2)
	rec := doCancel(t, p, http.MethodPost, `{"model":"nowhere"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if recs[0].count() != 1 || recs[1].count() != 1 {
		t.Fatalf("рассылка: %d/%d, want 1/1 — приказ дешёвый, воркер решит сам",
			recs[0].count(), recs[1].count())
	}
}

func TestR229_ClientCancelNoBackendsIsNotTimeout(t *testing.T) {
	p, _ := newCancelTestProxy(t, 0)
	start := time.Now()
	rec := doCancel(t, p, http.MethodPost, `{"model":"m1"}`)
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if elapsed > time.Second {
		t.Fatalf("ответ занял %s — клиент не должен ждать «отменять нечего»", elapsed)
	}
	if body := rec.Body.String(); !strings.Contains(body, "no_active_generation") {
		t.Errorf("в ответе нет no_active_generation: %s", body)
	}
}

func TestR229_ClientCancelRejectsGet(t *testing.T) {
	p, _ := newCancelTestProxy(t, 1)
	rec := doCancel(t, p, http.MethodGet, "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/cancel = %d, want 405", rec.Code)
	}
}

func TestR229_ClientCancelForwardsRequestID(t *testing.T) {
	p, recs := newCancelTestProxy(t, 1)
	rec := doCancel(t, p, http.MethodPost, `{"request_id":"req-42","user_id":"alice"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := recs[0].last()
	if got["request_id"] != "req-42" || got["user_id"] != "alice" {
		t.Errorf("поля клиента не доехали: %v", got)
	}
}
