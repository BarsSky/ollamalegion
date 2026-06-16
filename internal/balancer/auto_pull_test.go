package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"ollama-loadbalancer/pkg/types"
)

// TestIsModelNotFoundError — проверка детекции ошибок Ollama
func TestIsModelNotFoundError(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected bool
	}{
		{
			name:     "model not found",
			body:     `{"error":"model \"llama3.2:1b\" not found"}`,
			expected: true,
		},
		{
			name:     "model does not exist",
			body:     `{"error":"model \"mistral\" does not exist"}`,
			expected: true,
		},
		{
			name:     "model not exist",
			body:     `{"error":"model not exist"}`,
			expected: true,
		},
		{
			name:     "model not supported",
			body:     `{"error":"model \"gpt-4\" not supported"}`,
			expected: true,
		},
		{
			name:     "other error",
			body:     `{"error":"internal server error"}`,
			expected: false,
		},
		{
			name:     "not found without model",
			body:     `{"error":"not found"}`,
			expected: false,
		},
		{
			name:     "empty error",
			body:     `{"error":""}`,
			expected: false,
		},
		{
			name:     "invalid json",
			body:     `not json`,
			expected: false,
		},
		{
			name:     "model not found mixed case",
			body:     `{"error":"Model \"test\" Not Found"}`,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isModelNotFoundError([]byte(tt.body))
			assert.Equal(t, tt.expected, result, "isModelNotFoundError(%q)", tt.body)
		})
	}
}

// TestNewAutoPullManager — проверка создания менеджера с значениями по умолчанию
func TestNewAutoPullManager(t *testing.T) {
	proxy := createTestProxy(t)

	config := types.AutoPullConfig{
		Enabled: true,
	}
	apm := NewAutoPullManager(proxy, config)

	assert.NotNil(t, apm)
	assert.Equal(t, proxy, apm.proxy)
	assert.Equal(t, 3, apm.config.MaxConcurrent, "default MaxConcurrent should be 3")
	assert.Equal(t, "5m", apm.config.PullTimeout, "default PullTimeout should be 5m")
	assert.Equal(t, 1, apm.config.RetryCount, "default RetryCount should be 1")
	assert.Empty(t, apm.activePulls)
	assert.NotNil(t, apm.httpClient)
}

// TestAutoPullConfigDefaults — проверка значений по умолчанию через SetConfig
func TestAutoPullConfigDefaults(t *testing.T) {
	proxy := createTestProxy(t)

	apm := NewAutoPullManager(proxy, types.AutoPullConfig{Enabled: true})

	// Проверяем GetConfig
	cfg := apm.GetConfig()
	assert.True(t, cfg.Enabled)
	assert.Equal(t, 3, cfg.MaxConcurrent)
	assert.Equal(t, "5m", cfg.PullTimeout)
	assert.Equal(t, 1, cfg.RetryCount)

	// Проверяем IsEnabled
	assert.True(t, apm.IsEnabled())

	// Выключаем
	apm.SetConfig(types.AutoPullConfig{Enabled: false})
	assert.False(t, apm.IsEnabled())
}

// TestAutoPullIsPullInProgress — проверка детекции активной загрузки
func TestAutoPullIsPullInProgress(t *testing.T) {
	proxy := createTestProxy(t)
	apm := NewAutoPullManager(proxy, types.AutoPullConfig{Enabled: true})

	assert.False(t, apm.IsPullInProgress("llama3"))

	// Добавляем фиктивный pull вручную
	apm.mu.Lock()
	apm.activePulls["llama3"] = &PullState{
		Model:     "llama3",
		BackendID: "backend-1",
		StartedAt: time.Now(),
		Done:      make(chan struct{}),
	}
	apm.mu.Unlock()

	assert.True(t, apm.IsPullInProgress("llama3"))
	assert.False(t, apm.IsPullInProgress("mistral"))
}

