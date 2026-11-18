package main

import (
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

// handleV1ChatCompletions — OpenAI-совместимый /v1/chat/completions endpoint.
// Поддерживает как streaming (SSE: text/event-stream), так и non-streaming ответы.
// При наличии tools/function calling:
//   - Инжектит definitions в system prompt
//   - После генерации парсит tool_calls из plain text выхода модели
//   - Если tool_calls найдены — возвращает finish_reason="tool_calls" + message.tool_calls
//   - Для streaming с tools: буферизирует полный ответ, затем отдаёт SSE с tool_calls
func handleV1ChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req openAIChatCompletionRequest
	// R106c (2026-10-12): терпимое декодирование — /v1/chat/completions это
	// клиентский API (Cline в OpenAI-режиме, Roo Code, OpenAI SDK шлют свои
	// расширения: stream_options, parallel_tool_calls, reasoning_effort …).
	// Незнакомые поля игнорируем, как это делает сам OpenAI/llama.cpp.
	if err := decodeClientJSON(r, types.MaxStreamingBodyBytes, &req, "handleV1ChatCompletions"); err != nil {
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
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages array is required")
		return
	}
	// R66b: logit_bias принимается, но не применяется — предупреждаем один раз.
	if len(req.LogitBias) > 0 {
		warnLogitBiasOnce("v1/chat/completions.logit_bias", len(req.LogitBias))
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
			logger.Get().Warnw("handleV1ChatCompletions: user exceeded MaxParallelPerUser",
				"user_id", userID, "max", max, "model", req.Model, "remote", r.RemoteAddr)
			writeError(w, http.StatusTooManyRequests,
				fmt.Sprintf("user %q exceeded MaxParallelPerUser=%d (in-flight requests). Wait for current requests to complete.",
					userID, max))
			return
		}
		defer backend.UserTracker().Release(userID)
	}

	// Round 18 P0.2 (2026-08-03): per-request cancel tracking для /api/cancel.
	// re-bind r к child context — иначе writeOpenAIChatStream смотрит на parent
	// и не видит отмену через /api/cancel.
	r, requestID, cancelCleanup := setupCancelTracking(r, req.Model, "cppworker-gpu", "oai")
	defer cancelCleanup()
	// R493-12a: ячейка загрузки + измерение. На OpenAI-поверхности в ответе нет
	// полей длительностей вообще, поэтому загрузка уезжает заголовком
	// (X-Cppworker-Load-Duration-Ms): балансер складывает её со своим ожиданием.
	r = r.WithContext(withLoadScopeR493(r.Context()))
	logger.Get().Debugw("handleV1ChatCompletions: cancel tracking enabled",
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
	// R62 (2026-09-14): use actualModel для всех downstream calls (chat template,
	// Infer, antiprompts, etc). Если dedup-pre-check загрузил модель под alias
	// (например requested="Qwen3-Instruct-2507-q4km" → loaded="qwen3-instruct"),
	// все эти функции ищут модель по имени в backend.GetModel() — нужен actual name.
	modelName := actualModel

	// Конвертируем openAIChatMessage → chatMessage.
	//
	// R434 (2026-11-05): преамбулу с инструментами здесь БОЛЬШЕ НЕ дописываем —
	// tools[] уходят в сборку промпта и сначала пробуются в самом chat template
	// (нативная секция инструментов модели). Текстовая преамбула добавляется
	// только если шаблон их не отрисовал (см. buildChatPromptWithOptions).
	chatMsgs := openAIToChatMessage(req.Messages)
	promptOpts := chatPromptOptions{}
	if len(req.Tools) > 0 && !toolsDisabledByChoice(req.ToolChoice) {
		promptOpts.Tools = req.Tools
		promptOpts.ToolChoice = req.ToolChoice
	}

	// R248/B3 (2026-10-27): картинки на OpenAI-пути.
	//
	// ВАЖНО: балансер (а значит и все OpenAI-клиенты) ходит именно сюда, а не в
	// /api/chat, поэтому проводка vision обязана быть здесь. Payload'ы
	// валидируются до сборки промпта, маркер проектора вставляется в сообщение
	// (перед текстом — так эти модели обучались), а иначе запрос ушёл бы в
	// текстовую генерацию и вернул правдоподобный, но не основанный на
	// изображении ответ.
	var visionImages [][]byte
	if payloads := chatImages(chatMsgs); len(payloads) > 0 {
		decoded, verr := decodeVisionImages(payloads)
		if verr != nil {
			writeVisionError(w, req.Model, verr)
			return
		}
		if !backend.VisionCapable(actualModel) {
			writeVisionError(w, req.Model,
				fmt.Errorf("vision_not_supported: model %s is loaded without mmproj", actualModel))
			return
		}
		marker := backend.MMProjMarker(actualModel)
		if marker == "" {
			writeVisionError(w, req.Model,
				fmt.Errorf("vision_not_supported: model %s has no media marker", actualModel))
			return
		}
		visionImages = decoded
		chatMsgs = injectMediaMarkers(chatMsgs, marker)
	}

	// Собираем промпт из сообщений с применением chat template из GGUF.
	prompt, err := buildChatPromptWithOptions(chatMsgs, actualModel, promptOpts)
	var usedNaive bool
	if err != nil {
		logger.Get().Warnw("chat template bridge failed, falling back to naive prompt",
			"model", modelName, "error", err)
		prompt = buildNaiveChatPromptFromMsgs(chatMsgs, actualModel)
		if prompt == "" {
			writeError(w, http.StatusInternalServerError, "build prompt failed: "+err.Error())
			return
		}
		usedNaive = true
	}

	// R248 (2026-10-27): отпечаток запроса. DSH-цикл «запрос таймаутит → повтор»
	// снаружи неотличим от «результат инструмента не доехал до модели»: в обоих
	// случаях приходят новые запросы и модель снова генерирует. В логе теперь
	// видно hash промпта, распределение ролей и объём tool-результатов —
	// одинаковый hash подряд = ретрай клиента; tool_results=0 при истории с
	// вызовом = потерянный результат.
	{
		roles, toolResults, toolBytes := promptSummary(chatMsgs)
		logger.Get().Infow("prompt fingerprint",
			"surface", "/v1/chat/completions", "model", actualModel,
			"hash", promptFingerprint(prompt), "prompt_len", len(prompt),
			"roles", roles, "tool_results", toolResults, "tool_result_bytes", toolBytes,
			"stream", req.Stream, "tools", len(req.Tools), "naive_prompt", usedNaive)
		if dbg := toolPromptDebugTail(prompt); dbg != "" {
			logger.Get().Warnw("tool prompt debug tail", "surface", "/v1/chat/completions",
				"model", actualModel, "tool_results", toolResults, "tail", dbg)
		}
	}

	params := bridge.DefaultGenerationParams()
	// Round 16 follow-up fix (2026-07-30): *float64 — nil = use default, *0.0 = greedy/no-top_p.
	// Раньше `if req.Temperature > 0` ИГНОРИРОВАЛО temperature=0 (Cline tool calls) → использовался
	// дефолт 0.7 → не-greedy sampling, недетерминированные tool calls.
	if req.Temperature != nil {
		params.Temperature = float32(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = float32(*req.TopP)
	}

	// R248: ход с результатом инструмента — детерминированный (greedy). Живой
	// дефект: при дефолтной температуре модель периодически повторяет тот же
	// tool_call вместо ответа по результату, и клиент (DSH/Cline) уходит в цикл.
	if applyToolFollowupDeterminism(chatMsgs, &params, req.Temperature != nil) {
		logger.Get().Infow("tool follow-up: greedy for determinism",
			"model", actualModel, "surface", "/v1/chat/completions")
	}
	// R69 (2026-09-23): max_output_tokens (Cline) читается.
	//
	// R83/v67 (2026-10-02): это ВЕРХНЯЯ ГРАНИЦА «сколько ответа клиент вообще
	// примет», а НЕ запрошенная длина. Раньше значение подставлялось в
	// effectiveMaxTokens → num_predict = 64000, и модель получала право
	// генерировать 64000 токенов (десятки минут на слабой карте, обрыв по
	// клиентскому таймауту, «пустой ответ»). Теперь длину задаёт политика
	// (max_tokens/num_predict → профиль → дефолт), а граница только уменьшает её.
	upperBound := 0
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		upperBound = *req.MaxOutputTokens
	}
	effectiveMaxTokens := req.MaxTokens
	// R463 (2026-11-06): единый резолвер бюджета вывода. Если клиент задал только
	// верхнюю границу (max_output_tokens), она может ПОДНЯТЬ дефолт модели —
	// иначе reasoning-модель на 27B съедала 8192 на размышления и ход кончался
	// done_reason=length без видимого ответа (клиент → overflow-recovery →
	// «text part … not found»).
	budget, budgetSource := resolveOutputBudgetR463(effectiveMaxTokens, upperBound, actualModel)
	if budget != params.NPredict {
		logger.Get().Infow("output budget resolved (R463)",
			"model", actualModel, "surface", "/v1/chat/completions",
			"source", budgetSource, "budget", budget,
			"n_predict_before", params.NPredict,
			"client_max_tokens", effectiveMaxTokens,
			"client_upper_bound", upperBound,
			"ceiling", maxNPredictCeilingR463())
	}
	params.NPredict = budget
	if upperBound > 0 && params.NPredict > upperBound {
		logger.Get().Infow(i18n.T("cppworker.handlers_openai.max_output_tokens_capped_chat"),
			"model", actualModel, "max_output_tokens", upperBound, "n_predict", params.NPredict)
		params.NPredict = upperBound
	}
	if req.Seed != 0 {
		params.Seed = req.Seed
	}
	if req.NumCtx > 0 {
		params.NCtxOverride = req.NumCtx
	}

	// R65d (2026-09-20): поля, добавленные для совместимости (аудит, находка 1.1).
	// Раз структура их принимает, они должны реально влиять на генерацию —
	// иначе «приняли и забыли» тихо ломает ожидания клиента.
	if req.TopK != nil {
		params.TopK = float32(*req.TopK)
	}
	if req.RepeatPenalty != nil {
		params.RepeatPenalty = float32(*req.RepeatPenalty)
	}
	if req.MinP != nil {
		params.MinP = float32(*req.MinP)
	}
	if req.FrequencyPenalty != nil {
		params.FrequencyPenalty = float32(*req.FrequencyPenalty)
	}
	if req.PresencePenalty != nil {
		params.PresencePenalty = float32(*req.PresencePenalty)
	}
	// num_predict — Ollama-алиас max_tokens; max_tokens имеет приоритет, если задан.
	if req.NumPredict != nil && *req.NumPredict > 0 && req.MaxTokens <= 0 {
		params.NPredict = ResolveNPredict(*req.NumPredict, actualModel)
	}

	// Bug fix (Round 4): прокидываем HasTools=true чтобы увеличить резерв
	// токенов под tools-prompt в n_ctx clamp. Без этого tools-запросы
	// получают 1024 токен резерва (как обычные чаты), и при 5+ tools с
	// длинными описаниями n_ctx overflow → reload-loop → 413 "n_ctx_too_large".
	ApplyCppCtxHeaderWithOptions(r, &params, CppCtxApplyOptions{
		HasTools: len(req.Tools) > 0,
	})

	// Antiprompts: дефолтные для формата промпта + пользовательские req.Stop.
	defaultAP := defaultAntipromptsForModel(actualModel)
	if len(defaultAP) > 0 {
		params.Antiprompts = append(params.Antiprompts, defaultAP...)
	}
	if !usedNaive {
		if isGemmaModel(actualModel) {
			params.Antiprompts = append(params.Antiprompts, []string{"<end_of_turn>", "<start_of_turn>user"}...)
		}
	}
	for _, s := range req.Stop {
		if s != "" {
			params.Antiprompts = append(params.Antiprompts, s)
		}
	}
	logger.Get().Debugw("handleV1ChatCompletions: antiprompts",
		"model", modelName, "used_naive", usedNaive,
		"antiprompts_count", len(params.Antiprompts),
		"antiprompts", params.Antiprompts)

	// Round 26 v0.5.13: n_ctx overflow detection + X-Model-Context-Warning.
	// Cutoff-bug: длинная беседа через OpenAI-совместимый API (Cline, Roo Code, IDE plugins, Hermes)
	// → prompt > n_ctx → reload loop → 413 abrupt stop. Pre-emptive: проверяем и
	// clamp n_predict + ставим warning header.
	openaiWarning, _ := ComputeContextWarning(modelName, prompt, params.NPredict, params.NCtxOverride)
	if openaiWarning.NPredict != params.NPredict {
		logger.Get().Warnw("handleV1ChatCompletions: clamping n_predict to fit n_ctx",
			"model", modelName, "old_n_predict", params.NPredict,
			"new_n_predict", openaiWarning.NPredict, "n_ctx", openaiWarning.NCtx,
			"prompt_tokens", openaiWarning.PromptTokens, "used_pct", openaiWarning.UsedPercent)
		params.NPredict = openaiWarning.NPredict
	}
	SetContextWarningHeader(w, openaiWarning)

	// ============================================================
	// Streaming
	// ============================================================
	// R248/B3: image-запросы стримингуются буферизованно (префилл libmtmd
	// синхронный): считаем ответ целиком и отдаём валидным SSE — один delta-чанк
	// + finish + [DONE]. Клиент (OpenWebUI/OpenAI SDK) получает обычный stream,
	// просто с одним чанком, вместо прежнего 400.
	if len(visionImages) > 0 && req.Stream {
		result, verr := generateBufferedOrImages(r.Context(), actualModel, prompt, params, len(req.Tools) > 0, visionImages)
		if verr != nil {
			if handleInferenceError(w, verr) {
				return
			}
			writeCppWorkerErrorWithBridgeInfo(w, http.StatusInternalServerError, "vision generate failed", verr)
			return
		}
		writeOpenAISSEFromResult(w, r, actualModel, result)
		return
	}
	if req.Stream {
		if len(req.Tools) > 0 {
			// Если есть tools — буферизируем полный ответ, чтобы распарсить tool_calls
			// перед отправкой SSE. Это trade-off: теряем real-time streaming,
			// но получаем корректные tool_calls + finish_reason.
			hasTools := len(req.Tools) > 0
			result, err := generateBufferedOrImages(r.Context(), actualModel, prompt, params, hasTools, visionImages)
			// 2026-06-25: snapshot для /v1/chat/completions (tools path).
			// Round 16 P2 fix (2026-07-30): closure pattern (consistent с streaming
			// path line 686). Раньше `defer recordLastPromptFromError(... err)`
			// captures err by value at defer time — fragile к refactoring (если
			// кто-то переставит defer выше err, тихо получит nil всегда).
			// Closure form гарантирует что err захватывается при return.
			defer func() {
				recordLastPromptFromError(modelName, "/v1/chat/completions", prompt, &params, hasTools, err)
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
				// Специальная обработка reload-disabled-for-tools (HTTP 413).
				// Это происходит при попытке reload для tools-запроса — бесполезно,
				// возвращаем actionable-сообщение сразу.
				if rdtErr, ok := err.(*ReloadDisabledForToolsError); ok {
					writeReloadDisabledForToolsResponse(w, rdtErr)
					return
				}
				// Специальная обработка reload-loop-limit (HTTP 413 с понятным message).
				if rllErr, ok := err.(*ReloadLoopLimitError); ok {
					writeReloadLoopLimitResponse(w, rllErr)
					return
				}
				writeCppWorkerErrorWithBridgeInfo(w, statusCode, "chat completions (tool) failed", err)
				return
			}
			// R113: схема инструментов запроса — arguments приводится к объекту.
			//
			// R216 (2026-10-21): РАЗМЫШЛЕНИЯ БОЛЬШЕ НЕ ТЕРЯЮТСЯ И НЕ ПРИКЛЕИВАЮТСЯ
			// К ОТВЕТУ. Буферизированная ветка (stream + tools) раньше отдавала
			// результат.Output целиком в delta.content: модель с reasoning
			// (gemma-4/Qwen3) писала `<think>…</think>` прямо в тело ответа, и
			// OpenWebUI показывал размышления как ответ без закрывающего маркера.
			// При наличии tool_calls размышления пропадали вовсе.
			// Теперь reasoning отделяется и уходит отдельным delta.reasoning_content.
			reasoningOut, visibleOut, hasReasoning := SplitReasoningContentFromPrompt(prompt, result.Output)
			if !hasReasoning {
				visibleOut = result.Output
			}
			calls := parseToolCallsForRequest(result.Output, req.Tools)

			// R247 (2026-10-26), этап A+F: STRICT-аргументы на /v1.
			//
			// Здесь ответ уже сгенерирован целиком и клиенту не отправлено ни
			// байта (буферизированная tools-ветка) — повтор безопасен. Клиент
			// объявил strict:true, значит аргументы обязаны соответствовать схеме;
			// иначе он ответит ошибкой инструмента и агент потратит ход.
			//
			// Повтор — с NUDGE: перечень нарушений уходит в системное сообщение,
			// иначе модель с высокой вероятностью повторит ту же форму аргументов.
			// Выключатель: CPPWORKER_STRICT_ARGS_RETRY=0.
			if len(calls) > 0 && strictArgsRetryEnabled() {
				if violations := strictToolCallViolations(calls, req.Tools); len(violations) > 0 {
					logger.Get().Warnw("strict tool args invalid (/v1), retrying generation once",
						"model", actualModel, "tool", calls[0].Function.Name,
						"violations", truncateForLog(strings.Join(violations, "; "), 300))
					retryPrompt := prompt
					if nudged, nerr := buildChatPromptWithOptions(appendToolRepairInstruction(chatMsgs, violations), actualModel, promptOpts); nerr == nil && nudged != "" {
						retryPrompt = nudged
					}
					if retried, retryErr := generateBufferedWithCancel(r.Context(), actualModel, retryPrompt, params, hasTools); retryErr == nil {
						retryCalls := parseToolCallsForRequest(retried.Output, req.Tools)
						if len(retryCalls) > 0 {
							if still := strictToolCallViolations(retryCalls, req.Tools); len(still) == 0 {
								result = retried
								calls = retryCalls
								reasoningOut, visibleOut, hasReasoning = SplitReasoningContentFromPrompt(prompt, result.Output)
								if !hasReasoning {
									visibleOut = result.Output
								}
								logger.Get().Infow("strict tool args valid after retry",
									"model", actualModel, "tool", calls[0].Function.Name)
							} else {
								logger.Get().Warnw("strict tool args still invalid after retry (/v1)",
									"model", actualModel,
									"violations", truncateForLog(strings.Join(still, "; "), 300))
							}
						}
					} else {
						logger.Get().Warnw("strict args retry failed (/v1)",
							"model", actualModel, "error", retryErr)
					}
				}
			}

			if len(calls) > 0 {
				// Round 15 (2026-07-29): default includeUsage=true (кроме явного
				// include_usage=false в stream_options). OpenAI/стандарт говорит
				// "по умолчанию false", но Cline/Roo/Aider/Continue и другие IDE-агенты
				// ВСЕГДА хотят usage в финальном чанке для отслеживания контекстного
				// окна. Без usage chunk клиент не знает prompt_tokens и не может
				// предупредить о превышении контекста — что и наблюдал пользователь
				// на локальной машине. Совместимость: клиент может явно opt-out
				// через "stream_options": {"include_usage": false}.
				includeUsage := req.StreamOptions == nil || req.StreamOptions.IncludeUsage
				writeToolCallsStream(w, r, actualModel, chatID(), time.Now().Unix(), calls, reasoningOut, prompt, includeUsage)
			} else {
				// Нет tool_calls — стримим как обычный текст.
				// R216: reasoning отделяем (см. комментарий выше): в content уходит
				// только видимый ответ, размышления — отдельным delta.reasoning_content.
				includeUsage := req.StreamOptions == nil || req.StreamOptions.IncludeUsage
				writeStaticTextStream(w, r, actualModel, chatID(), time.Now().Unix(), reasoningOut, visibleOut, prompt, includeUsage)
			}
			return
		}
		// Без tools — обычный real-time streaming
		includeUsage := req.StreamOptions == nil || req.StreamOptions.IncludeUsage
		writeOpenAIChatStream(w, r, actualModel, prompt, params, includeUsage, req.Tools)
		return
	}

	// ============================================================
	// Non-streaming
	// ============================================================
	start := time.Now()
	chatID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	hasTools := len(req.Tools) > 0
	result, err := generateBufferedOrImages(r.Context(), actualModel, prompt, params, hasTools, visionImages)
	// 2026-06-25: snapshot для /v1/chat/completions (non-stream path).
	// Round 16 P2 fix (2026-07-30): closure form (см. tools path выше).
	defer func() {
		recordLastPromptFromError(modelName, "/v1/chat/completions", prompt, &params, hasTools, err)
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
		// Специальная обработка reload-disabled-for-tools (HTTP 413).
		if rdtErr, ok := err.(*ReloadDisabledForToolsError); ok {
			writeReloadDisabledForToolsResponse(w, rdtErr)
			return
		}
		// Специальная обработка reload-loop-limit (HTTP 413 с понятным message).
		if rllErr, ok := err.(*ReloadLoopLimitError); ok {
			writeReloadLoopLimitResponse(w, rllErr)
			return
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "chat completions failed", err)
		return
	}
	durationMs := time.Since(start).Milliseconds()

	// Пытаемся распарсить tool_calls из выходного текста (если были tools)
	var hasToolCalls bool
	var toolCalls []openAIToolCall
	if len(req.Tools) > 0 {
		toolCalls = parseToolCallsForRequest(result.Output, req.Tools)
		hasToolCalls = len(toolCalls) > 0
	}

	// Строим message
	message := map[string]interface{}{
		"role": "assistant",
	}
	// R108 (2026-10-13): разбор reasoning'а — БЕЗУСЛОВНЫЙ и по тегам формата.
	//
	// Раньше он был в развилке IsReasoningEnabledForRequest(modelName) плюс два
	// почти одинаковых блока «post-hoc auto-detect»; модель вне whitelist отдавала
	// маркеры размышлений прямо в message.content, и клиент (Cline/OpenWebUI)
	// показывал их как обычный текст ответа. См. «Политика отдачи reasoning» в
	// reasoning_content.go.
	reasoningOut, visibleOut, hasReasoning := SplitReasoningContentFromPrompt(prompt, result.Output)
	if hasReasoning && backend != nil {
		// Автодетект остаётся, но только ради per-model состояния (подходящие
		// дефолты n_predict на следующих запросах) — на разбор он не влияет.
		if backend.AutoEnableReasoning(modelName) {
			logger.Get().Warnw("reasoning auto-detected (non-stream)",
				"handler", "handleV1ChatCompletions",
				"model", modelName,
				"reasoning_chars", len(reasoningOut),
				"hint", "model emits reasoning tags but is not in reasoning whitelist; "+
					"auto-enabled per-model reasoning defaults for future requests. "+
					"To pre-set, use load-with-params enableReasoning=true "+
					"or set CPPWORKER_REASONING_ARCHS env var.")
		}
	}
	if hasToolCalls {
		// R440: дамп исходящих аргументов (CPPWORKER_DEBUG_TOOL_ARGS=1).
		logOutgoingToolCalls(modelName, "/v1/chat/completions", toolCalls, result.Output)
		// tool_calls распарсены из СЫРОГО output (см. выше): вызов может стоять и
		// внутри reasoning-блока, а parseToolCallsFromOutput сам снимает фенс и
		// служебные токены.
		if hasReasoning {
			message["reasoning_content"] = reasoningOut
		}
		if strings.TrimSpace(visibleOut) != "" {
			// Проза перед вызовом инструмента — это ответ, его не выбрасываем.
			// R108 (2026-10-13): чистим хвостовые служебные токены шаблона — как
			// нативный /api/chat (см. cleanFinalContent).
			// R113 (2026-10-13): и вырезаем разметку вызова — она не ответ
			// (в теле ответа клиент видит вызов инструмента текстом).
			message["content"] = stripToolCallMarkup(cleanFinalContent(visibleOut))
		} else {
			message["content"] = nil
		}
		message["tool_calls"] = toolCalls
	} else {
		// R430 (2026-11-04): разметка вызова, которую разбор не понял, не уходит
		// клиенту в content — режем по её началу и добавляем пояснение; сырой
		// вывод сохраняем в диагностику. Тот же контракт, что на /api/chat.
		parsedForVisible := result.Output
		if hasReasoning {
			// R108 (2026-10-13): видимый ответ чистим так же, как нативный
			// /api/chat — иначе один и тот же ответ выглядит по-разному на двух
			// поверхностях («ок» против «ок<end_of_turn>»), а клиент
			// OpenAI-поверхности видит служебный токен шаблона как текст.
			parsedForVisible = visibleOut
		}
		content, unparsedMarkup := nonStreamVisibleContentR430(parsedForVisible)
		message["content"] = content
		if hasReasoning {
			message["reasoning_content"] = reasoningOut
		}
		if unparsedMarkup {
			recordUnparsedCapture(modelName, "/v1/chat/completions", "openai_nonstream_markup",
				result.Output, parsedForVisible, 0, 0, 0, req.Tools)
		}
	}

	usage := buildUsage(prompt, result.Output, modelName)
	finishReason := "stop"
	if hasToolCalls {
		finishReason = "tool_calls"
	} else {
		// R91 (2026-10-08): видимый ответ не должен быть пустым, если модель
		// что-то сгенерировала. Незакрытый reasoning-блок (живой случай gemma-4
		// при малом max_tokens: 97 токенов, все в reasoning, content пуст) раньше
		// доезжал до клиента как успешный ответ без текста. Под tool_calls
		// фолбэк НЕ применяем: там клиент получает действие, а не текст.
		//
		// R216: НЕ применяем и тогда, когда блок размышлений открыт САМИМ ПРОМПТОМ
		// (шаблон дописал `<think>`): этот текст — размышления, и копировать их в
		// content значит ровно то, на что жаловался оператор («размышления попали
		// в основной блок, тела ответа нет»). Клиент получает reasoning_content и
		// finish_reason=length — честно: ответа ещё нет, упёрлись в max_tokens.
		rc, _ := message["reasoning_content"].(string)
		cur, _ := message["content"].(string)
		// R231 (2026-10-24): в СТРИМЕ (в т.ч. buffered-стриме с tools) фолбэк не
		// применяем — reasoning_content уже доставлен клиенту, копия в content
		// даёт дубль «ответ = размышления» (живая жалоба Cline). Честный ответ:
		// content пуст, finish_reason=length. Для не-стрима R91 сохранён.
		if vis, used := ContentFromUnclosedReasoning(rc, cur); used && !PromptOpensReasoning(prompt) && !req.Stream {
			message["content"] = vis
			logger.Get().Warnw(i18n.T("cppworker.handlers_openai.unclosed_reasoning_as_content"),
				"model", modelName, "reasoning_chars", len(rc))
		}
		// R91 (2026-10-09): честная причина остановки — как в streaming-пути
		// (writeOpenAIChatStream). Здесь нет счётчика реально отданных токенов,
		// поэтому опираемся на тот же расчёт, что уходит клиенту в usage:
		// completion_tokens >= n_predict означает упор в кап (живой факт:
		// max_tokens=96 → completion_tokens=97, а клиенту уходило "stop").
		if ct, ok := usage["completion_tokens"].(int); ok && params.NPredict > 0 && ct >= params.NPredict {
			finishReason = "length"
			logger.Get().Warnw(i18n.T("cppworker.handlers_openai.truncated_by_max_tokens"),
				"model", modelName, "max_tokens", params.NPredict, "completion_tokens", ct)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
		"usage":       usage,
		"duration_ms": durationMs,
	})
}

// chatID генерирует уникальный ID чата.
func chatID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}
