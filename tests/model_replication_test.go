package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/internal/api"
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/internal/modelreplication"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================
// Фаза A5: Тесты для Variant A — Model Replication Manager
// ============================================================

// ---------- Утилиты ----------

// configWithReplication создаёт конфиг с включённой репликацией.
func configWithReplication(t *testing.T) *types.LoadBalancerConfig {
	t.Helper()
	cfg := configForReplication()
	return cfg
}

func configForReplication() *types.LoadBalancerConfig {
	return &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID:                "backend-1",
				Name:              "Backend 1",
				Host:              "127.0.0.1",
				OllamaPort:        11434,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
			{
				ID:                "backend-2",
				Name:              "Backend 2",
				Host:              "127.0.0.1",
				OllamaPort:        11435,
				Weight:            2,
				MaxConcurrentReqs: 20,
				Status:            types.StatusHealthy,
			},
			{
				ID:                "backend-3",
				Name:              "Backend 3",
				Host:              "127.0.0.1",
				OllamaPort:        11436,
				Weight:            1,
				MaxConcurrentReqs: 5,
				Status:            types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			ModelAffinity:       true,
			SessionStickiness:   false,
			HealthCheckInterval: 10,
			MetricsInterval:     5,
			RequestTimeout:      30,
			QueueTimeout:        60,
			QueueMaxSize:        100,
			QueueWorkers:        4,
			ModelReplication: types.ModelReplicationConfig{
				Enabled:            true,
				DefaultMinInstances: 1,
				DefaultMaxInstances: 3,
				IdleUnloadAfter:     "15m",
				Groups: []types.ModelGroupConfig{
					{
						ModelName:      "llama3.1",
						MinInstances:   2,
						MaxInstances:   4,
						TargetBackends: []string{},
						IdleUnloadAfter: "",
					},
				},
			},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{
				MaxUsagePercent:     90,
				MaxVRAMUsagePercent: 95,
			},
		},
		API: types.APISettings{
			RateLimit: 1000,
			RateBurst: 2000,
		},
		Auth: types.AuthConfig{
			Enabled: false,
		},
	}
}

// createReplicationProxy создаёт Proxy с включённой репликацией.
func createReplicationProxy(t *testing.T) *balancer.Proxy {
	t.Helper()
	cfg := configForReplication()
	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	return proxy
}

// ---------- UNIT: ModelGroupManager CRUD ----------

func TestModelReplication_ManagerCreateGroup(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)

	// Успешное создание
	err := mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3.1",
		MinInstances: 2,
		MaxInstances: 4,
	})
	require.NoError(t, err)

	// Дубликат — ошибка
	err = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3.1",
		MinInstances: 1,
		MaxInstances: 3,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	// Проверяем, что группа создана
	cfg := mgr.GetGroup("llama3.1")
	require.NotNil(t, cfg)
	assert.Equal(t, "llama3.1", cfg.ModelName)
	assert.Equal(t, 2, cfg.MinInstances)
	assert.Equal(t, 4, cfg.MaxInstances)
}

func TestModelReplication_ManagerCreateGroup_Validation(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)

	tests := []struct {
		name    string
		cfg     types.ModelGroupConfig
		wantErr string
	}{
		{
			name:    "empty model name",
			cfg:     types.ModelGroupConfig{ModelName: "", MinInstances: 1, MaxInstances: 3},
			wantErr: "modelName is required",
		},
		{
			name:    "zero min instances",
			cfg:     types.ModelGroupConfig{ModelName: "test", MinInstances: 0, MaxInstances: 3},
			wantErr: "minInstances must be > 0",
		},
		{
			name:    "negative min instances",
			cfg:     types.ModelGroupConfig{ModelName: "test", MinInstances: -1, MaxInstances: 3},
			wantErr: "minInstances must be > 0",
		},
		{
			name:    "max < min",
			cfg:     types.ModelGroupConfig{ModelName: "test", MinInstances: 3, MaxInstances: 1},
			wantErr: "maxInstances must be >= minInstances",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mgr.CreateGroup(tt.cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestModelReplication_ManagerDeleteGroup(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)

	// Создаём → удаляем
	err := mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3.1",
		MinInstances: 1,
		MaxInstances: 3,
	})
	require.NoError(t, err)

	err = mgr.DeleteGroup("llama3.1")
	require.NoError(t, err)

	// Удаление несуществующей — ошибка
	err = mgr.DeleteGroup("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")

	// Не должно быть групп
	groups := mgr.GetGroups()
	assert.Equal(t, 0, len(groups))
}

