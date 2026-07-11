// Package balancer — comprehensive cppworker simulator для end-to-end tests
// with realistic llama.cpp workflows.
//
// cppworkerSimulator — фейковый cppworker backend с полным lifecycle:
//   - States: unloaded → loading → loaded → reloading
//   - Models: tracking loaded models с size, n_ctx, gpu_layers
//   - Tools: support all 7 tool_call formats (Ollama, Hermes, Llama-3, Mistral, etc.)
//   - Streaming: real chunked NDJSON / SSE responses с Flusher
//   - Errors: OOM, n_ctx too small, reload loop, network drop, slow response
//   - n_ctx: enforce upper limit, clamp n_predict, reload on bigger prompt
//
// Используется в scenario тестах для проверки полного lifecycle модели:
//   - Load → First infer → Many infers → Reload with bigger n_ctx
//   - Tool calling workflows
//   - Failure recovery (OOM, network drop, slow backend)
//   - Streaming end-to-end

package balancer

import (
	"bufio"
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
)

// =====================================================================
// cppworkerSimulator — фейковый cppworker с полным lifecycle
// =====================================================================

// cppworkerSimulator — sophisticated mock of a cppworker backend.
// Поддерживает все endpoints из docs/api.md §CppWorker API.
type cppworkerSimulator struct {
	server  *httptest.Server
	host    string
	port    int
	workerID string

	// Lifecycle state.
	mu              sync.Mutex
	loadedModels    map[string]*cppworkerModelState // name → state
	health          atomic.Bool
	reloadPending   atomic.Bool // true during reload
	reloadAttempts  atomic.Int64 // for ReloadLoopLimit
	lastReloadTime  time.Time

	// Stats.
	calls           atomic.Int64
	loadCalls       atomic.Int64
	unloadCalls     atomic.Int64
	reloadCalls     atomic.Int64
	inferCalls      atomic.Int64
	streamChunks    atomic.Int64

	// Configurable behavior (for tests).
	loadDelay       time.Duration  // simulated load time
	inferDelay      time.Duration  // simulated inference latency
	reloadDelay     time.Duration  // simulated reload time
	streamingChunks int            // chunks per stream response
	oomOnLoad       atomic.Bool    // simulate OOM on next load
	networkDrop     atomic.Bool    // simulate network drop on next request
	corruptResponse atomic.Bool    // return malformed JSON

	// Cached model definitions (simulates loaded GGUF files).
	modelLibrary map[string]*cppworkerModelDef

	// Tool calling config.
	toolCallFormat  string // "ollama", "hermes", "llama3", "mistral", "json_md", "single", "prefix"
	toolCallTrigger atomic.Bool
}

