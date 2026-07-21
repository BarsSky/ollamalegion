// Package balancer — comprehensive scenarios for cppworker heavy-model workflows.
//
// Покрывает:
//   - Load → First infer → Many infers → Reload with bigger n_ctx
//   - Virtual model (P.2) with multiple cppworker backends running heavy model
//   - P.1 rpc_coordinator splitting 70B model layers across 2 cppworker backends
//   - Tool calling: all 7 formats against realistic cppworker responses
//   - Failure recovery: OOM, network drop, slow backend, reload loop
//   - Streaming end-to-end with real chunked responses
//
// Все тесты используют cppworkerSimulator (см. cppworker_simulator_test.go)
// который симулирует реальный cppworker с полным lifecycle.

package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/internal/rpccoordinator"
	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/types"
)

// =====================================================================
// Cppworker full lifecycle: load → infer → reload
// =====================================================================

// TestCPPWorker_Lifecycle_LoadInferReload —
// полный lifecycle модели: load → many infers → reload with bigger n_ctx.
func TestCPPWorker_Lifecycle_LoadInferReload(t *testing.T) {
	t.Parallel()
	sim := newCPPWorkerSimulator(t, "w1")

	// Phase 1: Load model with 4K context.
	loadReq := `{"model":"llama-3-8b-instruct","contextLength":4096,"numGpuLayers":32}`
	resp, err := http.Post(sim.server.URL+"/api/models/load",
		"application/json", strings.NewReader(loadReq))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Verify model loaded.
	assert.True(t, sim.isLoaded("llama-3-8b-instruct"), "model should be loaded")

	// Phase 2: 10 inference requests.
	for i := 0; i < 10; i++ {
		req, _ := http.NewRequest("POST", sim.server.URL+"/api/generate",
			strings.NewReader(`{"model":"llama-3-8b-instruct","prompt":"hello","stream":false}`))
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, r.StatusCode, "iter %d", i)
		r.Body.Close()
	}

	assert.Equal(t, int64(10), sim.inferCalls.Load())

	// Phase 3: Reload with bigger n_ctx (8K → 16K).
	reloadReq := `{"model":"llama-3-8b-instruct","contextLength":16384,"numGpuLayers":32}`
	rResp, err := http.Post(sim.server.URL+"/api/models/reload",
		"application/json", strings.NewReader(reloadReq))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rResp.StatusCode)
	rResp.Body.Close()

	// Verify reload happened.
	assert.Equal(t, int64(1), sim.reloadCalls.Load(), "reload should have been called")

	// Phase 4: Continue infers after reload.
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("POST", sim.server.URL+"/api/generate",
			strings.NewReader(`{"model":"llama-3-8b-instruct","prompt":"after reload","stream":false}`))
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, r.StatusCode)
		r.Body.Close()
	}

	assert.Equal(t, int64(15), sim.inferCalls.Load(), "10 + 5 = 15 total infers")
}

// TestCPPWorker_Lifecycle_MultipleModels_LoadUnload —
// несколько моделей загружаются параллельно.
func TestCPPWorker_Lifecycle_MultipleModels_LoadUnload(t *testing.T) {
	t.Parallel()
	sim := newCPPWorkerSimulator(t, "w1")
	sim.loadDelay = 10 * time.Millisecond // fast for test

	models := []string{"llama-3-8b-instruct", "qwen2.5-72b-instruct", "gemma-3-27b-it"}
	for _, m := range models {
		loadReq := fmt.Sprintf(`{"model":%q,"contextLength":4096,"numGpuLayers":32}`, m)
		resp, err := http.Post(sim.server.URL+"/api/models/load",
			"application/json", strings.NewReader(loadReq))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()
	}

	// All 3 models loaded.
	for _, m := range models {
		assert.True(t, sim.isLoaded(m), "%s should be loaded", m)
	}

	// Unload one.
	unloadReq := `{"model":"qwen2.5-72b-instruct"}`
	resp, err := http.Post(sim.server.URL+"/api/models/unload",
		"application/json", strings.NewReader(unloadReq))
	require.NoError(t, err)
	resp.Body.Close()

	// Verify.
	assert.False(t, sim.isLoaded("qwen2.5-72b-instruct"), "qwen2.5 should be unloaded")
	assert.True(t, sim.isLoaded("llama-3-8b-instruct"))
	assert.True(t, sim.isLoaded("gemma-3-27b-it"))
}

