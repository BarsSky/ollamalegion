// CppBackend Worker — HTTP сервер для llama.cpp GGUF моделей
//
// Предоставляет REST API для загрузки/выгрузки GGUF моделей,
// инференса (синхронного и стриминг), управления multi-GPU,
// совместимый с API форматом Ollama.
//
// Usage:
//   cppworker --port 18091 --models-dir ./models
//   cppworker --port 18091 --gpu-layers -1 --flash-attn
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// Флаги командной строки
// ============================================================

var (
	port          = flag.Int("port", 18091, "HTTP server port")
	modelsDir     = flag.String("models-dir", "./models", "Directory with GGUF model files")
	ctxSize       = flag.Int("ctx-size", 4096, "Default context size")
	batchSize     = flag.Int("batch-size", 512, "Default batch size")
	gpuLayers     = flag.Int("gpu-layers", -1, "GPU layers (-1=all, 0=CPU)")
	flashAttn     = flag.Bool("flash-attn", true, "Enable Flash Attention")
	numa          = flag.Bool("numa", false, "Enable NUMA optimization")
	noMmap        = flag.Bool("no-mmap", false, "Disable mmap")
	verbose       = flag.Bool("verbose", false, "Verbose logging")
	allowedOrigin = flag.String("cors-origin", "*", "CORS allowed origin")
	envFile       = flag.String("env", "", "Path to .env configuration file (optional)")
	preloadModels = flag.Bool("preload-models", false, "Preload all .gguf models at startup (disabled by default — use with care, may exhaust VRAM)")
)

// ============================================================
// Global state
// ============================================================

var backend *cppbackend.Backend
var uptimeStart = time.Now()

// ============================================================
// Middleware
// ============================================================

// authMiddleware проверяет API_TOKEN для защищённых эндпоинтов (PUT/POST/DELETE)
func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := os.Getenv("API_TOKEN")
		if token == "" {
			next(w, r)
			return
		}
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") || strings.TrimPrefix(authHeader, "Bearer ") != token {
			writeError(w, http.StatusUnauthorized, "invalid or missing API token")
			return
		}
		next(w, r)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", *allowedOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-HF-Token")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		flusher, _ := w.(http.Flusher)
		lw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK, flusher: flusher}
		next.ServeHTTP(lw, r)
		duration := time.Since(start)
		remoteIP := r.RemoteAddr
		if idx := strings.LastIndex(r.RemoteAddr, ":"); idx > 0 {
			remoteIP = r.RemoteAddr[:idx]
		}
		logger.Get().Infow("HTTP request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lw.statusCode,
			"duration", duration.String(),
			"remote", remoteIP)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	flusher    http.Flusher
}

func (lw *loggingResponseWriter) WriteHeader(code int) {
	lw.statusCode = code
	lw.ResponseWriter.WriteHeader(code)
}

func (lw *loggingResponseWriter) Flush() {
	if lw.flusher != nil {
		lw.flusher.Flush()
	}
}

// ============================================================
// JSON утилиты
// ============================================================

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		logger.Get().Errorw("failed to write JSON response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ============================================================
// Health & Info handlers
// ============================================================

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": backend.Version(),
	})
}

func handleInfo(w http.ResponseWriter, r *http.Request) {
	status := backend.Status()
	writeJSON(w, http.StatusOK, status)
}