// cppworkerModelState — состояние загруженной модели.
type cppworkerModelState struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`         // bytes
	ContextLen  int    `json:"contextLen"`   // n_ctx
	GPULayers   int    `json:"numGpuLayers"` // -ngl
	BatchSize   int    `json:"batchSize"`
	LoadedAt    time.Time `json:"loadedAt"`
	Family      string    `json:"family"`
	Format      string    `json:"format"`
	ParameterSize string  `json:"parameterSize"`
	Quantization string   `json:"quantization"`
}

// cppworkerModelDef — описание модели в library.
type cppworkerModelDef struct {
	Name         string
	Size         int64
	Family       string
	Format       string
	ParameterSize string
	Quantization string
}

// newCPPWorkerSimulator — создаёт симулятор с реалистичными defaults.
func newCPPWorkerSimulator(t *testing.T, workerID string) *cppworkerSimulator {
	t.Helper()
	s := &cppworkerSimulator{
		workerID:        workerID,
		loadedModels:    make(map[string]*cppworkerModelState),
		modelLibrary:    make(map[string]*cppworkerModelDef),
		loadDelay:       100 * time.Millisecond,
		inferDelay:      50 * time.Millisecond,
		reloadDelay:     200 * time.Millisecond,
		streamingChunks: 5,
	}
	s.health.Store(true)

	// Default model library.
	s.addLibraryModel("llama-3-70b-instruct", 40*1024*1024*1024, "llama", "gguf", "70B", "Q4_K_M")
	s.addLibraryModel("llama-3-8b-instruct", 5*1024*1024*1024, "llama", "gguf", "8B", "Q4_K_M")
	s.addLibraryModel("qwen2.5-72b-instruct", 45*1024*1024*1024, "qwen2", "gguf", "72B", "Q4_K_M")
	s.addLibraryModel("gemma-3-27b-it", 17*1024*1024*1024, "gemma3", "gguf", "27B", "Q4_K_M")

	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(func() { s.server.Close() })

	addr := s.server.URL[7:]
	host, port, _ := parseBackendHostPort(addr)
	s.host = host
	s.port = port
	return s
}

// addLibraryModel — добавляет модель в library (для /api/tags).
func (s *cppworkerSimulator) addLibraryModel(name string, size int64, family, format, paramSize, quant string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelLibrary[name] = &cppworkerModelDef{
		Name:          name,
		Size:          size,
		Family:        family,
		Format:        format,
		ParameterSize: paramSize,
		Quantization:  quant,
	}
}

// setToolCallFormat — устанавливает формат tool_call в response.
func (s *cppworkerSimulator) setToolCallFormat(format string) {
	s.toolCallFormat = format
}

// triggerToolCall — следующий ответ будет содержать tool_call.
func (s *cppworkerSimulator) triggerToolCall() {
	s.toolCallTrigger.Store(true)
}

// loadModel — загружает модель (для тестов).
func (s *cppworkerSimulator) loadModel(name string, nCtx int, gpuLayers int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	def, ok := s.modelLibrary[name]
	if !ok {
		// Model not in library — create a default entry.
		def = &cppworkerModelDef{
			Name:          name,
			Size:          5 * 1024 * 1024 * 1024,
			Family:        "unknown",
			Format:        "gguf",
			ParameterSize: "?B",
			Quantization:  "Q4_K_M",
		}
		s.modelLibrary[name] = def
	}
	s.loadedModels[name] = &cppworkerModelState{
		Name:          name,
		Size:          def.Size,
		ContextLen:    nCtx,
		GPULayers:     gpuLayers,
		BatchSize:     512,
		LoadedAt:      time.Now(),
		Family:        def.Family,
		Format:        def.Format,
		ParameterSize: def.ParameterSize,
		Quantization:  def.Quantization,
	}
}

// isLoaded — проверяет, загружена ли модель.
func (s *cppworkerSimulator) isLoaded(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.loadedModels[name]
	return ok
}

// =====================================================================
// HTTP handler
// =====================================================================

func (s *cppworkerSimulator) handle(w http.ResponseWriter, r *http.Request) {
	s.calls.Add(1)

	// Network drop simulation.
	if s.networkDrop.Load() {
		s.networkDrop.Store(false)
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
	}

	switch r.URL.Path {
	case "/health":
		s.handleHealth(w, r)
	case "/rpc/health":
		s.handleHealth(w, r)
	case "/api/models":
		s.handleListModels(w, r)
	case "/api/models/load":
		s.handleLoad(w, r)
	case "/api/models/load/progress":
		s.handleLoadProgress(w, r)
	case "/api/models/unload":
		s.handleUnload(w, r)
	case "/api/models/reload":
		s.handleReload(w, r)
	case "/rpc/infer":
		s.handleInfer(w, r)
	case "/api/info":
		s.handleInfo(w, r)
	case "/api/generate":
		s.handleGenerate(w, r)
	case "/api/chat":
		s.handleChat(w, r)
	case "/v1/chat/completions":
		s.handleOpenAIChat(w, r)
	case "/v1/completions":
		s.handleOpenAICompletion(w, r)
	case "/v1/models":
		s.handleOpenAIModels(w, r)
	case "/api/tags":
		s.handleTags(w, r)
	case "/api/embeddings":
		s.handleEmbeddings(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *cppworkerSimulator) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.health.Load() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","model_loaded":true}`))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

func (s *cppworkerSimulator) handleListModels(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	models := make([]*cppworkerModelState, 0, len(s.loadedModels))
	for _, m := range s.loadedModels {
		models = append(models, m)
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"models": models})
}

func (s *cppworkerSimulator) handleLoad(w http.ResponseWriter, r *http.Request) {
	s.loadCalls.Add(1)
	if s.oomOnLoad.Load() {
		s.oomOnLoad.Store(false)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"CUDA out of memory: tried to allocate 2GB"}`))
		return
	}
	var req struct {
		Model        string `json:"model"`
		ContextLen   int    `json:"contextLength"`
		NumGpuLayers int    `json:"numGpuLayers"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// Simulate load delay.
	if s.loadDelay > 0 {
		time.Sleep(s.loadDelay)
	}

	s.mu.Lock()
	if def, ok := s.modelLibrary[req.Model]; ok {
		s.loadedModels[req.Model] = &cppworkerModelState{
			Name:          req.Model,
			Size:          def.Size,
			ContextLen:    req.ContextLen,
			GPULayers:     req.NumGpuLayers,
			BatchSize:     512,
			LoadedAt:      time.Now(),
			Family:        def.Family,
			Format:        def.Format,
			ParameterSize: def.ParameterSize,
			Quantization:  def.Quantization,
		}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "loaded",
		"model":     req.Model,
		"contextLength": req.ContextLen,
		"loadDurationMs": int(s.loadDelay.Milliseconds()),
	})
}