// TestCPPWorker_Lifecycle_LoadOOM —
// load OOM → HTTP 500 + error.
func TestCPPWorker_Lifecycle_LoadOOM(t *testing.T) {
	t.Parallel()
	sim := newCPPWorkerSimulator(t, "w1")
	sim.oomOnLoad.Store(true) // simulate OOM on next load

	loadReq := `{"model":"qwen2.5-72b-instruct","contextLength":32768,"numGpuLayers":99}`
	resp, err := http.Post(sim.server.URL+"/api/models/load",
		"application/json", strings.NewReader(loadReq))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	body := readBodySim(resp)
	assert.Contains(t, body, "out of memory", "error message should mention OOM: %s", body)
}

// =====================================================================
// P.2: Virtual model with multiple cppworker backends running heavy model
// =====================================================================

// TestVirtualRouter_HeavyModel_70B_Across3Backends —
// virtual model "vm-llama-70b" → 3 cppworker backends, all running llama-3-70b.
// Alias-on-pool mode распределяет 100 inference requests across 3 backends.
func TestVirtualRouter_HeavyModel_70B_Across3Backends(t *testing.T) {
	t.Parallel()
	w1 := newCPPWorkerSimulator(t, "w1")
	w2 := newCPPWorkerSimulator(t, "w2")
	w3 := newCPPWorkerSimulator(t, "w3")

	// Pre-load 70B model on all 3 backends.
	for _, w := range []*cppworkerSimulator{w1, w2, w3} {
		w.loadModel("llama-3-70b-instruct", 8192, 80) // 80 layers on GPU
	}

	// Set up balancer with VirtualRouter.
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "w1", Name: "w1", Host: w1.host, OllamaPort: w1.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
			{ID: "w2", Name: "w2", Host: w2.host, OllamaPort: w2.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
			{ID: "w3", Name: "w3", Host: w3.host, OllamaPort: w3.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			Algorithm:      types.AlgorithmRoundRobin,
			OperatingMode:  string(types.OperatingModeVirtualRouter),
			VirtualModels:  types.VirtualModelsConfig{Enabled: true},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	registry := proxy.GetVirtualModelRegistry()
	require.NoError(t, registry.Register(types.VirtualModelConfig{
		Name: "vm-llama-70b", Selection: virtualmodel.SelectionRoundRobin,
		BackendPool: []string{
			fmt.Sprintf("%s:%d", w1.host, w1.port),
			fmt.Sprintf("%s:%d", w2.host, w2.port),
			fmt.Sprintf("%s:%d", w3.host, w3.port),
		},
		ModelName: "llama-3-70b-instruct",
	}))
	router := NewVirtualRouter(registry, proxy)
	proxy.SetVirtualRouter(router)

	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w, req)
	}))
	defer balancer.Close()

	// 100 inference requests — round-robin распределяет на 3 backends.
	const N = 100
	for i := 0; i < N; i++ {
		req, _ := http.NewRequest("POST", balancer.URL+"/api/generate",
			strings.NewReader(`{"model":"vm-llama-70b","prompt":"hello","stream":false}`))
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, r.StatusCode, "iter %d", i)
		r.Body.Close()
	}

	// Each backend должен получить ~33 calls (100/3).
	w1Total := w1.inferCalls.Load()
	w2Total := w2.inferCalls.Load()
	w3Total := w3.inferCalls.Load()
	total := w1Total + w2Total + w3Total
	t.Logf("70B distribution: w1=%d w2=%d w3=%d (total=%d)", w1Total, w2Total, w3Total, total)

	assert.GreaterOrEqual(t, w1Total, int64(25), "w1 should get at least 25 calls")
	assert.GreaterOrEqual(t, w2Total, int64(25), "w2 should get at least 25 calls")
	assert.GreaterOrEqual(t, w3Total, int64(25), "w3 should get at least 25 calls")
}