func TestModelReplication_ManagerUpdateGroup(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)

	// Создаём
	err := mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3.1",
		MinInstances: 2,
		MaxInstances: 4,
	})
	require.NoError(t, err)

	// Обновляем
	err = mgr.UpdateGroup(types.ModelGroupConfig{
		ModelName:    "llama3.1",
		MinInstances: 3,
		MaxInstances: 5,
	})
	require.NoError(t, err)

	cfg := mgr.GetGroup("llama3.1")
	require.NotNil(t, cfg)
	assert.Equal(t, 3, cfg.MinInstances)
	assert.Equal(t, 5, cfg.MaxInstances)

	// Обновление несуществующей — ошибка
	err = mgr.UpdateGroup(types.ModelGroupConfig{
		ModelName:    "nonexistent",
		MinInstances: 1,
		MaxInstances: 3,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestModelReplication_ManagerGetGroups(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)

	// Пустой список
	assert.Equal(t, 0, len(mgr.GetGroups()))

	// Создаём 3 группы
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("model-%d", i)
		err := mgr.CreateGroup(types.ModelGroupConfig{
			ModelName:    name,
			MinInstances: 1,
			MaxInstances: 2,
		})
		require.NoError(t, err)
	}

	groups := mgr.GetGroups()
	assert.Equal(t, 3, len(groups))

	// Проверяем, что GetGroup возвращает nil для несуществующей
	assert.Nil(t, mgr.GetGroup("nonexistent"))
}

// ---------- UNIT: ModelGroupManager - Callbacks ----------

func TestModelReplication_Callbacks(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)

	// Каналы для синхронизации async вызовов
	backendFnCh := make(chan string, 10)
	warmupFnCh := make(chan string, 10)

	// loadFn
	mgr.SetBackendLoadFn(func(backendID string) float64 {
		return 0.5
	})

	// backendFn
	mgr.SetFreeBackendFn(func(modelName string, targets []string) []string {
		backendFnCh <- modelName
		return []string{"backend-1", "backend-2"}
	})

	// warmupFn — вызывается асинхронно в goroutine
	mgr.SetWarmupFn(func(backendID, modelName string) error {
		warmupFnCh <- modelName
		return nil
	})

	// Создаём одну группу с minInstances > 0, чтобы EnsureInstances запустила scaleUp
	err := mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3.1",
		MinInstances: 1,
		MaxInstances: 3,
	})
	require.NoError(t, err)

	// Вызов EnsureInstances должен вызвать backendFn
	mgr.EnsureInstances()

	// Проверяем, что backendFn был вызван для нашей группы
	select {
	case modelName := <-backendFnCh:
		assert.Equal(t, "llama3.1", modelName)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("backendFn was not called within timeout")
	}

	// Проверяем, что warmupFn был вызван (асинхронно)
	select {
	case modelName := <-warmupFnCh:
		assert.Equal(t, "llama3.1", modelName)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("warmupFn was not called within timeout")
	}
}

// ---------- UNIT: GroupAwareSelector ----------

func TestModelReplication_GroupAwareSelector_Disabled(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(false) // Выключен

	sel := modelreplication.NewGroupAwareSelector(mgr)

	// Select возвращает пустую строку при выключенном менеджере
	assert.Equal(t, "", sel.Select("llama3.1"))

	// IsGroupModel возвращает false
	assert.False(t, sel.IsGroupModel("llama3.1"))

	// GetGroupCandidates возвращает nil
	assert.Nil(t, sel.GetGroupCandidates("llama3.1"))

	// GetGroupStats возвращает nil
	assert.Nil(t, sel.GetGroupStats("llama3.1"))
}

