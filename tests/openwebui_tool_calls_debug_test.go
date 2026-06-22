// openwebui_tool_calls_debug_test.go — диагностические тесты для воспроизведения проблемы
// "использован один источник, но самого ответа нет" при запросе с tools от OpenWebUI.
//
// Каждый сценарий trace'ит запрос/ответ по всей цепочке OpenWebUI → balancer → backend mock
// и явно логирует, на каком этапе теряется информация о tool_calls.
//
// Сценарии:
//   A. llama.cpp/cppworker SSE с delta.tool_calls и [DONE] — happy path через proxyRequestLlamaCpp.
//   B. Ollama-бэкенд NDJSON напрямую — путь через proxyRequest (без SSE-трансляции).
//   C. Mock как в expanded_mock.go для tools (NDJSON без [DONE]) — проверка регрессии.
//   D. cppworker behavior: text content + role chunk + tool_calls chunk + [DONE] — полная имитация.
package tests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// makeOpenWebUIBody формирует тело запроса в Ollama-формате, как его шлёт OpenWebUI.
func makeOpenWebUIBody() []byte {
	req := map[string]interface{}{
		"model": "qwen2.5:7b-instruct-q4_K_M",
		"messages": []map[string]string{
			{"role": "user", "content": "Search for the latest AI news"},
		},
		"stream": true,
		"tools": []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "search",
					"description": "Search the web for information",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"q": map[string]interface{}{
								"type":        "string",
								"description": "Search query",
							},
						},
						"required": []string{"q"},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(req)
	return body
}

// buildCppWorkerSSE формирует реалистичный SSE-поток как у llama.cpp/cppworker.
func buildCppWorkerSSE() []string {
	createdAt := time.Now().Unix()
	chatID := "chatcmpl-debug"

	chunk1, _ := json.Marshal(map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": createdAt,
		"model":   "qwen2.5:7b-instruct-q4_K_M",
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil},
		},
	})
	chunk2, _ := json.Marshal(map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": createdAt,
		"model":   "qwen2.5:7b-instruct-q4_K_M",
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]string{"content": "I'll search for AI news"}},
		},
	})
	toolCalls := []map[string]interface{}{
		{
			"index": 0, "id": "call_search_123", "type": "function",
			"function": map[string]interface{}{
				"name": "search", "arguments": `{"q":"AI news"}`,
			},
		},
	}
	chunk3, _ := json.Marshal(map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": createdAt,
		"model":   "qwen2.5:7b-instruct-q4_K_M",
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{"tool_calls": toolCalls},
				"finish_reason": "tool_calls",
			},
		},
	})
	return []string{
		fmt.Sprintf("data: %s\n\n", chunk1),
		fmt.Sprintf("data: %s\n\n", chunk2),
		fmt.Sprintf("data: %s\n\n", chunk3),
		"data: [DONE]\n\n",
	}
}

// analyseResponse — общий парсер ответа balancer'а (SSE или NDJSON).
// Возвращает флаги наличия ключевых элементов.
type diagResult struct {
	chunkCount       int
	hasContent       bool
	hasToolCalls     bool
	finishReason     string
	hasDONESentinel  bool
	hasDoneBool      bool
	lastChunkSnippet string
	rawResponse      string
}

func analyseResponse(t *testing.T, responseBody []byte) diagResult {
	t.Helper()
	r := diagResult{rawResponse: string(responseBody)}

	scanner := bufio.NewScanner(bytes.NewReader(responseBody))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		r.chunkCount++

		var data string
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		} else {
			data = line
		}

		if data == "[DONE]" {
			r.hasDONESentinel = true
			continue
		}

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Logf("  ⚠ невалидный JSON в чанке: %v | raw=%q", err, line)
			continue
		}

		if msg, ok := chunk["message"].(map[string]interface{}); ok {
			if content, ok := msg["content"].(string); ok && content != "" {
				r.hasContent = true
			}
			if tc, ok := msg["tool_calls"]; ok && tc != nil {
				if arr, ok := tc.([]interface{}); ok && len(arr) > 0 {
					r.hasToolCalls = true
					t.Logf("  ✓ NDJSON message.tool_calls найден (len=%d): %v", len(arr), tc)
				}
			}
		}
		if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if delta, ok := choice["delta"].(map[string]interface{}); ok {
					if content, ok := delta["content"].(string); ok && content != "" {
						r.hasContent = true
					}
					if tc, ok := delta["tool_calls"]; ok && tc != nil {
						r.hasToolCalls = true
					}
				}
				if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
					r.finishReason = fr
				}
			}
		}
		if done, ok := chunk["done"].(bool); ok && done {
			r.hasDoneBool = true
			if dr, ok := chunk["done_reason"].(string); ok {
				t.Logf("  done_reason=%q", dr)
			}
		}
		r.lastChunkSnippet = data
	}

	if err := scanner.Err(); err != nil {
		t.Logf("  ⚠ scanner error: %v", err)
	}
	return r
}

