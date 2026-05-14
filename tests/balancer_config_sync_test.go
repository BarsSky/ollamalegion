package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/internal/api"
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOperatingModePropagation проверяет полную цепочку:
// 1. PUT /api/v1/cluster/config с operatingMode
// 2. GET /api/v1/cluster/config — возвращает тот же operatingMode
// 3. GET /api/v1/cluster — ClusterState содержит operatingMode
func TestOperatingModePropagation(t *testing.T) {
	// Создаём минимальную конфигурацию
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Port: 18080, APIPort: 18081},
		API: types.APISettings{
			RateLimit: 10000,
			RateBurst: 20000,
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmResourceAware,
			OperatingMode: "standard",
			QueueMaxSize:  100,
			QueueTimeout:  300,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 85},
			CPU:    types.CPULimits{MaxUsagePercent: 80},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
			Disk:   types.DiskLimits{MinFreeMB: 10240},
		},
	}

	// Создаём прокси и API-сервер
	proxy := balancer.NewProxy(cfg)
	healthChecker := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := api.NewServer(proxy, cfg, healthChecker)

	// Подменяем configSaver чтобы он не пытался писать на диск
	server.SetConfigSaver(func() error { return nil })

	tests := []struct {
		name        string
		initialMode string
		targetMode  string
		wantEnabled string // какой enabled-флаг должен быть true после переключения
	}{
		{
			name:        "switch standard to replication",
			initialMode: "standard",
			targetMode:  "replication",
			wantEnabled: "modelReplication",
		},
		{
			name:        "switch replication to rpc_coordinator",
			initialMode: "replication",
			targetMode:  "rpc_coordinator",
			wantEnabled: "rpcCoordinator",
		},
		{
			name:        "switch rpc to virtual_router",
			initialMode: "rpc_coordinator",
			targetMode:  "virtual_router",
			wantEnabled: "virtualModels",
		},
		{
			name:        "switch virtual to distributed_inference",
			initialMode: "virtual_router",
			targetMode:  "distributed_inference",
			wantEnabled: "distInference",
		},
		{
			name:        "switch back to standard",
			initialMode: "distributed_inference",
			targetMode:  "standard",
			wantEnabled: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Сбрасываем все enabled-флаги и ставим начальный режим
			cfg.Balancing.ModelReplication.Enabled = false
			cfg.Balancing.RpcCoordinator.Enabled = false
			cfg.Balancing.VirtualModels.Enabled = false
			cfg.Balancing.DistInference.Enabled = false
			cfg.Balancing.OperatingMode = tt.initialMode
			switch tt.initialMode {
			case "replication":
				cfg.Balancing.ModelReplication.Enabled = true
			case "rpc_coordinator":
				cfg.Balancing.RpcCoordinator.Enabled = true
			case "virtual_router":
				cfg.Balancing.VirtualModels.Enabled = true
			case "distributed_inference":
				cfg.Balancing.DistInference.Enabled = true
			}

			// Шаг 1: PUT /api/v1/cluster/config с новым operatingMode
			body := map[string]interface{}{
				"operatingMode": tt.targetMode,
			}
			bodyJSON, _ := json.Marshal(body)

			req := httptest.NewRequest(http.MethodPut, "/api/v1/cluster/config", bytes.NewReader(bodyJSON))
			rr := httptest.NewRecorder()
			server.ServeHTTP(rr, req)

			require.Equal(t, http.StatusOK, rr.Code, "PUT config should succeed")

			var putResp map[string]interface{}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &putResp))
			configBlock, ok := putResp["config"].(map[string]interface{})
			require.True(t, ok, "response should contain config block")

			assert.Equal(t, tt.targetMode, configBlock["operatingMode"],
				"PUT response should reflect new operatingMode")

			// Шаг 2: GET /api/v1/cluster/config — проверяем сохранение
			req = httptest.NewRequest(http.MethodGet, "/api/v1/cluster/config", nil)
			rr = httptest.NewRecorder()
			server.ServeHTTP(rr, req)

			require.Equal(t, http.StatusOK, rr.Code, "GET config should succeed")

			var getResp map[string]interface{}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &getResp))

			assert.Equal(t, tt.targetMode, getResp["operatingMode"],
				"GET config should return persisted operatingMode")

			// Шаг 3: GET /api/v1/cluster — ClusterState содержит operatingMode
			req = httptest.NewRequest(http.MethodGet, "/api/v1/cluster", nil)
			rr = httptest.NewRecorder()
			server.ServeHTTP(rr, req)

			require.Equal(t, http.StatusOK, rr.Code, "GET cluster should succeed")

			var clusterState types.ClusterState
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &clusterState))

			assert.Equal(t, tt.targetMode, clusterState.OperatingMode,
				"ClusterState should carry operatingMode")

			// Шаг 4: Проверяем что именно один enabled-флаг установлен
			switch tt.wantEnabled {
			case "modelReplication":
				assert.True(t, cfg.Balancing.ModelReplication.Enabled, "ModelReplication should be enabled")
				assert.False(t, cfg.Balancing.RpcCoordinator.Enabled, "RpcCoordinator should be disabled")
			case "rpcCoordinator":
				assert.True(t, cfg.Balancing.RpcCoordinator.Enabled, "RpcCoordinator should be enabled")
				assert.False(t, cfg.Balancing.ModelReplication.Enabled, "ModelReplication should be disabled")
			case "virtualModels":
				assert.True(t, cfg.Balancing.VirtualModels.Enabled, "VirtualModels should be enabled")
				assert.False(t, cfg.Balancing.RpcCoordinator.Enabled, "RpcCoordinator should be disabled")
			case "distInference":
				assert.True(t, cfg.Balancing.DistInference.Enabled, "DistInference should be enabled")
				assert.False(t, cfg.Balancing.VirtualModels.Enabled, "VirtualModels should be disabled")
			case "":
				assert.False(t, cfg.Balancing.ModelReplication.Enabled, "all variants should be disabled")
				assert.False(t, cfg.Balancing.RpcCoordinator.Enabled)
				assert.False(t, cfg.Balancing.VirtualModels.Enabled)
				assert.False(t, cfg.Balancing.DistInference.Enabled)
			}
		})
	}
}