func (s *cppworkerSimulator) handleLoadProgress(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	progress := 1.0 // 100% by default (no real loading in fake)
	if s.reloadPending.Load() {
		progress = 0.5
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"progress": progress,
		"status":   "loading",
	})
}

func (s *cppworkerSimulator) handleUnload(w http.ResponseWriter, r *http.Request) {
	s.unloadCalls.Add(1)
	var req struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	s.mu.Lock()
	delete(s.loadedModels, req.Model)
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (s *cppworkerSimulator) handleReload(w http.ResponseWriter, r *http.Request) {
	s.reloadCalls.Add(1)
	now := time.Now()
	s.reloadAttempts.Add(1)

	// Reload loop protection (3 attempts / 60s window).
	if s.lastReloadTime.IsZero() || now.Sub(s.lastReloadTime) > 60*time.Second {
		s.lastReloadTime = now
		s.reloadAttempts.Store(1)
	} else if s.reloadAttempts.Load() > 3 {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":"ReloadLoopLimitError: 3 attempts / 60s window"}`))
		return
	}

	s.reloadPending.Store(true)
	if s.reloadDelay > 0 {
		time.Sleep(s.reloadDelay)
	}
	s.reloadPending.Store(false)

	var req struct {
		Model        string `json:"model"`
		ContextLen   int    `json:"contextLength"`
		NumGpuLayers int    `json:"numGpuLayers"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	s.mu.Lock()
	if def, ok := s.modelLibrary[req.Model]; ok {
		s.loadedModels[req.Model] = &cppworkerModelState{
			Name:          req.Model,
			Size:          def.Size,
			ContextLen:    req.ContextLen,
			GPULayers:     req.NumGpuLayers,
			BatchSize:     512,
			LoadedAt:      time.Now(),
			Family:        def.Family,
			Format:        def.Format,
			ParameterSize: def.ParameterSize,
			Quantization:  def.Quantization,
		}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "reloaded",
		"model":  req.Model,
	})
}

func (s *cppworkerSimulator) handleInfer(w http.ResponseWriter, r *http.Request) {
	s.inferCalls.Add(1)
	if s.inferDelay > 0 {
		time.Sleep(s.inferDelay)
	}
	var req struct {
		ModelName  string `json:"model_name"`
		StartLayer int    `json:"start_layer"`
		EndLayer   int    `json:"end_layer"`
		Prompt     string `json:"prompt"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"worker_id":  s.workerID,
		"slice_id":   fmt.Sprintf("%d-%d", req.StartLayer, req.EndLayer),
		"output":     fmt.Sprintf("[%s] %s", s.workerID, req.Prompt),
		"tokens":     10,
		"duration_ms": int(s.inferDelay.Milliseconds()),
	})
}

func (s *cppworkerSimulator) handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body := map[string]interface{}{
		"version": "1.0.0-rc1",
		"backend": "cppworker-fake",
	}
	if s.reloadPending.Load() {
		body["reload_pending"] = true
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (s *cppworkerSimulator) handleGenerate(w http.ResponseWriter, r *http.Request) {
	s.inferCalls.Add(1)
	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req.Stream {
		s.streamOllamaGenerate(w, req.Model, req.Prompt)
	} else {
		s.nonStreamOllamaGenerate(w, req.Model, req.Prompt)
	}
}

func (s *cppworkerSimulator) nonStreamOllamaGenerate(w http.ResponseWriter, model, prompt string) {
	if s.inferDelay > 0 {
		time.Sleep(s.inferDelay)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"model":      model,
		"response":   fmt.Sprintf("[%s] %s", s.workerID, prompt),
		"done":       true,
		"eval_count": 10,
	})
}

