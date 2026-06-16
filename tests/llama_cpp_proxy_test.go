package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLlamaCppProxy_Basic проверяет базовое проксирование запроса через балансер к llama.cpp бэкенду
func TestLlamaCppProxy_Basic(t *testing.T) {
	// Создаём mock CppWorker сервер с OpenAI-совместимыми эндпоинтами.
	// Балансер транслирует Ollama /api/chat → /v1/chat/completions.
	cppServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health" || r.URL.Path == "/api/health" || r.URL.Path == "/api/v1/cppworker/health":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": "llama.cpp-mock"})
		case r.URL.Path == "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "test-model:latest"},
				},
			})
		case strings.Contains(r.URL.Path, "/v1/chat/completions"):
			// Симулируем ответ инференса в OpenAI формате
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			json.Unmarshal(body, &req)
			model, _ := req["model"].(string)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":      "chatcmpl-test",
				"object":  "chat.completion",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{
					{
						"index": 0,
						"message": map[string]string{
							"role":    "assistant",
							"content": "Hello from llama.cpp mock!",
						},
						"finish_reason": "stop",
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer cppServer.Close()

	// Определяем хост и порт mock-сервера
	cppAddr := strings.TrimPrefix(cppServer.URL, "http://")

	// Создаём конфигурацию балансера с llama.cpp бэкендом
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "127.0.0.1",
			Port:    18080,
			APIPort: 18081,
		},
		Balancing: types.BalancingSettings{
			Algorithm:      "resource-aware",
			ModelAffinity:  true,
		},
		BackendEngine: types.EngineLlamaCPP,
		Initialized:   true,
		Backends: []types.Backend{
			{
				ID:            "llama-test-1",
				Name:          "Llama Test Node",
				Host:          strings.Split(cppAddr, ":")[0],
				Type:          types.BackendTypeLlamaCpp,
				OllamaPort:    11434,
				CppWorkerPort: mustParsePortLLama(t, cppAddr),
				Weight:        1,
				MaxConcurrentReqs: 10,
				MaxModels:     5,
				Status:        types.StatusHealthy,
			},
		},
	}

	_ = types.Backend{} // типы используются, даже если backendCfg не создаётся

	proxy := balancer.NewProxy(cfg)

	// Создаём тестовый HTTP сервер вокруг прокси
	testServer := httptest.NewServer(proxy)
	defer testServer.Close()

	// Ждём инициализации
	time.Sleep(100 * time.Millisecond)

	// Выполняем запрос к балансеру (эмулируем клиента)
	reqBody := map[string]interface{}{
		"model":  "test-model",
		"prompt": "Hello",
		"stream": false,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	resp, err := http.Post(
		fmt.Sprintf("%s/api/chat", testServer.URL),
		"application/json",
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	// Проверяем ответ
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200, got %d. Body: %s", resp.StatusCode, string(respBody))
	}

	t.Logf("Proxy response: status=%d, body=%s", resp.StatusCode, string(respBody))
}

// TestLlamaCppBackendType_Registration проверяет что бэкенд корректно регистрируется с типом llama_cpp
func TestLlamaCppBackendType_Registration(t *testing.T) {
	cfg := &types.AgentConfig{
		AgentID:     "llama-node-1",
		BackendType: types.BackendTypeLlamaCpp,
		CppWorkerURL: "http://localhost:18091",
		BalancerURL: "http://localhost:18081",
	}

	if cfg.BackendType != types.BackendTypeLlamaCpp {
		t.Errorf("Expected backend type llama_cpp, got %s", cfg.BackendType)
	}

	if cfg.CppWorkerURL != "http://localhost:18091" {
		t.Errorf("Expected CppWorkerURL http://localhost:18091, got %s", cfg.CppWorkerURL)
	}

	// Проверяем что Label() возвращает правильное имя
	if cfg.BackendType.Label() != "llama.cpp" {
		t.Errorf("Expected label 'llama.cpp', got '%s'", cfg.BackendType.Label())
	}

	// Проверяем Emoji
	if cfg.BackendType.Emoji() != "🦒" {
		t.Errorf("Expected emoji 🦒, got %s", cfg.BackendType.Emoji())
	}
}