// TestAutoPullGetActivePulls — проверка получения списка активных загрузок
func TestAutoPullGetActivePulls(t *testing.T) {
	proxy := createTestProxy(t)
	apm := NewAutoPullManager(proxy, types.AutoPullConfig{Enabled: true})

	// Пустой список
	pulls := apm.GetActivePulls()
	assert.Empty(t, pulls)

	// Добавляем pull
	apm.mu.Lock()
	apm.activePulls["llama3"] = &PullState{
		Model:     "llama3",
		BackendID: "backend-1",
		StartedAt: time.Now().Add(-30 * time.Second),
		Done:      make(chan struct{}),
	}
	apm.mu.Unlock()

	pulls = apm.GetActivePulls()
	assert.Len(t, pulls, 1)
	assert.Equal(t, "llama3", pulls[0]["model"])
	assert.Equal(t, "backend-1", pulls[0]["backendId"])
	assert.GreaterOrEqual(t, pulls[0]["elapsedSec"], 29)
}

// TestAutoPullEnsureModelDisabled — проверка возврата ошибки при выключенном AutoPull
func TestAutoPullEnsureModelDisabled(t *testing.T) {
	proxy := createTestProxy(t)
	apm := NewAutoPullManager(proxy, types.AutoPullConfig{Enabled: false})

	backendID, err := apm.EnsureModel("llama3")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "auto-pull disabled")
	assert.Empty(t, backendID)
}

// TestAutoPullGetPullTimeout — проверка парсинга таймаута
func TestAutoPullGetPullTimeout(t *testing.T) {
	proxy := createTestProxy(t)

	tests := []struct {
		config   types.AutoPullConfig
		expected time.Duration
	}{
		{types.AutoPullConfig{Enabled: true, PullTimeout: "5m"}, 5 * time.Minute},
		{types.AutoPullConfig{Enabled: true, PullTimeout: "10m"}, 10 * time.Minute},
		{types.AutoPullConfig{Enabled: true, PullTimeout: "30s"}, 30 * time.Second},
		{types.AutoPullConfig{Enabled: true, PullTimeout: ""}, 5 * time.Minute},   // default
		{types.AutoPullConfig{Enabled: true, PullTimeout: "invalid"}, 5 * time.Minute}, // fallback
	}

	for _, tt := range tests {
		t.Run(tt.config.PullTimeout, func(t *testing.T) {
			apm := NewAutoPullManager(proxy, tt.config)
			assert.Equal(t, tt.expected, apm.getPullTimeout())
		})
	}
}

// TestAutoPullCleanupStalePulls — проверка очистки устаревших загрузок
func TestAutoPullCleanupStalePulls(t *testing.T) {
	proxy := createTestProxy(t)
	apm := NewAutoPullManager(proxy, types.AutoPullConfig{Enabled: true})

	// Добавляем несколько pull'ов
	apm.mu.Lock()
	apm.activePulls["old"] = &PullState{
		Model:     "old",
		BackendID: "backend-1",
		StartedAt: time.Now().Add(-10 * time.Minute),
		Done:      make(chan struct{}),
	}
	apm.activePulls["new"] = &PullState{
		Model:     "new",
		BackendID: "backend-2",
		StartedAt: time.Now(),
		Done:      make(chan struct{}),
	}
	apm.mu.Unlock()

	assert.Len(t, apm.activePulls, 2)

	// Чистим старше 5 минут
	apm.CleanupStalePulls(5 * time.Minute)

	apm.mu.Lock()
	assert.Len(t, apm.activePulls, 1)
	_, oldExists := apm.activePulls["old"]
	assert.False(t, oldExists, "old pull should be cleaned")
	_, newExists := apm.activePulls["new"]
	assert.True(t, newExists, "new pull should remain")
	apm.mu.Unlock()
}

