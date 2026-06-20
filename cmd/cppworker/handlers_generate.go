package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// Generate handlers
// ============================================================

// normalizeGenerateRequest приводит Ollama-формат (options.*, system, raw)
// к единому виду, понятному buildGenerationParams.
func normalizeGenerateRequest(req *generateRequest) {
	if req.Options.Temperature > 0 && req.Temperature == 0 {
		req.Temperature = req.Options.Temperature
	}
	if req.Options.TopP > 0 && req.TopP == 0 {
		req.TopP = req.Options.TopP
	}
	if req.Options.TopK > 0 && req.TopK == 0 {
		req.TopK = req.Options.TopK
	}
	if req.Options.MinP > 0 && req.MinP == 0 {
		req.MinP = req.Options.MinP
	}
	if req.Options.TypicalP > 0 && req.TypicalP == 0 {
		req.TypicalP = req.Options.TypicalP
	}
	if req.Options.TfsZ > 0 && req.TfsZ == 0 {
		req.TfsZ = req.Options.TfsZ
	}
	if req.Options.NumPredict > 0 && req.MaxTokens == 0 {
		req.MaxTokens = req.Options.NumPredict
	}
	if req.Options.RepeatPenalty > 0 && req.RepeatPenalty == 0 {
		req.RepeatPenalty = req.Options.RepeatPenalty
	}
	if req.Options.FrequencyPenalty != 0 && req.FrequencyPenalty == 0 {
		req.FrequencyPenalty = req.Options.FrequencyPenalty
	}
	if req.Options.PresencePenalty != 0 && req.PresencePenalty == 0 {
		req.PresencePenalty = req.Options.PresencePenalty
	}
	if req.Options.Seed != 0 && req.Seed == 0 {
		req.Seed = req.Options.Seed
	}
	if req.Options.NumCtx > 0 && req.NumCtx == 0 {
		req.NumCtx = req.Options.NumCtx
	}

	// Ollama /api/generate: system prompt prepend
	if !req.Raw && req.System != "" {
		req.Prompt = req.System + "\n" + req.Prompt
	}

	_ = req.KeepAlive // accepted for compatibility
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
	if req.MinP > 0 {
		params.MinP = float32(req.MinP)
	}
	if req.TypicalP > 0 {
		params.TypicalP = float32(req.TypicalP)
	}
	if req.TfsZ > 0 {
		params.TfsZ = float32(req.TfsZ)
	}
	if req.MaxTokens > 0 {
		params.NPredict = req.MaxTokens
	}
	if req.RepeatPenalty > 0 {
		params.RepeatPenalty = float32(req.RepeatPenalty)
	}
	if req.FrequencyPenalty != 0 {
		params.FrequencyPenalty = float32(req.FrequencyPenalty)
	}
	if req.PresencePenalty != 0 {
		params.PresencePenalty = float32(req.PresencePenalty)
	}
	if req.Options.RepeatLastN > 0 {
		params.RepeatLastN = req.Options.RepeatLastN
	}
	if req.Options.Mirostat > 0 {
		params.Mirostat = req.Options.Mirostat
	}
	if req.Options.MirostatTau > 0 {
		params.MirostatTau = float32(req.Options.MirostatTau)
	}
	if req.Options.MirostatEta > 0 {
		params.MirostatEta = float32(req.Options.MirostatEta)
	}
	if req.Seed != 0 || req.Options.Seed != 0 {
		params.Seed = req.Seed
		if params.Seed == 0 {
			params.Seed = req.Options.Seed
		}
	}
	if req.NumCtx > 0 {
		params.NCtxOverride = req.NumCtx
	}
	params.Antiprompts = append(params.Antiprompts, parseStopSequences(req.Options.Stop)...)
	params.StopSequences = params.Antiprompts
	return params
}

// parseStopSequences нормализует Ollama options.stop (string, []string или []interface{})
// в плоский []string для Antiprompts.
func parseStopSequences(stop interface{}) []string {
	if stop == nil {
		return nil
	}
	switch v := stop.(type) {
	case string:
		if v != "" {
			return []string{v}
		}
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// runGenerateCore — общая часть handleGenerate/handleOllamaGenerate:
// валидация, нормализация Ollama-формата, lazy-load, построение params.
func runGenerateCore(w http.ResponseWriter, r *http.Request, req generateRequest) (bridge.GenerationParams, string, bool) {
	normalizeGenerateRequest(&req)

	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return bridge.GenerationParams{}, "", false
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return bridge.GenerationParams{}, "", false
	}

	if err := ensureModelLoaded(req.Model); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return bridge.GenerationParams{}, "", false
		}
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return bridge.GenerationParams{}, "", false
	}

	params := buildGenerationParams(req)
	applyCppCtxHeader(r, &params)
	return params, req.Prompt, true
}