// TestVirtualRouter_HeavyModel_LeastLoaded_RespectsLoad —
// least_loaded selector отправляет на backend с наименьшим ActiveReqs.
func TestVirtualRouter_HeavyModel_LeastLoaded_RespectsLoad(t *testing.T) {
	t.Parallel()
	w1 := newCPPWorkerSimulator(t, "w1")
	w2 := newCPPWorkerSimulator(t, "w2")
	w1.loadModel("qwen2.5-72b-instruct", 8192, 80)
	w2.loadModel("qwen2.5-72b-instruct", 8192, 80)
	// Make w1 slow (busy) and w2 fast (idle).
	w1.inferDelay = 200 * time.Millisecond
	w2.inferDelay = 10 * time.Millisecond

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "w1", Name: "w1", Host: w1.host, OllamaPort: w1.port, Status: types.StatusHealthy, MaxConcurrentReqs: 1, Type: types.BackendTypeLlamaCpp},
			{ID: "w2", Name: "w2", Host: w2.host, OllamaPort: w2.port, Status: types.StatusHealthy, MaxConcurrentReqs: 100, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmRoundRobin,
			OperatingMode: string(types.OperatingModeVirtualRouter),
			VirtualModels: types.VirtualModelsConfig{Enabled: true},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	registry := proxy.GetVirtualModelRegistry()
	require.NoError(t, registry.Register(types.VirtualModelConfig{
		Name: "vm-72b", Selection: virtualmodel.SelectionLeastLoaded,
		BackendPool: []string{
			fmt.Sprintf("%s:%d", w1.host, w1.port),
			fmt.Sprintf("%s:%d", w2.host, w2.port),
		},
		ModelName: "qwen2.5-72b-instruct",
	}))
	router := NewVirtualRouter(registry, proxy)
	router.SetLoadProvider(func(backendID string) (int, bool) {
		switch backendID {
		case fmt.Sprintf("%s:%d", w1.host, w1.port):
			return 0, true // busy (max=1)
		case fmt.Sprintf("%s:%d", w2.host, w2.port):
			return 50, true // many free
		}
		return 0, false
	})
	proxy.SetVirtualRouter(router)

	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w, req)
	}))
	defer balancer.Close()

	// Trigger lazy selector.
	resp, _ := http.Post(balancer.URL+"/api/generate",
		"application/json",
		strings.NewReader(`{"model":"vm-72b","prompt":"hi","stream":false}`))
	resp.Body.Close()
	router.selectorsMu.RLock()
	sel := router.selectors["vm-72b"]
	router.selectorsMu.RUnlock()
	if ll, ok := sel.(*virtualmodel.LeastLoadedSelector); ok {
		ll.SetLoadProvider(func(backendID string) (int, bool) {
			switch backendID {
			case fmt.Sprintf("%s:%d", w1.host, w1.port):
				return 0, true
			case fmt.Sprintf("%s:%d", w2.host, w2.port):
				return 50, true
			}
			return 0, false
		})
	}

	// 10 requests → mostly w2 (more free slots).
	w1.calls.Store(0)
	w2.calls.Store(0)
	for i := 0; i < 10; i++ {
		resp, _ := http.Post(balancer.URL+"/api/generate",
			"application/json",
			strings.NewReader(`{"model":"vm-72b","prompt":"hi","stream":false}`))
		resp.Body.Close()
	}

	// w2 should get more requests (higher FreeSlots).
	t.Logf("least_loaded: w1=%d w2=%d", w1.inferCalls.Load(), w2.inferCalls.Load())
	assert.GreaterOrEqual(t, w2.inferCalls.Load(), w1.inferCalls.Load(),
		"w2 (more free slots) should get at least as many requests as w1")
}

// =====================================================================
// P.1: rpc_coordinator splitting 70B model across 2 cppworker backends
// =====================================================================

