package rpccoordinator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"ollama-loadbalancer/pkg/types"
)

// ========== ModelCoordinator Tests ==========

func TestNewModelCoordinator(t *testing.T) {
	cfg := types.RpcCoordinatorConfig{
		Enabled:  true,
		Timeout:  "30s",
		Protocol: "http",
	}
	coord := NewModelCoordinator(cfg)
	require.NotNil(t, coord)
	assert.True(t, coord.Enabled())
	assert.Equal(t, 0, len(coord.workers))
	assert.Equal(t, 0, len(coord.models))
}

func TestModelCoordinator_SetEnabled(t *testing.T) {
	cfg := types.RpcCoordinatorConfig{Enabled: true}
	coord := NewModelCoordinator(cfg)
	assert.True(t, coord.Enabled())

	coord.SetEnabled(false)
	assert.False(t, coord.Enabled())

	coord.SetEnabled(true)
	assert.True(t, coord.Enabled())
}

func TestRegisterWorker_Success(t *testing.T) {
	// Мок worker HTTP сервер
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rpc/health" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"status": "healthy"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true, Protocol: "http"})
	cfg := types.RpcWorkerConfig{
		WorkerID:    "worker-1",
		Host:        "127.0.0.1",
		Port:        18080,
		SliceLayers: "1-40",
	}

	err := coord.RegisterWorker(cfg)
	require.NoError(t, err)

	// Worker зарегистрирован даже если health check не удался (см. логику)
	w := coord.GetWorker("worker-1")
	assert.NotNil(t, w)
	assert.Equal(t, "worker-1", w.WorkerID)
}

func TestRegisterWorker_MissingID(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	err := coord.RegisterWorker(types.RpcWorkerConfig{Host: "127.0.0.1"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "workerID is required")
}

func TestRegisterWorker_MissingHost(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	err := coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w1"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "host is required")
}

func TestUnregisterWorker(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w1", Host: "127.0.0.1", Port: 18080})

	coord.UnregisterWorker("w1")
	assert.Nil(t, coord.GetWorker("w1"))
}

func TestListWorkers(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w1", Host: "h1", Port: 18080})
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w2", Host: "h2", Port: 18081})

	workers := coord.ListWorkers()
	assert.Len(t, workers, 2)

	ids := make(map[string]bool)
	for _, w := range workers {
		ids[w.WorkerID] = true
	}
	assert.True(t, ids["w1"])
	assert.True(t, ids["w2"])
}

// ========== Distributed Model Tests ==========

func TestRegisterDistributedModel_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rpc/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true, Protocol: "http"})
	h, p := parseHostPort(server.Listener.Addr().String())
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w1", Host: h, Port: p})
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w2", Host: h, Port: p + 1})

	err := coord.RegisterDistributedModel("llama-70b", "Distributed 70B", []LayerSlice{
		{StartLayer: 1, EndLayer: 40, WorkerID: "w1"},
		{StartLayer: 41, EndLayer: 80, WorkerID: "w2"},
	})
	require.NoError(t, err)

	dm := coord.GetDistributedModel("llama-70b")
	require.NotNil(t, dm)
	assert.Equal(t, "llama-70b", dm.Name)
	assert.Len(t, dm.SliceLayers, 2)
	assert.Len(t, dm.Workers, 2)
}

func TestRegisterDistributedModel_MissingName(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	err := coord.RegisterDistributedModel("", "", []LayerSlice{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "model name is required")
}

func TestRegisterDistributedModel_NoSlices(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	err := coord.RegisterDistributedModel("model", "", []LayerSlice{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "at least one slice is required")
}

func TestRegisterDistributedModel_WorkerNotFound(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	err := coord.RegisterDistributedModel("model", "", []LayerSlice{
		{StartLayer: 1, EndLayer: 40, WorkerID: "nonexistent"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "worker nonexistent not registered")
}

func TestHasDistributedModel(t *testing.T) {
	// Мок-сервер для health check
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rpc/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true, Protocol: "http"})
	h, p := parseHostPort(server.Listener.Addr().String())
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w1", Host: h, Port: p})
	coord.RegisterDistributedModel("m1", "", []LayerSlice{
		{StartLayer: 1, EndLayer: 10, WorkerID: "w1"},
	})

	assert.True(t, coord.HasDistributedModel("m1"))
	assert.False(t, coord.HasDistributedModel("m2"))
}

func TestListDistributedModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rpc/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true, Protocol: "http"})
	h, p := parseHostPort(server.Listener.Addr().String())
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w1", Host: h, Port: p})
	coord.RegisterDistributedModel("m1", "desc1", []LayerSlice{
		{StartLayer: 1, EndLayer: 10, WorkerID: "w1"},
	})

	models := coord.ListDistributedModels()
	require.Len(t, models, 1)
	assert.Equal(t, "m1", models[0]["name"])
	assert.Equal(t, "desc1", models[0]["description"])
	assert.Equal(t, 1, models[0]["slices"])
}