func handleGPUInfo(w http.ResponseWriter, r *http.Request) {
	metrics := backend.GetGPUMetrics()
	devices := backend.GetGPUDevices()
	result := make([]map[string]interface{}, len(devices))
	for i, dev := range devices {
		result[i] = map[string]interface{}{
			"index":       dev.Index,
			"name":        dev.Name,
			"vramTotalMB": dev.VRAMTotalMB,
			"vramFreeMB":  dev.VRAMFreeMB,
		}
		if i < len(metrics) {
			for k, v := range metrics[i] {
				result[i][k] = v
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"gpuCount": backend.GetGPUCount(),
		"devices":  result,
	})
}

// ============================================================
// Model management handlers
// ============================================================

type loadModelRequest struct {
	Name        string    `json:"name"`
	Path        string    `json:"path,omitempty"`
	GPULayers   *int      `json:"gpuLayers,omitempty"`
	ContextSize *int      `json:"contextSize,omitempty"`
	BatchSize   *int      `json:"batchSize,omitempty"`
	TensorSplit []float32 `json:"tensorSplit,omitempty"`
	FlashAttn   *bool     `json:"flashAttn,omitempty"`
	NUMA        *bool     `json:"numa,omitempty"`
	UseMmap     *bool     `json:"useMmap,omitempty"`
}

func defaultBoolPtr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func defaultIntPtr(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

func handleLoadModel(w http.ResponseWriter, r *http.Request) {
	var req loadModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	modelPath := req.Path
	modelName := req.Name

	// Резолвим путь для HF-скачанных моделей (префикс hf:)
	if strings.HasPrefix(modelName, "hf:") && backend.HFDownloader() != nil {
		// hf:repo/filename → ищем локальный путь
		parts := strings.TrimPrefix(modelName, "hf:")
		localPath, err := backend.HFDownloader().GetLocalPath(parts)
		if err == nil && localPath != "" {
			modelPath = localPath
		}
	}

	// Если путь не задан явно, ищем через ModelManager
	if modelPath == "" {
		mm := backend.ModelManager()
		if mm != nil {
			// Сначала пробуем найти через FindModelByPath (имя/частичное совпадение)
			foundPath, err := mm.FindModelByPath(modelName)
			if err == nil {
				modelPath = foundPath
			} else {
				// Fallback: конструируем путь из modelsDir
				modelPath = filepath.Join(*modelsDir, modelName)
				if !strings.HasSuffix(modelPath, ".gguf") {
					matches, err := filepath.Glob(modelPath + "*.gguf")
					if err == nil && len(matches) > 0 {
						modelPath = matches[0]
					} else {
						modelPath += ".gguf"
					}
				}
			}
		} else {
			modelPath = filepath.Join(*modelsDir, modelName)
			if !strings.HasSuffix(modelPath, ".gguf") {
				matches, err := filepath.Glob(modelPath + "*.gguf")
				if err == nil && len(matches) > 0 {
					modelPath = matches[0]
				} else {
					modelPath += ".gguf"
				}
			}
		}
	}

	// Собираем опции загрузки
	opts := cppbackend.LoadModelOpts{
		GPULayers:   defaultIntPtr(req.GPULayers, -1),
		ContextSize: defaultIntPtr(req.ContextSize, *ctxSize),
		BatchSize:   defaultIntPtr(req.BatchSize, *batchSize),
		FlashAttn:   defaultBoolPtr(req.FlashAttn, *flashAttn),
		NUMA:        defaultBoolPtr(req.NUMA, *numa),
		UseMmap:     defaultBoolPtr(req.UseMmap, !*noMmap),
		TensorSplit: req.TensorSplit,
	}

	logger.Get().Infow("loading model",
		"name", modelName,
		"path", modelPath,
		"gpuLayers", opts.GPULayers,
		"ctxSize", opts.ContextSize,
		"batchSize", opts.BatchSize,
		"flashAttn", opts.FlashAttn,
		"numa", opts.NUMA,
		"tensorSplit", opts.TensorSplit)

	if err := backend.LoadModelWithOpts(modelName, modelPath, opts); err != nil {
		logger.Get().Errorw("failed to load model", "name", modelName, "error", err)
		writeError(w, http.StatusInternalServerError, "load failed: "+err.Error())
		return
	}
	model, err := backend.GetModel(modelName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "loaded",
		"model":  model,
	})
}

func handleUnloadModel(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name query parameter is required")
		return
	}
	logger.Get().Infow("unloading model", "name", name)
	if err := backend.UnloadModel(name); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "unloaded",
		"name":   name,
	})
}

func handleListModels(w http.ResponseWriter, r *http.Request) {
	models := backend.ListModels()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"models": models,
		"count":  len(models),
	})
}

func handleGetModel(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name query parameter is required")
		return
	}
	model, err := backend.GetModel(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, model)
}

// ============================================================
// Inference handlers
// ============================================================

type generateRequest struct {
	Model            string  `json:"model"`
	Prompt           string  `json:"prompt"`
	System           string  `json:"system,omitempty"`
	Temperature      float64 `json:"temperature,omitempty"`
	TopP             float64 `json:"topP,omitempty"`
	TopK             int     `json:"topK,omitempty"`
	MaxTokens        int     `json:"maxTokens,omitempty"`
	RepeatPenalty    float64 `json:"repeatPenalty,omitempty"`
	FrequencyPenalty float64 `json:"frequencyPenalty,omitempty"`
	PresencePenalty  float64 `json:"presencePenalty,omitempty"`
	Seed             int     `json:"seed,omitempty"`
	Stream           bool    `json:"stream"`
}

