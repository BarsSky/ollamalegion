package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"strings"
	"time"
)

// handleV1Completions — OpenAI-совместимый /v1/completions endpoint.
func handleV1Completions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req openAICompletionRequest
	// R106c (2026-10-12): терпимое декодирование клиентского тела (/v1/completions).
	if err := decodeClientJSON(r, types.MaxStreamingBodyBytes, &req, "handleV1Completions"); err != nil {
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
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if req.Prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	// R66b: logit_bias принимается, но не применяется — предупреждаем один раз.
	if len(req.LogitBias) > 0 {
		warnLogitBiasOnce("v1/completions.logit_bias", len(req.LogitBias))
	}

	// InFlight counter: защищает активные запросы от reload-обрыва.
	if req.Model != "" && backend.InFlight() != nil {
		backend.InFlight().Inc(req.Model)
		defer backend.InFlight().Dec(req.Model)
	}

	// Round 18 P0.3 (2026-08-04): per-user parallel admission (fair-share).
	userID := getUserID(r)
	if max := maxParallelPerUser(); max > 0 {
		if !backend.UserTracker().TryAcquire(userID, max) {
			logger.Get().Warnw("handleV1Completions: user exceeded MaxParallelPerUser",
				"user_id", userID, "max", max, "model", req.Model, "remote", r.RemoteAddr)
			writeError(w, http.StatusTooManyRequests,
				fmt.Sprintf("user %q exceeded MaxParallelPerUser=%d (in-flight requests). Wait for current requests to complete.",
					userID, max))
			return
		}
		defer backend.UserTracker().Release(userID)
	}

	// Round 18 P0.2 (2026-08-03): per-request cancel tracking для /api/cancel.
	// re-bind r к child context — иначе writeOpenAICompletionStream смотрит на parent
	// и не видит отмену через /api/cancel.
	r, requestID, cancelCleanup := setupCancelTracking(r, req.Model, "cppworker-gpu", "oai-cmpl")
	defer cancelCleanup()
	// R493-12a: та же ячейка и тот же заголовок, что в v1/chat (см. выше).
	r = r.WithContext(withLoadScopeR493(r.Context()))
	logger.Get().Debugw("handleV1Completions: cancel tracking enabled",
		"request_id", requestID, "model", req.Model, "user_id", userID)

	actualModel, err := loadModelMeasuredR493(r.Context(), req.Model, req.NumCtx)
	if err != nil {
		// R119 (2026-10-15): 503/404/500 — одна точка перевода (см.
		// writeModelLoadError). До этого неизвестная модель давала 500 с пустым
		// телом, и OpenAI-клиент не мог показать причину.
		writeModelLoadError(w, req.Model, err)
		return
	}
	markLoadHeaderR493(w, r.Context())
	// R62 (2026-09-14): use actualModel для downstream calls.
	modelName := actualModel

	params := bridge.DefaultGenerationParams()
	// Round 16 follow-up fix (2026-07-30): *float64 — nil = use default, *0.0 = explicit 0.
	if req.Temperature != nil {
		params.Temperature = float32(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = float32(*req.TopP)
	}
	// R69 (2026-09-23): max_output_tokens (Cline) читается.
	// R83/v67 (2026-10-02): как ВЕРХНЯЯ ГРАНИЦА, а не как длина ответа (см.
	// подробное обоснование в buildOpenAIChatParams и handlers_generate.go).
	upperBound2 := 0
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		upperBound2 = *req.MaxOutputTokens
	}
	effectiveMaxTokens2 := req.MaxTokens
	// R463: тот же единый резолвер бюджета (см. buildOpenAIChatParams).
	budget2, budgetSource2 := resolveOutputBudgetR463(effectiveMaxTokens2, upperBound2, modelName)
	if budget2 != params.NPredict {
		logger.Get().Infow("output budget resolved (R463)",
			"model", modelName, "surface", "/v1/completions",
			"source", budgetSource2, "budget", budget2,
			"n_predict_before", params.NPredict,
			"client_max_tokens", effectiveMaxTokens2,
			"client_upper_bound", upperBound2,
			"ceiling", maxNPredictCeilingR463())
	}
	params.NPredict = budget2
	if upperBound2 > 0 && params.NPredict > upperBound2 {
		logger.Get().Infow(i18n.T("cppworker.handlers_openai.max_output_tokens_capped_completions"),
			"model", modelName, "max_output_tokens", upperBound2, "n_predict", params.NPredict)
		params.NPredict = upperBound2
	}
	if req.PresencePenalty != nil {
		params.PresencePenalty = float32(*req.PresencePenalty)
	}
	if req.FrequencyPenalty != nil {
		params.FrequencyPenalty = float32(*req.FrequencyPenalty)
	}
	if req.Seed != 0 {
		params.Seed = req.Seed
	}
	if req.NumCtx > 0 {
		params.NCtxOverride = req.NumCtx
	}
	ApplyCppCtxHeader(r, &params)

	if req.Stream {
		// Round 15 (2026-07-29): default include_usage=true (см. подробный
		// комментарий в /v1/chat/completions handler). Cline и другие
		// IDE-агенты ВСЕГДА хотят usage chunk для tracking контекстного окна.
		includeUsage := req.StreamOptions == nil || req.StreamOptions.IncludeUsage
		writeOpenAICompletionStream(w, r, actualModel, req.Prompt, params, includeUsage)
		return
	}

	start := time.Now()
	// /v1/completions (legacy text completions) — tools не поддерживаются,
	// reload разрешён при n_ctx overflow.
	result, err := generateBufferedWithCancel(r.Context(), actualModel, req.Prompt, params, false)
	// 2026-06-25: snapshot для /v1/completions.
	// Round 16 P2 fix (2026-07-30): closure form (consistent со всеми другими).
	defer func() {
		recordLastPromptFromError(actualModel, "/v1/completions", req.Prompt, &params, false, err)
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
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "completions failed", err)
		return
	}
	durationMs := time.Since(start).Milliseconds()

	// 2026-07-01: разделяем output на (reasoning_content, text) на верхнем уровне
	// ответа. Это позволяет OpenWebUI/Cline отображать think-блок и видимый ответ
	// раздельно.
	//
	// R108 (2026-10-13): безусловно — разбор идёт по тегам формата, а не по имени
	// модели (см. «Политика отдачи reasoning» в reasoning_content.go). Автодетект
	// остаётся только ради per-model состояния для будущих запросов.
	textValue := result.Output
	reasoningValue := ""
	if r, c, has := SplitReasoningContent(result.Output); has {
		textValue = c
		reasoningValue = r
		if backend != nil && backend.AutoEnableReasoning(modelName) {
			logger.Get().Warnw("reasoning auto-detected (non-stream /v1/completions)",
				"handler", "handleV1Completions",
				"model", modelName,
				"reasoning_chars", len(r),
				"hint", "model emits reasoning tags but is not in reasoning whitelist; "+
					"auto-enabled per-model reasoning defaults for future requests. "+
					"To pre-set, use load-with-params enableReasoning=true "+
					"or set CPPWORKER_REASONING_ARCHS env var.")
		}
	}

	resp := map[string]interface{}{
		"id":      fmt.Sprintf("cmpl-%d", time.Now().UnixNano()),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				// R108 (2026-10-13): видимый текст чистим от хвостовых служебных
				// токенов шаблона — так же, как нативные пути.
				"text":          cleanFinalContent(textValue),
				"index":         0,
				"finish_reason": "stop",
			},
		},
		"usage":       buildUsage(req.Prompt, result.Output, modelName),
		"duration_ms": durationMs,
	}
	if reasoningValue != "" {
		resp["reasoning_content"] = reasoningValue
	}
	writeJSON(w, http.StatusOK, resp)
}

