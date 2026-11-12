// worker_identity_r493_test.go — R493-12i (2026-11-12): идентичность ВОРКЕРА, а не
// агента.
//
// ЖИВОЙ ДЕФЕКТ. В bundled-стеке бэкенд регистрирует встроенный агент, и поля
// workerVersion/workerCommit он заполнял своим version.Get(). На стенде это дало
// workerCommit=9d42a329 при образе gpu-261110-1925, чей /app/identity.json несёт
// commit=24b3a151 — при проверке выкладки это читалось как «раскатали не тот
// образ». Здесь проверяется правило: воркер спрашиваем по HTTP, свою
// идентичность помечаем отдельно и честно называем источник.
package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newWorkerIdentityAgentR493 — агент, смотрящий на тестовый «воркер».
func newWorkerIdentityAgentR493(t *testing.T, handler http.HandlerFunc) *Agent {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	prev := cppWorkerProbeTimeout
	cppWorkerProbeTimeout = 2 * time.Second
	t.Cleanup(func() { cppWorkerProbeTimeout = prev })
	return &Agent{
		config:     &types.AgentConfig{AgentID: "w1", CppWorkerURL: srv.URL},
		httpClient: srv.Client(),
	}
}

// TestR493_WorkerIdentityFromWorkerHTTP — идентичность берётся у ВОРКЕРА.
func TestR493_WorkerIdentityFromWorkerHTTP(t *testing.T) {
	a := newWorkerIdentityAgentR493(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/info" {
			t.Errorf("агент спросил %q, ожидался /api/info", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","build_version":"r261112-2146",` +
			`"build_commit":"32153c14","build_date":"2026-11-12T18:46:37Z","rpc_proto":"6.0.0"}`))
	})

	ident := resolveWorkerIdentityR493(a)
	if ident.Source != workerIdentityFromWorkerR493 {
		t.Fatalf("Source = %q, ожидался %q: идентичность воркера получена, значит и метка такая",
			ident.Source, workerIdentityFromWorkerR493)
	}
	if ident.Version != "r261112-2146" || ident.Commit != "32153c14" ||
		ident.BuildDate != "2026-11-12T18:46:37Z" || ident.RPCProto != "6.0.0" {
		t.Fatalf("идентичность воркера искажена: %+v", ident)
	}
}

// TestR493_WorkerIdentityFallbackWhenWorkerSilent — воркер не отвечает: берём
// свою, но помечаем источник, иначе подмена невидима.
func TestR493_WorkerIdentityFallbackWhenWorkerSilent(t *testing.T) {
	a := newWorkerIdentityAgentR493(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ident := resolveWorkerIdentityR493(a)
	if ident.Source != workerIdentityFromAgentR493 {
		t.Fatalf("Source = %q, ожидался %q (воркер ответил 503)", ident.Source, workerIdentityFromAgentR493)
	}
}

// TestR493_WorkerIdentityFallbackWhenNoBuildFields — старый образ воркера без
// build-полей: пусто — это «неизвестно», а не «факт», значит fallback с меткой.
func TestR493_WorkerIdentityFallbackWhenNoBuildFields(t *testing.T) {
	a := newWorkerIdentityAgentR493(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","version":"0.2.0 (real llama.cpp linked)"}`))
	})
	ident := resolveWorkerIdentityR493(a)
	if ident.Source != workerIdentityFromAgentR493 {
		t.Fatalf("Source = %q, ожидался %q (в /api/info нет build-полей)",
			ident.Source, workerIdentityFromAgentR493)
	}
}

// TestR493_WorkerIdentityNoAddress — адрес воркера неизвестен: fallback, без
// паники и без запроса «в никуда».
func TestR493_WorkerIdentityNoAddress(t *testing.T) {
	a := &Agent{config: &types.AgentConfig{AgentID: "w1"}}
	ident := resolveWorkerIdentityR493(a)
	if ident.Source != workerIdentityFromAgentR493 {
		t.Fatalf("Source = %q, ожидался %q (адрес воркера неизвестен)",
			ident.Source, workerIdentityFromAgentR493)
	}
	// nil-агент не должен паниковать: вызов ниже — и есть проверка.
	if ident := resolveWorkerIdentityR493(nil); ident.Source != workerIdentityFromAgentR493 {
		t.Fatalf("nil-агент: Source = %q, ожидался %q", ident.Source, workerIdentityFromAgentR493)
	}
}

// TestR493_WorkerIdentityIgnoresAgentFields — поля агента из /api/info (если
// воркер их тоже отдаёт) НЕ должны подменять build_*: берём только build-*.
func TestR493_WorkerIdentityIgnoresAgentFields(t *testing.T) {
	a := newWorkerIdentityAgentR493(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"build_version":"r-worker","build_commit":"worker123",` +
			`"version":"agent-version","commit":"agent123"}`))
	})
	ident := resolveWorkerIdentityR493(a)
	if ident.Commit != "worker123" {
		t.Fatalf("Commit = %q, ожидался worker123: поля без префикса build_ — не про воркера", ident.Commit)
	}
}