// TestOperatingModeValidation проверяет отклонение невалидного operatingMode
func TestOperatingModeValidation(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Port: 18080, APIPort: 18081},
		API: types.APISettings{
			RateLimit: 10000,
			RateBurst: 20000,
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmResourceAware,
			OperatingMode: "standard",
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
			CPU: types.CPULimits{MaxUsagePercent: 80},
		},
	}

	proxy := balancer.NewProxy(cfg)
	healthChecker := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := api.NewServer(proxy, cfg, healthChecker)
	server.SetConfigSaver(func() error { return nil })

	body := map[string]interface{}{
		"operatingMode": "invalid_mode_xyz",
	}
	bodyJSON, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/cluster/config", bytes.NewReader(bodyJSON))
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code, "invalid mode should return 400")

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.False(t, resp["success"].(bool), "success should be false")
}

// TestOperatingModeInClusterState проверяет что ClusterState всегда содержит
// актуальный OperatingMode и он синхронизирован с runtime-config
func TestOperatingModeInClusterState(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Port: 18080, APIPort: 18081},
		API: types.APISettings{
			RateLimit: 10000,
			RateBurst: 20000,
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmResourceAware,
			OperatingMode: "standard",
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
			CPU: types.CPULimits{MaxUsagePercent: 80},
		},
	}

	proxy := balancer.NewProxy(cfg)
	healthChecker := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := api.NewServer(proxy, cfg, healthChecker)
	server.SetConfigSaver(func() error { return nil })

	// Добавляем фейковый бэкенд чтобы кластер был не пустым
	_ = proxy.AddBackend(types.Backend{
		ID:   "test-backend",
		Host: "127.0.0.1",
	})

	tests := []struct {
		name       string
		targetMode string
	}{
		{"replication mode in cluster state", "replication"},
		{"rpc_coordinator mode in cluster state", "rpc_coordinator"},
		{"virtual_router mode in cluster state", "virtual_router"},
		{"distributed_inference mode in cluster state", "distributed_inference"},
		{"standard mode in cluster state", "standard"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// PUT смены режима
			body := map[string]interface{}{"operatingMode": tt.targetMode}
			bodyJSON, _ := json.Marshal(body)

			req := httptest.NewRequest(http.MethodPut, "/api/v1/cluster/config", bytes.NewReader(bodyJSON))
			rr := httptest.NewRecorder()
			server.ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code)

			// GET cluster/state — проверяем OperatingMode
			req = httptest.NewRequest(http.MethodGet, "/api/v1/cluster", nil)
			rr = httptest.NewRecorder()
			server.ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code)

			var state types.ClusterState
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &state))

			// КЛЮЧЕВАЯ ПРОВЕРКА: OperatingMode в ClusterState соответствует тому что мы установили
			assert.Equal(t, tt.targetMode, state.OperatingMode,
				"ClusterState must reflect the current operating mode")

			// Дополнительно: проверяем что enabled-флаг синхронизирован
			switch tt.targetMode {
			case "replication":
				assert.True(t, cfg.Balancing.ModelReplication.Enabled, "ModelReplication.Enabled must be true")
			case "rpc_coordinator":
				assert.True(t, cfg.Balancing.RpcCoordinator.Enabled, "RpcCoordinator.Enabled must be true")
			case "virtual_router":
				assert.True(t, cfg.Balancing.VirtualModels.Enabled, "VirtualModels.Enabled must be true")
			case "distributed_inference":
				assert.True(t, cfg.Balancing.DistInference.Enabled, "DistInference.Enabled must be true")
			case "standard":
				assert.False(t, cfg.Balancing.ModelReplication.Enabled, "all variant flags must be disabled")
				assert.False(t, cfg.Balancing.RpcCoordinator.Enabled)
				assert.False(t, cfg.Balancing.VirtualModels.Enabled)
				assert.False(t, cfg.Balancing.DistInference.Enabled)
			}
		})
	}
}