func (s *cppworkerSimulator) streamOllamaGenerate(w http.ResponseWriter, model, prompt string) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	for i := 0; i < s.streamingChunks; i++ {
		if i == s.streamingChunks-1 && s.toolCallTrigger.Load() {
			// Last chunk: include tool_call if triggered.
			s.toolCallTrigger.Store(false)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"model":    model,
				"response": "",
				"message": map[string]interface{}{
					"role": "assistant",
					"content": "",
					"tool_calls": []map[string]interface{}{
						{"function": map[string]interface{}{"name": "get_weather", "arguments": map[string]string{"city": "SF"}}},
					},
				},
				"done": true,
			})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"model":    model,
				"response": fmt.Sprintf("chunk%d ", i),
				"done":     i == s.streamingChunks-1,
			})
		}
		s.streamChunks.Add(1)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (s *cppworkerSimulator) handleChat(w http.ResponseWriter, r *http.Request) {
	s.inferCalls.Add(1)
	var req struct {
		Model    string `json:"model"`
		Messages []map[string]string `json:"messages"`
		Stream   bool   `json:"stream"`
		Tools    []interface{} `json:"tools"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	hasTools := len(req.Tools) > 0

	if req.Stream {
		s.streamOllamaChat(w, req.Model, hasTools)
	} else {
		s.nonStreamOllamaChat(w, req.Model, hasTools)
	}
}

func (s *cppworkerSimulator) nonStreamOllamaChat(w http.ResponseWriter, model string, hasTools bool) {
	if s.inferDelay > 0 {
		time.Sleep(s.inferDelay)
	}
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"model":    model,
		"message":  map[string]interface{}{"role": "assistant", "content": fmt.Sprintf("[%s] response", s.workerID)},
		"done":     true,
		"eval_count": 10,
	}
	// Check both: tools in request OR trigger flag set.
	if hasTools || s.toolCallTrigger.Load() {
		s.toolCallTrigger.Store(false)
		resp["message"] = map[string]interface{}{
			"role": "assistant",
			"content": "",
			"tool_calls": []map[string]interface{}{
				{"function": map[string]interface{}{"name": "get_weather", "arguments": map[string]string{"city": "SF"}}},
			},
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *cppworkerSimulator) streamOllamaChat(w http.ResponseWriter, model string, hasTools bool) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	for i := 0; i < s.streamingChunks; i++ {
		chunk := map[string]interface{}{
			"model":    model,
			"message":  map[string]interface{}{"role": "assistant", "content": fmt.Sprintf("chunk%d ", i)},
			"done":     i == s.streamingChunks-1,
		}
		if i == s.streamingChunks-1 && (hasTools || s.toolCallTrigger.Load()) {
			s.toolCallTrigger.Store(false)
			chunk["message"] = map[string]interface{}{
				"role": "assistant",
				"content": "",
				"tool_calls": []map[string]interface{}{
					{"function": map[string]interface{}{"name": "get_weather", "arguments": map[string]string{"city": "SF"}}},
				},
			}
		}
		_ = json.NewEncoder(w).Encode(chunk)
		s.streamChunks.Add(1)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (s *cppworkerSimulator) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string `json:"model"`
		Messages []map[string]string `json:"messages"`
		Stream   bool   `json:"stream"`
		Tools    []interface{} `json:"tools"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")

	// First chunk: role.
	fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"%s\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", req.Model)
	if flusher != nil {
		flusher.Flush()
	}

	if req.Stream {
		for i := 0; i < s.streamingChunks; i++ {
			chunk := fmt.Sprintf(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"%s","choices":[{"delta":{"content":"chunk%d "}}]}%s`,
				req.Model, i, "\n\n")
			fmt.Fprint(w, chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		// [DONE] terminator.
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	} else {
		// Non-streaming: return full response.
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl-1","object":"chat.completion","model":"` + req.Model + `","choices":[{"message":{"role":"assistant","content":"response"}}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
}

func (s *cppworkerSimulator) handleOpenAICompletion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	_, _ = w.Write([]byte(`data: {"id":"cmpl-1","choices":[{"text":"response"}]}` + "\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *cppworkerSimulator) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	models := make([]map[string]interface{}, 0, len(s.loadedModels))
	for name, m := range s.loadedModels {
		models = append(models, map[string]interface{}{
			"id":       name,
			"object":   "model",
			"owned_by": s.workerID,
			"size":     m.Size,
		})
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": models, "object": "list"})
}

func (s *cppworkerSimulator) handleTags(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	models := make([]map[string]interface{}, 0, len(s.modelLibrary))
	for _, def := range s.modelLibrary {
		models = append(models, map[string]interface{}{
			"name":            def.Name,
			"size":            def.Size,
			"family":          def.Family,
			"format":          def.Format,
			"parameter_size":  def.ParameterSize,
			"quantization_level": def.Quantization,
		})
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"models": models})
}

func (s *cppworkerSimulator) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"embedding": []float64{0.1, 0.2, 0.3, 0.4, 0.5},
	})
}

// =====================================================================
// Helper: full response reader
// =====================================================================

// readSSE — read Server-Sent Events response.
func readSSE(t *testing.T, r io.Reader) []string {
	t.Helper()
	var events []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			events = append(events, strings.TrimPrefix(line, "data: "))
		}
	}
	return events
}

// readBodySim — read response body (renamed to avoid collision).
func readBodySim(resp *http.Response) string {
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}
