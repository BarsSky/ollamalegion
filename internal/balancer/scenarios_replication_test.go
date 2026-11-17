// Package balancer — scenario tests для replication groups + auto-pull.
//
// Покрывает:
//   - Replication group CRUD (через proxy config)
//   - Min/max instances enforcement
//   - Auto-pull: enable/disable, status endpoint
//   - Replication reconcile scenarios

package balancer

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/pkg/types"
)

// =====================================================================
// Replication groups
// =====================================================================

// TestReplication_Scenario_GroupRegistered — replication group
// appears in config and is loaded by proxy.
func TestReplication_Scenario_GroupRegistered(t *testing.T) {
	t.Parallel()
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: "127.0.0.1", OllamaPort: 11434, Status: types.StatusHealthy},
			{ID: "o2", Name: "o2", Host: "127.0.0.1", OllamaPort: 11435, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			ModelReplication: types.ModelReplicationConfig{
				Enabled:             true,
				DefaultMinInstances: 1,
				DefaultMaxInstances: 3,
				IdleUnloadAfter:     "10m",
				Groups: []types.ModelGroupConfig{
					{ModelName: "llama-replicated", MinInstances: 2, MaxInstances: 4,
						TargetBackends: []string{"o1", "o2"}},
				},
			},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := newProxyWithCleanup(t, conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Group exists в config.
	assert.Equal(t, 1, len(conf.Balancing.ModelReplication.Groups))
	assert.Equal(t, "llama-replicated", conf.Balancing.ModelReplication.Groups[0].ModelName)
	assert.Equal(t, 2, conf.Balancing.ModelReplication.Groups[0].MinInstances)
	assert.Equal(t, 4, conf.Balancing.ModelReplication.Groups[0].MaxInstances)
}

// TestReplication_Scenario_GroupMinMax — min/max instances валидируются.
func TestReplication_Scenario_GroupMinMax(t *testing.T) {
	t.Parallel()
	group := types.ModelGroupConfig{
		ModelName:      "test",
		MinInstances:   1,
		MaxInstances:   5,
		TargetBackends: []string{"o1", "o2"},
	}
	assert.LessOrEqual(t, group.MinInstances, group.MaxInstances,
		"MinInstances should be <= MaxInstances")
}

// TestReplication_Scenario_EmptyGroup — пустой TargetBackends.
func TestReplication_Scenario_EmptyGroup(t *testing.T) {
	t.Parallel()
	group := types.ModelGroupConfig{
		ModelName:      "test",
		MinInstances:   0,
		MaxInstances:   0,
		TargetBackends: []string{},
	}
	assert.Equal(t, 0, group.MinInstances)
	assert.Empty(t, group.TargetBackends)
}

// TestReplication_Scenario_IdleUnloadSeconds — idle unload time настроен.
func TestReplication_Scenario_IdleUnloadSeconds(t *testing.T) {
	t.Parallel()
	conf := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			ModelReplication: types.ModelReplicationConfig{
				IdleUnloadAfter: "10m",
			},
		},
	}
	assert.Equal(t, "10m", conf.Balancing.ModelReplication.IdleUnloadAfter)
}

// TestReplication_Scenario_DisabledByDefault — replication disabled by default.
func TestReplication_Scenario_DisabledByDefault(t *testing.T) {
	t.Parallel()
	conf := &types.LoadBalancerConfig{}
	assert.False(t, conf.Balancing.ModelReplication.Enabled)
}

// =====================================================================
// Auto-pull
// =====================================================================

// TestAutoPull_Scenario_EnabledInConfig — autopull enabled.
func TestAutoPull_Scenario_EnabledInConfig(t *testing.T) {
	t.Parallel()
	conf := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			AutoPull: types.AutoPullConfig{Enabled: true},
		},
	}
	proxy := newProxyWithCleanup(t, conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	assert.True(t, conf.Balancing.AutoPull.Enabled)
}

// TestAutoPull_Scenario_DisabledByDefault — autopull disabled by default.
func TestAutoPull_Scenario_DisabledByDefault(t *testing.T) {
	t.Parallel()
	conf := &types.LoadBalancerConfig{}
	assert.False(t, conf.Balancing.AutoPull.Enabled)
}

// TestAutoPull_Scenario_ProxyInitializesWithAutoPull — proxy initializes
// with autopull manager when enabled.
func TestAutoPull_Scenario_ProxyInitializesWithAutoPull(t *testing.T) {
	t.Parallel()
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: "127.0.0.1", OllamaPort: 11434, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			AutoPull:  types.AutoPullConfig{Enabled: true},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	require.NotPanics(t, func() {
		proxy := newProxyWithCleanup(t, conf)
		proxy.SetQueueManagerProxy()
		defer proxy.queueMgr.Stop()
	})
}