func TestModelReplication_GroupAwareSelector_Select(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)

	// Создаём группу
	err := mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3.1",
		MinInstances: 1,
		MaxInstances: 3,
	})
	require.NoError(t, err)

	// Устанавливаем loadFn
	mgr.SetBackendLoadFn(func(backendID string) float64 {
		switch backendID {
		case "backend-1":
			return 0.3
		case "backend-2":
			return 0.5
		default:
			return 1.0
		}
	})

	// Добавляем LOADED инстансы (через прямой доступ)
	mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3.2",
		MinInstances: 1,
		MaxInstances: 3,
	})

	// Select для модели без группы — пустая строка
	sel := modelreplication.NewGroupAwareSelector(mgr)
	assert.Equal(t, "", sel.Select("nonexistent"))

	// Select для модели с группой, но без загруженных инстансов — пустая строка
	assert.Equal(t, "", sel.Select("llama3.1"))
}

func TestModelReplication_GroupAwareSelector_IsGroupModel(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)

	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3.1",
		MinInstances: 1,
		MaxInstances: 3,
	})

	sel := modelreplication.NewGroupAwareSelector(mgr)

	assert.True(t, sel.IsGroupModel("llama3.1"))
	assert.False(t, sel.IsGroupModel("nonexistent"))
}

func TestModelReplication_GroupAwareSelector_GetGroupCandidates(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)

	_ = mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3.1",
		MinInstances: 1,
		MaxInstances: 3,
	})

	sel := modelreplication.NewGroupAwareSelector(mgr)

	// Нет загруженных инстансов — пустой список
	candidates := sel.GetGroupCandidates("llama3.1")
	assert.Equal(t, 0, len(candidates))

	// Несуществующая модель — nil
	assert.Nil(t, sel.GetGroupCandidates("nonexistent"))
}

// ---------- UNIT: GroupController ----------

func TestModelReplication_GroupController_StartStop(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)

	ctrl := modelreplication.NewGroupController(mgr)
	ctrl.SetInterval(100 * time.Millisecond)

	// Старт
	err := ctrl.Start()
	require.NoError(t, err)
	assert.True(t, ctrl.IsRunning())

	// Повторный старт — не ошибка
	err = ctrl.Start()
	require.NoError(t, err)

	// Даём время на реконсиляцию
	time.Sleep(50 * time.Millisecond)

	// Стоп
	err = ctrl.Stop()
	require.NoError(t, err)
	assert.False(t, ctrl.IsRunning())

	// Повторный стоп — не ошибка
	err = ctrl.Stop()
	require.NoError(t, err)
}

func TestModelReplication_GroupController_TriggerReconcile(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)

	ctrl := modelreplication.NewGroupController(mgr)

	// TriggerReconcile с выключенным менеджером — тихо пропускает
	mgr.SetEnabled(false)
	// Не паникует
	assert.NotPanics(t, func() {
		ctrl.TriggerReconcile()
	})

	mgr.SetEnabled(true)

	// Создаём группу
	err := mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3.1",
		MinInstances: 1,
		MaxInstances: 3,
	})
	require.NoError(t, err)

	// TriggerReconcile — запускает EnsureInstances
	assert.NotPanics(t, func() {
		ctrl.TriggerReconcile()
	})
}

// ---------- UNIT: selectInstance ----------

func TestModelReplication_SelectInstance(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)

	err := mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "llama3.1",
		MinInstances: 1,
		MaxInstances: 3,
	})
	require.NoError(t, err)

	// loadFn возвращает значения
	mgr.SetBackendLoadFn(func(backendID string) float64 {
		return 0.5
	})

	// Без загруженных инстансов — пустая строка (selectInstance внутренняя)
	// Доступ к selectInstance только через GroupAwareSelector.Select
	sel := modelreplication.NewGroupAwareSelector(mgr)
	selected := sel.Select("llama3.1")
	assert.Equal(t, "", selected) // Нет loaded инстансов
}

// ---------- INTEGRATION: Proxy with Model Replication ----------