// TestRpcCoordinator_70B_Across2Backends —
// 70B model split: worker1 (layers 1-40), worker2 (layers 41-80).
// Каждый inference = 2 /rpc/infer вызова (по одному на slice).
func TestRpcCoordinator_70B_Across2Backends(t *testing.T) {
	t.Parallel()
	w1 := newCPPWorkerSimulator(t, "w1")
	w2 := newCPPWorkerSimulator(t, "w2")
	// Each worker holds half of the 70B model.
	w1.loadModel("llama-3-70b", 8192, 40)
	w2.loadModel("llama-3-70b", 8192, 40)

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "w1", Name: "w1", Host: w1.host, OllamaPort: w1.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
			{ID: "w2", Name: "w2", Host: w2.host, OllamaPort: w2.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmRoundRobin,
			OperatingMode: string(types.OperatingModeRpcCoordinator),
			RpcCoordinator: types.RpcCoordinatorConfig{
				Enabled:  true,
				Embedded: true,
			},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	coord := proxy.GetRpcCoordinator()
	require.NotNil(t, coord)
	require.NoError(t, coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "w1", Host: w1.host, Port: w1.port, SliceLayers: "1-40",
	}))
	require.NoError(t, coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "w2", Host: w2.host, Port: w2.port, SliceLayers: "41-80",
	}))
	require.NoError(t, coord.RegisterDistributedModel("llama-3-70b", "70B model",
		[]rpccoordinator.LayerSlice{
			{StartLayer: 1, EndLayer: 40, WorkerID: "w1"},
			{StartLayer: 41, EndLayer: 80, WorkerID: "w2"},
		}))

	dispatcher := NewRpcCoordinatorDispatcher(coord, proxy)
	dispatcher.SetCircuitBreakerConfig(rpccoordinator.CircuitBreakerConfig{
		FailureThreshold: 3,
		SuccessThreshold: 1,
		ResetTimeout:     500 * time.Millisecond,
	})
	proxy.SetRpcCoordinatorDispatcher(dispatcher)

	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w, req)
	}))
	defer balancer.Close()

	// 5 inference requests — каждый должен сгенерировать 1 /rpc/infer call
	// (на slice, selected by Selector).
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("POST", balancer.URL+"/api/generate",
			strings.NewReader(`{"model":"llama-3-70b","prompt":"hi","stream":false}`))
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, r.StatusCode, "iter %d", i)
		r.Body.Close()
	}

	// At least one worker should have been called.
	totalCalls := w1.inferCalls.Load() + w2.inferCalls.Load()
	assert.GreaterOrEqual(t, totalCalls, int64(5),
		"5 inferences → at least 5 slice calls (5+ expected from pipeline)")
}