// TestAutoPull_Scenario_RemainingModels_Listed — auto-pull для missing
// models triggers pull на backend.
func TestAutoPull_Scenario_RemainingModels_Listed(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			AutoPull:  types.AutoPullConfig{Enabled: true},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := newProxyWithCleanup(t, conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Config validation прошла.
	assert.True(t, conf.Balancing.AutoPull.Enabled)
}

// =====================================================================
// Replication: live scenarios
// =====================================================================

// TestReplication_Scenario_LiveRequestReplicated —
// запрос к replicated model маршрутизируется на любой из targetBackends.
func TestReplication_Scenario_LiveRequestReplicated(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	o2 := newOllamaFake(t, "o2")

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Status: types.StatusHealthy},
			{ID: "o2", Name: "o2", Host: o2.host, OllamaPort: o2.port, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := newProxyWithCleanup(t, conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w, req)
	}))
	defer balancer.Close()

	// 10 запросов — round-robin между o1 и o2.
	for i := 0; i < 10; i++ {
		resp, err := http.Post(balancer.URL+"/api/generate", "application/json",
			strings.NewReader(`{"model":"llama","prompt":"hi","stream":false}`))
		require.NoError(t, err)
		resp.Body.Close()
	}

	// Both backends get requests.
	assert.GreaterOrEqual(t, o1.calls.Load(), int64(1), "o1 should get requests")
	assert.GreaterOrEqual(t, o2.calls.Load(), int64(1), "o2 should get requests")
	t.Logf("replicated: o1=%d o2=%d", o1.calls.Load(), o2.calls.Load())
}

// TestReplication_Scenario_OneBackendDown_StillWorks —
// если один из replicated backends down, другой принимает запросы.
func TestReplication_Scenario_OneBackendDown_StillWorks(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	o2 := newOllamaFake(t, "o2")

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Status: types.StatusHealthy},
			{ID: "o2", Name: "o2", Host: o2.host, OllamaPort: o2.port, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := newProxyWithCleanup(t, conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Mark o1 unhealthy.
	proxy.mu.Lock()
	proxy.backends["o1"].Backend.Status = types.StatusUnhealthy
	proxy.mu.Unlock()

	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w, req)
	}))
	defer balancer.Close()

	// Запросы — только на o2.
	for i := 0; i < 5; i++ {
		resp, err := http.Post(balancer.URL+"/api/generate", "application/json",
			strings.NewReader(`{"model":"llama","prompt":"hi","stream":false}`))
		require.NoError(t, err)
		resp.Body.Close()
	}

	assert.Equal(t, int64(0), o1.calls.Load(), "o1 unhealthy: should get 0 calls")
	assert.GreaterOrEqual(t, o2.calls.Load(), int64(1), "o2 should get all calls")
}

// =====================================================================
// Concurrent replication
// =====================================================================

// replProbeBodyPrefix — сколько байт тела ответа хватает, чтобы опознать
// источник отказа по тексту (диагностика долга 12l).
//
// ЗАЧЕМ 200: тела отказов балансера короткие и самодостаточные, и все известные
// ветки укладываются в них целиком, с запасом: `Service overloaded` — 18 байт,
// `Service unavailable - all backends busy` — 39, `... all backends failed` — 41,
// `... no healthy backends for read endpoint` — 59 (плюс перевод строки от
// `http.Error`). 200 байт хватает и на JSON-тело (`{"error":...}`), а полный
// ответ фейка в 100 параллельных запросах в лог не лезет.
const replProbeBodyPrefix = 200

// replProbeRecord — что удалось выяснить про ОДИН запрос из N.
//
// ЗАЧЕМ. Долг 12l: падение «expected 100, actual 99» случилось ОДИН раз в
// полном прогоне `go test -tags llama_stub ./...`, и разобрать его было нечем —
// тест считал только «код 200 или нет», не записывая ни кода, ни тела, ни
// длительности. По такому выводу нельзя отличить 503 очереди/допуска от 502
// бэкенда и от обрыва связи клиента.
//
// Каждый goroutine пишет в СВОЙ индекс среза (results[i]) — поэтому ни мьютекса,
// ни atomic на строки не нужно, а -race остаётся чистым.
type replProbeRecord struct {
	err         error
	elapsed     time.Duration
	status      int
	contentType string
	requestID   string
	backendID   string
	retryAfter  string
	queueWait   string
	body        string
	truncated   bool
}