// ========== Infer Tests ==========

func TestInfer_Disabled(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: false})
	_, err := coord.Infer(context.Background(), InferRequest{ModelName: "m1", Prompt: "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disabled")
}

func TestInfer_ModelNotFound(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	_, err := coord.Infer(context.Background(), InferRequest{ModelName: "m1", Prompt: "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestInfer_SingleSlice(t *testing.T) {
	// Мок worker, который возвращает JSON-ответ
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req SliceInferRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Проверяем правильность среза
		if req.StartLayer != 1 || req.EndLayer != 10 {
			http.Error(w, "wrong layers", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "hello from slice",
			"done":     true,
		})
	}))
	defer server.Close()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true, Protocol: "http"})
	addr := server.Listener.Addr().String()
	host, port := parseHostPort(addr)
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w1", Host: host, Port: port})

	err := coord.RegisterDistributedModel("test-model", "", []LayerSlice{
		{StartLayer: 1, EndLayer: 10, WorkerID: "w1"},
	})
	require.NoError(t, err)

	resp, err := coord.Infer(context.Background(), InferRequest{
		ModelName: "test-model",
		Prompt:    "hello",
	})
	require.NoError(t, err)
	assert.NotNil(t, resp)
	// Output будет JSON-строка из ответа worker'а
	assert.Contains(t, resp.Output, "hello from slice")
	assert.Equal(t, 1, len(resp.SliceStats))
	assert.True(t, resp.SliceStats[0].Success)
}

func TestInfer_PipelineTwoSlices(t *testing.T) {
	var callOrder []string
	var mu sync.Mutex

	makeHandler := func(workerName string, marker string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/rpc/health" {
				w.WriteHeader(http.StatusOK)
				return
			}
			if r.URL.Path != "/rpc/infer" {
				w.WriteHeader(http.StatusNotFound)
				return
			}

			var req SliceInferRequest
			json.NewDecoder(r.Body).Decode(&req)

			mu.Lock()
			callOrder = append(callOrder, fmt.Sprintf("%s:%d-%d", workerName, req.StartLayer, req.EndLayer))
			mu.Unlock()

			inputStr := string(req.Input)
			output := inputStr + marker

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"response": output,
				"done":     true,
			})
		}
	}

	// Worker 1: первый срез (слои 1-2)
	worker1 := httptest.NewServer(makeHandler("w1", " [processed-by-w1]"))
	defer worker1.Close()

	// Worker 2: второй срез (слои 3-4)
	worker2 := httptest.NewServer(makeHandler("w2", " [processed-by-w2]"))
	defer worker2.Close()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true, Protocol: "http"})

	h1, p1 := parseHostPort(worker1.Listener.Addr().String())
	h2, p2 := parseHostPort(worker2.Listener.Addr().String())

	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w1", Host: h1, Port: p1})
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w2", Host: h2, Port: p2})

	err := coord.RegisterDistributedModel("pipeline-model", "", []LayerSlice{
		{StartLayer: 1, EndLayer: 2, WorkerID: "w1"},
		{StartLayer: 3, EndLayer: 4, WorkerID: "w2"},
	})
	require.NoError(t, err)

	resp, err := coord.Infer(context.Background(), InferRequest{
		ModelName: "pipeline-model",
		Prompt:    "initial-prompt",
	})
	require.NoError(t, err)
	assert.NotNil(t, resp)

	// Проверяем порядок вызовов
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, callOrder, 2)
	assert.Equal(t, "w1:1-2", callOrder[0])
	assert.Equal(t, "w2:3-4", callOrder[1])

	// Проверяем что pipeline передал результат
	assert.Contains(t, resp.Output, "initial-prompt")
}