// setupLlamaCppProxy создаёт прокси с cppworker-бэкендом.
func setupLlamaCppProxy(t *testing.T, backendSrvURL string) (*httptest.Server, *balancer.Proxy) {
	t.Helper()
	host, port := parseHostPort(backendSrvURL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{
				ID:                "cppworker-test",
				Name:              "Test CppWorker",
				Host:              host,
				OllamaPort:        port,
				CppWorkerPort:     port, // Мок слушает на этом же порту
				AgentPort:         9090,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
				Type:              types.BackendTypeLlamaCpp,
				Engine:            types.EngineLlamaCPP,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     true,
			SessionStickiness: true,
			RequestTimeout:    30,
			QueueTimeout:      60,
			QueueMaxSize:      100,
			QueueWorkers:      4,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 95},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:   types.DiskLimits{MinFreeMB: 1024},
		},
	}
	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()
	proxy.UpdateMetrics("cppworker-test", &types.BackendMetrics{
		ID: "cppworker-test",
		GPU: types.GPUMetrics{UsagePercent: 30, MemoryTotal: 24576, MemoryUsed: 8000, MemoryFree: 16576},
		System: types.SystemMetrics{CPUUsagePercent: 20, MemoryTotal: 65536, MemoryUsed: 16000, MemoryFree: 49536, DiskFree: 20480},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels: []types.RunningModel{{Name: "qwen2.5:7b-instruct-q4_K_M", VRAMUsage: 6000}},
		},
	})
	proxyServer := httptest.NewServer(proxy)
	return proxyServer, proxy
}

// ============================================================================
// Сценарий A: cppworker с полным SSE-потоком (delta.tool_calls + [DONE])
// Имитирует happy path через proxyRequestLlamaCpp.
// Ожидание: writeStreamingSSEDone эмитит финальный NDJSON с tool_calls.
// ============================================================================