// TestAutoPullFindBackendWithModelReady — поиск бэкенда с уже загруженной моделью
func TestAutoPullFindBackendWithModelReady(t *testing.T) {
	proxy := createTestProxy(t)

	// Подготавливаем метрики: на backend-1 есть модель llama3
	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.metrics["backend-1"] = &types.BackendMetrics{
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "llama3:8b"},
				{Name: "mistral:7b"},
			},
		},
	}
	proxy.metricsMgr.metrics["backend-2"] = &types.BackendMetrics{
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "codellama:7b"},
			},
		},
	}
	proxy.metricsMgr.mu.Unlock()

	apm := NewAutoPullManager(proxy, types.AutoPullConfig{Enabled: true})

	// Точное совпадение
	backendID := apm.findBackendWithModelReady("llama3:8b")
	assert.Equal(t, "backend-1", backendID)

	// Частичное совпадение (Contains)
	backendID = apm.findBackendWithModelReady("llama3")
	assert.Equal(t, "backend-1", backendID)

	// Нет такой модели
	backendID = apm.findBackendWithModelReady("nonexistent")
	assert.Empty(t, backendID)
}

// TestAutoPullExecutePullSuccess — проверка успешного pull модели
func TestAutoPullExecutePullSuccess(t *testing.T) {
	// Создаём тестовый Ollama сервер, который успешно обрабатывает pull
	pullHandled := false
	ollamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pull" && r.Method == http.MethodPost {
			pullHandled = true

			// Проверяем тело запроса
			var reqBody map[string]interface{}
			json.NewDecoder(r.Body).Decode(&reqBody)
			assert.Equal(t, "llama3", reqBody["name"])
			assert.False(t, reqBody["stream"].(bool), "stream should be false")

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"success"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ollamaServer.Close()

	// Создаём прокси с тестовым бэкендом
	config := createTestConfig()
	config.Backends[0].Host = "127.0.0.1"
	config.Backends[0].OllamaPort = mustParsePort(ollamaServer.URL)
	config.Backends[0].Status = types.StatusHealthy
	proxy := NewProxy(config)

	apm := NewAutoPullManager(proxy, types.AutoPullConfig{Enabled: true})

	// Создаём состояние pull'а
	state := &PullState{
		Model:     "llama3",
		BackendID: "backend-1",
		StartedAt: time.Now(),
		Done:      make(chan struct{}),
	}

	// Выполняем pull (синхронно, т.к. executePull сам закроет Done)
	apm.executePull(state)

	assert.True(t, pullHandled, "/api/pull should have been called")
	assert.NoError(t, state.Error)
	assert.Equal(t, http.StatusOK, state.HTTPStatus)
}

