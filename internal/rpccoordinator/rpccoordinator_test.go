package rpccoordinator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- Coordinator Tests ----

func TestNewCoordinator(t *testing.T) {
	c := NewCoordinator()
	if c == nil {
		t.Fatal("NewCoordinator() returned nil")
	}
	if c.enabled {
		t.Error("New coordinator should be disabled by default")
	}
	if c.protocol != "http" {
		t.Errorf("Expected default protocol 'http', got '%s'", c.protocol)
	}
}

func TestCoordinatorSetEnabled(t *testing.T) {
	c := NewCoordinator()
	if c.IsEnabled() {
		t.Error("Should be disabled initially")
	}
	c.SetEnabled(true)
	if !c.IsEnabled() {
		t.Error("Should be enabled after SetEnabled(true)")
	}
	c.SetEnabled(false)
	if c.IsEnabled() {
		t.Error("Should be disabled after SetEnabled(false)")
	}
}

func TestCoordinatorSetConfig(t *testing.T) {
	c := NewCoordinator()
	c.SetConfig("http://coordinator:8080", 9090, "grpc")

	status := c.GetStatus()
	if status["url"] != "http://coordinator:8080" {
		t.Errorf("Expected url 'http://coordinator:8080', got '%v'", status["url"])
	}
	if status["port"] != 9090 {
		t.Errorf("Expected port 9090, got '%v'", status["port"])
	}
	if status["protocol"] != "grpc" {
		t.Errorf("Expected protocol 'grpc', got '%v'", status["protocol"])
	}
}

func TestCoordinatorAddWorker(t *testing.T) {
	c := NewCoordinator()

	w1 := NewModelWorker("worker-1", "backend-1", "localhost", 11434, "1-20")
	w2 := NewModelWorker("worker-2", "backend-2", "localhost", 11435, "21-40")

	c.AddWorker(w1)
	c.AddWorker(w2)

	workers := c.GetWorkers()
	if len(workers) != 2 {
		t.Errorf("Expected 2 workers, got %d", len(workers))
	}
	if workers[0].WorkerID() != "worker-1" {
		t.Errorf("Expected worker-1, got '%s'", workers[0].WorkerID())
	}
	if workers[1].WorkerID() != "worker-2" {
		t.Errorf("Expected worker-2, got '%s'", workers[1].WorkerID())
	}
}

func TestCoordinatorInferDisabled(t *testing.T) {
	c := NewCoordinator()
	c.SetEnabled(false)

	_, err := c.Infer("test-model", []byte("test prompt"), nil)
	if err == nil {
		t.Fatal("Expected error when coordinator is disabled")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("Expected 'disabled' error, got: %v", err)
	}
}

func TestCoordinatorInferNoWorkers(t *testing.T) {
	c := NewCoordinator()
	c.SetEnabled(true)

	_, err := c.Infer("test-model", []byte("test prompt"), nil)
	if err == nil {
		t.Fatal("Expected error when no workers")
	}
	if !strings.Contains(err.Error(), "no workers") {
		t.Errorf("Expected 'no workers' error, got: %v", err)
	}
}

func TestCoordinatorInferWithWorker(t *testing.T) {
	// Create a test server that handles both:
	// 1. /infer — запрос от Coordinator к Worker
	// 2. /api/generate — запрос от Worker к Ollama
	ollamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/infer" {
			// Coordinator sends InferRequest to worker at /infer
			var req InferRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// Worker would forward to Ollama, but in test we just return a response
			resp := InferResponse{
				Model:    req.Model,
				Response: "Hello from Ollama!",
				Done:     true,
				WorkerID: "test-worker",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		if r.URL.Path == "/api/generate" {
			var req OllamaRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			resp := OllamaResponse{
				Model:    req.Model,
				Response: "Worker inference result for " + req.Model,
				Done:     true,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
	}))
	defer ollamaServer.Close()

	// Extract host and port from the test server URL
	hostPort := strings.TrimPrefix(ollamaServer.URL, "http://")
	parts := strings.Split(hostPort, ":")
	host := parts[0]
	port := 0
	if len(parts) > 1 {
		port = parseInt(parts[1])
	}

	c := NewCoordinator()
	c.SetEnabled(true)

	worker := NewModelWorker("test-worker", "test-backend", host, port, "1-40")
	c.AddWorker(worker)

	result, err := c.Infer("test-model", []byte("test prompt"), nil)
	if err != nil {
		t.Fatalf("Infer failed: %v", err)
	}

	var resp InferResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if resp.Model != "test-model" {
		t.Errorf("Expected model 'test-model', got '%s'", resp.Model)
	}
	if resp.Response != "Hello from Ollama!" {
		t.Errorf("Expected 'Hello from Ollama!', got '%s'", resp.Response)
	}
	if resp.WorkerID != "test-worker" {
		t.Errorf("Expected workerID 'test-worker', got '%s'", resp.WorkerID)
	}
}