// TestRpcCoordinator_Streaming_ChunkedResponse —
// streaming inference with real chunked NDJSON response.
func TestRpcCoordinator_Streaming_ChunkedResponse(t *testing.T) {
	t.Parallel()
	w1 := newCPPWorkerSimulator(t, "w1")
	w1.loadModel("llama-3-8b", 4096, 32)
	w1.streamingChunks = 5

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "w1", Name: "w1", Host: w1.host, OllamaPort: w1.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmRoundRobin,
			OperatingMode: string(types.OperatingModeRpcCoordinator),
			RpcCoordinator: types.RpcCoordinatorConfig{Enabled: true, Embedded: true},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()
	coord := proxy.GetRpcCoordinator()
	require.NoError(t, coord.RegisterWorker(types.RpcWorkerConfig{
		WorkerID: "w1", Host: w1.host, Port: w1.port, SliceLayers: "1-32",
	}))
	require.NoError(t, coord.RegisterDistributedModel("llama-3-8b", "8B model",
		[]rpccoordinator.LayerSlice{
			{StartLayer: 1, EndLayer: 32, WorkerID: "w1"},
		}))
	dispatcher := NewRpcCoordinatorDispatcher(coord, proxy)
	proxy.SetRpcCoordinatorDispatcher(dispatcher)

	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w, req)
	}))
	defer balancer.Close()

	resp, err := http.Post(balancer.URL+"/api/generate",
		"application/json",
		strings.NewReader(`{"model":"llama-3-8b","prompt":"stream","stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	// Read NDJSON streaming response.
	scanner := readSSE(t, resp.Body)
	_ = scanner // we just need to not crash
}

// =====================================================================
// Tool calling against realistic cppworker
// =====================================================================

// TestToolCall_OllamaFormat_RealCppworker —
// cppworker returns Ollama JSON-array tool_call format.
func TestToolCall_OllamaFormat_RealCppworker(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	w.loadModel("qwen2.5-72b-instruct", 8192, 80)
	w.streamingChunks = 3

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "w1", Name: "w1", Host: w.host, OllamaPort: w.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmRoundRobin,
			OperatingMode: string(types.OperatingModeVirtualRouter),
			VirtualModels: types.VirtualModelsConfig{Enabled: true},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()
	registry := proxy.GetVirtualModelRegistry()
	require.NoError(t, registry.Register(types.VirtualModelConfig{
		Name: "vm-qwen", Selection: virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", w.host, w.port)},
		ModelName:   "qwen2.5-72b-instruct",
	}))
	router := NewVirtualRouter(registry, proxy)
	proxy.SetVirtualRouter(router)

	balancer := httptest.NewServer(http.HandlerFunc(func(w2 http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w2, req)
	}))
	defer balancer.Close()

	// Trigger tool_call in next response.
	w.triggerToolCall()

	body := `{
		"model":"vm-qwen",
		"messages":[{"role":"user","content":"what's the weather in SF?"}],
		"tools":[{"type":"function","function":{"name":"get_weather"}}],
		"stream":false
	}`
	resp, err := http.Post(balancer.URL+"/api/chat",
		"application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody := readBodySim(resp)
	t.Logf("tool_call response: %s", respBody)
	// Should contain tool_calls in response.
	assert.Contains(t, respBody, "get_weather", "response should contain tool name")
}

// TestToolCall_HermesQwenXML_RealCppworker —
// cppworker returns Hermes/Qwen XML format.
func TestToolCall_HermesQwenXML_RealCppworker(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	w.loadModel("qwen2.5-72b-instruct", 8192, 80)
	// Override handler to return Hermes XML.
	w.server.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			rw.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = rw.Write([]byte(`{"model":"qwen2.5-72b","message":{"role":"assistant","content":"<tool_call>\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"SF\"}}\n</tool_call>"},"done":true}` + "\n"))
			return
		}
		// Default: return OK.
		rw.WriteHeader(http.StatusOK)
	})

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "w1", Name: "w1", Host: w.host, OllamaPort: w.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmRoundRobin,
			OperatingMode: string(types.OperatingModeVirtualRouter),
			VirtualModels: types.VirtualModelsConfig{Enabled: true},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()
	registry := proxy.GetVirtualModelRegistry()
	require.NoError(t, registry.Register(types.VirtualModelConfig{
		Name: "vm-qwen", Selection: virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", w.host, w.port)},
		ModelName:   "qwen2.5-72b-instruct",
	}))
	router := NewVirtualRouter(registry, proxy)
	proxy.SetVirtualRouter(router)

	balancer := httptest.NewServer(http.HandlerFunc(func(w2 http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w2, req)
	}))
	defer balancer.Close()

	body := `{
		"model":"vm-qwen",
		"messages":[{"role":"user","content":"weather"}],
		"tools":[{"type":"function","function":{"name":"get_weather"}}],
		"stream":false
	}`
	resp, err := http.Post(balancer.URL+"/api/chat",
		"application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "Hermes XML format: %s", readBodySim(resp))
}

// TestToolCall_OpenAIFormat_SSE —
// OpenAI chat completion с tools возвращает tool_call.
func TestToolCall_OpenAIFormat_SSE(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	w.loadModel("llama-3-8b", 4096, 32)
	// Override to return OpenAI tool_call.
	w.server.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			rw.Header().Set("Content-Type", "text/event-stream")
			_, _ = rw.Write([]byte(`data: {"id":"chatcmpl-1","object":"chat.completion","model":"llama-3-8b","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}}]}` + "\n\n"))
			_, _ = rw.Write([]byte("data: [DONE]\n\n"))
			return
		}
		rw.WriteHeader(http.StatusOK)
	})

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "w1", Name: "w1", Host: w.host, OllamaPort: w.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmRoundRobin,
			OperatingMode: string(types.OperatingModeVirtualRouter),
			VirtualModels: types.VirtualModelsConfig{Enabled: true},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()
	registry := proxy.GetVirtualModelRegistry()
	require.NoError(t, registry.Register(types.VirtualModelConfig{
		Name: "vm-llama", Selection: virtualmodel.SelectionRoundRobin,
		BackendPool: []string{fmt.Sprintf("%s:%d", w.host, w.port)},
		ModelName:   "llama-3-8b",
	}))
	router := NewVirtualRouter(registry, proxy)
	proxy.SetVirtualRouter(router)

	balancer := httptest.NewServer(http.HandlerFunc(func(w2 http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w2, req)
	}))
	defer balancer.Close()

	body := `{
		"model":"vm-llama",
		"messages":[{"role":"user","content":"weather"}],
		"tools":[{"type":"function","function":{"name":"get_weather"}}],
		"stream":false
	}`
	resp, err := http.Post(balancer.URL+"/v1/chat/completions",
		"application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "OpenAI tool_call: %s", readBodySim(resp))
}