// TestAutoPullExecutePullError — проверка ошибки при pull модели
func TestAutoPullExecutePullError(t *testing.T) {
	// Создаём тестовый Ollama сервер, который возвращает ошибку
	ollamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/pull" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"model \"llama3\" not found"}`))
			return
		}
	}))
	defer ollamaServer.Close()

	config := createTestConfig()
	config.Backends[0].Host = "127.0.0.1"
	config.Backends[0].OllamaPort = mustParsePort(ollamaServer.URL)
	config.Backends[0].Status = types.StatusHealthy
	proxy := NewProxy(config)

	apm := NewAutoPullManager(proxy, types.AutoPullConfig{Enabled: true})

	state := &PullState{
		Model:     "llama3",
		BackendID: "backend-1",
		StartedAt: time.Now(),
		Done:      make(chan struct{}),
	}

	apm.executePull(state)

	assert.Error(t, state.Error)
	assert.Contains(t, state.Error.Error(), "ollama pull failed")
	assert.Contains(t, state.Error.Error(), "model")
	assert.Equal(t, http.StatusBadRequest, state.HTTPStatus)
}

// TestAutoPullExecutePullBackendNotFound — проверка ошибки при несуществующем бэкенде
func TestAutoPullExecutePullBackendNotFound(t *testing.T) {
	proxy := createTestProxy(t)
	apm := NewAutoPullManager(proxy, types.AutoPullConfig{Enabled: true})

	state := &PullState{
		Model:     "llama3",
		BackendID: "nonexistent-backend",
		StartedAt: time.Now(),
		Done:      make(chan struct{}),
	}

	apm.executePull(state)

	assert.Error(t, state.Error)
	assert.Contains(t, state.Error.Error(), "nonexistent-backend")
}

// TestAutoPullFindBestBackendForPull — проверка выбора лучшего бэкенда
func TestAutoPullFindBestBackendForPull(t *testing.T) {
	proxy := createTestProxy(t)

	// Настраиваем метрики: backend-2 имеет больше свободного места
	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.metrics["backend-1"] = &types.BackendMetrics{
		System: types.SystemMetrics{
			DiskFree: 50 * 1024, // 50 GB
		},
		Ollama: types.OllamaMetrics{
			AvailableModels: []types.RunningModel{
				{Name: "codellama:7b"},
			},
		},
	}
	proxy.metricsMgr.metrics["backend-2"] = &types.BackendMetrics{
		System: types.SystemMetrics{
			DiskFree: 100 * 1024, // 100 GB
		},
		Ollama: types.OllamaMetrics{
			AvailableModels: []types.RunningModel{
				{Name: "llama3:8b"},
			},
		},
	}
	proxy.metricsMgr.mu.Unlock()

	apm := NewAutoPullManager(proxy, types.AutoPullConfig{Enabled: true})

	// Ищем бэкенд для llama3 — должен выбрать backend-2 (больше места + есть available)
	backendID := apm.findBestBackendForPull("llama3")
	assert.Equal(t, "backend-2", backendID,
		"should pick backend-2 (more free disk + model available)")

	// Для модели codellama — backend-1 (есть available) или backend-2 (больше места)
	backendID = apm.findBestBackendForPull("codellama")
	assert.NotEmpty(t, backendID, "should find some backend")
}

// TestAutoPullBackendStatusUnhealthy — проверка пропуска unhealthy бэкендов
func TestAutoPullBackendStatusUnhealthy(t *testing.T) {
	proxy := createTestProxy(t)

	// Делаем backend-1 unhealthy
	proxy.mu.Lock()
	proxy.backends["backend-1"].Backend.Status = types.StatusUnhealthy
	proxy.mu.Unlock()

	// Настраиваем метрики
	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.metrics["backend-1"] = &types.BackendMetrics{
		System: types.SystemMetrics{DiskFree: 100 * 1024},
	}
	proxy.metricsMgr.metrics["backend-2"] = &types.BackendMetrics{
		System: types.SystemMetrics{DiskFree: 10 * 1024},
	}
	proxy.metricsMgr.mu.Unlock()

	apm := NewAutoPullManager(proxy, types.AutoPullConfig{Enabled: true})

	// findBackendWithModelReady — должен пропустить unhealthy
	apm.proxy.metricsMgr.mu.Lock()
	apm.proxy.metricsMgr.metrics["backend-1"].Ollama.RunningModels = []types.RunningModel{
		{Name: "llama3:8b"},
	}
	apm.proxy.metricsMgr.mu.Unlock()

	// Даже если на backend-1 есть модель, он unhealthy — не должен вернуться
	backendID := apm.findBackendWithModelReady("llama3:8b")
	assert.Empty(t, backendID, "unhealthy backends should be skipped")

	// findBestBackendForPull — тоже должен пропустить unhealthy
	backendID = apm.findBestBackendForPull("llama3")
	assert.Equal(t, "backend-2", backendID, "should pick healthy backend-2")
}

// TestAutoPullEnsureModelConcurrentLimit — проверка лимита одновременных загрузок
func TestAutoPullEnsureModelConcurrentLimit(t *testing.T) {
	proxy := createTestProxy(t)
	apm := NewAutoPullManager(proxy, types.AutoPullConfig{
		Enabled:       true,
		MaxConcurrent: 1,
	})

	// Заполняем activePulls до лимита
	apm.mu.Lock()
	apm.activePulls["busy-model"] = &PullState{
		Model:     "busy-model",
		BackendID: "backend-1",
		StartedAt: time.Now(),
		Done:      make(chan struct{}),
	}
	apm.mu.Unlock()

	// EnsureModel должен вернуть ошибку о превышении лимита, даже до поиска бэкенда
	_, err := apm.EnsureModel("another-model")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "max concurrent pulls reached")
}

// TestAutoPullEnsureModelDedup — проверка dedup (повторный запрос той же модели)
func TestAutoPullEnsureModelDedup(t *testing.T) {
	proxy := createTestProxy(t)
	apm := NewAutoPullManager(proxy, types.AutoPullConfig{
		Enabled:       true,
		MaxConcurrent: 3,
		PullTimeout:   "2s",
	})

	// Создаём pull, который быстро завершится успешно
	done := make(chan struct{})
	apm.mu.Lock()
	apm.activePulls["llama3"] = &PullState{
		Model:     "llama3",
		BackendID: "backend-1",
		StartedAt: time.Now(),
		Done:      done,
		Error:     nil, // успех
	}
	apm.mu.Unlock()

	// Завершаем pull через небольшое время (симуляция успешной загрузки)
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(done)
	}()

	// Повторный запрос той же модели — должен дождаться и вернуть backendID
	backendID, err := apm.EnsureModel("llama3")
	assert.NoError(t, err)
	assert.Equal(t, "backend-1", backendID)
}

// TestAutoPullEnsureModelDedupError — проверка dedup с ошибкой
func TestAutoPullEnsureModelDedupError(t *testing.T) {
	proxy := createTestProxy(t)
	apm := NewAutoPullManager(proxy, types.AutoPullConfig{
		Enabled:       true,
		MaxConcurrent: 3,
		PullTimeout:   "2s",
	})

	// Создаём pull, который завершится с ошибкой
	done := make(chan struct{})
	apm.mu.Lock()
	apm.activePulls["llama3"] = &PullState{
		Model:     "llama3",
		BackendID: "backend-1",
		StartedAt: time.Now(),
		Done:      done,
		Error:     fmt.Errorf("download failed"),
	}
	apm.mu.Unlock()

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(done)
	}()

	_, err := apm.EnsureModel("llama3")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "download failed")
}

// TestAutoPullEnsureModelTimeout — проверка таймаута ожидания
func TestAutoPullEnsureModelTimeout(t *testing.T) {
	proxy := createTestProxy(t)
	apm := NewAutoPullManager(proxy, types.AutoPullConfig{
		Enabled:       true,
		MaxConcurrent: 3,
		PullTimeout:   "100ms", // очень короткий таймаут
	})

	// Создаём pull, который никогда не завершается
	neverDone := make(chan struct{})
	apm.mu.Lock()
	apm.activePulls["llama3"] = &PullState{
		Model:     "llama3",
		BackendID: "backend-1",
		StartedAt: time.Now(),
		Done:      neverDone,
	}
	apm.mu.Unlock()

	_, err := apm.EnsureModel("llama3")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

// Helper: mustParsePort — извлечение порта из URL тестового сервера
func mustParsePort(urlStr string) int {
	var port int
	fmt.Sscanf(urlStr, "http://127.0.0.1:%d", &port)
	if port == 0 {
		fmt.Sscanf(urlStr, "http://localhost:%d", &port)
	}
	if port == 0 {
		port = 11434
	}
	return port
}

// Helper: createTestProxy — создание прокси с двумя тестовыми бэкендами
func createTestProxy(t *testing.T) *Proxy {
	t.Helper()

	config := createTestConfig()
	// Добавляем минимальную конфигурацию auto-pull
	config.Balancing.AutoPull = types.AutoPullConfig{
		Enabled:       true,
		MaxConcurrent: 3,
		PullTimeout:   "5m",
		RetryCount:    1,
	}

	proxy := NewProxy(config)
	require.NotNil(t, proxy)

	// Оба бэкенда healthy
	for _, state := range proxy.backends {
		state.Backend.Status = types.StatusHealthy
	}

	return proxy
}

// TestAutoPullFullScenario — интеграционный тест полного сценария:
// proxyRequest → ModelNotFoundError → AutoPull.EnsureModel → executePull → retry → success
func TestAutoPullFullScenario(t *testing.T) {
	// Счётчик запросов к /api/generate (первый = 404, второй = 200 после pull)
	generateCallCount := 0
	pullCalled := false
	var mu sync.Mutex

	// Создаём тестовый Ollama сервер
	ollamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch r.URL.Path {
		case "/api/generate":
			generateCallCount++
			if generateCallCount == 1 {
				// Первый вызов — модель не найдена
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":"model \"test-model:latest\" not found"}`))
				return
			}
			// Второй вызов — после pull, успех
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"model":"test-model:latest","response":"generated text","done":true}`))

		case "/api/pull":
			pullCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"success"}`))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ollamaServer.Close()

	// Создаём конфигурацию с AutoPull включённым
	config := createTestConfig()
	config.Backends[0].Host = "127.0.0.1"
	config.Backends[0].OllamaPort = mustParsePort(ollamaServer.URL)
	config.Balancing.AutoPull = types.AutoPullConfig{
		Enabled:       true,
		MaxConcurrent: 3,
		PullTimeout:   "10s",
		RetryCount:    1,
	}
	// Убираем второй бэкенд, чтобы не было альтернатив
	config.Backends = config.Backends[:1]

	proxy := NewProxy(config)
	require.NotNil(t, proxy)
	require.NotNil(t, proxy.AutoPull, "AutoPullManager should be initialized")

	// Этот тест проверяет Ollama-flow: ServeHTTP → proxyRequest →
	// ModelNotFoundError → AutoPull → executePull → retry → success.
	// В текущей архитектуре ServeHTTP сначала вызывает routeRequest, который
	// при OperatingMode="" (default в createTestConfig) допускает оба типа
	// бэкендов и сначала пробует LlamaCppRouter. Тестовый backend-1 имеет
	// пустой Type (Ollama по умолчанию), поэтому LlamaCppRouter.handleGenerate
	// возвращает 503 "no llama.cpp backend available" ещё до того, как мы
	// дошли до proxyRequest. Отключаем llama.cpp роутер в тесте, чтобы
	// flow шёл по основной Ollama-ветке, для которой тест и писался.
	proxy.llamaCppRouter = nil

	// Добавляем метрики, чтобы backend прошёл checkResourceLimits.
	// Модель НЕ указываем в RunningModels — пусть AutoPull сам её загрузит.
	// selectBackend выберет backend-1 через P3 (free) или P4 (fallback).
	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.metrics["backend-1"] = &types.BackendMetrics{
		System: types.SystemMetrics{
			DiskFree: 100000, // выше MinFreeMB=1000, чтобы checkResourceLimits пропустил
		},
	}
	proxy.metricsMgr.mu.Unlock()

	// Отправляем запрос на /api/generate с моделью, которой нет на бэкенде
	body := `{"model":"test-model:latest","prompt":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// В production ServeHTTP устанавливает modelContextKey после парсинга body.
	// Тест шлёт запрос напрямую — ставим ключ вручную, чтобы isModelNotFoundError
	// в proxyRequest нашёл modelFromCtx и EnsureModel получил имя модели.
	req = req.WithContext(context.WithValue(req.Context(), modelContextKey, "test-model:latest"))
	w := httptest.NewRecorder()

	// Выполняем через ServeHTTP — полный цикл:
	// selectBackend → proxyRequest → ModelNotFoundError → AutoPull → executePull → retry → success
	proxy.ServeHTTP(w, req)

	// Проверяем результаты
	mu.Lock()
	assert.Equal(t, 2, generateCallCount, "should have made 2 generate requests (1st fail, 2nd success)")
	assert.True(t, pullCalled, "auto-pull should have been triggered")
	mu.Unlock()

	// Проверяем успешный ответ
	assert.Equal(t, http.StatusOK, w.Code, "final response should be 200 OK")
	assert.Contains(t, w.Body.String(), "generated text", "response should contain generated text")

	// Проверяем, что activePulls очищен после успешного pull
	assert.False(t, proxy.AutoPull.IsPullInProgress("test-model"), "pull should be cleaned up after success")
}