type generateResponse struct {
	Model        string  `json:"model"`
	Response     string  `json:"response"`
	Done         bool    `json:"done"`
	Tokens       int     `json:"tokens"`
	DurationMs   int64   `json:"durationMs"`
	TokensPerSec float64 `json:"tokensPerSec,omitempty"`
}

type streamChunk struct {
	Model    string `json:"model"`
	Token    string `json:"token"`
	Response string `json:"response,omitempty"`
	Done     bool   `json:"done"`
}

func buildGenerationParams(req generateRequest) bridge.GenerationParams {
	params := bridge.DefaultGenerationParams()
	if req.Temperature > 0 {
		params.Temperature = float32(req.Temperature)
	}
	if req.TopP > 0 {
		params.TopP = float32(req.TopP)
	}
	if req.TopK > 0 {
		params.TopK = float32(req.TopK)
	}
	if req.MaxTokens > 0 {
		params.NPredict = req.MaxTokens
	}
	if req.RepeatPenalty > 0 {
		params.RepeatPenalty = float32(req.RepeatPenalty)
	}
	if req.FrequencyPenalty > 0 {
		params.FrequencyPenalty = float32(req.FrequencyPenalty)
	}
	if req.PresencePenalty > 0 {
		params.PresencePenalty = float32(req.PresencePenalty)
	}
	if req.Seed != 0 {
		params.Seed = req.Seed
	}
	return params
}

func handleGenerate(w http.ResponseWriter, r *http.Request) {
	var req generateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	// Ленивая загрузка модели если она ещё не в памяти
	if err := ensureModelLoaded(req.Model); err != nil {
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}
	params := buildGenerationParams(req)
	if req.Stream {
		writeStreamResponse(w, r, req.Model, req.Prompt, params)
		return
	}
	start := time.Now()
	result, err := backend.Generate(req.Model, req.Prompt, params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate failed: "+err.Error())
		return
	}
	duration := time.Since(start)
	tokens := countTokens(result.Output)
	resp := generateResponse{
		Model:      req.Model,
		Response:   result.Output,
		Done:       true,
		Tokens:     tokens,
		DurationMs: duration.Milliseconds(),
	}
	if duration.Seconds() > 0 {
		resp.TokensPerSec = float64(tokens) / duration.Seconds()
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeStreamResponse(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ctx := r.Context()
	tokens := 0
	start := time.Now()
	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		data := streamChunk{Model: modelName, Token: token, Done: false}
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
		tokens++
		return true
	}
	if err := backend.GenerateStream(modelName, prompt, params, callback); err != nil {
		errJSON, _ := json.Marshal(streamChunk{Model: modelName, Done: true})
		fmt.Fprintf(w, "data: %s\n\n", errJSON)
		flusher.Flush()
		return
	}
	duration := time.Since(start)
	finalJSON, _ := json.Marshal(streamChunk{Model: modelName, Done: true})
	fmt.Fprintf(w, "data: %s\n\n", finalJSON)
	tps := float64(0)
	if duration.Seconds() > 0 {
		tps = float64(tokens) / duration.Seconds()
	}
	metricsData := map[string]interface{}{
		"model": modelName, "tokens": tokens,
		"durationMs": duration.Milliseconds(), "tokensPerSec": tps,
	}
	metricsJSON, _ := json.Marshal(metricsData)
	fmt.Fprintf(w, "event: metrics\ndata: %s\n\n", metricsJSON)
	flusher.Flush()
}

// ============================================================
// Embeddings handler
// ============================================================

type embeddingsRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

func handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req embeddingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" || req.Input == "" {
		writeError(w, http.StatusBadRequest, "model and input are required")
		return
	}
	embeddings, err := backend.GetEmbeddings(req.Model, req.Input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "embeddings failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":      req.Model,
		"embeddings": embeddings,
	})
}

// ============================================================
// Ollama-compatible API handlers
// ============================================================

