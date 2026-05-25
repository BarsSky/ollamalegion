package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/internal/api"
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
)

// newEIServer creates a server for export/import tests
func newEIServer(t *testing.T) (*api.Server, *types.LoadBalancerConfig) {
	t.Helper()
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:           "0.0.0.0",
			Port:           18080,
			APIPort:        18081,
			TLSPort:        8443,
			TrustedProxies: []string{"127.0.0.1/8"},
			StatePath:      "data/state.json",
		},
		Backends: []types.Backend{
			{ID: "agent-1", Name: "GPU Server 1", Host: "10.0.0.1", OllamaPort: 11434, AgentPort: 18032, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
			{ID: "agent-2", Name: "GPU Server 2", Host: "10.0.0.2", OllamaPort: 11434, AgentPort: 18032, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
		},
		BackendEngine: types.EngineOllamaAPI,
		Initialized:   true,
		Balancing: types.BalancingSettings{
			Algorithm:          types.AlgorithmResourceAware,
			ModelAffinity:      true,
			SessionStickiness:  true,
			UseEnhancedScoring: true,
			OperatingMode:      "replication",
			ModelReplication: types.ModelReplicationConfig{
				Enabled:             true,
				DefaultMinInstances: 2,
				DefaultMaxInstances: 5,
			},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 85, MaxTemperature: 85},
			CPU:    types.CPULimits{MaxUsagePercent: 80},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
			Disk:   types.DiskLimits{MinFreeMB: 10240},
		},
	}
	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10, 3)
	server := api.NewServer(proxy, config, healthChecker)
	server.SetConfigSaver(func() error { return nil })
	return server, config
}

func TestConfigExport_Full(t *testing.T) {
	server, _ := newEIServer(t)

	rr := callHandler(server.GetExportHandler(), http.MethodGet, "/api/v1/config/export")
	assert.Equal(t, http.StatusOK, rr.Code)

	var result map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&result)
	assert.True(t, result["success"].(bool))
	assert.NotEmpty(t, result["exportedAt"])

	cfg := result["config"].(map[string]interface{})
	assert.Contains(t, cfg, "loadBalancer")
	assert.Contains(t, cfg, "backends")
	assert.Contains(t, cfg, "balancing")

	backends := cfg["backends"].([]interface{})
	assert.Len(t, backends, 2)
}

func TestConfigExport_Empty(t *testing.T) {
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
	}
	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10, 3)
	server := api.NewServer(proxy, config, healthChecker)

	rr := callHandler(server.GetExportHandler(), http.MethodGet, "/api/v1/config/export")
	assert.Equal(t, http.StatusOK, rr.Code)

	var result map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&result)
	assert.True(t, result["success"].(bool))
}

func TestConfigImport_Valid(t *testing.T) {
	server, _ := newEIServer(t)

	newCfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmLeastConn,
			OperatingMode: "standard",
		},
		Backends: []types.Backend{
			{ID: "agent-new", Host: "10.0.0.3", OllamaPort: 11434, Status: types.StatusStarting, Type: types.BackendTypeOllama},
		},
		BackendEngine: types.EngineOllamaAPI,
	}
	payload, _ := json.Marshal(map[string]interface{}{"config": newCfg})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/import", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.GetImportHandler()(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var result map[string]interface{}
	json.NewDecoder(w.Body).Decode(&result)
	assert.True(t, result["success"].(bool))
	assert.Len(t, server.GetConfig().Backends, 1)
	assert.Equal(t, "agent-new", server.GetConfig().Backends[0].ID)
}