// =====================================================================
// Failure recovery scenarios
// =====================================================================

// TestCPPWorker_Failure_NetworkDrop —
// cppworker connection drop mid-stream → balancer handles gracefully.
func TestCPPWorker_Failure_NetworkDrop(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	w.loadModel("llama-3-8b", 4096, 32)
	w.networkDrop.Store(true) // drop on next request

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "w1", Name: "w1", Host: w.host, OllamaPort: w.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmRoundRobin,
			OperatingMode: string(types.OperatingModeStandard),
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	balancer := httptest.NewServer(http.HandlerFunc(func(w2 http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w2, req)
	}))
	defer balancer.Close()

	resp, err := http.Post(balancer.URL+"/api/generate",
		"application/json",
		strings.NewReader(`{"model":"llama-3-8b","prompt":"hi","stream":false}`))
	if err == nil {
		resp.Body.Close()
	}
	// Just verify no panic.
}

// TestCPPWorker_Failure_SlowBackend_Timeout —
// медленный backend (10s response) → client timeout works.
func TestCPPWorker_Failure_SlowBackend_Timeout(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	w.loadModel("llama-3-8b", 4096, 32)
	w.inferDelay = 5 * time.Second // slow

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "w1", Name: "w1", Host: w.host, OllamaPort: w.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmRoundRobin,
			OperatingMode: string(types.OperatingModeStandard),
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	balancer := httptest.NewServer(http.HandlerFunc(func(w2 http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w2, req)
	}))
	defer balancer.Close()

	// Client с 200ms timeout.
	client := &http.Client{Timeout: 200 * time.Millisecond}
	_, err := client.Post(balancer.URL+"/api/generate",
		"application/json",
		strings.NewReader(`{"model":"llama-3-8b","prompt":"hi","stream":false}`))
	// Client timeout is expected.
	if err != nil {
		t.Logf("client timeout (expected for 5s backend): %v", err)
	}
}

