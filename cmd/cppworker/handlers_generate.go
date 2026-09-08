package main

import (
	"encoding/json"

	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Generate handlers
// ============================================================

// normalizeGenerateRequest приводит Ollama-формат (options.*, system, raw)
// к единому виду, понятному buildGenerationParams.
//
// Round 16 follow-up fix (2026-07-30): req.* теперь *float64/*int (nil = unset).
// Конвертация из Options.* в req.* использует указатели чтобы отличить
// "Options.Temperature = 0.7" (явно задано) от "Options.Temperature = 0"
// (Ollama-клиент часто не заполняет поле — нужно использовать дефолт).
// Раньше `if Options.X > 0` означало что 0 → "не задано" — баг.
//
// Логика: если req.X == nil И Options.X != 0 → req.X = &Options.X
// (Options.X == 0 трактуем как "клиент не задал" для полей которые по
// дефолту > 0 — temperature, top_p и т.д.).
func normalizeGenerateRequest(req *generateRequest) {
	if req.Temperature == nil && req.Options.Temperature != 0 {
		v := req.Options.Temperature
		req.Temperature = &v
	}
	if req.TopP == nil && req.Options.TopP != 0 {
		v := req.Options.TopP
		req.TopP = &v
	}
	if req.TopK == nil && req.Options.TopK != 0 {
		v := req.Options.TopK
		req.TopK = &v
	}
	if req.MinP == nil && req.Options.MinP != 0 {
		v := req.Options.MinP
		req.MinP = &v
	}
	if req.TypicalP == nil && req.Options.TypicalP != 0 {
		v := req.Options.TypicalP
		req.TypicalP = &v
	}
	if req.TfsZ == nil && req.Options.TfsZ != 0 {
		v := req.Options.TfsZ
		req.TfsZ = &v
	}
	if req.MaxTokens == 0 && req.Options.NumPredict > 0 {
		req.MaxTokens = req.Options.NumPredict
	}
	if req.RepeatPenalty == nil && req.Options.RepeatPenalty != 0 {
		v := req.Options.RepeatPenalty
		req.RepeatPenalty = &v
	}
	if req.FrequencyPenalty == nil && req.Options.FrequencyPenalty != 0 {
		v := req.Options.FrequencyPenalty
		req.FrequencyPenalty = &v
	}
	if req.PresencePenalty == nil && req.Options.PresencePenalty != 0 {
		v := req.Options.PresencePenalty
		req.PresencePenalty = &v
	}
	if req.Seed == 0 && req.Options.Seed != 0 {
		req.Seed = req.Options.Seed
	}
	if req.NumCtx == 0 && req.Options.NumCtx > 0 {
		req.NumCtx = req.Options.NumCtx
	}

	// Ollama /api/generate: system prompt prepend
	if !req.Raw && req.System != "" {
		req.Prompt = req.System + "\n" + req.Prompt
	}

	// Round 11 (2026-07-28) SOFT Prompt Injection для EnableReasoning:
	// Prepend thinking instruction к prompt (Ollama /api/generate не
	// имеет system role, поэтому подмешиваем в prompt). Работает для
	// любой instruction-tuned модели (gemma-4-it, llama-3-it, и т.д.).
	//
	// Round 14b (2026-07-28): native enable_thinking НЕ применяется здесь,
	// потому что /api/generate использует single prompt (без messages[]).
	// Native path через common_chat_templates_apply требует structured
	// chat messages — это /api/chat endpoint (см. handlers_chat.go).
	// Для моделей с native thinking (Qwen3-thinking) рекомендуется
	// использовать /api/chat вместо /api/generate.
	//
	// Round 17.1 fix (2026-07-31): добавлена инструкция "Wrap your reasoning
	// in <reasoning>...</reasoning> tags". Без этого модели без нативного thinking
	// эмитят reasoning как обычный текст — парсер не split'ит. С тегами
	// парсер SplitReasoningContent корректно разделяет reasoning vs content.
	// См. handlers_chat.go:injectThinkingInstruction — подробное обоснование.
	if currentConfig != nil && currentConfig.EnableReasoning && !req.Raw {
		const thinkingInstruction = "Before answering, use detailed step-by-step thinking. " +
			"Reason about the problem carefully, consider different angles, " +
			"show your work, then provide a clear final answer. " +
			"Structure your response: first explain your reasoning, then give the answer. " +
			"IMPORTANT: Wrap your step-by-step reasoning inside <reasoning>...</reasoning> tags. " +
			"Your final answer (the user-facing response) should be OUTSIDE the </reasoning> tag."
		req.Prompt = thinkingInstruction + "\n\n" + req.Prompt
	}

	// keep_alive: парсим и сохраняем в LastUsedAt (если != 0).
	// Формат Ollama:
	//   "5m", "30s", "1h"   — duration (парсим через time.ParseDuration)
	//   "0" / "0s"          — выгрузить модель сразу после ответа (handled below)
	//   "-1" / ""           — бесконечно (default), но мы ставим 30 минут как разумный fallback
	//
	// ВАЖНО (2026-06-22): раньше это поле игнорировалось полностью (_ = req.KeepAlive).
	// Это означало, что клиенты OpenWebUI/Cline, рассчитывающие на стандартный Ollama-семантик
	// keep_alive, получали выгрузку модели по IdleUnloadManager cppworker (если он включён) или
	// UnloadScheduler балансировщика (если включён) — вне зависимости от того, что они запросили.
	// Теперь keep_alive ПРАВИЛЬНО влияет на время жизни модели в VRAM:
	//   - "5m" → lastUsedAt += 5 минут
	//   - "0"  → unload immediately after response (handled in runGenerateCore caller)
	//   - ""   → дефолт 30 минут
	keepAliveDur := parseKeepAlive(req.KeepAlive)
	req._keepAliveDuration = keepAliveDur
}

func buildGenerationParams(req generateRequest) bridge.GenerationParams {
	params := bridge.DefaultGenerationParams()
	// Round 16 follow-up fix (2026-07-30): *float64/*int — nil = use default, *0.0 = explicit 0.
	// Раньше `if > 0` ИГНОРИРОВАЛО temperature=0, top_p=0, top_k=0 (no filter)
	// и т.п. от клиента → подставлялся дефолт cppworker → не то что хотел клиент.
	if req.Temperature != nil {
		params.Temperature = float32(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = float32(*req.TopP)
	}
	if req.TopK != nil {
		params.TopK = float32(*req.TopK)
	}
	if req.MinP != nil {
		params.MinP = float32(*req.MinP)
	}
	if req.TypicalP != nil {
		params.TypicalP = float32(*req.TypicalP)
	}
	if req.TfsZ != nil {
		params.TfsZ = float32(*req.TfsZ)
	}
	if req.MaxTokens > 0 {
		params.NPredict = req.MaxTokens
	}
	if req.RepeatPenalty != nil {
		params.RepeatPenalty = float32(*req.RepeatPenalty)
	}
	if req.FrequencyPenalty != nil {
		params.FrequencyPenalty = float32(*req.FrequencyPenalty)
	}
	if req.PresencePenalty != nil {
		params.PresencePenalty = float32(*req.PresencePenalty)
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
//
// ВАЖНО (2026-06-22): если keep_alive == "0" (клиент явно попросил выгрузить),
// runGenerateCore вызывает backend.UnloadModel(modelName) после успешного инференса
// через caller'а (handleGenerate/handleOllamaGenerate должны вызвать applyKeepAlive).
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
	ApplyCppCtxHeader(r, &params)
	return params, req.Prompt, true
}

// applyKeepAlive — применяет результат парсинга req.KeepAlive после инференса.
//
// Семантика:
//
//	keep_alive == "0"  → выгружает модель из VRAM (UnloadModel).
//	keep_alive == "5m" → lastUsedAt += 5 минут (через backend.UpdateLastUsed).
//	keep_alive == ""   → дефолт 30 минут (через backend.UpdateLastUsed).
//
// Вызывается в defer-блоке handleGenerate/handleOllamaGenerate ПОСЛЕ успешного
// инференса. Это сохраняет модель в VRAM на запрошенное время даже после отправки
// ответа клиенту.
func applyKeepAlive(modelName string, keepAlive time.Duration) {
	if modelName == "" {
		return
	}
	if keepAlive <= 0 {
		// Клиент явно попросил выгрузить (keep_alive="0").
		logger.Get().Infow("model unload requested via keep_alive=0",
			"name", modelName)
		if err := backend.UnloadModel(modelName); err != nil {
			logger.Get().Warnw("applyKeepAlive: unload failed",
				"name", modelName, "error", err)
		}
		return
	}
	// Продлеваем время жизни модели.
	backend.UpdateLastUsed(modelName, keepAlive)
}

// isEmptyInferenceResult — true, если бэкенд вернул успех, но без текста.
func isEmptyInferenceResult(result *bridge.InferenceResult) bool {
	return result != nil && result.Status == 0 && strings.TrimSpace(result.Output) == ""
}

func handleGenerate(w http.ResponseWriter, r *http.Request) {
	var req generateRequest
	// Round 36 Phase 2: strict JSON decoder (rejects unknown fields).
	if err := types.DecodeJSONRequest(r.Body, types.MaxStreamingBodyBytes, &req); err != nil {
		switch {
		case errors.Is(err, types.ErrBodyEmpty):
			writeError(w, http.StatusBadRequest, "empty request body")
		case errors.Is(err, types.ErrBodyTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		default:
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return
	}
	modelName := req.Model
	// InFlight counter: защищает активные запросы от reload-обрыва.
	// handleReloadModel ждёт WaitZero перед UnloadModel.
	if modelName != "" && backend.InFlight() != nil {
		backend.InFlight().Inc(modelName)
		defer backend.InFlight().Dec(modelName)
	}

	// Round 18 P0.3 (2026-08-04): per-user parallel admission (fair-share).
	userID := getUserID(r)
	if max := currentConfig.MaxParallelPerUser; max > 0 {
		if !backend.UserTracker().TryAcquire(userID, max) {
			logger.Get().Warnw("handleGenerate: user exceeded MaxParallelPerUser",
				"user_id", userID, "max", max, "model", modelName, "remote", r.RemoteAddr)
			writeError(w, http.StatusTooManyRequests,
				fmt.Sprintf("user %q exceeded MaxParallelPerUser=%d (in-flight requests). Wait for current requests to complete.",
					userID, max))
			return
		}
		defer backend.UserTracker().Release(userID)
	}

	// Round 18 P0.2 (2026-08-03): per-request cancel tracking.
	// re-bind r к child context через setupCancelTracking (фикс первого P0.2-бага —
	// иначе writeGenerateStreamResponse смотрит на parent и не видит отмену).
	r, requestID, cancelCleanup := setupCancelTracking(r, modelName, "cppworker-gpu", "gen")
	defer cancelCleanup()
	logger.Get().Debugw("handleGenerate: cancel tracking enabled",
		"request_id", requestID, "model", modelName, "user_id", userID)
	params, prompt, ok := runGenerateCore(w, r, req)
	if !ok {
		return
	}

	// Round 26 v0.5.13: n_ctx overflow detection + X-Model-Context-Warning.
	// Cutoff-bug: длинная беседа в OpenWebUI → prompt > n_ctx → reload loop → 413.
	// Pre-emptive: проверяем и clamp n_predict + ставим warning header.
	genWarning, _ := ComputeContextWarning(modelName, prompt, params.NPredict, params.NCtxOverride)
	if genWarning.NPredict != params.NPredict {
		logger.Get().Warnw("handleGenerate: clamping n_predict to fit n_ctx",
			"model", modelName, "old_n_predict", params.NPredict,
			"new_n_predict", genWarning.NPredict, "n_ctx", genWarning.NCtx,
			"prompt_tokens", genWarning.PromptTokens, "used_pct", genWarning.UsedPercent)
		params.NPredict = genWarning.NPredict
	}
	SetContextWarningHeader(w, genWarning)

	// keep_alive: после успешного ответа применяется в defer.
	// Семантика: "0" → unload, "5m" → lastUsedAt += 5 минут, "" → дефолт 30 минут.
	defer func() {
		applyKeepAlive(modelName, req._keepAliveDuration)
	}()

	if req.Stream {
		writeStreamResponse(w, r, modelName, prompt, params)
		return
	}

	start := time.Now()
	// /api/generate endpoint: tools не поддерживаются.
	result, err := generateWithRamFallback(modelName, prompt, params, false)
	// 2026-06-25: записываем snapshot последнего inference для endpoint /debug/last-prompt.
	// Round 16 P2 fix (2026-07-30): closure form (consistent со всеми другими endpoints).
	defer func() {
		recordLastPromptFromError(modelName, "/api/generate", prompt, &params, false, err)
	}()
	if err != nil {
		// 2026-06-24: PromptExceedsNCtxError → HTTP 413 (см. inference.go).
		if handleInferenceError(w, err) {
			return
		}
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		// Специальная обработка reload-loop-limit (HTTP 413 с понятным message).
		if rllErr, ok := err.(*ReloadLoopLimitError); ok {
			writeReloadLoopLimitResponse(w, rllErr)
			return
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
	// 2026-06-24: preflight n_ctx check BEFORE flushing headers.
	if err := clampNPredictToFitContext(modelName, prompt, &params); err != nil {
		var perr *PromptExceedsNCtxError
		if errors.As(err, &perr) {
			writePromptExceedsNCtxResponse(w, perr)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()
	ctx := r.Context()
	// Round 31 #6: abort_watcher для streaming generate.
	if handle, ok := backend.GetHandle(modelName); ok {
		_ = NewAbortWatcher(ctx, handle)
	}
	tokens := 0
	var outputBuf strings.Builder
	start := time.Now()
	createdAt := time.Now().UTC().Format(time.RFC3339)
	// 2026-06-25: snapshot для /api/generate streaming.
	var streamErr error
	defer func() {
		recordLastPromptFromError(modelName, "/api/generate", prompt, &params, false, streamErr)
	}()
	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		outputBuf.WriteString(token)
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
	// /api/generate streaming: tools не поддерживаются.
	streamErr = generateStreamWithRamFallback(modelName, prompt, params, callback, false)
	if streamErr != nil {
		maybeRestartOnMemorySlotError(streamErr, modelName)
		errChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"response":    "",
			"done":        true,
			"done_reason": "error",
			"error":       streamErr.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}
	fullOutput := outputBuf.String()
	// 2026-06-24: проверка на пустой ответ после cleanFinalContent (gemma antiprompt).
	if strings.TrimSpace(cleanFinalContent(fullOutput)) == "" {
		errChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"response":    "",
			"done":        true,
			"done_reason": "error",
			"error":       "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, prompt too long, or model issue.",
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
		"created_at":        createdAt,
		"response":          "",
		"done":              true,
		"done_reason":       "stop",
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
	// R60.16 (2026-09-08): use BOM-stripping decoder (Windows PowerShell
	// Out-File default adds EF BB BF BOM → 400 without this).
	if err := decodeJSONRequest(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	modelName := req.Model
	// InFlight counter: защищает активные запросы от reload-обрыва.
	if modelName != "" && backend.InFlight() != nil {
		backend.InFlight().Inc(modelName)
		defer backend.InFlight().Dec(modelName)
	}
	params, prompt, ok := runGenerateCore(w, r, req)
	if !ok {
		return
	}

	// keep_alive: после успешного ответа применяется в defer.
	defer func() {
		applyKeepAlive(modelName, req._keepAliveDuration)
	}()

	if req.Stream {
		writeOllamaStream(w, r, modelName, prompt, params)
		return
	}
	start := time.Now()
	// /api/generate (Ollama) — tools не поддерживаются, reload разрешён при n_ctx overflow.
	result, err := generateWithRamFallback(modelName, prompt, params, false)
	// 2026-06-25: записываем snapshot последнего inference для endpoint /debug/last-prompt.
	// Round 16 P2 fix (2026-07-30): closure form (consistent со всеми другими endpoints).
	defer func() {
		recordLastPromptFromError(modelName, "/api/ollama/generate", prompt, &params, false, err)
	}()
	if err != nil {
		// 2026-06-24: PromptExceedsNCtxError → HTTP 413 (см. inference.go).
		if handleInferenceError(w, err) {
			return
		}
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		// Специальная обработка reload-loop-limit (HTTP 413 с понятным message).
		if rllErr, ok := err.(*ReloadLoopLimitError); ok {
			writeReloadLoopLimitResponse(w, rllErr)
			return
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

	// 2026-07-01: для reasoning-моделей (qwen3.5/qwen3.6/deepseek-r1/gemma-4)
	// разделяем output на (reasoning, response) и кладём в отдельные поля.
	// Ollama API использует `thinking` (не `reasoning`!), OpenAI — `reasoning_content`.
	// Здесь используем `thinking` для совместимости с Ollama-клиентами (Cline/OpenWebUI).
	response := result.Output
	thinking := ""
	if IsReasoningModel(modelName) {
		r, c, has := SplitReasoningContent(result.Output)
		if has {
			response = c
			thinking = r
		}
	}

	resp := map[string]interface{}{
		"model":                modelName,
		"response":             response,
		"done":                 true,
		"context":              []int{},
		"total_duration":       duration.Microseconds() * 1000,
		"load_duration":        loadDuration * 1000,
		"prompt_eval_count":    promptEvalCount,
		"prompt_eval_duration": duration.Microseconds() * 1000,
		"eval_count":           tokens,
		"eval_duration":        duration.Microseconds() * 1000,
	}
	if thinking != "" {
		resp["thinking"] = thinking
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeOllamaStream(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	// 2026-06-24: preflight n_ctx check BEFORE flushing headers.
	if err := clampNPredictToFitContext(modelName, prompt, &params); err != nil {
		var perr *PromptExceedsNCtxError
		if errors.As(err, &perr) {
			writePromptExceedsNCtxResponse(w, perr)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Round 32 (2026-08-09): emit immediate prefill heartbeat для /api/generate.
	// NDJSON {"done":false} — клиент видит что запрос "пошёл" сразу,
	// не дожидаясь 5-30s C-bridge prefill. См. handlers_chat.go для подробностей.
	prefillHB := map[string]interface{}{"done": false}
	prefillHBJSON, _ := json.Marshal(prefillHB)
	fmt.Fprintf(w, "%s\n", prefillHBJSON)
	flusher.Flush()
	// Round 31 #6: abort_watcher для ollama-generate streaming.
	if handle, ok := backend.GetHandle(modelName); ok {
		_ = NewAbortWatcher(r.Context(), handle)
	}
	tokens := 0
	var outputBuf strings.Builder
	start := time.Now()
	createdAt := time.Now().UTC().Format(time.RFC3339)
	// 2026-06-25: snapshot для /api/ollama/generate streaming.
	var streamErr error
	defer func() {
		recordLastPromptFromError(modelName, "/api/ollama/generate", prompt, &params, false, streamErr)
	}()
	// 2026-07-01: reasoning-парсер (для reasoning-моделей).
	ogParser := NewReasoningStreamState()
	ogIsReasoning := IsReasoningModel(modelName)

	callback := func(token string) bool {
		select {
		case <-r.Context().Done():
			return false
		default:
		}
		outputBuf.WriteString(token)
		if ogIsReasoning {
			thinkingDelta, responseDelta := ogParser.Feed(token)
			if thinkingDelta != "" {
				tc := map[string]interface{}{"model": modelName, "thinking": thinkingDelta, "done": false}
				tcJSON, _ := json.Marshal(tc)
				fmt.Fprintf(w, "%s\n", tcJSON)
				flusher.Flush()
			}
			if responseDelta != "" {
				rc := map[string]interface{}{"model": modelName, "response": responseDelta, "done": false}
				rcJSON, _ := json.Marshal(rc)
				fmt.Fprintf(w, "%s\n", rcJSON)
				flusher.Flush()
			}
			tokens++
			return true
		}
		chunk := map[string]interface{}{"model": modelName, "response": token, "done": false}
		jsonData, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", jsonData)
		flusher.Flush()
		tokens++
		return true
	}
	// /api/generate streaming: tools не поддерживаются.
	streamErr = generateStreamWithRamFallback(modelName, prompt, params, callback, false)
	if streamErr != nil {
		maybeRestartOnMemorySlotError(streamErr, modelName)
		errJSON, _ := json.Marshal(map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"response":    "",
			"done":        true,
			"done_reason": "error",
			"error":       streamErr.Error(),
		})
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}
	fullOutput := outputBuf.String()
	// 2026-06-24: проверка на пустой ответ после cleanFinalContent (gemma antiprompt).
	if strings.TrimSpace(cleanFinalContent(fullOutput)) == "" {
		errJSON, _ := json.Marshal(map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"response":    "",
			"done":        true,
			"done_reason": "error",
			"error":       "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, prompt too long, or model issue.",
		})
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
		"model": modelName, "done": true, "done_reason": "stop",
		"created_at":        createdAt,
		"total_duration":    duration.Microseconds() * 1000,
		"eval_count":        tokens,
		"eval_duration":     duration.Microseconds() * 1000,
		"tokens_per_second": tps,
	})
	fmt.Fprintf(w, "%s\n", doneJSON)
	flusher.Flush()
}

// defaultKeepAliveDuration — дефолтное значение keep_alive, если клиент его
// не задал (пустая строка) или задал "-1" / "0" без явной просьбы выгрузить.
// 30 минут — разумный баланс между «модель живёт для текущей сессии» и
// «не занимает VRAM вечно, если пользователь забыл выгрузить».
const defaultKeepAliveDuration = 30 * time.Minute

// parseKeepAlive парсит Ollama-стиль keep_alive в time.Duration.
//
// Поддерживаемые форматы:
//
//	"5m", "30s", "1h"  — duration через time.ParseDuration
//	"0", "0s", "0ms"   — 0 (выгрузить модель сразу после ответа, handled separately)
//	"-1", ""           — дефолт defaultKeepAliveDuration (30 минут)
//
// Возвращает 0 только если клиент ЯВНО попросил выгрузить (keep_alive="0").
// Иначе возвращает положительную длительность.
func parseKeepAlive(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" || s == "-1" {
		return defaultKeepAliveDuration
	}
	if s == "0" || s == "0s" || s == "0ms" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		logger.Get().Warnw("parseKeepAlive: invalid keep_alive value, using default",
			"keep_alive", s, "error", err, "default", defaultKeepAliveDuration.String())
		return defaultKeepAliveDuration
	}
	if d < 0 {
		return defaultKeepAliveDuration
	}
	return d
}