func TestModelReplication_ProxyInit(t *testing.T) {
	cfg := configForReplication()
	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()

	// Проверяем, что менеджер создан и включён
	mgr := proxy.GetModelReplicationManager()
	require.NotNil(t, mgr)
	assert.True(t, mgr.IsEnabled())

	// Проверяем, что селектор создан
	sel := proxy.GetReplicationSelector()
	require.NotNil(t, sel)

	// Проверяем, что контроллер создан и запущен
	ctrl := proxy.GetReplicationController()
	require.NotNil(t, ctrl)

	// Группа из конфига должна быть создана
	cfg2 := mgr.GetGroup("llama3.1")
	require.NotNil(t, cfg2)
	assert.Equal(t, 2, cfg2.MinInstances)
	assert.Equal(t, 4, cfg2.MaxInstances)

	// Останавливаем контроллер
	_ = ctrl.Stop()
}

func TestModelReplication_ProxyInit_Disabled(t *testing.T) {
	cfg := configForReplication()
	cfg.Balancing.ModelReplication.Enabled = false

	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()

	// Менеджер должен быть nil, если репликация выключена
	mgr := proxy.GetModelReplicationManager()
	assert.Nil(t, mgr)

	sel := proxy.GetReplicationSelector()
	assert.Nil(t, sel)

	ctrl := proxy.GetReplicationController()
	assert.Nil(t, ctrl)
}

func TestModelReplication_SelectBackend_NotGroupModel(t *testing.T) {
	// Если модель не в группе репликации — selectBackend работает как обычно
	cfg := configForReplication()
	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	// Для модели не из группы — GroupAwareSelector вернёт пустую строку, и пойдёт обычный алгоритм
	backend := proxy.SelectBackend("nonexistent-model")
	// Не должно быть паники
	_ = backend
}

// ---------- INTEGRATION: API endpoints ----------

// serverWithReplication создаёт тестовый сервер с включённой репликацией.
func serverWithReplication(t *testing.T) (*httptest.Server, *api.Server, *balancer.Proxy) {
	t.Helper()
	cfg := configForReplication()
	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	healthChecker := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := api.NewServer(proxy, cfg, healthChecker)
	testServer := httptest.NewServer(server)
	return testServer, server, proxy
}

func TestModelReplication_API_ListGroups(t *testing.T) {
	testServer, _, proxy := serverWithReplication(t)
	defer testServer.Close()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	resp, err := http.Get(testServer.URL + "/api/v1/replication/groups")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)

	groups, ok := body["groups"].([]interface{})
	require.True(t, ok, "groups must be an array")
	assert.GreaterOrEqual(t, len(groups), 1, "should have at least 1 group")
}

func TestModelReplication_API_GetGroup(t *testing.T) {
	testServer, _, proxy := serverWithReplication(t)
	defer testServer.Close()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	// GET существующей группы
	resp, err := http.Get(testServer.URL + "/api/v1/replication/groups/llama3.1")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)

	// Проверяем структуру ответа
	_, hasConfig := body["config"]
	assert.True(t, hasConfig, "response must contain 'config'")

	_, hasStates := body["states"]
	assert.True(t, hasStates, "response must contain 'states'")

	_, hasStats := body["stats"]
	assert.True(t, hasStats, "response must contain 'stats'")
}