func TestCoordinatorInferWorkerError(t *testing.T) {
	// Create a server that returns error
	ollamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not found", http.StatusNotFound)
	}))
	defer ollamaServer.Close()

	hostPort := strings.TrimPrefix(ollamaServer.URL, "http://")
	parts := strings.Split(hostPort, ":")
	host := parts[0]
	port := 0
	if len(parts) > 1 {
		port = parseInt(parts[1])
	}

	c := NewCoordinator()
	c.SetEnabled(true)
	worker := NewModelWorker("err-worker", "err-backend", host, port, "1-40")
	c.AddWorker(worker)

	_, err := c.Infer("test-model", []byte("test"), nil)
	if err == nil {
		t.Fatal("Expected error from worker")
	}
}

func TestCoordinatorGetStatus(t *testing.T) {
	c := NewCoordinator()
	c.SetEnabled(true)
	c.SetConfig("http://rpc:8080", 8080, "http")

	w1 := NewModelWorker("w1", "b1", "host1", 11434, "1-20")
	c.AddWorker(w1)

	status := c.GetStatus()
	if status["enabled"] != true {
		t.Errorf("Expected enabled=true, got %v", status["enabled"])
	}
	if status["url"] != "http://rpc:8080" {
		t.Errorf("Expected url 'http://rpc:8080', got %v", status["url"])
	}

	workers, ok := status["workers"].([]map[string]interface{})
	if !ok {
		// It might be a different type due to JSON serialization in tests
		// Just check it exists
		if status["workers"] == nil {
			t.Error("Expected workers in status")
		}
	} else {
		if len(workers) != 1 {
			t.Errorf("Expected 1 worker, got %d", len(workers))
		}
	}
}

// ---- ModelWorker Tests ----

func TestNewModelWorker(t *testing.T) {
	w := NewModelWorker("test-worker", "test-backend", "localhost", 11434, "1-40")
	if w == nil {
		t.Fatal("NewModelWorker() returned nil")
	}
	if w.WorkerID() != "test-worker" {
		t.Errorf("Expected workerID 'test-worker', got '%s'", w.WorkerID())
	}
	if w.BackendID() != "test-backend" {
		t.Errorf("Expected backendID 'test-backend', got '%s'", w.BackendID())
	}
	if w.Host() != "localhost" {
		t.Errorf("Expected host 'localhost', got '%s'", w.Host())
	}
	if w.Port() != 11434 {
		t.Errorf("Expected port 11434, got %d", w.Port())
	}
	if w.SliceLayers() != "1-40" {
		t.Errorf("Expected sliceLayers '1-40', got '%s'", w.SliceLayers())
	}
}

func TestModelWorkerInfer(t *testing.T) {
	// Create test Ollama server
	ollamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req OllamaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		resp := OllamaResponse{
			Model:    req.Model,
			Response: "Worker inference result for " + req.Model,
			Done:     true,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ollamaServer.Close()

	hostPort := strings.TrimPrefix(ollamaServer.URL, "http://")
	parts := strings.Split(hostPort, ":")
	host := parts[0]
	port := 0
	if len(parts) > 1 {
		port = parseInt(parts[1])
	}

	worker := NewModelWorker("test-worker", "test-backend", host, port, "1-40")

	input := InferRequest{
		Model:  "test-model",
		Prompt: "test prompt",
	}
	inputJSON, _ := json.Marshal(input)

	result, err := worker.Infer(inputJSON)
	if err != nil {
		t.Fatalf("Worker Infer failed: %v", err)
	}

	var resp InferResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("Failed to parse worker response: %v", err)
	}

	if resp.Model != "test-model" {
		t.Errorf("Expected model 'test-model', got '%s'", resp.Model)
	}
	if resp.Response != "Worker inference result for test-model" {
		t.Errorf("Unexpected response: '%s'", resp.Response)
	}
	if resp.WorkerID != "test-worker" {
		t.Errorf("Expected workerID 'test-worker', got '%s'", resp.WorkerID)
	}
}

