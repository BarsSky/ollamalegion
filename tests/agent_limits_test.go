package tests

import (
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// newTestProxySimple — минимальный proxy для unit-тестов balancing
func newTestProxySimple(t *testing.T) *balancer.Proxy {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 18081, StatePath: "testdata/state.json"},
		Backends: []types.Backend{
			{ID: "test-agent-1", Name: "test-agent-1", Host: "192.168.1.100", OllamaPort: 11434, AgentPort: 9090, Weight: 100, MaxConcurrentReqs: 8, Status: types.StatusHealthy, HasAgent: true},
		},
		Balancing: types.BalancingSettings{
			ModelAffinity:     false,
			SessionStickiness: false,
			RequestTimeout:    30,
			FirstByteTimeout:  2,
			Prewarm:           types.PrewarmConfig{Enabled: false},
		},
	}
	proxy := balancer.NewProxy(cfg)
	proxy.AddBackend(cfg.Backends[0])
	return proxy
}

// TestAgentReceivesRuntimeLimits — проверяет установку runtime-лимитов
func TestAgentReceivesRuntimeLimits(t *testing.T) {
	proxy := newTestProxySimple(t)
	err := proxy.UpdateBackendLimits("test-agent-1", 4, 6)
	if err != nil {
		t.Fatalf("UpdateBackendLimits failed: %v", err)
	}

	state := proxy.GetBackendState("test-agent-1")
	if state == nil {
		t.Fatal("backend state not found")
	}
	if state.Backend.RuntimeMaxModels != 4 {
		t.Errorf("expected RuntimeMaxModels=4, got %d", state.Backend.RuntimeMaxModels)
	}
	if state.Backend.RuntimeMaxConcurrentRequests != 6 {
		t.Errorf("expected RuntimeMaxConcurrentRequests=6, got %d", state.Backend.RuntimeMaxConcurrentRequests)
	}
	_ = time.Now()
}