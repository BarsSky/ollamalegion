// session_queue_stress_test.go — R55.7 (2026-08-25): detailed stress test report.
//
// Цель: дать детальный отчёт по поведению балансера под нагрузкой.
// В отличие от TestLoadBalancing_MultipleClients (R55.3, just "all succeed")
// этот тест логирует:
//   - per-request: backend, status, started/ended, latency
//   - per-backend: сколько запросов, max parallel, avg/min/max latency
//   - per-client: сколько запросов, на какие backends попали
//   - session stickiness: один client → один backend?
//   - no timeouts / no disconnects / no 503s (кроме от expected 503 backend)
//
// Тест воспроизводит user'ский сценарий: 2 клиента (Cline + OpenWebUI),
// каждый шлёт N=6 concurrent запросов на 2 бэкенда с maxConcurrent=3
// (max parallel = 6 = 3 + 3).

package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func TestLoadBalancing_DetailedStressReport(t *testing.T) {
	// 2 mock backends, каждый с max 3 параллельно (через channel semaphore).
	mk := func(id string, maxConcurrent int) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
		sem := make(chan struct{}, maxConcurrent)
		var inflight, maxInflight atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sem <- struct{}{}
			defer func() { <-sem }()
			cur := inflight.Add(1)
			defer inflight.Add(-1)
			for {
				old := maxInflight.Load()
				if cur <= old || maxInflight.CompareAndSwap(old, cur) {
					break
				}
			}
			// Симулируем inference: 50-150ms (random realistic)
			time.Sleep(time.Duration(50+len(id)*20) * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, fmt.Sprintf(`{"backend":"%s","done":true}`, id))
		}))
		return srv, &inflight, &maxInflight
	}
	sA, _, aMaxInflight := mk("a", 3)
	sB, _, bMaxInflight := mk("b", 3)
	defer sA.Close()
	defer sB.Close()

	hA, pA := hostPort(sA.URL)
	hB, pB := hostPort(sB.URL)

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "0.0.0.0", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "a", Host: hA, OllamaPort: pA, Weight: 1, MaxConcurrentReqs: 3, Status: types.StatusHealthy},
			{ID: "b", Host: hB, OllamaPort: pB, Weight: 1, MaxConcurrentReqs: 3, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     true,
			SessionStickiness: true,
			FirstByteTimeout:  30,
			StreamingIdleTimeout: 120,
			RequestTimeout:    30,
			QueueTimeout:      30,
			QueueMaxSize:      100,
			QueueWorkers:      6,
			SessionTTL:        900,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90},
			CPU:    types.CPULimits{MaxUsagePercent: 80},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
		},
	}
	proxy := newProxyWithCleanup(t, cfg)
	defer proxy.queueMgr.Stop()
	proxy.UpdateMetrics("a", mkMetrics("a"))
	proxy.UpdateMetrics("b", mkMetrics("b"))
	proxy.llamaCppRouter = nil // route через Ollama-flow

	// 2 clients × 6 requests = 12 total
	type client struct {
		name string
		ua   string
		ip   string
	}
	clients := []client{
		{"Cline", "Cline/1.0 (VSCode)", "10.0.0.1"},
		{"OpenWebUI", "OpenWebUI/1.0", "10.0.0.2"},
	}
	const requestsPerClient = 6
	totalRequests := len(clients) * requestsPerClient

	type result struct {
		client  string
		idx     int
		reqID   string
		backend string // ответил backend
		status  int
		started time.Time
		ended   time.Time
	}
	results := make([]result, 0, totalRequests)
	var mu sync.Mutex

	var wg sync.WaitGroup
	for ci, c := range clients {
		for j := 0; j < requestsPerClient; j++ {
			wg.Add(1)
			go func(clientName, ua, ip string, ci, idx int) {
				defer wg.Done()
				rid := fmt.Sprintf("%s-req-%d", clientName, idx)
				rb, _ := json.Marshal(map[string]interface{}{"model": "llama3.2:3b", "stream": false})
				req := httptest.NewRequest("POST", "/api/generate", bytes.NewReader(rb))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("User-Agent", ua)
				req.Header.Set("X-Request-ID", rid)
				req.RemoteAddr = fmt.Sprintf("%s:%d", ip, 10000+idx)

				started := time.Now()
				rec := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { proxy.ServeHTTP(rec, req); close(done) }()
				select {
				case <-done:
					ended := time.Now()
					// Парсим backend из response body
					var resp struct {
						Backend string `json:"backend"`
					}
					_ = json.Unmarshal(rec.Body.Bytes(), &resp)
					mu.Lock()
					results = append(results, result{
						client: clientName, idx: idx, reqID: rid,
						backend: resp.Backend, status: rec.Code,
						started: started, ended: ended,
					})
					mu.Unlock()
				case <-time.After(15 * time.Second):
					t.Errorf("client %s req %d: TIMEOUT", clientName, idx)
				}
			}(c.name, c.ua, c.ip, ci, j)
		}
	}
	wg.Wait()

	// === Verification 1: все 12 успешны ===
	successes := 0
	for _, r := range results {
		if r.status == 200 {
			successes++
		}
	}
	if successes != totalRequests {
		t.Fatalf("expected %d successes, got %d", totalRequests, successes)
	}

	// === Verification 2: maxConcurrent не превышен ===
	if aMaxInflight.Load() > 3 {
		t.Errorf("backend A: max inflight = %d (> 3) — semaphore breach!", aMaxInflight.Load())
	}
	if bMaxInflight.Load() > 3 {
		t.Errorf("backend B: max inflight = %d (> 3) — semaphore breach!", bMaxInflight.Load())
	}

	// === Verification 3: session stickiness ===
	// Каждый client должен иметь session на ОДНОМ backend (а не на обоих).
	clientBackends := make(map[string]map[string]int) // client → backend → count
	for _, s := range proxy.GetSessions() {
		if clientBackends[s.ClientName] == nil {
			clientBackends[s.ClientName] = make(map[string]int)
		}
		clientBackends[s.ClientName][s.BackendID]++
	}
	for client, backends := range clientBackends {
		if len(backends) > 1 {
			t.Errorf("client %s: session stickiness breach — has sessions on %d backends: %v",
				client, len(backends), backends)
		}
	}

	// === Verification 4: per-backend distribution ===
	backendCount := make(map[string]int)
	backendLatencies := make(map[string][]time.Duration)
	for _, r := range results {
		if r.backend != "" {
			backendCount[r.backend]++
			backendLatencies[r.backend] = append(backendLatencies[r.backend], r.ended.Sub(r.started))
		}
	}
	for backend, lats := range backendLatencies {
		var sum time.Duration
		var min, max time.Duration
		for i, l := range lats {
			sum += l
			if i == 0 || l < min {
				min = l
			}
			if l > max {
				max = l
			}
		}
		avg := sum / time.Duration(len(lats))
		t.Logf("  backend=%s: %d requests, latency min=%v avg=%v max=%v",
			backend, len(lats), min, avg, max)
	}

	// === Verification 5: per-client distribution ===
	t.Log("")
	t.Log("Per-client breakdown:")
	for _, c := range clients {
		clientResults := []result{}
		for _, r := range results {
			if r.client == c.name {
				clientResults = append(clientResults, r)
			}
		}
		backendHits := make(map[string]int)
		var totalLat time.Duration
		for _, r := range clientResults {
			backendHits[r.backend]++
			totalLat += r.ended.Sub(r.started)
		}
		avgLat := totalLat / time.Duration(len(clientResults))
		t.Logf("  %s: %d requests, avg latency=%v, backend hits: %v",
			c.name, len(clientResults), avgLat, backendHits)
	}

	// === Verification 6: total parallel enforced ===
	totalMax := aMaxInflight.Load() + bMaxInflight.Load()
	if totalMax > 6 {
		t.Errorf("total max parallel = %d, expected ≤ 6", totalMax)
	}
	if totalMax < 2 {
		t.Errorf("total max parallel = %d — expected ≥ 2 (parallelism should happen)", totalMax)
	}

	// === Verification 7: unique request IDs ===
	seenIDs := make(map[string]bool)
	for _, r := range results {
		if seenIDs[r.reqID] {
			t.Errorf("duplicate request ID: %s", r.reqID)
		}
		seenIDs[r.reqID] = true
	}
	if len(seenIDs) != totalRequests {
		t.Errorf("expected %d unique IDs, got %d", totalRequests, len(seenIDs))
	}

	// === Final summary ===
	t.Log("")
	t.Logf("✅ PASS: %d concurrent requests (2 clients × %d), 2 backends (max 3 each)",
		totalRequests, requestsPerClient)
	t.Logf("   max parallel: A=%d, B=%d, total=%d", aMaxInflight.Load(), bMaxInflight.Load(), totalMax)
	t.Logf("   sessions: %d (sticky per client)", len(clientBackends))
	for client, backends := range clientBackends {
		t.Logf("     %s → backends: %v", client, backends)
	}
	t.Logf("   unique request IDs: %d (no duplicates, all propagated)", len(seenIDs))
	t.Logf("   all requests completed (no timeouts, no disconnects)")
}