func TestModelWorkerInferWithParameters(t *testing.T) {
	ollamaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req OllamaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Check that parameters are passed through
		if req.Options == nil {
			t.Error("Expected options to be passed")
		} else {
			if req.Options["temperature"] != "0.7" {
				t.Errorf("Expected temperature=0.7, got '%v'", req.Options["temperature"])
			}
		}
		if req.Stream != false {
			t.Error("Expected stream=false")
		}
		if req.SliceInfo != "1-20" {
			t.Errorf("Expected sliceInfo '1-20', got '%s'", req.SliceInfo)
		}

		resp := OllamaResponse{
			Model:    req.Model,
			Response: "Parameterized result",
			Done:     true,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer ollamaServer.Close()

	hostPort := strings.TrimPrefix(ollamaServer.URL, "http://")
	parts := strings.Split(hostPort, ":")
	host := parts[0]
	port := 0
	if len(parts) > 1 {
		port = parseInt(parts[1])
	}

	worker := NewModelWorker("param-worker", "param-backend", host, port, "1-20")

	input := InferRequest{
		Model:  "param-model",
		Prompt: "test",
		Parameters: map[string]string{
			"temperature": "0.7",
			"top_p":       "0.9",
		},
	}
	inputJSON, _ := json.Marshal(input)

	result, err := worker.Infer(inputJSON)
	if err != nil {
		t.Fatalf("Worker Infer with params failed: %v", err)
	}

	var resp InferResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}
	if resp.Response != "Parameterized result" {
		t.Errorf("Unexpected response: '%s'", resp.Response)
	}
}

func TestModelWorkerInferInvalidInput(t *testing.T) {
	worker := NewModelWorker("test", "test", "localhost", 11434, "1-40")

	// Send invalid JSON
	_, err := worker.Infer([]byte("not valid json"))
	if err == nil {
		t.Fatal("Expected error for invalid input")
	}
}

func TestModelWorkerGetStatus(t *testing.T) {
	w := NewModelWorker("status-worker", "status-backend", "192.168.1.1", 11434, "1-20")
	status := w.GetStatus()

	if status["workerID"] != "status-worker" {
		t.Errorf("Expected workerID 'status-worker', got '%v'", status["workerID"])
	}
	if status["backendID"] != "status-backend" {
		t.Errorf("Expected backendID 'status-backend', got '%v'", status["backendID"])
	}
	if status["host"] != "192.168.1.1" {
		t.Errorf("Expected host '192.168.1.1', got '%v'", status["host"])
	}
	if status["port"] != 11434 {
		t.Errorf("Expected port 11434, got '%v'", status["port"])
	}
	if status["sliceLayers"] != "1-20" {
		t.Errorf("Expected sliceLayers '1-20', got '%v'", status["sliceLayers"])
	}
}

func TestCoordinatorGetStatusMap(t *testing.T) {
	c := NewCoordinator()
	c.SetEnabled(true)
	c.SetConfig("http://test:8080", 8080, "http")

	status := c.GetStatusMap()
	if status["enabled"] != true {
		t.Errorf("Expected enabled=true, got %v", status["enabled"])
	}
}

func TestCoordinatorConcurrentAccess(t *testing.T) {
	c := NewCoordinator()

	// Concurrent writes
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(idx int) {
			c.SetEnabled(idx%2 == 0)
			c.AddWorker(NewModelWorker(
				"w-"+string(rune('0'+idx)),
				"b-"+string(rune('0'+idx)),
				"host", 11434+idx,
				"1-40",
			))
			done <- true
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	// Concurrent reads
	for i := 0; i < 10; i++ {
		go func() {
			c.IsEnabled()
			c.GetWorkers()
			c.GetStatus()
			done <- true
		}()
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	// Should have at least some workers
	workers := c.GetWorkers()
	if len(workers) == 0 {
		t.Error("Expected at least some workers after concurrent adds")
	}
}

// Helper
func parseInt(s string) int {
	var n int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

// TestCoordinatorStartStop tests that Start/Stop don't error
func TestCoordinatorStartStop(t *testing.T) {
	c := NewCoordinator()
	if err := c.Start(); err != nil {
		t.Errorf("Start() failed: %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Errorf("Stop() failed: %v", err)
	}
}

// TestModelWorkerStartStop tests that Start/Stop don't error
func TestModelWorkerStartStop(t *testing.T) {
	w := NewModelWorker("test", "test", "localhost", 11434, "1-40")
	if err := w.Start(); err != nil {
		t.Errorf("Start() failed: %v", err)
	}
	if err := w.Stop(); err != nil {
		t.Errorf("Stop() failed: %v", err)
	}
}