// writeOpenAICompletionStream — streaming ответ в формате SSE для /v1/completions.
//
// 2026-06-29: токены стримятся инкрементально в callback (ниже). Финальный
// чанк содержит ТОЛЬКО finish_reason="stop" с пустым text — иначе клиент
// (после доработки семантики OpenAI-compatible) видит ДУБЛЬ: N чанков с
// токенами + ещё один с ПОЛНЫМ text. Предыдущая версия шла "open" на спорное
// поведение OpenWebUI; текущий фикс выровнен с /v1/chat/completions.
// writeOpenAICompletionStream — streaming ответ в формате SSE для /v1/completions.
//
// 2026-06-29: токены стримятся инкрементально в callback (ниже). Финальный
// чанк содержит ТОЛЬКО finish_reason="stop" с пустым text — иначе клиент
// (после доработки семантики OpenAI-compatible) видит ДУБЛЬ: N чанков с
// токенами + ещё один с ПОЛНЫМ text. Предыдущая версия шла "open" на спорное
// поведение OpenWebUI; текущий фикс выровнен с /v1/chat/completions.
//
// 2026-07-01: для reasoning-моделей (qwen3.5/qwen3.6/deepseek-r1/gemma-4)
// эмитим отдельный `reasoning_content` (если есть) и `text` (если есть) в
// каждом SSE-чанке, аналогично writeOpenAIChatStream.
//
// 2026-07-29 (Round 15): includeUsage добавляет финальный usage-чанк с
// prompt_tokens/completion_tokens/total_tokens — нужно Cline и другим
// IDE-агентам для tracking контекстного окна. По дефолту ВКЛЮЧЕНО (если
// клиент явно не передал stream_options.include_usage=false).
func writeOpenAICompletionStream(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams, includeUsage bool) {
	// 2026-06-24: preflight n_ctx check BEFORE WriteHeader — он пишет свой статус
	// и тело, поэтому до создания потокового писателя (иначе заголовки потока
	// уже были бы зафиксированы).
	if err := clampNPredictToFitContext(modelName, prompt, &params); err != nil {
		var perr *PromptExceedsNCtxError
		if errors.As(err, &perr) {
			writePromptExceedsNCtxResponse(w, perr)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// R493 (этап 7): поток пишется через общий writer — обрыв клиента виден
	// в debug/last-stream и RecordStreamBreak.
	sw, ok := newStreamWriterR493(w, r, "writeOpenAICompletionStream", modelName, "text/event-stream")
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	ctx := r.Context()
	// R63: per-infer AbortWatcher создаётся внутри GenerateStream (inference.go:521).
	completionID := fmt.Sprintf("cmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	// 2026-06-25: snapshot для /v1/completions streaming.
	var streamErr error
	defer func() {
		recordLastPromptFromErrorWithContext(modelName, "/v1/completions", prompt, &params, false, streamErr, r)
	}()

	// Накапливаем полный output параллельно для финального чанка.
	var outputBuf strings.Builder

	// 2026-07-01: reasoning-парсер (по аналогии с writeOpenAIChatStream).
	//
	// R108 (2026-10-13): разбор БЕЗУСЛОВНЫЙ (по тегам формата, не по имени
	// модели); auto-detect-блок удалён — он утекал начало маркера в видимый текст
	// до момента срабатывания. Автодетект остаётся только ради per-model
	// состояния для будущих запросов. Политика: reasoning_content.go.
	rcParser := NewReasoningStreamState()
	// R216: если шаблон открыл блок размышлений в ПРОМПТЕ, вывод начинается внутри него.
	rcParser.SetImpliedOpen(PromptOpensReasoning(prompt))
	rcAutoEnableDone := false

	// writeCompletionChunk — низкоуровневый helper, эмитит ОДИН SSE чанк с указанным
	// полями (text + опционально reasoning_content). Возвращает false при обрыве.
	writeCompletionChunk := func(text, reasoning string) bool {
		if text == "" && reasoning == "" {
			return true
		}
		choice := map[string]interface{}{"index": 0, "finish_reason": nil}
		if text != "" {
			choice["text"] = text
		}
		if reasoning != "" {
			choice["reasoning_content"] = reasoning
		}
		chunk := map[string]interface{}{
			"id":      completionID,
			"object":  "text_completion",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{choice},
		}
		jsonData, _ := json.Marshal(chunk)
		return sw.Writef("data: %s\n\n", jsonData)
	}

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		outputBuf.WriteString(token)

		// 2026-07-01: reasoning-парсер — разделяем токен на (reasoning, text) и
		// эмитим отдельный `reasoning_content` в SSE.
		// R108 (2026-10-13): безусловно (разбор — по тегам, не по имени модели).
		{
			reasoningDelta, textDelta := rcParser.Feed(token)
			// R108: автодетект — только per-model состояние для будущих запросов.
			if !rcAutoEnableDone && reasoningDelta != "" {
				rcAutoEnableDone = true
				if backend != nil && backend.AutoEnableReasoning(modelName) {
					logger.Get().Warnw("reasoning auto-detected mid-stream",
						"handler", "writeOpenAICompletionStream",
						"model", modelName,
						"output_len", len(outputBuf.String()),
						"hint", "model emits reasoning tags but is not in reasoning whitelist; "+
							"auto-enabled per-model reasoning defaults. To pre-set, use load-with-params "+
							"enableReasoning=true or set CPPWORKER_REASONING_ARCHS env var.")
				}
			}
			return writeCompletionChunk(textDelta, reasoningDelta)
		}
	}
	// writeOpenAICompletionStream — /v1/completions, tools не поддерживаются.
	streamErr = generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false)
	if streamErr != nil {
		maybeRestartOnMemorySlotError(streamErr, modelName)
		// Специальная обработка reload-loop-limit.
		if rllErr, ok := streamErr.(*ReloadLoopLimitError); ok {
			errChunk := map[string]interface{}{
				"id":      completionID,
				"object":  "text_completion",
				"created": created,
				"model":   modelName,
				"choices": []map[string]interface{}{
					{
						"text":          "",
						"index":         0,
						"finish_reason": "error",
					},
				},
				"error":           rllErr.Error(),
				"code":            "reload_loop_limit",
				"attempts":        rllErr.Count,
				"elapsed_seconds": rllErr.Elapsed.Seconds(),
			}
			errJSON, _ := json.Marshal(errChunk)
			sw.Writef("data: %s\n\n", errJSON)
			sw.Writef("data: [DONE]\n\n")
			sw.Flush()
			return
		}
		errChunk := map[string]interface{}{
			"id":      completionID,
			"object":  "text_completion",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"text":          "",
					"index":         0,
					"finish_reason": "error",
				},
			},
			"error": streamErr.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		sw.Writef("data: %s\n\n", errJSON)
		sw.Writef("data: [DONE]\n\n")
		sw.Flush()
		return
	}

	// Финальный чанк с полным output (раньше был пустой text — клиент видел пустой ответ).
	// 2026-06-24: проверка на пустой ответ после cleanFinalContent (gemma antiprompt).
	fullOutput := outputBuf.String()
	// R83/v67 (2026-10-02): финальный сброс reasoning-парсера (см. подробный
	// комментарий в writeOpenAIChatStream) — выпускаем удержанный хвост.
	// R108 (2026-10-13): безусловно (разбор — по тегам, не по имени модели).
	{
		rTail, tTail := rcParser.FinalizeForStream()
		if rTail != "" || tTail != "" {
			writeCompletionChunk(sanitizeStreamText(tTail), sanitizeStreamText(rTail))
		}
	}
	if strings.TrimSpace(cleanFinalContent(fullOutput)) == "" {
		errChunk := map[string]interface{}{
			"id":      completionID,
			"object":  "text_completion",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"text":          "",
					"index":         0,
					"finish_reason": "error",
				},
			},
			"error": "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, prompt too long, or model issue.",
		}
		errJSON, _ := json.Marshal(errChunk)
		sw.Writef("data: %s\n\n", errJSON)
		sw.Writef("data: [DONE]\n\n")
		sw.Flush()
		return
	}
	// 2026-06-29: финальный чанк с пустым text. Токены уже стримились
	// инкрементально в callback; чтобы не дублировать, шлём только role
	// (через пустой text) и finish_reason="stop". Подробнее см. комментарий
	// в writeOpenAIChatStream (аналогичная фиксация).
	stopChunk := map[string]interface{}{
		"id":      completionID,
		"object":  "text_completion",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"text":          "",
				"finish_reason": "stop",
			},
		},
	}
	stopJSON, _ := json.Marshal(stopChunk)
	sw.Writef("data: %s\n\n", stopJSON)
	sw.Flush()

	// Round 15 (2026-07-29): финальный usage chunk (если includeUsage=true)
	// — перед [DONE]. OpenAI stream_options.include_usage=true. Cline/прочие
	// IDE-агенты читают prompt_tokens из этого чанка и трекают контекст.
	writeOpenAIUsageChunk(sw, completionID, created, modelName, prompt, fullOutput, includeUsage)

	sw.Writef("data: [DONE]\n\n")
	sw.Flush()
}