func TestDebugOpenWebUI_ToolCalls_ScenarioA_CppWorkerSSE(t *testing.T) {
	requestBody := makeOpenWebUIBody()
	t.Logf("═══ SCENARIO A: cppworker SSE с delta.tool_calls + [DONE] ═══")
	t.Logf("OpenWebUI request body: %s", string(requestBody))

	var backendRequestCount int64
	var capturedBody string

	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&backendRequestCount, 1)
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)

		t.Logf("\n[BALANCER → BACKEND] %s %s", r.Method, r.URL.Path)
		t.Logf("Backend body: %s", capturedBody)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err == nil {
			if tools, ok := req["tools"]; ok {
				if arr, ok := tools.([]interface{}); ok {
					t.Logf("✓ tools передан в backend (len=%d)", len(arr))
				}
			} else {
				t.Logf("✗ tools ОТСУТСТВУЕТ в backend request!")
			}
			if stream, ok := req["stream"].(bool); ok {
				t.Logf("  stream flag: %v", stream)
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		for _, line := range buildCppWorkerSSE() {
			fmt.Fprintf(w, "%s", line)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer backendSrv.Close()

	proxyServer, proxy := setupLlamaCppProxy(t, backendSrv.URL)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	t.Logf("\n[BALANCER → CLIENT] HTTP %d, Content-Type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	respBody, _ := io.ReadAll(resp.Body)
	t.Logf("Response RAW:\n%s", string(respBody))

	r := analyseResponse(t, respBody)
	t.Logf("\n═══ SCENARIO A RESULT ═══")
	t.Logf("Backend hit count: %d", atomic.LoadInt64(&backendRequestCount))
	t.Logf("Chunks received by client: %d", r.chunkCount)
	t.Logf("Has content: %v", r.hasContent)
	t.Logf("Has tool_calls: %v", r.hasToolCalls)
	t.Logf("Finish reason: %q", r.finishReason)
	t.Logf("Has [DONE] sentinel: %v", r.hasDONESentinel)
	t.Logf("Has done:true: %v", r.hasDoneBool)

	if !r.hasToolCalls {
		t.Errorf("❌ Scenario A FAIL: tool_calls не дошли до клиента")
	}
	if !r.hasDoneBool {
		t.Errorf("❌ Scenario A FAIL: финальный NDJSON чанк с done:true отсутствует")
	}
}

// ============================================================================
// Сценарий B: Ollama-бэкенд (NDJSON напрямую)
// Путь через proxyRequest без SSE-трансляции.
// Ожидание: NDJSON чанк с done:true + message.tool_calls доходит как есть.
// ============================================================================

func TestDebugOpenWebUI_ToolCalls_ScenarioB_OllamaDirect(t *testing.T) {
	requestBody := makeOpenWebUIBody()
	t.Logf("═══ SCENARIO B: Ollama NDJSON напрямую (без SSE-трансляции) ═══")

	var backendRequestCount int64
	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&backendRequestCount, 1)
		t.Logf("\n[BALANCER → OLLAMA BACKEND] %s %s", r.Method, r.URL.Path)

		// Ollama возвращает NDJSON: один чанк с done:true + tool_calls.
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		toolCalls := []map[string]interface{}{
			{
				"id":   "call_search_123",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "search",
					"arguments": `{"q":"AI news"}`,
				},
			},
		}
		chunk := map[string]interface{}{
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"message": map[string]interface{}{
				"role":       "assistant",
				"content":    "",
				"tool_calls": toolCalls,
			},
			"done":          true,
			"done_reason":   "stop",
			"total_duration": 1234567890,
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer backendSrv.Close()

	// Ollama-бэкенд (тип по умолчанию).
	host, port := parseHostPort(backendSrv.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{
				ID: "ollama-test", Name: "Test Ollama",
				Host: host, OllamaPort: port, AgentPort: 9090,
				Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy,
				// Без явного Type — нормализуется в ollama.
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, ModelAffinity: true,
			SessionStickiness: true, RequestTimeout: 30,
			QueueTimeout: 60, QueueMaxSize: 100, QueueWorkers: 4,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 95},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:   types.DiskLimits{MinFreeMB: 1024},
		},
	}
	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()
	proxy.UpdateMetrics("ollama-test", &types.BackendMetrics{
		ID:     "ollama-test",
		GPU:    types.GPUMetrics{UsagePercent: 30, MemoryTotal: 24576, MemoryUsed: 8000, MemoryFree: 16576},
		System: types.SystemMetrics{CPUUsagePercent: 20, MemoryTotal: 65536, MemoryUsed: 16000, MemoryFree: 49536, DiskFree: 20480},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels: []types.RunningModel{{Name: "qwen2.5:7b-instruct-q4_K_M", VRAMUsage: 6000}},
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	t.Logf("\n[BALANCER → CLIENT] HTTP %d, Content-Type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	respBody, _ := io.ReadAll(resp.Body)
	t.Logf("Response RAW:\n%s", string(respBody))

	r := analyseResponse(t, respBody)
	t.Logf("\n═══ SCENARIO B RESULT ═══")
	t.Logf("Backend hit count: %d", atomic.LoadInt64(&backendRequestCount))
	t.Logf("Chunks received by client: %d", r.chunkCount)
	t.Logf("Has content: %v", r.hasContent)
	t.Logf("Has tool_calls: %v", r.hasToolCalls)
	t.Logf("Has done:true: %v", r.hasDoneBool)

	if !r.hasToolCalls {
		t.Errorf("❌ Scenario B FAIL: tool_calls не дошли до клиента")
	}
	if !r.hasDoneBool {
		t.Errorf("❌ Scenario B FAIL: done:true отсутствует")
	}
}

// ============================================================================
// Сценарий C: Регрессия — как в expanded_mock.go: NDJSON без [DONE] и без done:true в стриме
// (мок имитирует "стрим с tools", но завершает одним чанком done:true без отдельного [DONE]).
// Путь — это proxyRequest для Ollama бэкенда (без SSE-трансляции).
// Ожидание: tool_calls доходят в первом же чанке.
// ============================================================================

func TestDebugOpenWebUI_ToolCalls_ScenarioC_ExpandedMockStyle(t *testing.T) {
	requestBody := makeOpenWebUIBody()
	t.Logf("═══ SCENARIO C: expanded_mock style — NDJSON с done:true без [DONE] ═══")

	var backendRequestCount int64
	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&backendRequestCount, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		toolCalls := []map[string]interface{}{
			{
				"id":   "call_mock_search",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "search",
					"arguments": `{"q":"test query"}`,
				},
			},
			{
				"id":   "call_mock_calculate",
				"type": "function",
				"function": map[string]interface{}{
					"name":      "calculate",
					"arguments": `{"expression":"2+2"}`,
				},
			},
		}
		// Стиль expanded_mock.go: один NDJSON чанк с done:true (без [DONE] sentinal).
		chunk := map[string]interface{}{
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"message": map[string]interface{}{
				"role":       "assistant",
				"content":    "",
				"tool_calls": toolCalls,
			},
			"done": true,
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer backendSrv.Close()

	host, port := parseHostPort(backendSrv.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 0, APIPort: 0},
		Backends: []types.Backend{
			{
				ID: "ollama-mock-style", Name: "Mock style",
				Host: host, OllamaPort: port, AgentPort: 9090,
				Weight: 1, MaxConcurrentReqs: 10, Status: types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, ModelAffinity: true,
			SessionStickiness: true, RequestTimeout: 30,
			QueueTimeout: 60, QueueMaxSize: 100, QueueWorkers: 4,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 95},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:   types.DiskLimits{MinFreeMB: 1024},
		},
	}
	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()
	proxy.UpdateMetrics("ollama-mock-style", &types.BackendMetrics{
		ID:     "ollama-mock-style",
		GPU:    types.GPUMetrics{UsagePercent: 30, MemoryTotal: 24576, MemoryUsed: 8000, MemoryFree: 16576},
		System: types.SystemMetrics{CPUUsagePercent: 20, MemoryTotal: 65536, MemoryUsed: 16000, MemoryFree: 49536, DiskFree: 20480},
		Ollama: types.OllamaMetrics{
			MaxModels: 5, MaxConcurrentRequests: 10, ActiveRequests: 0, OllamaAvailable: true,
			RunningModels: []types.RunningModel{{Name: "qwen2.5:7b-instruct-q4_K_M", VRAMUsage: 6000}},
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	t.Logf("\n[BALANCER → CLIENT] HTTP %d, Content-Type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	t.Logf("Response RAW:\n%s", string(respBody))

	r := analyseResponse(t, respBody)
	t.Logf("\n═══ SCENARIO C RESULT ═══")
	t.Logf("Backend hit count: %d", atomic.LoadInt64(&backendRequestCount))
	t.Logf("Chunks received by client: %d", r.chunkCount)
	t.Logf("Has content: %v", r.hasContent)
	t.Logf("Has tool_calls: %v", r.hasToolCalls)
	t.Logf("Has done:true: %v", r.hasDoneBool)

	if !r.hasToolCalls {
		t.Errorf("❌ Scenario C FAIL: tool_calls не дошли до клиента")
	}
	if !r.hasDoneBool {
		t.Errorf("❌ Scenario C FAIL: done:true отсутствует — клиент зависнет")
	}
}

// ============================================================================
// Сценарий D: Полная имитация cppworker — content + role + tool_calls в одном delta
// (как у некоторых моделей, которые стримят tool_calls через content).
// ============================================================================

func TestDebugOpenWebUI_ToolCalls_ScenarioD_CppWorkerMixedChunks(t *testing.T) {
	requestBody := makeOpenWebUIBody()
	t.Logf("═══ SCENARIO D: cppworker с content + tool_calls в разных delta чанках ═══")

	var backendRequestCount int64
	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&backendRequestCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		createdAt := time.Now().Unix()
		chatID := "chatcmpl-d"

		// Чанк 1: только role
		c1, _ := json.Marshal(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "created": createdAt,
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", c1)
		if flusher != nil {
			flusher.Flush()
		}

		// Чанк 2: только content (без tool_calls)
		c2, _ := json.Marshal(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "created": createdAt,
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"content": "I'll search."}},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", c2)
		if flusher != nil {
			flusher.Flush()
		}

		// Чанк 3: tool_calls с finish_reason=tool_calls
		tcs := []map[string]interface{}{
			{"index": 0, "id": "call_d1", "type": "function",
				"function": map[string]interface{}{"name": "search", "arguments": `{"q":"x"}`}},
		}
		c3, _ := json.Marshal(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "created": createdAt,
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         map[string]interface{}{"tool_calls": tcs},
					"finish_reason": "tool_calls",
				},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", c3)
		if flusher != nil {
			flusher.Flush()
		}

		fmt.Fprintf(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer backendSrv.Close()

	proxyServer, proxy := setupLlamaCppProxy(t, backendSrv.URL)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	t.Logf("\n[BALANCER → CLIENT] HTTP %d, Content-Type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	t.Logf("Response RAW:\n%s", string(respBody))

	r := analyseResponse(t, respBody)
	t.Logf("\n═══ SCENARIO D RESULT ═══")
	t.Logf("Backend hit count: %d", atomic.LoadInt64(&backendRequestCount))
	t.Logf("Chunks received by client: %d", r.chunkCount)
	t.Logf("Has content: %v", r.hasContent)
	t.Logf("Has tool_calls: %v", r.hasToolCalls)
	t.Logf("Finish reason: %q", r.finishReason)
	t.Logf("Has [DONE] sentinel: %v", r.hasDONESentinel)
	t.Logf("Has done:true: %v", r.hasDoneBool)

	if !r.hasToolCalls {
		t.Errorf("❌ Scenario D FAIL: tool_calls не дошли до клиента")
	}
	if !r.hasDoneBool {
		t.Errorf("❌ Scenario D FAIL: финальный NDJSON с done:true отсутствует")
	}
}

// ============================================================================
// Сценарий E: cppworker стримит tool_calls через content как Hermes/Qwen-style
// JSON tool call: `<tool_call>{"name": "search", "arguments": {...}}</tool_call>`.
// Это критичный кейс: многие модели (Qwen 2.5, Hermes-2) не используют нативный
// delta.tool_calls от OpenAI, а эмиттят JSON в content.
//
// proxyRequestLlamaCpp должен детектировать такой JSON и сформировать
// NDJSON чанк с message.tool_calls (через detectAndExtractToolCallsFromContent).
// ============================================================================

func TestDebugOpenWebUI_ToolCalls_ScenarioE_HermesStyleContentToolCall(t *testing.T) {
	requestBody := makeOpenWebUIBody()
	t.Logf("═══ SCENARIO E: cppworker Hermes-style tool call в content ═══")

	var backendRequestCount int64
	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&backendRequestCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		createdAt := time.Now().Unix()
		chatID := "chatcmpl-e"

		// Чанк 1: role
		c1, _ := json.Marshal(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "created": createdAt,
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", c1)
		if flusher != nil {
			flusher.Flush()
		}

		// Чанк 2: content с tool_call JSON (Hermes-style). Разбиваем на части,
		// как реально делает cppworker при стриминге.
		c2a, _ := json.Marshal(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "created": createdAt,
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"content": "<tool_call>"}, "finish_reason": nil},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", c2a)
		if flusher != nil {
			flusher.Flush()
		}

		c2b, _ := json.Marshal(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "created": createdAt,
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"content": `{"name": "search", `}, "finish_reason": nil},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", c2b)
		if flusher != nil {
			flusher.Flush()
		}

		c2c, _ := json.Marshal(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "created": createdAt,
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"content": `"arguments": {"q": "AI news"}}`}, "finish_reason": nil},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", c2c)
		if flusher != nil {
			flusher.Flush()
		}

		c2d, _ := json.Marshal(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "created": createdAt,
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"content": "}</tool_call>"}, "finish_reason": nil},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", c2d)
		if flusher != nil {
			flusher.Flush()
		}

		// Чанк 3: finish
		c3, _ := json.Marshal(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "created": createdAt,
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop"},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", c3)
		if flusher != nil {
			flusher.Flush()
		}

		fmt.Fprintf(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer backendSrv.Close()

	proxyServer, proxy := setupLlamaCppProxy(t, backendSrv.URL)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	t.Logf("\n[BALANCER → CLIENT] HTTP %d, Content-Type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	t.Logf("Response RAW:\n%s", string(respBody))

	r := analyseResponse(t, respBody)
	t.Logf("\n═══ SCENARIO E RESULT ═══")
	t.Logf("Backend hit count: %d", atomic.LoadInt64(&backendRequestCount))
	t.Logf("Chunks received by client: %d", r.chunkCount)
	t.Logf("Has content: %v", r.hasContent)
	t.Logf("Has tool_calls: %v", r.hasToolCalls)
	t.Logf("Has done:true: %v", r.hasDoneBool)

	if !r.hasToolCalls {
		t.Errorf("❌ Scenario E FAIL: tool_calls НЕ извлечены из content (Hermes-style)")
		t.Errorf("   Это и есть корень бага 'использован один источник, но ответа нет'.")
	}
	if !r.hasDoneBool {
		t.Errorf("❌ Scenario E FAIL: финальный NDJSON с done:true отсутствует")
	}
}

// ============================================================================
// Сценарий E2: cppworker шлёт ВЕСЬ `<tool_call>{...}</tool_call>` в одном content-чанке
// (это самый частый случай в реальном cppworker для Qwen2.5 / Hermes).
// Этот сценарий проверяет, что detectAndExtractToolCallsFromContent вызывается
// в translateSSEChatToOllama и корректно извлекает tool_calls из content.
// ============================================================================

func TestDebugOpenWebUI_ToolCalls_ScenarioE2_HermesSingleChunk(t *testing.T) {
	requestBody := makeOpenWebUIBody()
	t.Logf("═══ SCENARIO E2: cppworker шлёт <tool_call>{...}</tool_call> одним content-чанком ═══")

	var backendRequestCount int64
	backendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&backendRequestCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		createdAt := time.Now().Unix()
		chatID := "chatcmpl-e2"

		// Чанк 1: role
		c1, _ := json.Marshal(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "created": createdAt,
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", c1)
		if flusher != nil {
			flusher.Flush()
		}

		// Чанк 2: ВЕСЬ tool_call в одном content (Hermes-style single chunk)
		// С переносами строк как часто выдаёт реальная модель.
		toolCallContent := "<tool_call>\n{\"name\": \"search\", \"arguments\": {\"q\": \"AI news\"}}\n</tool_call>"
		c2, _ := json.Marshal(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "created": createdAt,
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]string{"content": toolCallContent}, "finish_reason": nil},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", c2)
		if flusher != nil {
			flusher.Flush()
		}

		// Чанк 3: finish
		c3, _ := json.Marshal(map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "created": createdAt,
			"model": "qwen2.5:7b-instruct-q4_K_M",
			"choices": []map[string]interface{}{
				{"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop"},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", c3)
		if flusher != nil {
			flusher.Flush()
		}

		fmt.Fprintf(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer backendSrv.Close()

	proxyServer, proxy := setupLlamaCppProxy(t, backendSrv.URL)
	defer proxyServer.Close()
	defer proxy.StopQueue()
	defer proxy.StopSessionManager()

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	t.Logf("\n[BALANCER → CLIENT] HTTP %d, Content-Type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	t.Logf("Response RAW:\n%s", string(respBody))

	r := analyseResponse(t, respBody)
	t.Logf("\n═══ SCENARIO E2 RESULT ═══")
	t.Logf("Backend hit count: %d", atomic.LoadInt64(&backendRequestCount))
	t.Logf("Chunks received by client: %d", r.chunkCount)
	t.Logf("Has content: %v", r.hasContent)
	t.Logf("Has tool_calls: %v", r.hasToolCalls)
	t.Logf("Has done:true: %v", r.hasDoneBool)

	if !r.hasToolCalls {
		t.Errorf("❌ Scenario E2 FAIL: tool_calls НЕ извлечены из Hermes-style content")
		t.Errorf("   Это и есть корень бага 'использован один источник, но ответа нет'.")
	}
	if !r.hasDoneBool {
		t.Errorf("❌ Scenario E2 FAIL: финальный NDJSON с done:true отсутствует")
	}
}