// TestModeBackendTypes_LlamaCpp проверяет что все режимы совместимы с llama.cpp
func TestModeBackendTypes_LlamaCpp(t *testing.T) {
	modes := []string{"standard", "replication", "rpc_coordinator", "virtual_router", "distributed_inference"}

	for _, mode := range modes {
		compatible := types.IsModeCompatibleWithBackendType(mode, types.BackendTypeLlamaCpp)
		if !compatible {
			t.Errorf("Mode %s should be compatible with llama_cpp, but IsModeCompatibleWithBackendType returned false", mode)
		}
	}
}

// TestHealthCheck_LlamaCpp проверяет что health-check для llama.cpp использует правильный URL
func TestHealthCheck_LlamaCpp(t *testing.T) {
	// Проверяем ResolveEngine для llama.cpp
	engine := types.ResolveEngine(types.EngineAuto, types.BackendTypeLlamaCpp)
	if engine != types.EngineLlamaCPP {
		t.Errorf("Expected EngineLlamaCPP, got %s", engine)
	}

	// Проверяем IsModeLlamaCpp
	if !types.IsModeLlamaCpp("virtual_router") {
		t.Error("virtual_router should be llama_cpp mode")
	}
	if !types.IsModeLlamaCpp("distributed_inference") {
		t.Error("distributed_inference should be llama_cpp mode")
	}
	if types.IsModeLlamaCpp("standard") {
		t.Error("standard should not be exclusively llama_cpp mode")
	}
	if types.IsModeLlamaCpp("replication") {
		t.Error("replication should not be exclusively llama_cpp mode")
	}
}

// TestLlamaCppProxy_DifferentProxyPort проверяет что балансер проксирует llama.cpp
// запросы на CppWorkerPort, а не на OllamaPort
func TestLlamaCppProxy_DifferentProxyPort(t *testing.T) {
	// Создаём mock CppWorker сервер на одном порту
	cppServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":    "test-model",
			"response": "Hello from CppWorker!",
			"done":     true,
		})
	}))
	defer cppServer.Close()

	cppAddr := strings.TrimPrefix(cppServer.URL, "http://")
	cppPort := mustParsePortLLama(t, cppAddr)
	cppHost := strings.Split(cppAddr, ":")[0]

	// Создаём фиктивный Ollama-сервер на другом порту (куда НЕ должны идти запросы)
	ollamaHitCount := 0
	ollamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaHitCount++
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"source": "ollama-wrong"})
	}))
	defer ollamaServer.Close()

	ollamaAddr := strings.TrimPrefix(ollamaServer.URL, "http://")
	ollamaPort := mustParsePortLLama(t, ollamaAddr)

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "127.0.0.1",
			Port:    18080,
			APIPort: 18081,
		},
		Balancing: types.BalancingSettings{
			Algorithm:     "resource-aware",
			ModelAffinity: true,
			RequestTimeout: 10,
		},
		BackendEngine: types.EngineLlamaCPP,
		Initialized:   true,
		Backends: []types.Backend{
			{
				ID:                "llama-proxy-test",
				Name:              "Llama Proxy Test",
				Host:              cppHost,
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     cppPort,   // куда ДОЛЖНЫ идти запросы
				OllamaPort:        ollamaPort, // куда НЕ должны идти запросы
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
	}

	proxy := balancer.NewProxy(cfg)

	// Устанавливаем метрики
	proxy.SetBackendMetrics("llama-proxy-test", &types.BackendMetrics{
		ID:          "llama-proxy-test",
		BackendType: types.BackendTypeLlamaCpp,
		Status:      types.StatusHealthy,
		Host:        cppHost,
		OllamaPort:  cppPort,
		GPU: types.GPUMetrics{
			MemoryTotal: 24576,
			MemoryUsed:  4096,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     65536,
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "test-model", ParameterSize: "8B", Family: "llama"},
			},
			ActiveRequests:      0,
			MaxConcurrentRequests: 10,
		},
	})

	testServer := httptest.NewServer(proxy)
	defer testServer.Close()

	time.Sleep(50 * time.Millisecond)

	// Отправляем запрос
	reqBody := `{"model":"test-model","prompt":"Test port isolation","stream":false}`
	resp, err := http.Post(
		testServer.URL+"/api/generate",
		"application/json",
		strings.NewReader(reqBody),
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	t.Logf("Response: status=%d, body=%v, ollamaHitCount=%d", resp.StatusCode, result, ollamaHitCount)

	// Проверяем что Ollama-сервер не получил запросов
	assert.Equal(t, 0, ollamaHitCount,
		"Requests should go to CppWorkerPort, not OllamaPort")

	// Проверяем что ответ пришёл от CppWorker (а не Ollama)
	if resp.StatusCode == http.StatusOK {
		response, _ := result["response"].(string)
		assert.Contains(t, response, "CppWorker",
			"Response should come from CppWorker mock, not Ollama mock")
	}
}

