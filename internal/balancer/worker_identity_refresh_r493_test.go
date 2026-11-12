// worker_identity_refresh_r493_test.go — R493-12i, вторая половина (2026-11-12):
// идентичность воркера обязана быть СВЕЖЕЙ, а не «той, что была при регистрации».
//
// ЖИВОЙ ДЕФЕКТ. Узел пересобрали (образ gpu-r261112-2146), а /api/v1/backends
// продолжал показывать workerVersion=261110-1925 / workerCommit=9d42a329: запись
// поднимается из config.json при каждом перезапуске балансера, а регистрация у
// воркера одноразовая (при старте). Поле читается как «чем собран узел», и
// устаревшее значение снова выглядит как «раскатали не то».
package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// identityServerR493 — тестовый воркер: /api/info с build-полями, счётчик хитов.
func identityServerR493(t *testing.T, reject bool) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/info" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&hits, 1)
		if reject {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"build_version":"r261112-2146","build_commit":"32153c14",` +
			`"build_date":"2026-11-12T18:46:37Z","rpc_proto":"6.0.0"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// identityProxyR493 — прокси с одним llama_cpp бэкендом, чья запись «старая».
func identityProxyR493(t *testing.T, host string, port int) *Proxy {
	t.Helper()
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{{
			ID: "b1", Name: "b1", Host: host, OllamaPort: 11434, CppWorkerPort: port,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
			MaxConcurrentReqs: 1, Weight: 1,
			WorkerVersion: "261110-1925", WorkerCommit: "9d42a329",
		}},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmResourceAware, MetricsInterval: 60},
	}
	return newProxyWithCleanup(t, conf)
}

// TestR493_WorkerIdentityRefreshedFromInfo — старая запись заменяется фактом с узла.
func TestR493_WorkerIdentityRefreshedFromInfo(t *testing.T) {
	srv, _ := identityServerR493(t, false)
	host, port := hostPortOfR493(t, srv.URL)
	p := identityProxyR493(t, host, port)

	poller := &llamaCppMetricsPoller{proxy: p, httpClient: srv.Client()}
	poller.refreshWorkerIdentityR493(backendInfo{id: "b1", host: host, port: port})

	p.mu.RLock()
	b := p.backends["b1"].Backend
	p.mu.RUnlock()
	if b.WorkerVersion != "r261112-2146" || b.WorkerCommit != "32153c14" {
		t.Fatalf("идентичность не обновлена: version=%q commit=%q (осталась из config.json)",
			b.WorkerVersion, b.WorkerCommit)
	}
	if b.WorkerIdentitySource != "worker-http" {
		t.Fatalf("источник идентичности не отмечен: %q (значения пришли из /api/info — это факт)", b.WorkerIdentitySource)
	}
	if b.WorkerBuildDate != "2026-11-12T18:46:37Z" || b.WorkerRPCProto != "6.0.0" {
		t.Fatalf("не все поля обновлены: %+v", b)
	}
}

// TestR493_WorkerIdentityTTL — второго запроса в пределах TTL нет (иначе каждый
// цикл метрик дёргал бы /api/info).
func TestR493_WorkerIdentityTTL(t *testing.T) {
	srv, hits := identityServerR493(t, false)
	host, port := hostPortOfR493(t, srv.URL)
	p := identityProxyR493(t, host, port)
	poller := &llamaCppMetricsPoller{proxy: p, httpClient: srv.Client()}
	bi := backendInfo{id: "b1", host: host, port: port}

	poller.refreshWorkerIdentityR493(bi)
	poller.refreshWorkerIdentityR493(bi)
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("/api/info опрошен %d раз, ожидался 1 (TTL %v)", got, workerIdentityTTLR493)
	}

	// По истечении TTL — снова можно.
	poller.identityRefresh.mu.Lock()
	poller.identityRefresh.last["b1"] = time.Now().Add(-workerIdentityTTLR493 - time.Second)
	poller.identityRefresh.mu.Unlock()
	poller.refreshWorkerIdentityR493(bi)
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("после TTL /api/info опрошен %d раз, ожидалось 2", got)
	}
}

// TestR493_WorkerIdentityErrorKeepsPrevious — ошибка узла не стирает известное.
func TestR493_WorkerIdentityErrorKeepsPrevious(t *testing.T) {
	srv, _ := identityServerR493(t, true)
	host, port := hostPortOfR493(t, srv.URL)
	p := identityProxyR493(t, host, port)
	poller := &llamaCppMetricsPoller{proxy: p, httpClient: srv.Client()}
	poller.refreshWorkerIdentityR493(backendInfo{id: "b1", host: host, port: port})

	p.mu.RLock()
	b := p.backends["b1"].Backend
	p.mu.RUnlock()
	if b.WorkerVersion != "261110-1925" {
		t.Fatalf("при ошибке /api/info запись изменена: %q", b.WorkerVersion)
	}
}

// hostPortOfR493 — разобрать адрес тестового сервера.
func hostPortOfR493(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	s := strings.TrimPrefix(rawURL, "http://")
	i := strings.LastIndex(s, ":")
	if i < 0 {
		t.Fatalf("не разобрать адрес %q", rawURL)
	}
	host := s[:i]
	port := 0
	for _, r := range s[i+1:] {
		if r < '0' || r > '9' {
			t.Fatalf("нечисловой порт в %q", rawURL)
		}
		port = port*10 + int(r-'0')
	}
	return host, port
}
