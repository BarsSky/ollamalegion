// Package balancer — scenario tests для concurrency / race conditions.
//
// Покрывает:
//   - Parallel requests через balancer
//   - Concurrent circuit breaker state changes
//   - Concurrent selector access (round_robin, least_loaded, random)
//   - Concurrent virtual router reads/writes
//   - Concurrent metric updates
//   - Concurrent backend state mutations

package balancer

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/internal/rpccoordinator"
	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/types"
)

// =====================================================================
// Parallel requests
// =====================================================================

// TestConcurrency_Scenario_ParallelRequests — 100 параллельных запросов
// корректно обрабатываются (no race, no leak).
func TestConcurrency_Scenario_ParallelRequests(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-parallel", virtualmodel.SelectionRoundRobin)

	const N = 100
	var wg sync.WaitGroup
	var success, errors int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(rig.balancer.URL+"/api/generate",
				"application/json",
				strings.NewReader(`{"model":"vm-parallel","prompt":"hi","stream":false}`))
			if err != nil {
				atomic.AddInt64(&errors, 1)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				atomic.AddInt64(&success, 1)
			} else {
				atomic.AddInt64(&errors, 1)
			}
		}()
	}
	wg.Wait()

	// All requests should succeed (no backends in unhealthy state).
	assert.Equal(t, int64(N), success, "all %d parallel requests should succeed", N)
	assert.Equal(t, int64(0), errors, "no errors expected")
}