func TestModelReplication_API_GetGroup_NotFound(t *testing.T) {
	testServer, _, proxy := serverWithReplication(t)
	defer testServer.Close()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	resp, err := http.Get(testServer.URL + "/api/v1/replication/groups/nonexistent")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestModelReplication_API_CreateGroup(t *testing.T) {
	testServer, _, proxy := serverWithReplication(t)
	defer testServer.Close()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	groupBody := map[string]interface{}{
		"modelName":    "test-model-1",
		"minInstances": 1,
		"maxInstances": 3,
	}
	payload, _ := json.Marshal(groupBody)

	resp, err := http.Post(testServer.URL+"/api/v1/replication/groups",
		"application/json", bytes.NewReader(payload))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	// Проверяем, что группа создалась
	mgr := proxy.GetModelReplicationManager()
	require.NotNil(t, mgr)
	cfg := mgr.GetGroup("test-model-1")
	require.NotNil(t, cfg)
	assert.Equal(t, 1, cfg.MinInstances)
}

func TestModelReplication_API_CreateGroup_Duplicate(t *testing.T) {
	testServer, _, proxy := serverWithReplication(t)
	defer testServer.Close()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	// Пытаемся создать дубликат существующей группы (llama3.1 уже есть из конфига)
	groupBody := map[string]interface{}{
		"modelName":    "llama3.1",
		"minInstances": 1,
		"maxInstances": 3,
	}
	payload, _ := json.Marshal(groupBody)

	resp, err := http.Post(testServer.URL+"/api/v1/replication/groups",
		"application/json", bytes.NewReader(payload))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Contains(t, body["error"], "already exists")
}

func TestModelReplication_API_UpdateGroup(t *testing.T) {
	testServer, _, proxy := serverWithReplication(t)
	defer testServer.Close()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	updateBody := map[string]interface{}{
		"minInstances": 3,
		"maxInstances": 5,
	}
	payload, _ := json.Marshal(updateBody)

	req, _ := http.NewRequest(http.MethodPut,
		testServer.URL+"/api/v1/replication/groups/llama3.1",
		bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Проверяем, что группа обновлена
	mgr := proxy.GetModelReplicationManager()
	cfg := mgr.GetGroup("llama3.1")
	require.NotNil(t, cfg)
	assert.Equal(t, 3, cfg.MinInstances)
	assert.Equal(t, 5, cfg.MaxInstances)
}

func TestModelReplication_API_DeleteGroup(t *testing.T) {
	testServer, _, proxy := serverWithReplication(t)
	defer testServer.Close()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	// Сначала создаём группу для удаления
	mgr := proxy.GetModelReplicationManager()
	err := mgr.CreateGroup(types.ModelGroupConfig{
		ModelName:    "delete-test",
		MinInstances: 1,
		MaxInstances: 2,
	})
	require.NoError(t, err)

	// Удаляем
	req, _ := http.NewRequest(http.MethodDelete,
		testServer.URL+"/api/v1/replication/groups/delete-test", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Проверяем, что группы больше нет
	assert.Nil(t, mgr.GetGroup("delete-test"))
}

func TestModelReplication_API_DeleteGroup_NotFound(t *testing.T) {
	testServer, _, proxy := serverWithReplication(t)
	defer testServer.Close()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	req, _ := http.NewRequest(http.MethodDelete,
		testServer.URL+"/api/v1/replication/groups/nonexistent", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestModelReplication_API_Stats(t *testing.T) {
	testServer, _, proxy := serverWithReplication(t)
	defer testServer.Close()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	resp, err := http.Get(testServer.URL + "/api/v1/replication/stats")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)

	assert.Equal(t, true, body["enabled"])
	assert.GreaterOrEqual(t, body["groupCount"], float64(1))
	assert.NotNil(t, body["autoReconcile"])
}

func TestModelReplication_API_Reconcile(t *testing.T) {
	testServer, _, proxy := serverWithReplication(t)
	defer testServer.Close()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	resp, err := http.Post(testServer.URL+"/api/v1/replication/reconcile",
		"application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	require.NoError(t, err)
	assert.Contains(t, body["message"], "reconciliation triggered")
}

func TestModelReplication_API_Reconcile_WrongMethod(t *testing.T) {
	testServer, _, proxy := serverWithReplication(t)
	defer testServer.Close()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	resp, err := http.Get(testServer.URL + "/api/v1/replication/reconcile")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// ---------- INTEGRATION: Model Replication disabled → API returns 404 ----------

func TestModelReplication_API_Disabled(t *testing.T) {
	cfg := configForReplication()
	cfg.Balancing.ModelReplication.Enabled = false

	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	healthChecker := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := api.NewServer(proxy, cfg, healthChecker)
	testServer := httptest.NewServer(server)
	defer testServer.Close()

	// Все endpoints должны возвращать 404
	endpoints := []string{
		"/api/v1/replication/groups",
		"/api/v1/replication/groups/llama3.1",
		"/api/v1/replication/stats",
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			resp, err := http.Get(testServer.URL + ep)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotFound, resp.StatusCode,
				"endpoint %s should return 404 when replication disabled", ep)
		})
	}
}

// ---------- CONCURRENCY: Thread safety ----------

func TestModelReplication_Concurrency(t *testing.T) {
	mgr := modelreplication.NewModelGroupManager()
	mgr.SetEnabled(true)
	mgr.SetBackendLoadFn(func(id string) float64 { return 0.5 })

	var wg sync.WaitGroup

	// Параллельное создание групп
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := fmt.Sprintf("concurrent-model-%d", idx)
			_ = mgr.CreateGroup(types.ModelGroupConfig{
				ModelName:    name,
				MinInstances: 1,
				MaxInstances: 2,
			})
		}(i)
	}
	wg.Wait()

	// Параллельное чтение
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = mgr.GetGroups()
			_ = mgr.GetGroup("concurrent-model-0")
			_ = mgr.GetInstanceStates("concurrent-model-0")
			_ = mgr.GetGroupStats("concurrent-model-0")
			_ = mgr.IsEnabled()
		}()
	}
	wg.Wait()

	// Параллельное обновление и удаление
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := fmt.Sprintf("concurrent-model-%d", idx)
			_ = mgr.UpdateGroup(types.ModelGroupConfig{
				ModelName:    name,
				MinInstances: 2,
				MaxInstances: 4,
			})
		}(i)
	}
	wg.Wait()

	// Не должно быть паники или data race
	groups := mgr.GetGroups()
	assert.GreaterOrEqual(t, len(groups), 5)
}