func handleOllamaGenerate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model   string `json:"model"`
		Prompt  string `json:"prompt"`
		System  string `json:"system,omitempty"`
		Options struct {
			Temperature   float64 `json:"temperature"`
			TopP          float64 `json:"top_p"`
			TopK          int     `json:"top_k"`
			NumPredict    int     `json:"num_predict"`
			RepeatPenalty float64 `json:"repeat_penalty"`
			Seed          int     `json:"seed"`
		} `json:"options"`
		Stream bool `json:"stream"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	genReq := generateRequest{Model: req.Model, Prompt: req.Prompt, Stream: req.Stream}
	if req.System != "" {
		genReq.System = req.System
	}
	if req.Options.Temperature > 0 {
		genReq.Temperature = req.Options.Temperature
	}
	if req.Options.TopP > 0 {
		genReq.TopP = req.Options.TopP
	}
	if req.Options.TopK > 0 {
		genReq.TopK = req.Options.TopK
	}
	if req.Options.NumPredict > 0 {
		genReq.MaxTokens = req.Options.NumPredict
	}
	if req.Options.RepeatPenalty > 0 {
		genReq.RepeatPenalty = req.Options.RepeatPenalty
	}
	if req.Options.Seed != 0 {
		genReq.Seed = req.Options.Seed
	}
	params := buildGenerationParams(genReq)
	if genReq.Stream {
		writeOllamaStream(w, r, genReq.Model, genReq.Prompt, params)
		return
	}
	start := time.Now()
	result, err := backend.Generate(genReq.Model, genReq.Prompt, params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	duration := time.Since(start)
	tokens := countTokens(result.Output)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":                genReq.Model,
		"response":             result.Output,
		"done":                 true,
		"context":              []int{},
		"total_duration":       duration.Microseconds() * 1000,
		"load_duration":        0,
		"prompt_eval_count":    0,
		"prompt_eval_duration": 0,
		"eval_count":           tokens,
		"eval_duration":        duration.Microseconds() * 1000,
	})
}

func writeOllamaStream(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	start := time.Now()
	callback := func(token string) bool {
		select {
		case <-r.Context().Done():
			return false
		default:
		}
		chunk := map[string]interface{}{"model": modelName, "response": token, "done": false}
		jsonData, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", jsonData)
		flusher.Flush()
		return true
	}
	if err := backend.GenerateStream(modelName, prompt, params, callback); err != nil {
		errJSON, _ := json.Marshal(map[string]interface{}{"model": modelName, "done": true, "error": err.Error()})
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}
	duration := time.Since(start)
	doneJSON, _ := json.Marshal(map[string]interface{}{
		"model": modelName, "done": true,
		"total_duration": duration.Microseconds() * 1000,
		"eval_count": 0, "eval_duration": duration.Microseconds() * 1000,
	})
	fmt.Fprintf(w, "%s\n", doneJSON)
	flusher.Flush()
}

func handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	// Используем map для дедупликации (загруженные модели имеют приоритет)
	modelMap := make(map[string]map[string]interface{})

	// 1. Сканируем файловую систему — все .gguf файлы (как Ollama)
	mm := backend.ModelManager()
	if mm != nil {
		fsModels := mm.ListModels()
		for _, fm := range fsModels {
			// Извлекаем имя модели из имени файла (без .gguf и пути)
			modelName := fm.Filename
			if strings.HasSuffix(strings.ToLower(modelName), ".gguf") {
				modelName = modelName[:len(modelName)-5] // убираем .gguf
			}
			digest := fmt.Sprintf("sha256:%x", fm.SizeBytes)
			modelMap[modelName] = map[string]interface{}{
				"name":        modelName,
				"model":       modelName,  // обязательно для OpenWebUI
				"modified_at": fm.ModifiedAt.Format(time.RFC3339),
				"size":        fm.SizeBytes,
				"digest":      digest,
				"details": map[string]interface{}{
					"format":             "gguf",
					"family":             fm.Architecture,
					"parameter_size":     "unknown",
					"quantization_level": fm.FileType,
				},
			}
		}
	}

	// 2. Добавляем/перезаписываем загруженные модели (имеют более точные метаданные)
	loadedModels := backend.ListModels()
	for _, m := range loadedModels {
		sizeBytes := m.SizeBytes
		if sizeBytes == 0 {
			// Fallback: используем размер из файловой системы
			if fsEntry, ok := modelMap[m.Name]; ok {
				if fsSize, ok2 := fsEntry["size"].(int64); ok2 && fsSize > 0 {
					sizeBytes = uint64(fsSize)
				}
			}
		}
		digest := fmt.Sprintf("sha256:%x", sizeBytes)
		modelMap[m.Name] = map[string]interface{}{
			"name":        m.Name,
			"model":       m.Name,  // обязательно для OpenWebUI
			"modified_at": m.LoadedAt.Format(time.RFC3339),
			"size":        sizeBytes,
			"digest":      digest,
			"details": map[string]interface{}{
				"format":             "gguf",
				"family":             m.Architecture,
				"parameter_size":     fmt.Sprintf("%.1fB", float64(m.NLayers*m.NEmbd)/1e9),
				"quantization_level": "unknown",
			},
		}
	}

	// 3. Собираем результирующий список
	ollamaModels := make([]map[string]interface{}, 0, len(modelMap))
	for _, v := range modelMap {
		ollamaModels = append(ollamaModels, v)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"models": ollamaModels})
}

// ============================================================
// File system & Utility handlers
// ============================================================

func handleListModelsDir(w http.ResponseWriter, r *http.Request) {
	mm := backend.ModelManager()
	if mm != nil {
		models := mm.ListModels()
		files := make([]map[string]interface{}, 0, len(models))
		for _, m := range models {
			files = append(files, map[string]interface{}{
				"name":       m.Filename,
				"sizeBytes":  m.SizeBytes,
				"modifiedAt": m.ModifiedAt.Format(time.RFC3339),
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"files": files, "dir": mm.GetModelsDir(), "count": len(files),
		})
		return
	}
	var files []map[string]interface{}
	entries, err := os.ReadDir(*modelsDir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"files": []interface{}{}, "dir": *modelsDir,
		})
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, map[string]interface{}{
			"name":       name,
			"sizeBytes":  info.Size(),
			"modifiedAt": info.ModTime().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"files": files, "dir": *modelsDir, "count": len(files),
	})
}

// ============================================================
// HuggingFace Handlers
// ============================================================

func handleHFSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, http.StatusBadRequest, "query parameter is required")
		return
	}
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := parseInt(l, 20); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	// Read HF token from X-HF-Token header (set by GgufApi in WebUI)
	hfToken := r.Header.Get("X-HF-Token")
	if hfToken == "" {
		hfToken = r.Header.Get("Authorization")
		if strings.HasPrefix(hfToken, "Bearer ") {
			hfToken = strings.TrimPrefix(hfToken, "Bearer ")
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if hfToken != "" {
		backend.HFDownloader().SetToken(hfToken)
	}
	results, err := backend.HFDownloader().SearchModels(ctx, query, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed: "+err.Error())
		return
	}
	if results == nil {
		results = []cppbackend.HFModelRepo{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": results, "count": len(results), "query": query,
	})
}

func handleHFFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	modelID := r.URL.Query().Get("modelId")
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "modelId parameter is required")
		return
	}
	revision := r.URL.Query().Get("revision")
	if revision == "" {
		revision = "main"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	files, err := backend.HFDownloader().ListModelFiles(ctx, modelID, revision)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list files failed: "+err.Error())
		return
	}
	if files == nil {
		files = []cppbackend.HFFileInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"files": files, "count": len(files), "modelId": modelID, "revision": revision,
	})
}

func handleHFDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req cppbackend.HFDownloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ModelID == "" {
		writeError(w, http.StatusBadRequest, "modelId is required")
		return
	}
	progress, err := backend.HFDownloader().StartDownload(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "download failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status": "started", "progress": progress,
	})
}

func handleHFDownloadProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	modelID := r.URL.Query().Get("modelId")
	filename := r.URL.Query().Get("filename")
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "modelId is required")
		return
	}
	progress, err := backend.HFDownloader().GetDownloadProgress(modelID, filename)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

func handleHFDownloads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	active := backend.HFDownloader().ListActiveDownloads()
	history := backend.HFDownloader().ListDownloadHistory()
	if active == nil {
		active = []cppbackend.HFDownloadProgress{}
	}
	if history == nil {
		history = []cppbackend.HFDownloadProgress{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"active": active, "history": history})
}

func handleHFCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		ModelID  string `json:"modelId"`
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ModelID == "" {
		writeError(w, http.StatusBadRequest, "modelId is required")
		return
	}
	if err := backend.HFDownloader().CancelDownload(req.ModelID, req.Filename); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// ============================================================
// CppWorker Version handler (Ollama-compatible)
// ============================================================

func handleCppWorkerVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version": backend.Version(),
	})
}

// ============================================================
// CppWorker Config handlers
// ============================================================

var currentConfig *cppbackend.Config

func handleCppWorkerGetConfig(w http.ResponseWriter, r *http.Request) {
	if currentConfig == nil {
		writeError(w, http.StatusServiceUnavailable, "backend not initialized yet")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"config":      currentConfig,
		"nodeName":    os.Getenv("NODE_NAME"),
		"nodeLabels":  os.Getenv("NODE_LABELS"),
		"balancerUrl": os.Getenv("BALANCER_URL"),
		"uptime":      time.Since(uptimeStart).String(),
	})
}

func handleCppWorkerUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "use PUT")
		return
	}
	if currentConfig == nil {
		writeError(w, http.StatusServiceUnavailable, "backend not initialized yet")
		return
	}
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	applied := []string{}
	if v, ok := updates["defaultCtxSize"]; ok {
		if ctx, ok := v.(float64); ok && ctx >= 256 {
			currentConfig.DefaultCtxSize = int(ctx)
			applied = append(applied, "defaultCtxSize")
		}
	}
	if v, ok := updates["defaultBatchSize"]; ok {
		if batch, ok := v.(float64); ok && batch >= 1 {
			currentConfig.DefaultBatchSize = int(batch)
			applied = append(applied, "defaultBatchSize")
		}
	}
	if v, ok := updates["defaultGpuLayers"]; ok {
		if gpu, ok := v.(float64); ok {
			currentConfig.DefaultGPULayers = int(gpu)
			applied = append(applied, "defaultGpuLayers")
		}
	}
	if v, ok := updates["defaultFlashAttn"]; ok {
		if fa, ok := v.(bool); ok {
			currentConfig.DefaultFlashAttn = fa
			applied = append(applied, "defaultFlashAttn")
		}
	}
	if v, ok := updates["defaultNuma"]; ok {
		if numa, ok := v.(bool); ok {
			currentConfig.DefaultNUMA = numa
			applied = append(applied, "defaultNuma")
		}
	}
	if v, ok := updates["defaultNThreads"]; ok {
		if threads, ok := v.(float64); ok {
			currentConfig.DefaultNThreads = int(threads)
			applied = append(applied, "defaultNThreads")
		}
	}
	envPath := *envFile
	if envPath == "" {
		envPath = "/app/.env"
	}
	if err := currentConfig.SaveConfigToDotEnv(envPath); err != nil {
		logger.Get().Warnw("failed to save config to .env", "path", envPath, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "updated", "applied": applied})
}

func handleCppWorkerReloadConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	envPath := *envFile
	if envPath == "" {
		envPath = "/app/.env"
	}
	if _, err := os.Stat(envPath); err != nil {
		writeError(w, http.StatusNotFound, "env file not found: "+envPath)
		return
	}
	if err := cppbackend.LoadDotEnvFile(envPath); err != nil {
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	newCfg := cppbackend.LoadConfigFromEnv()
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			newCfg.Port = *port
		case "models-dir":
			newCfg.ModelsDir = *modelsDir
		case "ctx-size":
			newCfg.DefaultCtxSize = *ctxSize
		case "batch-size":
			newCfg.DefaultBatchSize = *batchSize
		case "gpu-layers":
			newCfg.DefaultGPULayers = *gpuLayers
		case "flash-attn":
			newCfg.DefaultFlashAttn = *flashAttn
		case "numa":
			newCfg.DefaultNUMA = *numa
		case "no-mmap":
			newCfg.DefaultUseMmap = !*noMmap
		}
	})
	*currentConfig = newCfg
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "reloaded", "config": currentConfig})
}

func parseInt(s string, defaultVal int) (int, error) {
	if s == "" {
		return defaultVal, nil
	}
	val := 0
	_, err := fmt.Sscanf(s, "%d", &val)
	if err != nil {
		return defaultVal, err
	}
	return val, nil
}

// ============================================================
// Router setup
// ============================================================

func setupRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/api/health", handleHealth)
	mux.HandleFunc("/info", handleInfo)
	mux.HandleFunc("/api/info", handleInfo)
	mux.HandleFunc("/api/gpu", handleGPUInfo)
	mux.HandleFunc("/api/models/load", handleLoadModel)
	mux.HandleFunc("/load", handleLoadModel) // alias for balancer warmup
	mux.HandleFunc("/api/models/unload", handleUnloadModel)
	mux.HandleFunc("/api/models", handleListModels)
	mux.HandleFunc("/api/model", handleGetModel)
	mux.HandleFunc("/api/models/files", handleListModelsDir)
	mux.HandleFunc("/api/generate", handleGenerate)
	mux.HandleFunc("/api/chat", handleChat)
	mux.HandleFunc("/api/embeddings", handleEmbeddings)
	mux.HandleFunc("/api/ollama/generate", handleOllamaGenerate)
	mux.HandleFunc("/api/ollama/tags", handleOllamaTags)
	mux.HandleFunc("/api/tags", handleOllamaTags)
	mux.HandleFunc("/api/version", handleCppWorkerVersion)
	mux.HandleFunc("/api/hf/search", handleHFSearch)
	mux.HandleFunc("/api/hf/files", handleHFFiles)
	mux.HandleFunc("/api/hf/download", handleHFDownload)
	mux.HandleFunc("/api/hf/progress", handleHFDownloadProgress)
	mux.HandleFunc("/api/hf/downloads", handleHFDownloads)
	mux.HandleFunc("/api/hf/cancel", handleHFCancel)
	mux.HandleFunc("/api/v1/cppworker/config", handleCppWorkerGetConfig)
	mux.HandleFunc("/api/v1/cppworker/config/update", authMiddleware(handleCppWorkerUpdateConfig))
	mux.HandleFunc("/api/v1/cppworker/config/reload", authMiddleware(handleCppWorkerReloadConfig))
	mux.HandleFunc("/api/v1/cppworker/health", handleHealth)
	mux.HandleFunc("/api/v1/cppworker/metrics", handleInfo)
	return corsMiddleware(loggingMiddleware(mux))
}

// ============================================================
// Main
// ============================================================

func main() {
	flag.Parse()
	logLevel := "info"
	if *verbose {
		logLevel = "debug"
	}
	logger.Init(logLevel)
	log := logger.Get()

	dotEnvPath := *envFile
	if dotEnvPath == "" {
		candidates := []string{".env", "config/cppworker.env", "/app/.env"}
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				dotEnvPath = p
				break
			}
		}
	}
	if dotEnvPath != "" {
		if err := cppbackend.LoadDotEnvFile(dotEnvPath); err != nil {
			log.Warnw("failed to load .env file, continuing with defaults", "path", dotEnvPath, "error", err)
		} else {
			log.Infow("loaded configuration from .env", "path", dotEnvPath)
		}
	}

	cfg := cppbackend.LoadConfigFromEnv()
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			cfg.Port = *port
		case "models-dir":
			cfg.ModelsDir = *modelsDir
		case "ctx-size":
			cfg.DefaultCtxSize = *ctxSize
		case "batch-size":
			cfg.DefaultBatchSize = *batchSize
		case "gpu-layers":
			cfg.DefaultGPULayers = *gpuLayers
		case "flash-attn":
			cfg.DefaultFlashAttn = *flashAttn
		case "numa":
			cfg.DefaultNUMA = *numa
		case "no-mmap":
			cfg.DefaultUseMmap = !*noMmap
		}
	})

	if err := cfg.Validate(); err != nil {
		log.Fatalw("invalid configuration", "error", err)
	}

	log.Infow("CppBackend Worker starting",
		"port", cfg.Port, "host", cfg.Host, "modelsDir", cfg.ModelsDir,
		"gpuLayers", cfg.DefaultGPULayers, "flashAttn", cfg.DefaultFlashAttn,
		"numa", cfg.DefaultNUMA)

	if err := os.MkdirAll(cfg.ModelsDir, 0755); err != nil {
		log.Fatalw("failed to create models directory", "dir", cfg.ModelsDir, "error", err)
	}

	cc := cfg
	currentConfig = &cc

	backend = cppbackend.NewBackend(cfg)
	if err := backend.Init(); err != nil {
		log.Fatalw("failed to initialize backend", "error", err)
	}
	log.Infow("Backend initialized", "version", backend.Version(), "gpuCount", backend.GetGPUCount())

	router := setupRouter()
	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Infow("HTTP server listening", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalw("HTTP server error", "error", err)
		}
	}()

	// Авто-загрузка моделей при старте (только если --preload-models)
	// По умолчанию ОТКЛЮЧЕНА — модели загружаются лениво балансировщиком через /api/models/load
	if *preloadModels {
		go autoLoadModels(cfg)
	} else {
		log.Infow("preload-models disabled (models will be loaded lazily by the balancer on first request)")
	}

	<-quit
	log.Infow("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backend.Close()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalw("Server forced to shutdown", "error", err)
	}
	log.Infow("CppBackend Worker stopped")
}

func countTokens(text string) int {
	if text == "" {
		return 0
	}
	return len([]rune(text)) / 4
}

// ensureModelLoaded — ленивая загрузка модели из файловой системы если она ещё не в памяти
func ensureModelLoaded(modelName string) error {
	// Проверяем, загружена ли уже модель
	if _, err := backend.GetModel(modelName); err == nil {
		return nil // уже загружена
	}

	log := logger.Get()
	log.Infow("lazy-loading model from filesystem", "model", modelName)

	// Ищем модель в файловой системе
	mm := backend.ModelManager()
	if mm != nil {
		// Сначала пробуем найти через FindModelByPath
		modelPath, err := mm.FindModelByPath(modelName)
		if err != nil {
			// Fallback: конструируем путь из modelsDir
			modelPath = filepath.Join(*modelsDir, modelName)
			if !strings.HasSuffix(modelPath, ".gguf") {
				matches, globErr := filepath.Glob(modelPath + "*.gguf")
				if globErr == nil && len(matches) > 0 {
					modelPath = matches[0]
				} else {
					modelPath += ".gguf"
				}
			}
		}

		opts := cppbackend.LoadModelOpts{
			GPULayers:   *gpuLayers,
			ContextSize: *ctxSize,
			BatchSize:   *batchSize,
			FlashAttn:   *flashAttn,
			NUMA:        *numa,
			UseMmap:     !*noMmap,
		}

		if err := backend.LoadModelWithOpts(modelName, modelPath, opts); err != nil {
			log.Errorw("lazy-load failed", "model", modelName, "path", modelPath, "error", err)
			return fmt.Errorf("failed to load model %s: %w", modelName, err)
		}

		log.Infow("lazy-load successful", "model", modelName, "path", modelPath)
		return nil
	}

	return fmt.Errorf("model %s not found in filesystem", modelName)
}

// autoLoadModels сканирует modelsDir и загружает все найденные .gguf файлы при старте
func autoLoadModels(cfg cppbackend.Config) {
	log := logger.Get()
	modelsDir := cfg.ModelsDir

	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		log.Warnw("auto-load: failed to read models directory", "dir", modelsDir, "error", err)
		return
	}

	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
			continue
		}
		modelPath := filepath.Join(modelsDir, name)
		modelName := strings.TrimSuffix(name, ".gguf")

		opts := cppbackend.LoadModelOpts{
			GPULayers:   cfg.DefaultGPULayers,
			ContextSize: cfg.DefaultCtxSize,
			BatchSize:   cfg.DefaultBatchSize,
			FlashAttn:   cfg.DefaultFlashAttn,
			NUMA:        cfg.DefaultNUMA,
			UseMmap:     cfg.DefaultUseMmap,
		}

		log.Infow("auto-loading model",
			"name", modelName,
			"path", modelPath,
			"gpuLayers", opts.GPULayers)

		if err := backend.LoadModelWithOpts(modelName, modelPath, opts); err != nil {
			log.Errorw("auto-load: failed to load model",
				"name", modelName, "path", modelPath, "error", err)
			continue
		}
		loaded++
		log.Infow("auto-loaded model successfully", "name", modelName)
	}

	if loaded > 0 {
		log.Infow("auto-load complete", "loaded", loaded, "dir", modelsDir)
	} else {
		log.Infow("auto-load: no .gguf files found", "dir", modelsDir)
	}
}

// ============================================================
// OpenWebUI-compatible /api/chat handler
// ============================================================

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Model         string      `json:"model"`
	CreatedAt     string      `json:"created_at"`
	Message       chatMessage `json:"message"`
	Done          bool        `json:"done"`
	TotalDuration int64       `json:"total_duration,omitempty"`
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages array is required")
		return
	}

	// Ленивая загрузка модели если она ещё не в памяти
	if err := ensureModelLoaded(req.Model); err != nil {
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	// Собираем prompt из messages (берём последнее сообщение пользователя)
	prompt := ""
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			prompt += "[SYSTEM] " + msg.Content + "\n"
		} else if msg.Role == "user" {
			prompt += msg.Content
		}
	}

	genReq := generateRequest{
		Model:  req.Model,
		Prompt: prompt,
		Stream: req.Stream,
	}
	if req.Temperature != nil {
		genReq.Temperature = *req.Temperature
	}
	if req.MaxTokens != nil {
		genReq.MaxTokens = *req.MaxTokens
	}

	params := buildGenerationParams(genReq)
	start := time.Now()
	result, err := backend.Generate(req.Model, prompt, params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chat generate failed: "+err.Error())
		return
	}
	duration := time.Since(start)

	resp := chatResponse{
		Model:     req.Model,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Message: chatMessage{
			Role:    "assistant",
			Content: result.Output,
		},
		Done:          true,
		TotalDuration: duration.Nanoseconds(),
	}
	writeJSON(w, http.StatusOK, resp)
}
