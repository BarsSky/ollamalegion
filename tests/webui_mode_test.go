package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== Тесты WebUI — API отдаёт operatingMode корректно ====================

func TestWebUI_ConfigEndpoint_ReturnsOperatingMode(t *testing.T) {
	testServer, _, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	resp, err := http.Get(baseURL + "/api/v1/cluster/config")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	// GET /api/v1/cluster/config возвращает плоский объект
	// operatingMode может быть пустым до вызова deriveOperatingMode
	assert.Contains(t, result, "operatingMode", "Config endpoint should expose operatingMode")
	operatingMode, _ := result["operatingMode"].(string)
	assert.True(t, operatingMode == "standard" || operatingMode == "",
		"operatingMode should be standard or empty, got: %s", operatingMode)
}

func TestWebUI_ClusterState_ReturnsOperatingMode(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// Устанавливаем метрики чтобы backend был в списке
	proxy.UpdateMetrics("agent-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	resp, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var clusterState map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&clusterState)
	require.NoError(t, err)

	// operatingMode должен быть на top-level в ClusterState
	assert.Contains(t, clusterState, "operatingMode", "ClusterState API should expose operatingMode")
	operatingMode, _ := clusterState["operatingMode"].(string)
	assert.True(t, operatingMode == "standard" || operatingMode == "",
		"Default cluster operatingMode should be standard or empty, got: %s", operatingMode)
}

func TestWebUI_ConfigUpdate_ChangesOperatingMode(t *testing.T) {
	testServer, server, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	// 1. Получаем начальный режим (GET возвращает плоский объект)
	resp, err := http.Get(baseURL + "/api/v1/cluster/config")
	require.NoError(t, err)
	defer resp.Body.Close()

	var initial map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&initial)
	initialMode, _ := initial["operatingMode"].(string)
	assert.True(t, initialMode == "standard" || initialMode == "",
		"Initial mode should be standard or empty, got: %s", initialMode)

	// 2. Переключаем в replication (PUT возвращает обёрнутый config)
	payload := map[string]interface{}{
		"operatingMode": "replication",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp2.Body.Close()

	var updateResult map[string]interface{}
	json.NewDecoder(resp2.Body).Decode(&updateResult)
	updateConfig := updateResult["config"].(map[string]interface{})
	assert.Equal(t, "replication", updateConfig["operatingMode"], "Update response should show new mode")

	// 3. Проверяем что GET /api/v1/cluster/config теперь возвращает replication
	time.Sleep(50 * time.Millisecond)
	resp3, err := http.Get(baseURL + "/api/v1/cluster/config")
	require.NoError(t, err)
	defer resp3.Body.Close()

	var afterUpdate map[string]interface{}
	json.NewDecoder(resp3.Body).Decode(&afterUpdate)
	assert.Equal(t, "replication", afterUpdate["operatingMode"], "GET config after update should reflect new mode")
}

func TestWebUI_ClusterState_ReflectsModeChange(t *testing.T) {
	testServer, server, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	// Устанавливаем метрики
	proxy.UpdateMetrics("agent-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	// Переключаем в virtual_router
	payload := map[string]interface{}{
		"operatingMode": "virtual_router",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	time.Sleep(50 * time.Millisecond)

	// GET /api/v1/cluster должен содержать operatingMode = virtual_router
	resp2, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp2.Body.Close()

	var clusterState map[string]interface{}
	err = json.NewDecoder(resp2.Body).Decode(&clusterState)
	require.NoError(t, err)

	assert.Equal(t, "virtual_router", clusterState["operatingMode"], "ClusterState should reflect mode change to virtual_router")
}

func TestWebUI_MetricsEndpoint_BackendListAvailable(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// Устанавливаем метрики на backend
	proxy.UpdateMetrics("agent-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	resp, err := http.Get(baseURL + "/api/v1/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var metrics map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&metrics)
	require.NoError(t, err)

	// WebUI использует /api/v1/metrics для получения списка backends
	assert.Contains(t, metrics, "backends", "Metrics endpoint should expose backends list")
	backends, ok := metrics["backends"].([]interface{})
	require.True(t, ok, "backends should be an array")
	assert.GreaterOrEqual(t, len(backends), 0, "backends array should be present")
}

func TestWebUI_ConfigEndpoint_ExposesAllModeRelatedFields(t *testing.T) {
	testServer, _, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	resp, err := http.Get(baseURL + "/api/v1/cluster/config")
	require.NoError(t, err)
	defer resp.Body.Close()

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	// WebUI ожидает эти поля для отображения настроек (GET возвращает плоский объект)
	requiredFields := []string{
		"algorithm",
		"operatingMode",
		"modelAffinity",
		"sessionStickiness",
		"useEnhancedScoring",
		"gpuMaxUsage",
		"vramMaxUsage",
		"cpuMaxUsage",
		"ramMaxUsage",
		"minFreeDisk",
	}

	for _, field := range requiredFields {
		assert.Contains(t, result, field, "Config endpoint should expose field required by WebUI: %s", field)
	}
}

func TestWebUI_ModeSwitch_InvalidMode_NoCrash(t *testing.T) {
	testServer, server, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	// WebUI отправляет operatingMode — если недопустимый, сервер должен вернуть 400, а не паниковать
	payload := map[string]interface{}{
		"operatingMode": "some_hacker_mode",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Должен быть 400, не 500
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "Invalid mode should return 400 Bad Request, not crash")

	// Следующий GET /api/v1/cluster/config должен работать нормально
	resp2, err := http.Get(baseURL + "/api/v1/cluster/config")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode, "Config endpoint should still work after bad mode request")
}

func TestWebUI_ModeSwitch_SyncFromServer(t *testing.T) {
	testServer, server, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	// Симулируем сценарий: WebUI загружает настройки, пользователь меняет режим,
	// WebUI снова загружает настройки для подтверждения

	// Шаг 1: Начальная загрузка (GET = плоский объект)
	resp, _ := http.Get(baseURL + "/api/v1/cluster/config")
	var initial map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&initial)
	resp.Body.Close()

	initialMode, _ := initial["operatingMode"].(string)
	assert.True(t, initialMode == "standard" || initialMode == "",
		"Initial mode should be standard or empty, got: %s", initialMode)

	// Шаг 2: Переключение через API (симулирует клик пользователя в WebUI)
	payload := map[string]interface{}{
		"operatingMode": "replication",
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()

	time.Sleep(50 * time.Millisecond)

	// Шаг 3: WebUI получает обновлённые настройки для синхронизации UI (GET = плоский)
	resp3, err := http.Get(baseURL + "/api/v1/cluster/config")
	require.NoError(t, err)
	defer resp3.Body.Close()

	var synced map[string]interface{}
	json.NewDecoder(resp3.Body).Decode(&synced)
	syncedMode := synced["operatingMode"].(string)

	// WebUI должен увидеть новый режим и обновить radio-кнопки
	assert.Equal(t, "replication", syncedMode, "WebUI sync should reflect server-side mode change")

	// Шаг 4: ClusterState тоже должен быть синхронизирован
	state := proxy.GetClusterState()
	assert.Equal(t, "replication", state.OperatingMode)
}

func TestWebUI_Dashboard_MetricsAvailableAfterModeChange(t *testing.T) {
	testServer, server, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	// Устанавливаем метрики
	for _, id := range []string{"agent-1", "agent-2"} {
		proxy.UpdateMetrics(id, &types.BackendMetrics{
			GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
			System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
			Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 10},
		})
	}

	// Переключаемся в distributed_inference
	payload := map[string]interface{}{
		"operatingMode": "distributed_inference",
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	time.Sleep(50 * time.Millisecond)

	// Dashboard использует /api/v1/cluster — метрики должны быть доступны
	resp2, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp2.Body.Close()

	var clusterState map[string]interface{}
	json.NewDecoder(resp2.Body).Decode(&clusterState)

	// Важные поля для dashboard
	assert.Equal(t, "distributed_inference", clusterState["operatingMode"])
	assert.Contains(t, clusterState, "totalBackends")
	assert.Contains(t, clusterState, "healthyBackends")
	assert.Contains(t, clusterState, "backends")
	assert.Contains(t, clusterState, "rps")
}

// ==================== Тесты WebUI — backendEngine синхронизация ====================

func TestWebUI_ConfigEndpoint_ReturnsBackendEngine(t *testing.T) {
	testServer, _, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	resp, err := http.Get(baseURL + "/api/v1/cluster/config")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	// GET /api/v1/cluster/config должен возвращать backendEngine
	assert.Contains(t, result, "backendEngine", "Config endpoint should expose backendEngine")
	backendEngine, _ := result["backendEngine"].(string)
	assert.True(t, backendEngine == "ollama_api" || backendEngine == "llama_cpp" || backendEngine == "",
		"backendEngine should be ollama_api, llama_cpp or empty, got: %s", backendEngine)
}

func TestWebUI_ClusterState_ReturnsBackendEngine(t *testing.T) {
	testServer, _, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	// Устанавливаем метрики чтобы backend был в списке
	proxy.UpdateMetrics("agent-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	resp, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var clusterState map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&clusterState)
	require.NoError(t, err)

	// backendEngine должен быть на top-level в ClusterState
	assert.Contains(t, clusterState, "backendEngine", "ClusterState API should expose backendEngine")
	assert.Contains(t, clusterState, "backendTypeCounts", "ClusterState API should expose backendTypeCounts")
}

func TestWebUI_ConfigUpdate_ChangesBackendEngine(t *testing.T) {
	testServer, server, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	// 1. Получаем начальный backendEngine
	resp, err := http.Get(baseURL + "/api/v1/cluster/config")
	require.NoError(t, err)
	defer resp.Body.Close()

	var initial map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&initial)

	// 2. Переключаем на llama_cpp
	payload := map[string]interface{}{
		"backendEngine": "llama_cpp",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp2.Body.Close()

	// 3. Проверяем что GET /api/v1/cluster/config возвращает llama_cpp
	time.Sleep(50 * time.Millisecond)
	resp3, err := http.Get(baseURL + "/api/v1/cluster/config")
	require.NoError(t, err)
	defer resp3.Body.Close()

	var afterUpdate map[string]interface{}
	json.NewDecoder(resp3.Body).Decode(&afterUpdate)
	assert.Equal(t, "llama_cpp", afterUpdate["backendEngine"],
		"GET config after update should reflect new backendEngine: llama_cpp")

	// 4. Переключаем обратно на ollama_api
	payload2 := map[string]interface{}{
		"backendEngine": "ollama_api",
	}
	body2, _ := json.Marshal(payload2)

	req2, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	resp4, _ := http.DefaultClient.Do(req2)
	resp4.Body.Close()

	time.Sleep(50 * time.Millisecond)

	resp5, _ := http.Get(baseURL + "/api/v1/cluster/config")
	var backToOllama map[string]interface{}
	json.NewDecoder(resp5.Body).Decode(&backToOllama)
	resp5.Body.Close()

	assert.Equal(t, "ollama_api", backToOllama["backendEngine"],
		"GET config after switching back should reflect ollama_api")
}

func TestWebUI_ClusterState_ReflectsBackendEngineChange(t *testing.T) {
	testServer, server, proxy, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	// Устанавливаем метрики
	proxy.UpdateMetrics("agent-1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 10},
	})

	// Переключаем на llama_cpp
	payload := map[string]interface{}{
		"backendEngine": "llama_cpp",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	time.Sleep(50 * time.Millisecond)

	// GET /api/v1/cluster должен содержать backendEngine = llama_cpp
	resp2, err := http.Get(baseURL + "/api/v1/cluster")
	require.NoError(t, err)
	defer resp2.Body.Close()

	var clusterState map[string]interface{}
	err = json.NewDecoder(resp2.Body).Decode(&clusterState)
	require.NoError(t, err)

	assert.Equal(t, "llama_cpp", clusterState["backendEngine"],
		"ClusterState should reflect backendEngine change to llama_cpp")
}

func TestWebUI_BackendEngineSync_FromServerAfterPageReload(t *testing.T) {
	// Симулирует сценарий: WebUI выбрал llama_cpp, страница перезагружена,
	// WebUI должен загрузить backendEngine из серверного конфига и показать llama_cpp
	testServer, server, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	// 1. Пользователь выбрал llama_cpp в WebUI
	payload := map[string]interface{}{
		"backendEngine": "llama_cpp",
		"operatingMode": "standard",
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	time.Sleep(50 * time.Millisecond)

	// 2. Симулируем перезагрузку страницы — WebUI делает GET /config
	//    (очищаем localStorage — симулируем новую сессию)
	resp2, _ := http.Get(baseURL + "/api/v1/cluster/config")
	var serverConfig map[string]interface{}
	json.NewDecoder(resp2.Body).Decode(&serverConfig)
	resp2.Body.Close()

	// 3. WebUI должен увидеть backendEngine = llama_cpp
	assert.Equal(t, "llama_cpp", serverConfig["backendEngine"],
		"After page reload, server config should return backendEngine=llama_cpp")
	assert.Equal(t, "standard", serverConfig["operatingMode"],
		"After page reload, operatingMode should be standard (compatible with llama_cpp)")
}

func TestWebUI_BackendEngineSwitch_InvalidValue_NoCrash(t *testing.T) {
	testServer, server, _, _ := setupTestEnvironment(t)
	defer testServer.Close()
	baseURL := testServer.URL

	server.SetConfigSaver(func() error { return nil })

	// WebUI отправляет backendEngine — если недопустимый, сервер должен вернуть 400, а не паниковать
	payload := map[string]interface{}{
		"backendEngine": "some_invalid_engine",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest("PUT", baseURL+"/api/v1/cluster/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Должен быть 400, не 500
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "Invalid backendEngine should return 400 Bad Request, not crash")

	// Следующий GET /api/v1/cluster/config должен работать нормально
	resp2, err := http.Get(baseURL + "/api/v1/cluster/config")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode, "Config endpoint should still work after bad backendEngine request")
}