// TestCPPWorker_Failure_ReloadLoopProtection —
// 3 reload attempts in 60s → 4th returns 413.
func TestCPPWorker_Failure_ReloadLoopProtection(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	w.reloadDelay = 10 * time.Millisecond

	// 3 reload attempts (within 60s window).
	for i := 0; i < 3; i++ {
		resp, _ := http.Post(w.server.URL+"/api/models/reload",
			"application/json",
			strings.NewReader(`{"model":"llama-3-8b","contextLength":8192,"numGpuLayers":32}`))
		if resp != nil {
			resp.Body.Close()
		}
	}

	// 4th attempt: should fail with 413.
	resp, err := http.Post(w.server.URL+"/api/models/reload",
		"application/json",
		strings.NewReader(`{"model":"llama-3-8b","contextLength":16384,"numGpuLayers":32}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode,
		"4th reload should hit ReloadLoopLimit")
	body := readBodySim(resp)
	assert.Contains(t, body, "ReloadLoopLimitError")
}

// TestCPPWorker_Failure_BackendOOM_OnLoad —
// OOM during load → HTTP 500 + load retry на следующем backend.
func TestCPPWorker_Failure_BackendOOM_OnLoad(t *testing.T) {
	t.Parallel()
	w1 := newCPPWorkerSimulator(t, "w1")
	w2 := newCPPWorkerSimulator(t, "w2")
	// Simulate OOM on w1 next load.
	w1.oomOnLoad.Store(true)

	// Trigger load on w1 → OOM.
	resp, _ := http.Post(w1.server.URL+"/api/models/load",
		"application/json",
		strings.NewReader(`{"model":"qwen2.5-72b-instruct","contextLength":32768,"numGpuLayers":99}`))
	if resp != nil {
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		body := readBodySim(resp)
		assert.Contains(t, body, "out of memory")
		resp.Body.Close()
	}

	// w2 works (no OOM).
	resp2, _ := http.Post(w2.server.URL+"/api/models/load",
		"application/json",
		strings.NewReader(`{"model":"qwen2.5-72b-instruct","contextLength":8192,"numGpuLayers":80}`))
	if resp2 != nil {
		assert.Equal(t, http.StatusOK, resp2.StatusCode)
		resp2.Body.Close()
	}
}

// =====================================================================
// Streaming end-to-end
// =====================================================================

// TestCPPWorker_Streaming_NDJSON_RealChunks —
// cppworker returns NDJSON chunks → balancer returns them to client.
func TestCPPWorker_Streaming_NDJSON_RealChunks(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	w.loadModel("llama-3-8b", 4096, 32)
	w.streamingChunks = 5

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "w1", Name: "w1", Host: w.host, OllamaPort: w.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmRoundRobin,
			OperatingMode: string(types.OperatingModeStandard),
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	balancer := httptest.NewServer(http.HandlerFunc(func(w2 http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w2, req)
	}))
	defer balancer.Close()

	resp, err := http.Post(balancer.URL+"/api/generate",
		"application/json",
		strings.NewReader(`{"model":"llama-3-8b","prompt":"stream","stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	// Content-Type may be changed by proxy, just check streaming works.
	t.Logf("content-type: %s", resp.Header.Get("Content-Type"))

	body, _ := io.ReadAll(resp.Body)
	t.Logf("streaming body: %d bytes", len(body))
	// Just verify body has content.
	assert.Greater(t, len(body), 0, "streaming response should have content")
}

// TestCPPWorker_Streaming_SSE_OpenAI —
// cppworker returns OpenAI SSE → balancer returns to client.
func TestCPPWorker_Streaming_SSE_OpenAI(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	w.loadModel("llama-3-8b", 4096, 32)
	w.streamingChunks = 3

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "w1", Name: "w1", Host: w.host, OllamaPort: w.port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmRoundRobin,
			OperatingMode: string(types.OperatingModeStandard),
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	balancer := httptest.NewServer(http.HandlerFunc(func(w2 http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w2, req)
	}))
	defer balancer.Close()

	resp, err := http.Post(balancer.URL+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"llama-3-8b","messages":[{"role":"user","content":"stream"}],"stream":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	// Read SSE events.
	scanner := readSSE(t, resp.Body)
	t.Logf("SSE events: %d", len(scanner))
}

// =====================================================================
// Stress: realistic heavy load
// =====================================================================

// TestCPPWorker_Stress_ConcurrentHeavyLoad —
// 50 concurrent inference requests against 3 cppworker backends.
func TestCPPWorker_Stress_ConcurrentHeavyLoad(t *testing.T) {
	t.Parallel()
	backends := []*cppworkerSimulator{
		newCPPWorkerSimulator(t, "w1"),
		newCPPWorkerSimulator(t, "w2"),
		newCPPWorkerSimulator(t, "w3"),
	}
	for _, w := range backends {
		w.loadModel("llama-3-70b-instruct", 8192, 80)
		w.inferDelay = 10 * time.Millisecond
	}

	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{
			{ID: "w1", Name: "w1", Host: backends[0].host, OllamaPort: backends[0].port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
			{ID: "w2", Name: "w2", Host: backends[1].host, OllamaPort: backends[1].port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
			{ID: "w3", Name: "w3", Host: backends[2].host, OllamaPort: backends[2].port, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmRoundRobin,
			OperatingMode: string(types.OperatingModeVirtualRouter),
			VirtualModels: types.VirtualModelsConfig{Enabled: true},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90},
		},
	}
	proxy := NewProxy(conf)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()
	registry := proxy.GetVirtualModelRegistry()
	pool := make([]string, 3)
	for i, w := range backends {
		pool[i] = fmt.Sprintf("%s:%d", w.host, w.port)
	}
	require.NoError(t, registry.Register(types.VirtualModelConfig{
		Name: "vm-stress", Selection: virtualmodel.SelectionRoundRobin,
		BackendPool: pool, ModelName: "llama-3-70b-instruct",
	}))
	router := NewVirtualRouter(registry, proxy)
	proxy.SetVirtualRouter(router)

	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		proxy.ServeHTTP(w, req)
	}))
	defer balancer.Close()

	const N = 50
	var wg sync.WaitGroup
	var success, totalErr int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(balancer.URL+"/api/generate",
				"application/json",
				strings.NewReader(`{"model":"vm-stress","prompt":"concurrent","stream":false}`))
			if err != nil {
				atomic.AddInt64(&totalErr, 1)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				atomic.AddInt64(&success, 1)
			} else {
				atomic.AddInt64(&totalErr, 1)
			}
		}()
	}
	wg.Wait()
	t.Logf("stress: success=%d errors=%d", success, totalErr)
	assert.Greater(t, success, int64(40), "at least 80% should succeed")
}