// String — одна строка на запрос для примера в логе падения.
func (r replProbeRecord) String() string {
	if r.err != nil {
		return fmt.Sprintf("transport error after %s: %v", r.elapsed.Round(time.Millisecond), r.err)
	}
	out := fmt.Sprintf("status=%d elapsed=%s content-type=%q request-id=%q backend=%q",
		r.status, r.elapsed.Round(time.Millisecond), r.contentType, r.requestID, r.backendID)
	if r.retryAfter != "" {
		out += fmt.Sprintf(" retry-after=%q", r.retryAfter)
	}
	if r.queueWait != "" {
		out += fmt.Sprintf(" queue-wait-ms=%q", r.queueWait)
	}
	if r.truncated {
		out += fmt.Sprintf(" body(первые %d байт)=%q", replProbeBodyPrefix, r.body)
	} else {
		out += fmt.Sprintf(" body=%q", r.body)
	}
	return out
}

// replProbeClient — клиент пробника с запасом на 100 одновременных соединений.
//
// ЗАЧЕМ НЕ http.Post (R493, долг 12l): 100 горутин одновременно открывают
// соединения к 127.0.0.1, а `http.DefaultClient` держит общий пул с малым
// MaxIdleConnsPerHost=2. На раннере это давало 1 из 100 ошибок «dial …
// Only one usage of each socket address» — клиент не мог занять эфемерный порт,
// хотя балансер тут ни при чём (запрос до него не доходил). Ниже — свой пул,
// чтобы соединения переиспользовались, а не открывались заново.
var replProbeClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 200,
		IdleConnTimeout:     30 * time.Second,
	},
}

// isClientDialError — ошибка «не удалось даже соединиться» (а не ответ сервера).
//
// Отличать обязательно: dial-ошибка означает, что балансер НЕ отказал, а клиент
// не смог дозвониться. Такая ошибка повторяется один раз (см. replProbe); если и
// повтор не соединился — она попадает в результат, и проверка падает, то есть
// реальная недоступность балансера по-прежнему валит тест.
func isClientDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

// replProbe — выполняет один запрос и записывает в results[idx] всё, что нужно
// для разбора. Смысл проверки не меняется: успех по-прежнему ровно «200»; тело
// читается только чтобы попасть в лог падения (фейк отвечает ~40 байтами JSON,
// поэтому это не влияет ни на длительность, ни на переиспользование соединений
// сколько-нибудь заметно).
func replProbe(balancerURL string, idx int, results []replProbeRecord) {
	started := time.Now()
	resp, err := replProbeClient.Post(balancerURL+"/api/generate", "application/json",
		strings.NewReader(`{"model":"llama","prompt":"hi","stream":false}`))
	if err != nil && isClientDialError(err) {
		// Долг 12l: исчерпание эфемерных портов на раннере. Один повтор с паузой;
		// счётчик повторов не нужен — важно, что ПОСЛЕ повтора ошибка всё ещё
		// валит тест (это проверяется отсутствием третьей попытки).
		time.Sleep(20 * time.Millisecond)
		resp, err = replProbeClient.Post(balancerURL+"/api/generate", "application/json",
			strings.NewReader(`{"model":"llama","prompt":"hi","stream":false}`))
	}
	if err != nil {
		results[idx] = replProbeRecord{err: err, elapsed: time.Since(started)}
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, replProbeBodyPrefix+1))
	rec := replProbeRecord{
		elapsed:     time.Since(started),
		status:      resp.StatusCode,
		contentType: resp.Header.Get("Content-Type"),
		requestID:   resp.Header.Get("X-Request-ID"),
		backendID:   resp.Header.Get("X-Backend-ID"),
		retryAfter:  resp.Header.Get("Retry-After"),
		queueWait:   resp.Header.Get("X-Queue-Wait-Ms"),
		body:        string(body),
	}
	if len(rec.body) > replProbeBodyPrefix {
		rec.body = rec.body[:replProbeBodyPrefix]
		rec.truncated = true
	}
	results[idx] = rec
}