func TestConfigImport_InvalidJSON(t *testing.T) {
	server, _ := newEIServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/import", bytes.NewReader([]byte(`{invalid`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.GetImportHandler()(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestConfigImport_MissingConfig(t *testing.T) {
	server, _ := newEIServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/import", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.GetImportHandler()(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestConfigImport_Merge(t *testing.T) {
	server, _ := newEIServer(t)
	assert.Len(t, server.GetConfig().Backends, 2)

	partial := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{OperatingMode: "standard"},
		Backends: []types.Backend{
			{ID: "agent-merged", Host: "localhost", OllamaPort: 11434, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
		},
	}
	payload, _ := json.Marshal(map[string]interface{}{"config": partial, "merge": true})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/import", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.GetImportHandler()(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, server.GetConfig().Backends, 1)
	assert.Equal(t, "agent-merged", server.GetConfig().Backends[0].ID)
	assert.Equal(t, "standard", server.GetConfig().Balancing.OperatingMode)
}

func TestConfigImport_InvalidMode(t *testing.T) {
	server, _ := newEIServer(t)

	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{OperatingMode: "invalid_mode_xyz"},
	}
	payload, _ := json.Marshal(map[string]interface{}{"config": cfg})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/import", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.GetImportHandler()(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestConfig_WrongMethods(t *testing.T) {
	server, _ := newEIServer(t)

	// GET on import → 405
	rr := callHandler(server.GetImportHandler(), http.MethodGet, "/api/v1/config/import")
	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)

	// POST on export → 405
	rr2 := callHandler(server.GetExportHandler(), http.MethodPost, "/api/v1/config/export")
	assert.Equal(t, http.StatusMethodNotAllowed, rr2.Code)
}

func TestConfigExport_BackendDetails(t *testing.T) {
	server, _ := newEIServer(t)

	rr := callHandler(server.GetExportHandler(), http.MethodGet, "/api/v1/config/export")
	var result map[string]interface{}
	json.NewDecoder(rr.Body).Decode(&result)

	cfg := result["config"].(map[string]interface{})
	backends := cfg["backends"].([]interface{})

	b1 := backends[0].(map[string]interface{})
	assert.Equal(t, "agent-1", b1["id"])
	assert.Equal(t, "GPU Server 1", b1["name"])
	assert.Equal(t, float64(11434), b1["ollamaPort"])
	assert.Equal(t, "healthy", b1["status"])
}

func TestConfigRoundtrip(t *testing.T) {
	server, _ := newEIServer(t)

	// Export
	r1 := callHandler(server.GetExportHandler(), http.MethodGet, "/api/v1/config/export")
	var e1 map[string]interface{}
	json.NewDecoder(r1.Body).Decode(&e1)
	c1 := e1["config"].(map[string]interface{})

	// Import back
	importCfg := mapFromExport(c1)
	payload, _ := json.Marshal(map[string]interface{}{"config": importCfg})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/import", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.GetImportHandler()(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Export again
	r2 := callHandler(server.GetExportHandler(), http.MethodGet, "/api/v1/config/export")
	var e2 map[string]interface{}
	json.NewDecoder(r2.Body).Decode(&e2)
	c2 := e2["config"].(map[string]interface{})

	assert.Equal(t, c1["backendEngine"], c2["backendEngine"])
}

func mapFromExport(exp map[string]interface{}) *types.LoadBalancerConfig {
	cfg := &types.LoadBalancerConfig{}
	if lb, ok := exp["loadBalancer"].(map[string]interface{}); ok {
		if h, _ := lb["host"].(string); h != "" {
			cfg.LoadBalancer.Host = h
		}
		if p, _ := lb["port"].(float64); p != 0 {
			cfg.LoadBalancer.Port = int(p)
		}
	}
	if be, _ := exp["backendEngine"].(string); be != "" {
		cfg.BackendEngine = types.BackendEngine(be)
	}
	if bal, ok := exp["balancing"].(map[string]interface{}); ok {
		if a, _ := bal["algorithm"].(string); a != "" {
			cfg.Balancing.Algorithm = types.BalancingAlgorithm(a)
		}
		if m, _ := bal["operatingMode"].(string); m != "" {
			cfg.Balancing.OperatingMode = m
		}
	}
	return cfg
}