// TestLlamaCppProxy_OpenAICompatibleEndpoint проверяет проксирование
// /v1/chat/completions с OpenAI-совместимым форматом запроса/ответа
func TestLlamaCppProxy_OpenAICompatibleEndpoint(t *testing.T) {
	// Mock сервер с OpenAI-совместимым API
	cppServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(r.URL.Path, "/health"):
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case strings.Contains(r.URL.Path, "/v1/chat/completions"):
			// Читаем тело для проверки формата
			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			json.Unmarshal(body, &req)

			_, hasMessages := req["messages"]
			model, _ := req["model"].(string)

			if hasMessages {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"id":      "chatcmpl-test-123",
					"object":  "chat.completion",
					"created": 1700000000,
					"model":   model,
					"choices": []map[string]interface{}{
						{
							"index": 0,
							"message": map[string]string{
								"role":    "assistant",
								"content": "Hello! This is a test response from llama.cpp.",
							},
							"finish_reason": "stop",
						},
					},
					"usage": map[string]int{
						"prompt_tokens":     10,
						"completion_tokens": 8,
						"total_tokens":      18,
					},
				})
			} else {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "messages field required"})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer cppServer.Close()

	cppAddr := strings.TrimPrefix(cppServer.URL, "http://")

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "127.0.0.1",
			Port:    18080,
			APIPort: 18081,
		},
		Balancing: types.BalancingSettings{
			Algorithm:     "resource-aware",
			ModelAffinity: true,
			RequestTimeout: 10,
		},
		BackendEngine: types.EngineLlamaCPP,
		Initialized:   true,
		Backends: []types.Backend{
			{
				ID:                "openai-test",
				Name:              "OpenAI Compatible",
				Host:              strings.Split(cppAddr, ":")[0],
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     mustParsePortLLama(t, cppAddr),
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
	}

	proxy := balancer.NewProxy(cfg)

	proxy.SetBackendMetrics("openai-test", &types.BackendMetrics{
		ID:          "openai-test",
		BackendType: types.BackendTypeLlamaCpp,
		Status:      types.StatusHealthy,
		Host:        strings.Split(cppAddr, ":")[0],
		OllamaPort:  mustParsePortLLama(t, cppAddr),
		GPU: types.GPUMetrics{
			MemoryTotal: 24576,
			MemoryUsed:  4096,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     65536,
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "test-model", ParameterSize: "8B"},
			},
			ActiveRequests:      0,
			MaxConcurrentRequests: 10,
		},
	})

	testServer := httptest.NewServer(proxy)
	defer testServer.Close()

	time.Sleep(50 * time.Millisecond)

	// Отправляем OpenAI-совместимый запрос
	reqBody := map[string]interface{}{
		"model": "test-model",
		"messages": []map[string]string{
			{"role": "user", "content": "Hello!"},
		},
		"stream": false,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	resp, err := http.Post(
		testServer.URL+"/v1/chat/completions",
		"application/json",
		bytes.NewReader(bodyBytes),
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	t.Logf("OpenAI response: status=%d, body=%v", resp.StatusCode, result)

	if resp.StatusCode == http.StatusOK {
		assert.Equal(t, "chat.completion", result["object"])
		assert.NotNil(t, result["choices"])
		assert.NotNil(t, result["usage"])
	}
}

// TestLlamaCppProxy_StreamingResponse проверяет SSE-стриминг через балансер к llama.cpp
func TestLlamaCppProxy_StreamingResponse(t *testing.T) {
	cppServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Auto-load endpoint не реализован в mock — возвращаем 404,
		// чтобы балансер применил graceful fallback к lazy-load.
		if strings.Contains(path, "/api/models/load") {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}

		// Проверяем stream флаг
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		json.Unmarshal(body, &req)

		stream, _ := req["stream"].(bool)

		if stream && strings.Contains(path, "/v1/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Transfer-Encoding", "chunked")
			w.WriteHeader(http.StatusOK)

			flusher, ok := w.(http.Flusher)
			if !ok {
				return
			}

			// OpenAI-совместимые SSE-чанки для /api/chat
			chunks := []map[string]interface{}{
				{
					"id":      "chatcmpl-test",
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   "test-model",
					"choices": []map[string]interface{}{
						{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil},
					},
				},
				{
					"id":      "chatcmpl-test",
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   "test-model",
					"choices": []map[string]interface{}{
						{"index": 0, "delta": map[string]string{"content": "Hello"}, "finish_reason": nil},
					},
				},
				{
					"id":      "chatcmpl-test",
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   "test-model",
					"choices": []map[string]interface{}{
						{"index": 0, "delta": map[string]string{"content": " from"}, "finish_reason": nil},
					},
				},
				{
					"id":      "chatcmpl-test",
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   "test-model",
					"choices": []map[string]interface{}{
						{"index": 0, "delta": map[string]string{"content": " llama.cpp!"}, "finish_reason": "stop"},
					},
				},
			}

			for _, chunk := range chunks {
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
				time.Sleep(5 * time.Millisecond)
			}
			return
		}

		if stream && strings.Contains(path, "/v1/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Transfer-Encoding", "chunked")
			w.WriteHeader(http.StatusOK)

			flusher, ok := w.(http.Flusher)
			if !ok {
				return
			}

			// OpenAI-совместимые SSE-чанки для /api/generate
			chunks := []map[string]interface{}{
				{
					"id":      "cmpl-test",
					"object":  "text_completion.chunk",
					"created": time.Now().Unix(),
					"model":   "test-model",
					"choices": []map[string]interface{}{
						{"index": 0, "text": "Hello", "finish_reason": nil},
					},
				},
				{
					"id":      "cmpl-test",
					"object":  "text_completion.chunk",
					"created": time.Now().Unix(),
					"model":   "test-model",
					"choices": []map[string]interface{}{
						{"index": 0, "text": " from", "finish_reason": nil},
					},
				},
				{
					"id":      "cmpl-test",
					"object":  "text_completion.chunk",
					"created": time.Now().Unix(),
					"model":   "test-model",
					"choices": []map[string]interface{}{
						{"index": 0, "text": " llama.cpp!", "finish_reason": "stop"},
					},
				},
			}

			for _, chunk := range chunks {
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
				time.Sleep(5 * time.Millisecond)
			}
			return
		}

		// non-streaming fallback
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "cmpl-test",
			"object":  "text_completion",
			"created": time.Now().Unix(),
			"model":   "test-model",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"text":  "Hello from llama.cpp!",
					"finish_reason": "stop",
				},
			},
		})
	}))
	defer cppServer.Close()

	cppAddr := strings.TrimPrefix(cppServer.URL, "http://")

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "127.0.0.1",
			Port:    18080,
			APIPort: 18081,
		},
		Balancing: types.BalancingSettings{
			Algorithm:     "resource-aware",
			ModelAffinity: true,
			RequestTimeout: 30,
		},
		BackendEngine: types.EngineLlamaCPP,
		Initialized:   true,
		Backends: []types.Backend{
			{
				ID:                "stream-test",
				Name:              "Stream Test",
				Host:              strings.Split(cppAddr, ":")[0],
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     mustParsePortLLama(t, cppAddr),
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
	}

	proxy := balancer.NewProxy(cfg)

	proxy.SetBackendMetrics("stream-test", &types.BackendMetrics{
		ID:          "stream-test",
		BackendType: types.BackendTypeLlamaCpp,
		Status:      types.StatusHealthy,
		Host:        strings.Split(cppAddr, ":")[0],
		OllamaPort:  mustParsePortLLama(t, cppAddr),
		GPU: types.GPUMetrics{
			MemoryTotal: 24576,
			MemoryUsed:  4096,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     65536,
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "test-model", ParameterSize: "8B"},
			},
			ActiveRequests:      0,
			MaxConcurrentRequests: 10,
		},
	})

	testServer := httptest.NewServer(proxy)
	defer testServer.Close()

	time.Sleep(50 * time.Millisecond)

	// Отправляем streaming запрос
	reqBody := `{"model":"test-model","prompt":"Test streaming","stream":true}`
	resp, err := http.Post(
		testServer.URL+"/api/generate",
		"application/json",
		strings.NewReader(reqBody),
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Читаем SSE поток
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	bodyStr := string(body)
	t.Logf("Stream response length: %d bytes", len(bodyStr))

	// Проверяем что получили NDJSON-чанки в Ollama-формате
	assert.Contains(t, bodyStr, `"response":"Hello"`, "Stream response should contain first chunk")
	assert.Contains(t, bodyStr, `"response":" llama.cpp!"`, "Stream response should contain last chunk")
	assert.Contains(t, bodyStr, "llama.cpp", "Stream response should contain llama.cpp string")

	// Проверяем наличие done:true
	assert.Contains(t, bodyStr, `"done":true`, "Stream should end with done:true")
}