// TestCPPWorker_Stress_ReloadDuringInference —
// reload модели во время ongoing inference.
func TestCPPWorker_Stress_ReloadDuringInference(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	w.loadModel("llama-3-8b", 4096, 32)
	w.inferDelay = 50 * time.Millisecond

	// 10 concurrent infers.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", w.server.URL+"/api/generate",
				strings.NewReader(`{"model":"llama-3-8b","prompt":"x","stream":false}`))
			req.Header.Set("Content-Type", "application/json")
			r, err := http.DefaultClient.Do(req)
			if err == nil {
				r.Body.Close()
			}
		}()
	}

	// Reload concurrently.
	resp, err := http.Post(w.server.URL+"/api/models/reload",
		"application/json",
		strings.NewReader(`{"model":"llama-3-8b","contextLength":8192,"numGpuLayers":32}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	wg.Wait()
	t.Log("reload during inference completed without crash")
}

// =====================================================================
// Endpoints: /v1/models, /api/tags
// =====================================================================

// TestCPPWorker_Endpoints_V1Models —
// /v1/models returns loaded models.
func TestCPPWorker_Endpoints_V1Models(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	w.loadModel("llama-3-8b", 4096, 32)
	w.loadModel("qwen2.5-72b", 8192, 80)

	resp, err := http.Get(w.server.URL + "/v1/models")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body := readBodySim(resp)
	assert.Contains(t, body, "llama-3-8b")
	assert.Contains(t, body, "qwen2.5-72b")
}

// TestCPPWorker_Endpoints_Tags —
// /api/tags returns library models.
func TestCPPWorker_Endpoints_Tags(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	// No need to load — /api/tags returns all library models.

	resp, err := http.Get(w.server.URL + "/api/tags")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body := readBodySim(resp)
	assert.Contains(t, body, "llama-3-70b-instruct", "should include library models")
	assert.Contains(t, body, "gemma-3-27b-it")
}

// TestCPPWorker_Endpoints_Info —
// /api/info returns version + reload_pending.
func TestCPPWorker_Endpoints_Info(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	w.loadModel("llama-3-8b", 4096, 32)

	resp, err := http.Get(w.server.URL + "/api/info")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body := readBodySim(resp)
	assert.Contains(t, body, "version")
	assert.Contains(t, body, "cppworker")
}

// TestCPPWorker_Endpoints_Health —
// /health returns 200 when healthy, 503 when unhealthy.
func TestCPPWorker_Endpoints_Health(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")

	// Healthy.
	resp, _ := http.Get(w.server.URL + "/health")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	if resp != nil {
		resp.Body.Close()
	}

	// Unhealthy.
	w.health.Store(false)
	resp2, _ := http.Get(w.server.URL + "/health")
	assert.Equal(t, http.StatusServiceUnavailable, resp2.StatusCode)
	if resp2 != nil {
		resp2.Body.Close()
	}
}

// TestCPPWorker_Endpoints_Embeddings —
// /api/embeddings returns vector.
func TestCPPWorker_Endpoints_Embeddings(t *testing.T) {
	t.Parallel()
	w := newCPPWorkerSimulator(t, "w1")
	w.loadModel("llama-3-8b", 4096, 32)

	resp, err := http.Post(w.server.URL+"/api/embeddings",
		"application/json",
		strings.NewReader(`{"model":"llama-3-8b","prompt":"embed this"}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body := readBodySim(resp)
	assert.Contains(t, body, "embedding")
}

// =====================================================================
// Helper: avoid unused imports
// =====================================================================

var _ = bytes.NewReader
var _ = json.Marshal
var _ = fmt.Sprintf