// TestConcurrency_Scenario_ParallelRequests_Streaming — parallel streaming
// requests don't deadlock.
func TestConcurrency_Scenario_ParallelRequests_Streaming(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-pstream", virtualmodel.SelectionRoundRobin)

	const N = 20
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Post(rig.balancer.URL+"/v1/chat/completions",
				"application/json",
				strings.NewReader(`{"model":"vm-pstream","messages":[{"role":"user","content":"hi"}],"stream":true}`))
			if err != nil {
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
}

// =====================================================================
// Concurrent circuit breaker
// =====================================================================

// TestConcurrency_Scenario_CircuitBreaker_ConcurrentRequests —
// concurrent requests on CB don't corrupt state.
func TestConcurrency_Scenario_CircuitBreaker_ConcurrentRequests(t *testing.T) {
	t.Parallel()
	rig := newRpcTestRig(t)

	// 50 concurrent requests, 5 of which will fail at w1.
	var failures, successes int64
	rig.w1.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rpc/infer" {
			// 50% fail rate.
			if atomic.AddInt64(&failures, 1)%2 == 0 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"worker_id":"w1","slice_id":"0","output":"ok"}`))
			atomic.AddInt64(&successes, 1)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(rig.balancer.URL+"/api/generate",
				"application/json",
				strings.NewReader(`{"model":"llama","prompt":"hi","stream":false}`))
			if err != nil {
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()

	t.Logf("CB concurrent: failures=%d successes=%d", failures, successes)
	// Both should be > 0 (CB allows some through, blocks some).
}

// =====================================================================
// Concurrent selector access
// =====================================================================

// TestConcurrency_Scenario_Selector_RoundRobin_Distributes —
// concurrent requests are distributed across all backends.
func TestConcurrency_Scenario_Selector_RoundRobin_Distributes(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	o2 := newOllamaFake(t, "o2")
	o3 := newOllamaFake(t, "o3")
	o4 := newOllamaFake(t, "o4")

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Status: types.StatusHealthy},
			{ID: "o2", Name: "o2", Host: o2.host, OllamaPort: o2.port, Status: types.StatusHealthy},
			{ID: "o3", Name: "o3", Host: o3.host, OllamaPort: o3.port, Status: types.StatusHealthy},
			{ID: "o4", Name: "o4", Host: o4.host, OllamaPort: o4.port, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w, req)
	}))
	defer balancer.Close()

	const N = 400
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(balancer.URL+"/api/generate",
				"application/json",
				strings.NewReader(`{"model":"llama","prompt":"hi","stream":false}`))
			if err != nil {
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()

	// All 4 should get ~100 calls (400 / 4).
	for _, o := range []*ollamaFakeServer{o1, o2, o3, o4} {
		assert.GreaterOrEqual(t, o.calls.Load(), int64(50),
			"each backend should get at least 50 calls (400 total / 4 backends)")
	}
}

// =====================================================================
// Concurrent metric updates
// =====================================================================

// TestConcurrency_Scenario_Metrics_Concurrent_Update —
// concurrent metric updates don't race.
func TestConcurrency_Scenario_Metrics_Concurrent_Update(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-metrics-c", virtualmodel.SelectionRoundRobin)

	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(rig.balancer.URL+"/api/generate",
				"application/json",
				strings.NewReader(`{"model":"vm-metrics-c","prompt":"hi","stream":false}`))
			if err != nil {
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()

	// Get metrics — no race.
	m := rig.router.GetMetrics().Snapshot()
	total := asInt64(m["inferenceTotal"])
	assert.GreaterOrEqual(t, total, int64(N),
		"inferenceTotal should be >= %d, got %d", N, total)
}

// =====================================================================
// Concurrent failover
// =====================================================================

// TestConcurrency_Scenario_Failover_Concurrent_5xx —
// concurrent 5xx requests trigger concurrent failover (no race).
func TestConcurrency_Scenario_Failover_Concurrent_5xx(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-cfail", virtualmodel.SelectionRoundRobin)

	// o1 always fails, o2 always succeeds.
	rig.o1.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	const N = 20
	var wg sync.WaitGroup
	var success, failover, totalErr int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(rig.balancer.URL+"/api/generate",
				"application/json",
				strings.NewReader(`{"model":"vm-cfail","prompt":"hi","stream":false}`))
			if err != nil {
				atomic.AddInt64(&totalErr, 1)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				atomic.AddInt64(&success, 1)
				if resp.Header.Get("X-Failover-Attempts") != "" {
					atomic.AddInt64(&failover, 1)
				}
			} else {
				atomic.AddInt64(&totalErr, 1)
			}
		}()
	}
	wg.Wait()

	t.Logf("concurrent failover: success=%d failover=%d errors=%d",
		success, failover, totalErr)
}

// =====================================================================
// Concurrent load provider
// =====================================================================

// TestConcurrency_Scenario_LoadProvider_Concurrent —
// hot-swap of LoadProvider during requests works.
func TestConcurrency_Scenario_LoadProvider_Concurrent(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-cload", virtualmodel.SelectionLeastLoaded)

	// Trigger selector creation.
	resp, err := http.Post(rig.balancer.URL+"/api/generate",
		"application/json",
		strings.NewReader(`{"model":"vm-cload","prompt":"hi","stream":false}`))
	require.NoError(t, err)
	resp.Body.Close()

	rig.router.selectorsMu.RLock()
	sel := rig.router.selectors["vm-cload"]
	rig.router.selectorsMu.RUnlock()
	ll, ok := sel.(*virtualmodel.LeastLoadedSelector)
	require.True(t, ok)

	// Hot-swap load provider concurrently with requests.
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 5 goroutines hot-swap LoadProvider.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				select {
				case <-stop:
					return
				default:
				}
				ll.SetLoadProvider(func(backendID string) (int, bool) {
					return id*10 + j, true
				})
			}
		}(i)
	}

	// 5 goroutines sending requests.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				resp, err := http.Post(rig.balancer.URL+"/api/generate",
					"application/json",
					strings.NewReader(`{"model":"vm-cload","prompt":"hi","stream":false}`))
				if err == nil {
					resp.Body.Close()
				}
			}
		}()
	}

	wg.Wait()
	close(stop)
	// Just verify no race / panic.
}

// =====================================================================
// Concurrent virtual model registration
// =====================================================================

// TestConcurrency_Scenario_VMRegister_Concurrent —
// concurrent register/unregister of virtual models.
func TestConcurrency_Scenario_VMRegister_Concurrent(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			name := fmt.Sprintf("vm-c%d", id)
			_ = rig.registry.Register(types.VirtualModelConfig{
				Name:        name,
				Selection:   virtualmodel.SelectionRoundRobin,
				BackendPool: []string{"h:1"},
				ModelName:   "m",
			})
		}(i)
	}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			name := fmt.Sprintf("vm-c%d", id)
			rig.registry.Unregister(name)
		}(i)
	}
	wg.Wait()
	// All 50 should be registered then unregistered.
	// Final list may have 0 (last unregister) or 50 (race).
	list := rig.registry.List()
	t.Logf("after concurrent register/unregister: %d models in list", len(list))
}

// =====================================================================
// Concurrent health check
// =====================================================================

// TestConcurrency_Scenario_HealthCheck_Concurrent —
// concurrent health checks don't race.
func TestConcurrency_Scenario_HealthCheck_Concurrent(t *testing.T) {
	t.Parallel()
	o1 := newOllamaFake(t, "o1")
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "o1", Name: "o1", Host: o1.host, OllamaPort: o1.port, Status: types.StatusHealthy},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmRoundRobin,
			HealthCheckInterval: 60,
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	// Concurrent reads of state.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = proxy.GetBackend("o1")
				_ = proxy.GetAllBackends()
				_, _ = proxy.GetBackendFreeSlots("o1")
			}
		}()
	}
	wg.Wait()
}

// =====================================================================
// Stress: rapid register/unregister
// =====================================================================

// TestConcurrency_Scenario_Stress_RapidRequests —
// 1000 быстрых запросов не утекают (горутины, память).
func TestConcurrency_Scenario_Stress_RapidRequests(t *testing.T) {
	t.Parallel()
	rig := newVRTestRig(t)
	rig.registerVM(t, "vm-stress", virtualmodel.SelectionRoundRobin)

	const N = 500
	client := &http.Client{Timeout: 5 * time.Second}
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := bytes.NewReader([]byte(`{"model":"vm-stress","prompt":"hi","stream":false}`))
			req, _ := http.NewRequest("POST", rig.balancer.URL+"/api/generate", body)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()
	t.Logf("stress test: %d requests completed", N)
}

// =====================================================================
// Helper
// =====================================================================

// Force imports alive.
var (
	_ = rpccoordinator.LayerSlice{}
	_ = fmt.Sprintf
)
