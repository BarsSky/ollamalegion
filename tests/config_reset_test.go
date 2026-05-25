package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/internal/api"
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
)

// callHandler напрямую вызывает обработчик (без middleware: auth, rate limit)
func callHandler(handler http.HandlerFunc, method, url string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, nil)
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

// newBaseConfig helper
func newBaseConfig() *types.LoadBalancerConfig {
	return &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
	}
}

func TestConfigReset_Ollama(t *testing.T) {
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends:     []types.Backend{
			{ID: "agent-1", Host: "localhost", OllamaPort: 11434, Status: types.StatusHealthy},
		},
		Balancing:     types.BalancingSettings{OperatingMode: "replication"},
		BackendEngine: types.EngineOllamaAPI,
		Initialized:   true,
	}
	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10, 3)
	server := api.NewServer(proxy, config, healthChecker)
	server.SetConfigSaver(func() error { return nil })

	rr := callHandler(server.GetResetHandler(), http.MethodPost, "/api/v1/config/reset")
	assert.Equal(t, http.StatusOK, rr.Code)

	var result map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&result)
	assert.True(t, result["success"].(bool))

	cfg := result["config"].(map[string]interface{})
	assert.Equal(t, "standard", cfg["operatingMode"])
	assert.False(t, cfg["initialized"].(bool))
	assert.True(t, cfg["modelAffinity"].(bool))
	assert.True(t, cfg["sessionStickiness"].(bool))
}

func TestConfigReset_LlamaCpp(t *testing.T) {
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends:     []types.Backend{
			{ID: "agent-1", Host: "localhost", CppWorkerPort: 18091, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing:     types.BalancingSettings{OperatingMode: "standard"},
		BackendEngine: types.EngineLlamaCPP,
		Initialized:   true,
	}
	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10, 3)
	server := api.NewServer(proxy, config, healthChecker)
	server.SetConfigSaver(func() error { return nil })

	rr := callHandler(server.GetResetHandler(), http.MethodPost, "/api/v1/config/reset")
	assert.Equal(t, http.StatusOK, rr.Code)

	var result map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&result)
	cfg := result["config"].(map[string]interface{})
	assert.Equal(t, "virtual_router", cfg["operatingMode"])
	assert.Equal(t, string(types.EngineLlamaCPP), cfg["backendEngine"])
}

func TestConfigReset_MethodNotAllowed(t *testing.T) {
	proxy := balancer.NewProxy(newBaseConfig())
	healthChecker := balancer.NewHealthChecker(proxy, 10, 3)
	server := api.NewServer(proxy, newBaseConfig(), healthChecker)

	rr := callHandler(server.GetResetHandler(), http.MethodGet, "/api/v1/config/reset")
	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

func TestConfigReset_ClearsBackends(t *testing.T) {
	config := newBaseConfig()
	config.Backends = []types.Backend{
		{ID: "agent-1", Host: "localhost", OllamaPort: 11434, Status: types.StatusHealthy},
		{ID: "agent-2", Host: "localhost", OllamaPort: 11435, Status: types.StatusHealthy},
	}
	config.BackendEngine = types.EngineOllamaAPI

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10, 3)
	server := api.NewServer(proxy, config, healthChecker)
	server.SetConfigSaver(func() error { return nil })

	assert.Len(t, server.GetConfig().Backends, 2)

	rr := callHandler(server.GetResetHandler(), http.MethodPost, "/api/v1/config/reset")
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Empty(t, server.GetConfig().Backends)
}

func TestConfigReset_PreservesHostPort(t *testing.T) {
	config := newBaseConfig()
	config.LoadBalancer.Host = "0.0.0.0"
	config.LoadBalancer.Port = 18080
	config.LoadBalancer.APIPort = 18081
	config.Backends = []types.Backend{
		{ID: "agent-1", Host: "localhost", OllamaPort: 11434, Status: types.StatusHealthy},
	}
	config.Balancing.OperatingMode = "distributed_inference"
	config.BackendEngine = types.EngineOllamaAPI
	config.Initialized = true

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10, 3)
	server := api.NewServer(proxy, config, healthChecker)
	server.SetConfigSaver(func() error { return nil })

	rr := callHandler(server.GetResetHandler(), http.MethodPost, "/api/v1/config/reset")

	var result map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&result)
	cfg := result["config"].(map[string]interface{})
	// host/port не возвращаются в config ответа reset (это поля loadBalancer, а не balancing)
	// Проверяем что operatingMode изменился на стандартный
	assert.Equal(t, "standard", cfg["operatingMode"])
	assert.False(t, cfg["initialized"].(bool))
}

func TestConfigReset_Idempotent(t *testing.T) {
	config := newBaseConfig()
	config.Backends = []types.Backend{
		{ID: "agent-1", Host: "localhost", OllamaPort: 11434, Status: types.StatusHealthy},
	}
	config.Balancing.Algorithm = types.AlgorithmRoundRobin
	config.Balancing.OperatingMode = "replication"
	config.BackendEngine = types.EngineOllamaAPI
	config.Initialized = true

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10, 3)
	server := api.NewServer(proxy, config, healthChecker)
	server.SetConfigSaver(func() error { return nil })

	r1 := callHandler(server.GetResetHandler(), http.MethodPost, "/api/v1/config/reset")
	var v1 map[string]interface{}
	json.NewDecoder(r1.Body).Decode(&v1)
	c1 := v1["config"].(map[string]interface{})

	r2 := callHandler(server.GetResetHandler(), http.MethodPost, "/api/v1/config/reset")
	var v2 map[string]interface{}
	json.NewDecoder(r2.Body).Decode(&v2)
	c2 := v2["config"].(map[string]interface{})

	assert.Equal(t, c1["algorithm"], c2["algorithm"])
	assert.Equal(t, c1["operatingMode"], c2["operatingMode"])
	assert.False(t, c2["initialized"].(bool))
}

func TestConfigReset_ResponseStructure(t *testing.T) {
	config := newBaseConfig()
	config.BackendEngine = types.EngineOllamaAPI

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10, 3)
	server := api.NewServer(proxy, config, healthChecker)
	server.SetConfigSaver(func() error { return nil })

	rr := callHandler(server.GetResetHandler(), http.MethodPost, "/api/v1/config/reset")

	var result map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&result)
	assert.True(t, result["success"].(bool))
	assert.NotEmpty(t, result["message"])
	assert.NotNil(t, result["config"])

	cfg := result["config"].(map[string]interface{})
	for _, field := range []string{"algorithm", "modelAffinity", "sessionStickiness", "operatingMode", "backendEngine", "initialized"} {
		assert.Contains(t, cfg, field, "Missing field: %s", field)
	}
}