// logReplicatedProbeDiagnostics — печатает гистограмму статусов и до 3 примеров
// тел. Вызывается всегда (одна строка сводки), подробности — только когда что-то
// не так: иначе 100 запросов в каждом прогоне засоряли бы вывод.
func logReplicatedProbeDiagnostics(t *testing.T, results []replProbeRecord, o1, o2 *ollamaFakeServer, proxy *Proxy) {
	t.Helper()

	byStatus := map[int]int{}
	byBackend := map[string]int{}
	var transport, ok int
	durs := make([]time.Duration, 0, len(results))
	for _, r := range results {
		if r.err != nil {
			transport++
			continue
		}
		byStatus[r.status]++
		if r.backendID != "" {
			byBackend[r.backendID]++
		}
		if r.status == http.StatusOK {
			ok++
		}
		durs = append(durs, r.elapsed)
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	pct := func(p float64) time.Duration {
		if len(durs) == 0 {
			return 0
		}
		i := int(float64(len(durs)-1) * p)
		return durs[i].Round(time.Millisecond)
	}

	// Гистограмма по возрастанию кода — чтобы вывод был стабильным и сравнимым
	// между прогонами.
	codes := make([]int, 0, len(byStatus))
	for code := range byStatus {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	hist := make([]string, 0, len(codes))
	for _, code := range codes {
		hist = append(hist, fmt.Sprintf("%d×%d", code, byStatus[code]))
	}
	// Куда реально ушли запросы (X-Backend-ID отдаёт сам балансер): если
	// гистограмма показывает только один узел, проверка «replicated» ничего не
	// проверила — это тоже видно без разбора тела.
	routed := make([]string, 0, len(byBackend))
	for id, n := range byBackend {
		routed = append(routed, fmt.Sprintf("%s=%d", id, n))
	}
	sort.Strings(routed)
	t.Logf("concurrent probe: n=%d ok=%d transport_errors=%d statuses=[%s] routed=[%s] "+
		"duration p50=%s p95=%s max=%s backend_calls o1=%d o2=%d",
		len(results), ok, transport, strings.Join(hist, " "), strings.Join(routed, " "),
		pct(0.50), pct(0.95), pct(1.0), o1.calls.Load(), o2.calls.Load())

	if ok == len(results) {
		return
	}

	// Состояние бэкендов на момент разбора: если балансер пометил узел
	// unhealthy по одному неудачному запросу, следующие 503 придут уже отсюда.
	proxy.mu.RLock()
	states := make([]string, 0, len(proxy.backends))
	for id, st := range proxy.backends {
		states = append(states, fmt.Sprintf("%s=%s", id, st.Backend.Status))
	}
	proxy.mu.RUnlock()
	sort.Strings(states)
	t.Logf("PROBE backends after run: %s", strings.Join(states, " "))

	shown := 0
	for i, r := range results {
		if shown == 3 {
			break
		}
		if r.err == nil && r.status == http.StatusOK {
			continue
		}
		t.Logf("PROBE example #%d (request %d/%d): %s", shown+1, i, len(results), r)
		shown++
	}
}

// TestReplication_Scenario_ConcurrentRequests_Replicated —
// 100 concurrent запросов к replicated model.
//
// Диагностика долга 12l: смысл проверки не изменился (успех = 200, ожидаем все
// 100), но каждый запрос теперь записывает код, тело (первые 200 байт),
// Content-Type, X-Request-ID, Retry-After, X-Queue-Wait-Ms и длительность, а на
// провале печатается гистограмма статусов и до 3 примеров. Раньше вывод
// «expected 100, actual 99» не позволял отличить 503 допуска от 502 бэкенда.
func TestReplication_Scenario_ConcurrentRequests_Replicated(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	o2 := newOllamaFake(t, "o2")

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Status: types.StatusHealthy},
			{ID: "o2", Name: "o2", Host: o2.host, OllamaPort: o2.port, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := newProxyWithCleanup(t, conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w, req)
	}))
	defer balancer.Close()

	const N = 100
	var wg sync.WaitGroup
	var success, totalErr int64
	// results[i] пишет только goroutine i — гонок нет, читаем после wg.Wait().
	results := make([]replProbeRecord, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			replProbe(balancer.URL, idx, results)
			if results[idx].err != nil {
				atomic.AddInt64(&totalErr, 1)
				return
			}
			if results[idx].status == http.StatusOK {
				atomic.AddInt64(&success, 1)
			} else {
				atomic.AddInt64(&totalErr, 1)
			}
		}(i)
	}
	wg.Wait()

	logReplicatedProbeDiagnostics(t, results, o1, o2, proxy)

	assert.Equal(t, int64(N), success, "all requests should succeed")
	// Отдельная строка для транспорта: она НЕ меняет исход (обрыв связи тоже
	// даёт success<100 и валит первый assert), но в выводе сразу видно, что это
	// не код ответа, а ошибка клиента — иначе разбор снова начинается с нуля.
	assert.Zero(t, totalErr, "no request may fail (transport error or non-200)")
}

// TestReplication_Scenario_BackendRecovery —
// unhealthy backend recovers, accepts requests again.
func TestReplication_Scenario_BackendRecovery(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := newProxyWithCleanup(t, conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Mark unhealthy → mark healthy.
	proxy.mu.Lock()
	proxy.backends["o1"].Backend.Status = types.StatusUnhealthy
	proxy.mu.Unlock()

	time.Sleep(100 * time.Millisecond)

	proxy.mu.Lock()
	proxy.backends["o1"].Backend.Status = types.StatusHealthy
	proxy.mu.Unlock()

	// Verify state restored.
	proxy.mu.RLock()
	state := proxy.backends["o1"]
	status := state.Backend.Status
	proxy.mu.RUnlock()
	assert.Equal(t, types.StatusHealthy, status)
}

// =====================================================================
// Helper
// =====================================================================

// Force imports alive.
var _ = fmt.Sprintf