// handleV1Embeddings — OpenAI-совместимый /v1/embeddings endpoint.
// Round 22 (2026-08-03): добавлена поддержка `input: []string` (batch) и
// lazy model load (раньше — только string + 500 если модель не загружена).
func handleV1Embeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Model string      `json:"model"`
		Input interface{} `json:"input"` // string OR []string
	}
	// R106c (2026-10-12): терпимое декодирование клиентского тела (/v1/embeddings).
	if err := decodeClientJSON(r, types.MaxRequestBodyBytes, &req, "handleV1Embeddings"); err != nil {
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
	if req.Model == "" || req.Input == nil {
		writeError(w, http.StatusBadRequest, "model and input are required")
		return
	}

	// Нормализуем input → []string
	var inputs []string
	switch v := req.Input.(type) {
	case string:
		inputs = []string{v}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				inputs = append(inputs, s)
			} else {
				writeError(w, http.StatusBadRequest, "input array must contain only strings")
				return
			}
		}
	default:
		writeError(w, http.StatusBadRequest, "input must be string or []string")
		return
	}
	if len(inputs) == 0 {
		writeError(w, http.StatusBadRequest, "input must contain at least one string")
		return
	}

	// Round 22: lazy model load (раньше handler возвращал 500 без load).
	// R62: возвращает actualModel (может быть alias) — handlers_embeddings.go использует его
	// в tokensToBridge / Embed call ниже.
	actualModel, err := ensureModelLoaded(r.Context(), req.Model)
	if err != nil {
		logger.Get().Errorw("v1/embeddings: model load failed", "model", req.Model, "error", err)
		// R119: 503/404/500 — одна точка перевода.
		writeModelLoadError(w, req.Model, err)
		return
	}
	modelName := actualModel

	// GetEmbeddings поддерживает только 1 input за раз. Для batch — вызываем
	// в цикле. (В будущем можно оптимизировать через batched API.)
	data := make([]map[string]interface{}, 0, len(inputs))
	totalTokens := 0
	for idx, text := range inputs {
		vec, err := backend.GetEmbeddings(modelName, text)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "embeddings failed: "+err.Error())
			return
		}
		data = append(data, map[string]interface{}{
			"object":    "embedding",
			"index":     idx,
			"embedding": vec,
		})
		totalTokens += buildPromptTokens(modelName, text)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   data,
		"model":  modelName,
		"usage": map[string]interface{}{
			"prompt_tokens": totalTokens,
			"total_tokens":  totalTokens,
		},
	})
}