// TestWebUIConfigSyncHardRule проверяет жёсткое правило:
// "Все настройки в WebUI всегда отображают актуальное состояние на балансере".
// Эмулируем сценарий: бэкенд меняет operatingMode → WebUI при следующем GET config
// получает обновлённое значение и отображает его корректно.
func TestWebUIConfigSyncHardRule(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Port: 18080, APIPort: 18081},
		API: types.APISettings{
			RateLimit: 10000,
			RateBurst: 20000,
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmResourceAware,
			OperatingMode: "standard",
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
			CPU: types.CPULimits{MaxUsagePercent: 80},
		},
	}

	proxy := balancer.NewProxy(cfg)
	healthChecker := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := api.NewServer(proxy, cfg, healthChecker)
	server.SetConfigSaver(func() error { return nil })

	// Шаг 1: Начальное состояние — standard
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/config", nil)
	rr := httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var initialResp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &initialResp))
	assert.Equal(t, "standard", initialResp["operatingMode"], "initial mode should be standard")

	// Шаг 2: Смена режима на replication через API (как это делает WebUI)
	putBody := map[string]interface{}{
		"operatingMode": "replication",
		"modelReplication": map[string]interface{}{
			"defaultMinInstances": 2,
			"defaultMaxInstances": 5,
			"idleUnloadAfter":     "5m",
		},
	}
	putJSON, _ := json.Marshal(putBody)

	req = httptest.NewRequest(http.MethodPut, "/api/v1/cluster/config", bytes.NewReader(putJSON))
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	// Шаг 3: WebUI делает GET config — должен получить ТОЧНОЕ значение
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cluster/config", nil)
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var afterPutResp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &afterPutResp))

	// ЖЁСТКАЯ ПРОВЕРКА: WebUI всегда видит актуальное состояние
	assert.Equal(t, "replication", afterPutResp["operatingMode"],
		"WebUI GET config must return the exact mode that was just set")
	assert.Equal(t, "replication", cfg.Balancing.OperatingMode,
		"runtime config must match the applied mode")

	// Шаг 4: ClusterState тоже должен быть синхронизирован
	req = httptest.NewRequest(http.MethodGet, "/api/v1/cluster", nil)
	rr = httptest.NewRecorder()
	server.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var clusterState types.ClusterState
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &clusterState))
	assert.Equal(t, "replication", clusterState.OperatingMode,
		"ClusterState.OperatingMode must be in sync with runtime config")

	// Шаг 5: Дополнительно проверяем что в конфиге установлены правильные enabled-флаги
	assert.True(t, cfg.Balancing.ModelReplication.Enabled, "ModelReplication must be enabled")
	assert.False(t, cfg.Balancing.RpcCoordinator.Enabled, "RpcCoordinator must be disabled")
}
