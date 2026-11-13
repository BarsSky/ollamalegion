// worker_identity_refresh_r493_test.go — R493-12i, вторая половина (2026-11-12):
// идентичность воркера обязана быть СВЕЖЕЙ, а не «той, что была при регистрации».
//
// ЖИВОЙ ДЕФЕКТ. Узел пересобрали (образ gpu-r261112-2146), а /api/v1/backends
// продолжал показывать workerVersion=261110-1925 / workerCommit=9d42a329: запись
// поднимается из config.json при каждом перезапуске балансера, а регистрация у
// воркера одноразовая (при старте). Поле читается как «чем собран узел», и
// устаревшее значение снова выглядит как «раскатали не то».
//
// ПРО ТОЧНЫЕ СЧЁТЧИКИ ЗАПРОСОВ. Первая версия теста считала хиты /api/info и
// падала в CI под -race: рядом тикает ФОНОВЫЙ поллер прокси и тоже имеет право
// спросить идентичность, поэтому «ровно 1 запрос» зависело от планировщика.
// Поэтому TTL проверяется там, где он и живёт — на identityRefreshR493.due
// (детерминированно), а сквозные тесты проверяют только результат обновления.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// identityServerR493 — тестовый воркер: /api/info с build-полями.
func identityServerR493(t *testing.T, reject bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/info" {
			http.NotFound(w, r)
			return
		}
		if reject {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"build_version":"r261112-2146","build_commit":"32153c14",` +
			`"build_date":"2026-11-12T18:46:37Z","rpc_proto":"6.0.0"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
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

// TestR493_IdentityRefreshTTL — «спрашиваем не чаще раза в TTL» проверяется на
// самом TTL-состоянии: детерминированно, без гонки с фоновым поллером.
func TestR493_IdentityRefreshTTL(t *testing.T) {
	var r identityRefreshR493
	now := time.Now()

	if !r.due("b1", now) {
		t.Fatal("первый опрос обязан быть разрешён")
	}
	if r.due("b1", now.Add(workerIdentityTTLR493/2)) {
		t.Fatalf("опрос раньше TTL (%v) разрешён — /api/info будет дёргаться каждый цикл метрик",
			workerIdentityTTLR493)
	}
	if !r.due("b1", now.Add(workerIdentityTTLR493+time.Second)) {
		t.Fatalf("после TTL (%v) опрос не разрешён", workerIdentityTTLR493)
	}
	// Разные бэкенды не мешают друг другу.
	if !r.due("b2", now) {
		t.Fatal("TTL одного узла не должен блокировать другой")
	}
}

// TestR493_WorkerIdentityRefreshedFromInfo — старая запись заменяется фактом с узла.
func TestR493_WorkerIdentityRefreshedFromInfo(t *testing.T) {
	srv := identityServerR493(t, false)
	host, port := hostPortOfR493(t, srv.URL)
	p := identityProxyR493(t, host, port)

	poller := &llamaCppMetricsPoller{proxy: p, httpClient: srv.Client()}
	poller.refreshWorkerIdentityR493(backendInfo{id: "b1", host: host, port: port})

	// Поля читаем под замком записи: рядом тикает фоновый поллер прокси и тоже
	// может обновлять идентичность (ровно та гонка, что поймал -race в CI).
	got := backendIdentitySnapshotR493(p, "b1")
	if got.version != "r261112-2146" || got.commit != "32153c14" {
		t.Fatalf("идентичность не обновлена: version=%q commit=%q (осталась из config.json)",
			got.version, got.commit)
	}
	if got.source != "worker-http" {
		t.Fatalf("источник идентичности не отмечен: %q (значения пришли из /api/info — это факт)",
			got.source)
	}
	if got.buildDate != "2026-11-12T18:46:37Z" || got.rpcProto != "6.0.0" {
		t.Fatalf("не все поля обновлены: %+v", got)
	}
}

// TestR493_WorkerIdentityErrorKeepsPrevious — ошибка узла не стирает известное.
func TestR493_WorkerIdentityErrorKeepsPrevious(t *testing.T) {
	srv := identityServerR493(t, true)
	host, port := hostPortOfR493(t, srv.URL)
	p := identityProxyR493(t, host, port)
	poller := &llamaCppMetricsPoller{proxy: p, httpClient: srv.Client()}
	poller.refreshWorkerIdentityR493(backendInfo{id: "b1", host: host, port: port})

	got := backendIdentitySnapshotR493(p, "b1")
	if got.version != "261110-1925" {
		t.Fatalf("при ошибке /api/info запись изменена: %q", got.version)
	}
}

// backendIdentitySnapshotR493 — снимок identity-полей под замками. Тесты обязаны
// читать так же, как продакшен: рядом работает фоновый поллер.
type backendIdentityR493 struct {
	version, commit, buildDate, rpcProto, source string
}

func backendIdentitySnapshotR493(p *Proxy, backendID string) backendIdentityR493 {
	p.mu.RLock()
	state := p.backends[backendID]
	p.mu.RUnlock()
	if state == nil || state.Backend == nil {
		return backendIdentityR493{}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return backendIdentityR493{
		version:   state.Backend.WorkerVersion,
		commit:    state.Backend.WorkerCommit,
		buildDate: state.Backend.WorkerBuildDate,
		rpcProto:  state.Backend.WorkerRPCProto,
		source:    state.Backend.WorkerIdentitySource,
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