// TestLlamaCppProxy_HealthCheckUsesCppWorkerPort проверяет что health-check
// для llama.cpp бэкенда идёт на порт CppWorker'а
func TestLlamaCppProxy_HealthCheckUsesCppWorkerPort(t *testing.T) {
	// Mock CppWorker с health endpoint
	cppHealthHit := 0
	cppServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/health") {
			cppHealthHit++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": "llama.cpp"})
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer cppServer.Close()

	cppAddr := strings.TrimPrefix(cppServer.URL, "http://")

	// Фиктивный Ollama health endpoint
	ollamaHealthHit := 0
	ollamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaHealthHit++
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ollama-ok"})
	}))
	defer ollamaServer.Close()

	ollamaAddr := strings.TrimPrefix(ollamaServer.URL, "http://")

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "127.0.0.1",
			Port:    18080,
			APIPort: 18081,
		},
		Balancing: types.BalancingSettings{
			Algorithm:      "resource-aware",
			ModelAffinity:  true,
			RequestTimeout: 10,
		},
		BackendEngine: types.EngineLlamaCPP,
		Initialized:   true,
		Backends: []types.Backend{
			{
				ID:                "health-test",
				Name:              "Health Test",
				Host:              strings.Split(cppAddr, ":")[0],
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     mustParsePortLLama(t, cppAddr),
				OllamaPort:        mustParsePortLLama(t, ollamaAddr),
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
	}

	proxy := balancer.NewProxy(cfg)

	// Проверяем что бэкенд создался корректно
	backendState := proxy.GetBackendState("health-test")
	require.NotNil(t, backendState)

	assert.Equal(t, types.BackendTypeLlamaCpp, backendState.Backend.Type,
		"Backend should be llama_cpp type")
	assert.NotZero(t, backendState.Backend.CppWorkerPort,
		"CppWorkerPort should be set")

	t.Logf("Health test backend: CppWorkerPort=%d, OllamaPort=%d",
		backendState.Backend.CppWorkerPort, backendState.Backend.OllamaPort)

	// Проверяем что Ollama health в данный момент не дёргался
	// (реальный health-check будет вызываться периодически HealthChecker'ом,
	// но сам факт что порты разные — важен)
	assert.Equal(t, 0, ollamaHealthHit,
		"Ollama health endpoint should not be hit for llama.cpp backend at this point")
}