// isEmptyInferenceResult — true, если бэкенд вернул успех, но без текста.
func isEmptyInferenceResult(result *bridge.InferenceResult) bool {
	return result != nil && result.Status == 0 && strings.TrimSpace(result.Output) == ""
}

func handleGenerate(w http.ResponseWriter, r *http.Request) {
	var req generateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	params, prompt, ok := runGenerateCore(w, r, req)
	if !ok {
		return
	}
	modelName := req.Model

	if req.Stream {
		writeStreamResponse(w, r, modelName, prompt, params)
		return
	}

	start := time.Now()
	result, err := generateWithRamFallback(modelName, prompt, params)
	if err != nil {
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "generate failed", err)
		return
	}
	if isEmptyInferenceResult(result) {
		logger.Get().Warnw("generate returned empty output", "model", modelName)
		writeError(w, http.StatusInternalServerError, "model produced an empty response")
		return
	}
	duration := time.Since(start)
	tokens := countTokens(modelName, result.Output)
	resp := generateResponse{
		Model:      modelName,
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

// writeStreamResponse — streaming ответ в формате NDJSON (application/x-ndjson).
func writeStreamResponse(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()
	ctx := r.Context()
	tokens := 0
	start := time.Now()
	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		chunk := map[string]interface{}{
			"model":    modelName,
			"response": token,
			"done":     false,
		}
		jsonData, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", jsonData)
		flusher.Flush()
		tokens++
		return true
	}
	if err := generateStreamWithRamFallback(modelName, prompt, params, callback); err != nil {
		maybeRestartOnMemorySlotError(err, modelName)
		errChunk := map[string]interface{}{
			"model": modelName,
			"done":  true,
			"error": err.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}
	duration := time.Since(start)
	tps := float64(0)
	if duration.Seconds() > 0 {
		tps = float64(tokens) / duration.Seconds()
	}
	doneChunk := map[string]interface{}{
		"model":             modelName,
		"done":              true,
		"total_duration":    duration.Microseconds() * 1000,
		"eval_count":        tokens,
		"eval_duration":     duration.Microseconds() * 1000,
		"tokens_per_second": tps,
	}
	doneJSON, _ := json.Marshal(doneChunk)
	fmt.Fprintf(w, "%s\n", doneJSON)
	flusher.Flush()
}

// handleOllamaGenerate — Ollama /api/ollama/generate и /api/generate (Ollama-формат).
func handleOllamaGenerate(w http.ResponseWriter, r *http.Request) {
	var req generateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	params, prompt, ok := runGenerateCore(w, r, req)
	if !ok {
		return
	}
	modelName := req.Model

	if req.Stream {
		writeOllamaStream(w, r, modelName, prompt, params)
		return
	}
	start := time.Now()
	result, err := generateWithRamFallback(modelName, prompt, params)
	if err != nil {
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "generate failed", err)
		return
	}
	if isEmptyInferenceResult(result) {
		logger.Get().Warnw("ollama generate returned empty output", "model", modelName)
		writeError(w, http.StatusInternalServerError, "model produced an empty response")
		return
	}
	promptEvalCount := countTokens(modelName, prompt)
	loadDuration := int64(0)
	if info, err := backend.GetModel(modelName); err == nil && !info.LoadedAt.IsZero() {
		loadDuration = time.Since(info.LoadedAt).Milliseconds()
	}
	duration := time.Since(start)
	tokens := countTokens(modelName, result.Output)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":                modelName,
		"response":             result.Output,
		"done":                 true,
		"context":              []int{},
		"total_duration":       duration.Microseconds() * 1000,
		"load_duration":        loadDuration * 1000,
		"prompt_eval_count":    promptEvalCount,
		"prompt_eval_duration": duration.Microseconds() * 1000,
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
	flusher.Flush()
	tokens := 0
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
		tokens++
		return true
	}
	if err := generateStreamWithRamFallback(modelName, prompt, params, callback); err != nil {
		maybeRestartOnMemorySlotError(err, modelName)
		errJSON, _ := json.Marshal(map[string]interface{}{"model": modelName, "done": true, "error": err.Error()})
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}
	duration := time.Since(start)
	tps := float64(0)
	if duration.Seconds() > 0 {
		tps = float64(tokens) / duration.Seconds()
	}
	doneJSON, _ := json.Marshal(map[string]interface{}{
		"model": modelName, "done": true,
		"total_duration":    duration.Microseconds() * 1000,
		"eval_count":        tokens,
		"eval_duration":     duration.Microseconds() * 1000,
		"tokens_per_second": tps,
	})
	fmt.Fprintf(w, "%s\n", doneJSON)
	flusher.Flush()
}
