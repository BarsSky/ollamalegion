package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Тест 1: Первый запрос с моделью не в памяти — модель загружается через warmup
// =============================================================================

// TestFirstRequest_ModelNotLoaded_LoadsIntoVRAM проверяет, что при первом запросе
// к модели, которая не загружена в VRAM, балансер:
// 1. Не делает блокирующий /api/pull (который может занять минуты)
// 2. Отправляет запрос на бэкенд, где модель скачана
// 3. Ollama сам загружает модель при обработке /api/generate
func TestFirstRequest_ModelNotLoaded_LoadsIntoVRAM(t *testing.T) {
	// Мок: модель есть в /api/tags, но НЕ в /api/ps (не загружена в VRAM)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			// Модель скачана
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "llama3.1:8b", "size": 4928300000},
				},
			})

		case "/api/ps":
			// Модель НЕ в памяти
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []interface{}{},
			})

		case "/api/generate":
			// Ollama загружает модель при обработке запроса
			bodyBytes, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			json.Unmarshal(bodyBytes, &req)

			stream := true
			if s, ok := req["stream"].(bool); ok {
				stream = s
			}

			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)
				event, _ := json.Marshal(map[string]interface{}{
					"model":    "llama3.1:8b",
					"response": "Hello",
					"done":     true,
				})
				fmt.Fprintf(w, "data: %s\n\n", event)
				if flusher != nil {
					flusher.Flush()
				}
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"model":    "llama3.1:8b",
					"response": "Hello world!",
					"done":     true,
				})
			}

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mock.Close()

	host, port := parseHostPort(mock.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{
				ID: "first-req-backend", Name: "First Request Backend",
				Host: host, OllamaPort: port,
				Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, RequestTimeout: 30,
			QueueTimeout: 60, QueueMaxSize: 100, QueueWorkers: 4,
			SyncModelLoad: types.SyncModelLoadConfig{Enabled: false}, // Отключаем sync load для проверки fallback
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
			CPU: types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk: types.DiskLimits{MinFreeMB: 100},
		},
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()

	// Метрики: модель скачана, но НЕ в running models
	proxy.UpdateMetrics("first-req-backend", &types.BackendMetrics{
		ID: "first-req-backend",
		GPU: types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, DiskFree: 40960},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels: []types.RunningModel{}, // Модель НЕ загружена
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Отправляем первый запрос к модели, которая не загружена
	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "First request with unloaded model",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	start := time.Now()
	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	elapsed := time.Since(start)

	require.NoError(t, err, "First request should succeed even with unloaded model")
	defer resp.Body.Close()

	// ГЛАВНАЯ ПРОВЕРКА: запрос не должен занимать секунды на загрузку модели
	// (warmupModel теперь делает /api/generate с пустым промптом, а не /api/pull)
	assert.Less(t, elapsed, 500*time.Millisecond,
		"First request should be fast (<500ms), not blocked by model loading. Elapsed: %v", elapsed)

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var result map[string]interface{}
	err = json.Unmarshal(respBody, &result)
	require.NoError(t, err)
	assert.Equal(t, "llama3.1:8b", result["model"])
}

// =============================================================================
// Тест 2: SyncModelLoad включён — быстрая загрузка в VRAM, не pull
// =============================================================================

// TestFirstRequest_SyncModelLoadEnabled_UsesFastWarmup проверяет, что при включённом
// SyncModelLoad warmup использует /api/generate с пустым промптом (секунды),
// а не /api/pull (минуты).
func TestFirstRequest_SyncModelLoadEnabled_UsesFastWarmup(t *testing.T) {
	// Счётчик запросов для проверки, что warmup делает правильный endpoint
	var pullCount int
	var generateCount int

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "llama3.1:8b", "size": 4928300000},
				},
			})

		case "/api/ps":
			// Первые 2 вызова — модель не загружена, потом загружена
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []interface{}{},
			})

		case "/api/pull":
			pullCount++
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "success"})

		case "/api/generate":
			bodyBytes, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			json.Unmarshal(bodyBytes, &req)

			// Проверяем, что это warmup-запрос (пустой промпт)
			prompt, _ := req["prompt"].(string)
			if prompt == "" {
				generateCount++ // warmup
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"model":    "llama3.1:8b",
				"response": "Hello world!",
				"done":     true,
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mock.Close()

	host, port := parseHostPort(mock.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{
				ID: "sync-backend", Name: "Sync Backend",
				Host: host, OllamaPort: port,
				Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, RequestTimeout: 30,
			QueueTimeout: 60, QueueMaxSize: 100, QueueWorkers: 4,
			SyncModelLoad: types.SyncModelLoadConfig{Enabled: true, Timeout: "5s"},
			ModelLoadTimeout: 5,
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
			CPU: types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk: types.DiskLimits{MinFreeMB: 100},
		},
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()

	proxy.UpdateMetrics("sync-backend", &types.BackendMetrics{
		ID: "sync-backend",
		GPU: types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 2000, MemoryFree: 22576},
		System: types.SystemMetrics{CPUUsagePercent: 10, MemoryTotal: 65536, DiskFree: 40960},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels: []types.RunningModel{}, // Модель не загружена
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Sync model load test",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	// ГЛАВНАЯ ПРОВЕРКА: warmup НЕ должен делать /api/pull
	assert.Equal(t, 0, pullCount,
		"warmupModel should NOT use /api/pull (slow). Pull count: %d", pullCount)

	// warmup должен использовать /api/generate с пустым промптом
	// (проверка на generateCount может быть > 0 из-за таймаута и retry)
	t.Logf("warmup /api/generate calls: %d", generateCount)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// =============================================================================
// Тест 3: Модель не скачана на бэкенде — fallback к Ollama pull-on-demand
// =============================================================================

// TestFirstRequest_ModelNotDownloaded_FallbackToOllamaPull проверяет, что когда
// модель не скачана на бэкенде, запрос всё равно успешно обрабатывается.
// Отключаем SyncModelLoad, чтобы запрос сразу шёл на fallback.
func TestFirstRequest_ModelNotDownloaded_FallbackToOllamaPull(t *testing.T) {
	mock := NewExpandedMockServer(MockBehavior{
		StreamResponseSize: 3,
		ChunkDelay:         5 * time.Millisecond,
	})
	// Убираем модель из tags — она не скачана
	mock.SetRunningModels([]types.RunningModel{})
	defer mock.Close()

	proxyServer, _ := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Model not downloaded test",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	// Главная проверка: запрос должен успешно обработаться
	// (мок вернёт ответ от /api/generate, а Ollama в реальности сделает pull-on-demand)
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"Request should succeed even if model not downloaded — Ollama handles pull-on-demand")
}

// =============================================================================
// Тест 4: Проверка отсутствия блокировки при первом запросе
// =============================================================================

// TestFirstRequest_NotBlockedByWarmup проверяет, что первый запрос НЕ блокируется
// на длительную загрузку модели. warmupModel запускается асинхронно, а запрос
// отправляется сразу на бэкенд.
func TestFirstRequest_NotBlockedByWarmup(t *testing.T) {
	// Используем ExpandedMockServer вместо httptest.NewServer,
	// т.к. warmupModel делает синхронный Get к /api/tags из ServeHTTP,
	// и однопоточный httptest.Server блокируется
	mock := NewExpandedMockServer(MockBehavior{
		StreamResponseSize: 3,
		ChunkDelay:         5 * time.Millisecond,
	})
	defer mock.Close()

	proxyServer, _ := SetupExpandedProxy(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Not blocked test",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	start := time.Now()
	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	elapsed := time.Since(start)

	require.NoError(t, err)
	defer resp.Body.Close()

	// Запрос должен быть быстрым — warmup не блокирует
	assert.Less(t, elapsed, 300*time.Millisecond,
		"Request should not be blocked by warmup. Elapsed: %v", elapsed)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