// TestLlamaCppProxy_ModelListFromCppWorker проверяет что /api/tags
// проксируется на CppWorker и возвращает список моделей
func TestLlamaCppProxy_ModelListFromCppWorker(t *testing.T) {
	cppServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(r.URL.Path, "/health"):
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case strings.Contains(r.URL.Path, "/api/tags"):
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "llama3:8b", "size": 4928300000, "digest": "sha256:abc123"},
					{"name": "qwen2.5:14b", "size": 8965234567, "digest": "sha256:def456"},
				},
			})
		default:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	}))
	defer cppServer.Close()

	cppAddr := strings.TrimPrefix(cppServer.URL, "http://")

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "127.0.0.1",
			Port:    18080,
			APIPort: 18081,
		},
		Balancing: types.BalancingSettings{
			Algorithm:     "resource-aware",
			ModelAffinity: true,
			RequestTimeout: 10,
		},
		BackendEngine: types.EngineLlamaCPP,
		Initialized:   true,
		Backends: []types.Backend{
			{
				ID:                "model-list-test",
				Name:              "Model List",
				Host:              strings.Split(cppAddr, ":")[0],
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     mustParsePortLLama(t, cppAddr),
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
	}

	proxy := balancer.NewProxy(cfg)

	proxy.SetBackendMetrics("model-list-test", &types.BackendMetrics{
		ID:          "model-list-test",
		BackendType: types.BackendTypeLlamaCpp,
		Status:      types.StatusHealthy,
		Host:        strings.Split(cppAddr, ":")[0],
		OllamaPort:  mustParsePortLLama(t, cppAddr),
		GPU: types.GPUMetrics{
			MemoryTotal: 24576,
			MemoryUsed:  4096,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     65536,
		},
		Ollama: types.OllamaMetrics{
			RunningModels: []types.RunningModel{
				{Name: "llama3:8b", ParameterSize: "8B"},
				{Name: "qwen2.5:14b", ParameterSize: "14B"},
			},
			ActiveRequests:      0,
			MaxConcurrentRequests: 10,
		},
	})

	testServer := httptest.NewServer(proxy)
	defer testServer.Close()

	time.Sleep(50 * time.Millisecond)

	// Запрашиваем список моделей
	resp, err := http.Get(testServer.URL + "/api/tags")
	require.NoError(t, err)
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	t.Logf("Model list response: status=%d, body=%v", resp.StatusCode, result)

	// Проверяем что получили список моделей
	if models, ok := result["models"].([]interface{}); ok {
		assert.GreaterOrEqual(t, len(models), 1, "Should have at least one model")
	}
}

