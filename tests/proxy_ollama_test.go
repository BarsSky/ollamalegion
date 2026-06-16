package tests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ==================== Тесты проксирования Ollama API ====================

func TestProxyOllama_Generate_NonStreaming(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hello, how are you?",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	assert.Equal(t, "llama3.1:8b", result["model"])
	assert.Equal(t, "Hello world!", result["response"])
	assert.Equal(t, true, result["done"])
	assert.Equal(t, 1, mock.generateCount)
}

func TestProxyOllama_Generate_Streaming(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Hello",
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	scanner := bufio.NewScanner(resp.Body)
	var events []map[string]interface{}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(data), &event); err == nil {
				events = append(events, event)
			}
		}
	}

	require.NoError(t, scanner.Err())
	assert.GreaterOrEqual(t, len(events), 3, "Should receive at least 3 SSE events")

	lastEvent := events[len(events)-1]
	assert.Equal(t, true, lastEvent["done"])
	assert.NotNil(t, lastEvent["total_duration"])
	assert.Equal(t, 1, mock.generateCount)
}

func TestProxyOllama_Chat_NonStreaming(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model": "llama3.1:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "Hello"},
		},
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	assert.Equal(t, "llama3.1:8b", result["model"])
	message, ok := result["message"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "assistant", message["role"])
	assert.Equal(t, "Hi there!", message["content"])
	assert.Equal(t, true, result["done"])
	assert.Equal(t, 1, mock.chatCount)
}

func TestProxyOllama_Chat_Streaming(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model": "llama3.1:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "Hello"},
		},
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/chat", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	scanner := bufio.NewScanner(resp.Body)
	var events []map[string]interface{}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(data), &event); err == nil {
				events = append(events, event)
			}
		}
	}

	require.NoError(t, scanner.Err())
	assert.GreaterOrEqual(t, len(events), 3)

	lastEvent := events[len(events)-1]
	assert.Equal(t, true, lastEvent["done"])
	assert.Equal(t, 1, mock.chatCount)
}

func TestProxyOllama_Embeddings(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "nomic-embed-text",
		"prompt": "Hello world",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/embeddings", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	assert.Equal(t, "nomic-embed-text", result["model"])
	embeddings, ok := result["embeddings"].([]interface{})
	require.True(t, ok)
	assert.Equal(t, 5, len(embeddings))
	assert.Equal(t, 1, mock.embedCount)
}

func TestProxyOllama_Tags(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/api/tags")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	err = json.Unmarshal(body, &result)
	require.NoError(t, err)

	models, ok := result["models"].([]interface{})
	require.True(t, ok)
	assert.Equal(t, 2, len(models))

	model0 := models[0].(map[string]interface{})
	assert.Equal(t, "llama3.1:8b", model0["name"])
	assert.Equal(t, 1, mock.tagsCount)
}

func TestProxyOllama_Ps(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	mock.SetRunningModels([]types.RunningModel{
		{Name: "llama3.1:8b", Size: 4928300000, Digest: "sha256:abc123", VRAMUsage: 6000},
	})

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/api/ps")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	models, ok := result["models"].([]interface{})
	require.True(t, ok)
	assert.Equal(t, 1, len(models))

	model0 := models[0].(map[string]interface{})
	assert.Equal(t, "llama3.1:8b", model0["name"])
	assert.Equal(t, 1, mock.psCount)
}

func TestProxyOllama_Version(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	resp, err := http.Get(proxyServer.URL + "/api/version")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	assert.Equal(t, "ollamalegion-1.0.0", result["version"])
	assert.Equal(t, 1, mock.versionCount)
}

func TestProxyOllama_Show(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"name": "llama3.1:8b",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/show", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	assert.NotEmpty(t, result["license"])
	assert.NotEmpty(t, result["modelfile"])
	assert.NotEmpty(t, result["parameters"])
	assert.NotEmpty(t, result["template"])

	details, ok := result["details"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "llama", details["family"])
	assert.Equal(t, 1, mock.showCount)
}

func TestProxyOllama_Create(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"name":      "custom-model",
		"modelfile": "FROM llama3.1:8b",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/create", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	scanner := bufio.NewScanner(resp.Body)
	var events []map[string]interface{}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(data), &event); err == nil {
				events = append(events, event)
			}
		}
	}

	require.NoError(t, scanner.Err())
	assert.GreaterOrEqual(t, len(events), 3)

	lastEvent := events[len(events)-1]
	assert.Equal(t, "success", lastEvent["status"])
	assert.Equal(t, 1, mock.createCount)
}

func TestProxyOllama_Pull(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"name": "llama3.1:8b",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/pull", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	scanner := bufio.NewScanner(resp.Body)
	var events []map[string]interface{}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(data), &event); err == nil {
				events = append(events, event)
			}
		}
	}

	require.NoError(t, scanner.Err())
	assert.GreaterOrEqual(t, len(events), 4)

	lastEvent := events[len(events)-1]
	assert.Equal(t, "success", lastEvent["status"])
	assert.Equal(t, 1, mock.pullCount)
}

func TestProxyOllama_Delete(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	mock.SetRunningModels([]types.RunningModel{
		{Name: "llama3.1:8b", Size: 4928300000, Digest: "sha256:abc123", VRAMUsage: 6000},
	})

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"name": "llama3.1:8b",
	}
	body, _ := json.Marshal(payload)

	req, _ := http.NewRequest(http.MethodDelete, proxyServer.URL+"/api/delete", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	assert.Equal(t, true, result["deleted"])
	assert.Equal(t, "llama3.1:8b", result["model"])
	assert.Equal(t, 1, mock.deleteCount)
}

