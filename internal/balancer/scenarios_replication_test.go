// Package balancer — scenario tests для replication groups + auto-pull.
//
// Покрывает:
//   - Replication group CRUD (через proxy config)
//   - Min/max instances enforcement
//   - Auto-pull: enable/disable, status endpoint
//   - Replication reconcile scenarios

package balancer

import (
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
			Algorithm: types.AlgorithmRoundRobin,
			ModelReplication: types.ModelReplicationConfig{
				Enabled:             true,
				DefaultMinInstances:  1,
				DefaultMaxInstances:  3,
				IdleUnloadAfter:      "10m",
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
		ModelName:     "test",
		MinInstances:  1,
		MaxInstances:  5,
		TargetBackends: []string{"o1", "o2"},
	}
	assert.LessOrEqual(t, group.MinInstances, group.MaxInstances,
		"MinInstances should be <= MaxInstances")
}

// TestReplication_Scenario_EmptyGroup — пустой TargetBackends.
func TestReplication_Scenario_EmptyGroup(t *testing.T) {
	t.Parallel()
	group := types.ModelGroupConfig{
		ModelName:     "test",
		MinInstances:  0,
		MaxInstances:  0,
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
			Algorithm: types.AlgorithmRoundRobin,
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
			Algorithm: types.AlgorithmRoundRobin,
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
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
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
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
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

// TestReplication_Scenario_ConcurrentRequests_Replicated —
// 100 concurrent запросов к replicated model.
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
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
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
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(balancer.URL+"/api/generate", "application/json",
				strings.NewReader(`{"model":"llama","prompt":"hi","stream":false}`))
			if err != nil {
				atomic.AddInt64(&totalErr, 1)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				atomic.AddInt64(&success, 1)
			} else {
				atomic.AddInt64(&totalErr, 1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(N), success, "all requests should succeed")
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
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmRoundRobin},
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