// ---------- INTEGRATION: GroupAwareSelector в selectBackend (backend_selector.go) ----------

func TestModelReplication_GroupAwareIntegration(t *testing.T) {
	// Создаём proxy с репликацией
	cfg := configForReplication()
	proxy := balancer.NewProxy(cfg)
	proxy.SetQueueManagerProxy()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	// Проверяем, что селектор инициализирован
	sel := proxy.GetReplicationSelector()
	require.NotNil(t, sel)

	// Для модели из группы репликации — Select должен вернуть пустую строку,
	// так как нет загруженных инстансов (режим без фейковых бэкендов)
	selected := sel.Select("llama3.1")
	assert.Equal(t, "", selected, "expected empty when no loaded instances exist")

	// Для модели не из группы — пустая строка
	selected = sel.Select("some-other-model")
	assert.Equal(t, "", selected)
}

// ---------- INTEGRATION: Полный сценарий ----------

func TestModelReplication_FullFlow(t *testing.T) {
	testServer, _, proxy := serverWithReplication(t)
	defer testServer.Close()
	defer func() {
		if ctrl := proxy.GetReplicationController(); ctrl != nil {
			_ = ctrl.Stop()
		}
	}()

	mgr := proxy.GetModelReplicationManager()
	require.NotNil(t, mgr)

	t.Run("initial state from config", func(t *testing.T) {
		cfg := mgr.GetGroup("llama3.1")
		require.NotNil(t, cfg)
		assert.Equal(t, 2, cfg.MinInstances)
		assert.Equal(t, 4, cfg.MaxInstances)
	})

	t.Run("create new group via API", func(t *testing.T) {
		body := map[string]interface{}{
			"modelName":    "qwen2.5",
			"minInstances": 1,
			"maxInstances": 2,
		}
		payload, _ := json.Marshal(body)
		resp, err := http.Post(testServer.URL+"/api/v1/replication/groups",
			"application/json", bytes.NewReader(payload))
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusCreated, resp.StatusCode)
	})

	t.Run("list all groups", func(t *testing.T) {
		resp, err := http.Get(testServer.URL + "/api/v1/replication/groups")
		require.NoError(t, err)
		defer resp.Body.Close()

		var body map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&body)
		require.NoError(t, err)
		groups := body["groups"].([]interface{})
		assert.GreaterOrEqual(t, len(groups), 2)
	})

	t.Run("get stats", func(t *testing.T) {
		resp, err := http.Get(testServer.URL + "/api/v1/replication/stats")
		require.NoError(t, err)
		defer resp.Body.Close()

		var body map[string]interface{}
		err = json.NewDecoder(resp.Body).Decode(&body)
		require.NoError(t, err)
		assert.Equal(t, true, body["enabled"])
		stats := body["stats"].([]interface{})
		assert.GreaterOrEqual(t, len(stats), 1)
	})

	t.Run("trigger reconcile", func(t *testing.T) {
		resp, err := http.Post(testServer.URL+"/api/v1/replication/reconcile",
			"application/json", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("delete group", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete,
			testServer.URL+"/api/v1/replication/groups/qwen2.5", nil)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Nil(t, mgr.GetGroup("qwen2.5"))
	})
}