func TestInfer_WorkerFailure(t *testing.T) {
	// Worker, который всегда возвращает 500
	badWorker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer badWorker.Close()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{
		Enabled:    true,
		Protocol:   "http",
		MaxRetries: 2,
	})
	h, p := parseHostPort(badWorker.Listener.Addr().String())
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w1", Host: h, Port: p})

	err := coord.RegisterDistributedModel("fail-model", "", []LayerSlice{
		{StartLayer: 1, EndLayer: 10, WorkerID: "w1"},
	})
	require.NoError(t, err)

	_, err = coord.Infer(context.Background(), InferRequest{
		ModelName: "fail-model",
		Prompt:    "hello",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed")
}

// ========== Job Management Tests ==========

func TestGetActiveJobs(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	assert.Equal(t, 0, coord.GetActiveJobs())

	// Мок задачи
	coord.mu.Lock()
	coord.activeJobs["job-1"] = &InferenceJob{JobID: "job-1", Status: JobRunning}
	coord.mu.Unlock()

	assert.Equal(t, 1, coord.GetActiveJobs())
}

func TestGetJobStatus(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})

	// Несуществующая задача
	status := coord.GetJobStatus("nonexistent")
	assert.Nil(t, status)

	// Существующая задача
	coord.mu.Lock()
	coord.activeJobs["job-1"] = &InferenceJob{
		JobID:     "job-1",
		ModelName: "m1",
		Status:    JobRunning,
		CreatedAt: time.Now(),
		SliceResults: map[string]*SliceResult{
			"w1": {WorkerID: "w1", Output: []byte("test")},
		},
		Errors: []error{},
	}
	coord.mu.Unlock()

	status = coord.GetJobStatus("job-1")
	require.NotNil(t, status)
	assert.Equal(t, "job-1", status["jobId"])
	assert.Equal(t, "m1", status["modelName"])
	assert.Equal(t, "running", status["status"])
	assert.Equal(t, 1, status["slices"])
	assert.Equal(t, 0, status["errors"])
}

func TestCancelJob(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	coord.mu.Lock()
	coord.activeJobs["job-1"] = &InferenceJob{
		JobID:    "job-1",
		Status:   JobRunning,
		ctx:      ctx,
		cancel:   cancel,
	}
	coord.mu.Unlock()

	// Отмена
	assert.True(t, coord.CancelJob("job-1"))
	assert.Equal(t, JobCancelled, coord.activeJobs["job-1"].Status)

	// Повторная отмена той же задачи
	assert.True(t, coord.CancelJob("job-1"))

	// Несуществующая задача
	assert.False(t, coord.CancelJob("nonexistent"))
}

func TestHealthCheck(t *testing.T) {
	// Healthy worker
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rpc/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer healthy.Close()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true, Protocol: "http"})
	h, p := parseHostPort(healthy.Listener.Addr().String())
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w1", Host: h, Port: p})

	result := coord.HealthCheck()
	require.Len(t, result, 1)
	assert.True(t, result["w1"])
}

func TestClose(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w1", Host: "127.0.0.1", Port: 18080})

	ctx, cancel := context.WithCancel(context.Background())
	coord.activeJobs["job-1"] = &InferenceJob{
		JobID:  "job-1",
		ctx:    ctx,
		cancel: cancel,
	}

	coord.Close()
	assert.False(t, coord.Enabled())
}

// ========== Concurrent Tests ==========

func TestConcurrentWorkerRegistration(t *testing.T) {
	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("worker-%d", idx)
			coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: id, Host: "127.0.0.1", Port: 18080 + idx})
		}(i)
	}
	wg.Wait()

	assert.Equal(t, 50, len(coord.workers))
}

func TestConcurrentInferAndCancel(t *testing.T) {
	// Worker, который обрабатывает запрос с небольшой задержкой
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rpc/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "ok",
			"done":     true,
		})
	}))
	defer worker.Close()

	coord := NewModelCoordinator(types.RpcCoordinatorConfig{Enabled: true, Protocol: "http"})
	h, p := parseHostPort(worker.Listener.Addr().String())
	coord.RegisterWorker(types.RpcWorkerConfig{WorkerID: "w1", Host: h, Port: p})
	coord.RegisterDistributedModel("concurrent-model", "", []LayerSlice{
		{StartLayer: 1, EndLayer: 10, WorkerID: "w1"},
	})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := coord.Infer(context.Background(), InferRequest{
				ModelName: "concurrent-model",
				Prompt:    fmt.Sprintf("prompt-%d", idx),
			})
			// Некоторые могут упасть, если worker перегружен
			_ = err
		}(i)
	}
	wg.Wait()

	// После завершения активных задач быть не должно
	assert.Equal(t, 0, coord.GetActiveJobs())
}

// ========== Helper ==========

func parseHostPort(addr string) (string, int) {
	// addr в формате "127.0.0.1:12345"
	var host string
	var port int
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			host = addr[:i]
			fmt.Sscanf(addr[i+1:], "%d", &port)
			break
		}
	}
	return host, port
}