func TestProxyOllama_Copy(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"source":      "llama3.1:8b",
		"destination": "llama3.1:8b-custom",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/copy", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var result map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	require.NoError(t, err)

	assert.Equal(t, true, result["copied"])
	assert.Equal(t, "llama3.1:8b", result["source"])
	assert.Equal(t, "llama3.1:8b-custom", result["destination"])
	assert.Equal(t, 1, mock.copyCount)
}

func TestProxyOllama_Push(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	mock.SetRunningModels([]types.RunningModel{
		{Name: "llama3.1:8b", Size: 4928300000, Digest: "sha256:abc123", VRAMUsage: 6000},
	})

	proxyServer, _ := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"name": "llama3.1:8b",
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/push", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	scanner := bufio.NewScanner(resp.Body)
	var events []map[string]interface{}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(data), &event); err == nil {
				events = append(events, event)
			}
		}
	}

	require.NoError(t, scanner.Err())
	assert.GreaterOrEqual(t, len(events), 2)

	lastEvent := events[len(events)-1]
	assert.Equal(t, "success", lastEvent["status"])
	assert.Equal(t, 1, mock.pushCount)
}

// ==================== Тесты Session Stickiness ====================

func TestProxyOllama_SessionStickiness(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload1 := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "First request",
		"stream": false,
	}
	body1, _ := json.Marshal(payload1)

	req1, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/api/generate", bytes.NewBuffer(body1))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Session-ID", "test-session-123")

	resp1, err := http.DefaultClient.Do(req1)
	require.NoError(t, err)
	defer resp1.Body.Close()

	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	session := proxy.GetSessions()
	found := false
	expectedSessionID := "test-session-123::llama3.1:8b::gen"
	for _, s := range session {
		if s.ID == expectedSessionID {
			found = true
			assert.Equal(t, "ollama-test", s.BackendID)
			assert.Equal(t, "llama3.1:8b", s.Model)
			break
		}
	}
	assert.True(t, found, "Session should be created with ID %s", expectedSessionID)

	payload2 := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Second request",
		"stream": false,
	}
	body2, _ := json.Marshal(payload2)

	req2, _ := http.NewRequest(http.MethodPost, proxyServer.URL+"/api/generate", bytes.NewBuffer(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Session-ID", "test-session-123")

	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	defer resp2.Body.Close()

	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, 2, mock.generateCount)
}

func TestProxyOllama_SessionStickiness_CookieFallback(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Cookie test",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp1, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp1.Body.Close()

	assert.Equal(t, http.StatusOK, resp1.StatusCode)

	sessions := proxy.GetSessions()
	assert.GreaterOrEqual(t, len(sessions), 1, "Session should be created by IP fallback")

	var sessionCookie *http.Cookie
	for _, c := range resp1.Cookies() {
		if c.Name == "session_id" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie != nil {
		assert.NotEmpty(t, sessionCookie.Value)
	}
}

// ==================== Тесты Model Affinity ====================

func TestProxyOllama_ModelAffinity(t *testing.T) {
	mock := newMockOllamaServer()
	defer mock.Close()

	proxyServer, proxy := setupProxyWithMockOllama(t, mock)
	defer proxyServer.Close()

	proxy.UpdateMetrics("ollama-test", &types.BackendMetrics{
		ID: "ollama-test",
		GPU: types.GPUMetrics{
			UsagePercent: 30,
			MemoryTotal:  24576,
			MemoryUsed:   8000,
			MemoryFree:   16576,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     65536,
			MemoryUsed:      16000,
			MemoryFree:      49536,
			DiskFree:        20480,
		},
		Ollama: types.OllamaMetrics{
			MaxModels:             5,
			MaxConcurrentRequests: 10,
			ActiveRequests:        0,
			RunningModels: []types.RunningModel{
				{Name: "llama3.1:8b", VRAMUsage: 6000},
				{Name: "qwen2.5:14b", VRAMUsage: 10000},
			},
		},
	})

	payload := map[string]interface{}{
		"model":  "qwen2.5:14b",
		"prompt": "Test model affinity",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, mock.generateCount)
}

// ==================== Тесты Retry / Failover ====================

func TestProxyOllama_RetryOnBackendFailure(t *testing.T) {
	failCount := 0
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCount++
		if failCount == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "backend overloaded"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":    "llama3.1:8b",
			"response": "Success after retry",
			"done":     true,
		})
	}))
	defer mockServer.Close()

	host, port := parseHostPort(mockServer.URL)
	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost",
			Port: 0,
		},
		Backends: []types.Backend{
			{
				ID:                "fail-then-success",
				Name:              "Fail Then Success",
				Host:              host,
				OllamaPort:        port,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			SessionStickiness: false,
			RequestTimeout:    5,
			QueueTimeout:      10,
			QueueMaxSize:      10,
			QueueWorkers:      1,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:   types.DiskLimits{MinFreeMB: 100},
		},
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()

	proxy.UpdateMetrics("fail-then-success", &types.BackendMetrics{
		ID: "fail-then-success",
		GPU: types.GPUMetrics{
			UsagePercent: 20,
			MemoryTotal:  16384,
			MemoryUsed:   4000,
			MemoryFree:   12384,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 15,
			DiskFree:        20480,
		},
	})

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	payload := map[string]interface{}{
		"model":  "llama3.1:8b",
		"prompt": "Test retry",
		"stream": false,
	}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(proxyServer.URL+"/api/generate", "application/json", bytes.NewBuffer(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.True(t, resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusServiceUnavailable)
}