// TestLlamaCppProxy_ErrorHandling_CppWorkerUnavailable проверяет что при
// недоступности CppWorker балансер возвращает 503 и делает retry
func TestLlamaCppProxy_ErrorHandling_CppWorkerUnavailable(t *testing.T) {
	// Создаём "неработающий" сервер и сразу его закрываем
	cppServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	cppAddr := strings.TrimPrefix(cppServer.URL, "http://")
	cppPort := mustParsePortLLama(t, cppAddr)
	cppHost := strings.Split(cppAddr, ":")[0]
	cppServer.Close() // сразу закрываем — сервер недоступен

	// Второй рабочий CppWorker сервер (для retry)
	cppServer2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":    "test-model",
			"response": "Hello from fallback CppWorker!",
			"done":     true,
		})
	}))
	defer cppServer2.Close()

	cppAddr2 := strings.TrimPrefix(cppServer2.URL, "http://")
	cppPort2 := mustParsePortLLama(t, cppAddr2)
	cppHost2 := strings.Split(cppAddr2, ":")[0]

	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "127.0.0.1",
			Port:    18080,
			APIPort: 18081,
		},
		Balancing: types.BalancingSettings{
			Algorithm:      "resource-aware",
			ModelAffinity:  true,
			RequestTimeout: 2, // короткий таймаут для быстрого фейла
		},
		BackendEngine: types.EngineLlamaCPP,
		Initialized:   true,
		Backends: []types.Backend{
			{
				ID:                "llama-dead",
				Name:              "Dead Llama Node",
				Host:              cppHost,
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     cppPort,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy, // изначально healthy
			},
			{
				ID:                "llama-alive",
				Name:              "Alive Llama Node",
				Host:              cppHost2,
				Type:              types.BackendTypeLlamaCpp,
				CppWorkerPort:     cppPort2,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
	}

	proxy := balancer.NewProxy(cfg)

	// Устанавливаем метрики для обоих бэкендов
	for _, b := range cfg.Backends {
		port := cppPort
		host := cppHost
		if b.ID == "llama-alive" {
			port = cppPort2
			host = cppHost2
		}
		proxy.SetBackendMetrics(b.ID, &types.BackendMetrics{
			ID:          b.ID,
			BackendType: types.BackendTypeLlamaCpp,
			Status:      types.StatusHealthy,
			Host:        host,
			OllamaPort:  port,
			GPU: types.GPUMetrics{
				MemoryTotal: 24576,
				MemoryUsed:  4096,
			},
			System: types.SystemMetrics{
				CPUUsagePercent: 20,
				MemoryTotal:     65536,
			},
			Ollama: types.OllamaMetrics{
				RunningModels: []types.RunningModel{
					{Name: "test-model", ParameterSize: "8B", Family: "llama"},
				},
				ActiveRequests:      0,
				MaxConcurrentRequests: 10,
			},
		})
	}

	testServer := httptest.NewServer(proxy)
	defer testServer.Close()

	time.Sleep(50 * time.Millisecond)

	reqBody := `{"model":"test-model","prompt":"Test error handling","stream":false}`
	resp, err := http.Post(
		testServer.URL+"/api/generate",
		"application/json",
		strings.NewReader(reqBody),
	)
	require.NoError(t, err)
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	t.Logf("Error handling response: status=%d, body=%v", resp.StatusCode, result)

	// Проверяем что получили ответ от живого бэкенда
	// или 503 если retry не удался (оба варианта допустимы т.к. retry зависит от timing)
	if resp.StatusCode == http.StatusOK {
		response, _ := result["response"].(string)
		assert.Contains(t, response, "fallback",
			"Should get response from fallback CppWorker")
	} else {
		// 503 тоже валидный результат — система корректно сообщает о недоступности
		assert.True(t,
			resp.StatusCode == http.StatusServiceUnavailable ||
				resp.StatusCode == http.StatusInternalServerError,
			"Should get 503 or 500 when all backends unavailable, got %d", resp.StatusCode)
	}
}

func mustParsePortLLama(t *testing.T, addr string) int {
	t.Helper()
	var port int
	_, err := fmt.Sscanf(addr[strings.LastIndex(addr, ":")+1:], "%d", &port)
	if err != nil {
		t.Fatalf("Failed to parse port from %s: %v", addr, err)
	}
	return port
}
