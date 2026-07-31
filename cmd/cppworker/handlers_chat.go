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
)

// ============================================================
// Chat types (Ollama /api/chat)
// ============================================================

type chatMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
	// 2026-07-01: reasoning-??????? ??? reasoning-??????? (qwen3.5, deepseek-r1,
	// gemma-4 ? ?.?.). ? OpenAI-??????????? ??????? ?????????? `reasoning_content`,
	// ? Ollama ? `reasoning`. ???????????? ??? non-streaming ??????, ????? ???????
	// ????? ?????????? think-???? ? ??????? ????? ?????????.
	Reasoning string `json:"reasoning,omitempty"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	NumCtx      *int          `json:"num_ctx,omitempty"`
	Tools       []openAITool  `json:"tools,omitempty"`
}

type chatResponse struct {
	Model         string      `json:"model"`
	CreatedAt     string      `json:"created_at"`
	Message       chatMessage `json:"message"`
	Done          bool        `json:"done"`
	TotalDuration int64       `json:"total_duration,omitempty"`
}

// ============================================================
// Chat handler
// ============================================================

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

	// InFlight counter: ???????? ???????? ??????? ?? reload-??????.
	if req.Model != "" && backend.InFlight() != nil {
		backend.InFlight().Inc(req.Model)
		defer backend.InFlight().Dec(req.Model)
	}

	// ??????? ???????? ??????
	if err := ensureModelLoaded(req.Model); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return
		}
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	// Inject tool definitions into system prompt if tools are provided
	msgs := req.Messages
	if len(req.Tools) > 0 {
		msgs = augmentSystemWithTools(msgs, req.Tools)
	}

	// ???????? prompt ????? GGUF chat template (???? ????????)
	// ApplyChatTemplate ?????????? tokenizer.chat_template ?? GGUF ??????????,
	// ??? ??????????? ?????????? ??????????? ? ???????? segfault ??? ?????? ??????.
	prompt, err := buildChatPrompt(msgs, req.Model)
	if err != nil {
		logger.Get().Errorw("handleChat: failed to build prompt", "model", req.Model, "error", err)
		writeError(w, http.StatusInternalServerError, "chat prompt error: "+err.Error())
		return
	}

	logger.Get().Debugw("handleChat: assembled prompt",
		"model", req.Model,
		"messages_count", len(msgs),
		"prompt_len", len(prompt),
		"has_tools", len(req.Tools) > 0,
		"stream", req.Stream)

	genReq := generateRequest{
		Model:  req.Model,
		Prompt: prompt,
		Stream: req.Stream,
	}
	if req.Temperature != nil {
		v := *req.Temperature
		genReq.Temperature = &v
	}
	if req.MaxTokens != nil {
		genReq.MaxTokens = *req.MaxTokens
	}
	if req.NumCtx != nil && *req.NumCtx > 0 {
		genReq.NumCtx = *req.NumCtx
	}

	params := buildGenerationParams(genReq)
	// Round 6 #6: прокидываем HasTools=true чтобы увеличить резерв токенов
	// под tools-prompt в n_ctx clamp (для Ollama-пути), как и в Round 4 для
	// OpenAI handlers. Без этого tools-запросы получают 1024 токен резерва →
	// n_ctx overflow → reload-loop → 413.
	ApplyCppCtxHeaderWithOptions(r, &params, CppCtxApplyOptions{
		HasTools: len(req.Tools) > 0,
	})
	// Antiprompts ??? gemma/non-gemma
	params.Antiprompts = append(params.Antiprompts, defaultAntipromptsForModel(req.Model)...)
	logger.Get().Debugw("handleChat: antiprompts",
		"model", req.Model, "count", len(params.Antiprompts),
		"antiprompts", params.Antiprompts)

	if req.Stream {
		// ??? stream=true && tools!=[] ??????? ?????? ??? ??????, ?? ???????????
		// ?????? output ???????????. ????? ?????????? ????????? ????????? tool_calls
		// ? ?????? ????????? ???? ? done:true + message.tool_calls ??? ?????????????.
		// ?????? ????? ??? bug: cppworker ????????? non-streaming JSON ????? writeJSON,
		// ??? ?????? OpenWebUI (?? ???? NDJSON ?????).
		if len(req.Tools) > 0 {
			writeChatStreamResponseWithTools(w, r, req.Model, prompt, params)
			return
		}
		writeChatStreamResponse(w, r, req.Model, prompt, params)
		return
	}

	start := time.Now()
	hasTools := len(req.Tools) > 0
	result, err := generateWithRamFallback(req.Model, prompt, params, hasTools)
	// 2026-06-25: ?????????? snapshot ?????????? inference ??? endpoint /debug/last-prompt.
	// ??? ???????? ??????????????? ?????? "prompt_exceeds_context" ? Cline/OpenWebUI.
	// Round 16 P2 fix (2026-07-30): closure form (consistent со всеми другими endpoints).
	defer func() {
		recordLastPromptFromError(req.Model, "/api/chat", prompt, &params, hasTools, err)
	}()
	if err != nil {
		// 2026-06-24: PromptExceedsNCtxError ? HTTP 413 (??. inference.go).
		if handleInferenceError(w, err) {
			return
		}
		// ??????????? ????????? reload-loop-limit (HTTP 413 ? ???????? message).
		if rllErr, ok := err.(*ReloadLoopLimitError); ok {
			writeReloadLoopLimitResponse(w, rllErr)
			return
		}
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "chat generate failed", err)
		return
	}
	duration := time.Since(start)

	// Parse tool_calls from output if tools were provided
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
	if len(req.Tools) > 0 {
		if calls := parseToolCallsFromOutput(result.Output); len(calls) > 0 {
			resp.Message.ToolCalls = calls
			resp.Message.Content = ""
		}
	} else if IsReasoningModel(req.Model) {
		// 2026-07-01: ??? reasoning-??????? (qwen3.5/qwen3.6/deepseek-r1/gemma-4)
		// ????????? output ?? (reasoning, content) ? ?????? ? ????????? ????
		// (Ollama API ?????????? `reasoning`, OpenAI ? `reasoning_content`).
		r, c, has := SplitReasoningContent(result.Output)
		if has {
			resp.Message.Content = c
			resp.Message.Reasoning = r
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildChatPrompt ?????????? ApplyChatTemplate ?? GGUF (???????????????) ???
// ?????? ?????? (fallback). ?????????? GGUF chat template ???????????
// ?????????? ??????????? ? ???????? segfault ??? ?????? ??????.
func buildChatPrompt(msgs []chatMessage, modelName string) (string, error) {
	// Извлекаем system промпт (если есть)
	system := extractSystemFromMessages(msgs)

	// Round 11 (2026-07-28) SOFT PROMPT Injection для EnableReasoning:
	// Если config.EnableReasoning=true, добавляем "thinking instruction"
	// к system промпту. Это работает универсально для ЛЮБОЙ модели
	// (включая gemma-4-it, который не эмитит нативные <think> блоки).
	//
	// Модели с native thinking (Qwen3-thinking, DeepSeek-R1) игнорируют
	// soft prompt и продолжают эмитить <think> блоки — парсер
	// (cmd/cppworker/reasoning_content.go) корректно их извлекает.
	//
	// Native C-bridge enable_thinking требует common::chat миграцию
	// (см. docs/CHANGELOG.md v0.4.6 P.14) — отложено.
	//
	// Round 14b (2026-07-28): native enable_thinking через
	// common_chat_templates_apply (см. c/bridge/csrc/chat_thinking.cpp).
	// Если native path работает И template поддерживает thinking —
	// soft prompt НЕ нужен, template сам эмитит <think> блоки.
	// Иначе fallback на soft prompt.
	if currentConfig != nil && currentConfig.EnableReasoning {
		nativePrompt, supportsThinking, err := backend.ApplyChatTemplateWithThinking(
			modelName, "" /* override */, msgsToBridge(msgs),
			true /* enableThinking */, true /* addGenerationPrompt */,
		)
		if err == nil && nativePrompt != "" {
			if supportsThinking {
				logger.Get().Debugw("buildChatPrompt: used native enable_thinking path",
					"model", modelName, "prompt_len", len(nativePrompt))
				return nativePrompt, nil
			}
			// Native API works but template doesn't support enable_thinking.
			// Fallback: re-apply legacy + soft prompt in system.
			logger.Get().Debugw("buildChatPrompt: native enable_thinking not supported by template, using soft prompt",
				"model", modelName, "prompt_len", len(nativePrompt))
			system = injectThinkingInstruction(system)
			prompt2, err2 := backend.ApplyChatTemplate(modelName, system, msgsToBridge(msgs), true)
			if err2 == nil && prompt2 != "" {
				return prompt2, nil
			}
			// Both native + legacy GGUF template failed — fall through to manual assembly.
		} else if err != nil && err != bridge.ErrNoChatTemplate {
			logger.Get().Warnw("buildChatPrompt: ApplyChatTemplateWithThinking failed, falling back",
				"model", modelName, "error", err)
		}
	}

	// Round 11 (2026-07-28) SOFT PROMPT PATH (для моделей без native support
	// или когда EnableReasoning=false): добавляем "thinking instruction"
	// к system промпту. Работает универсально для ЛЮБОЙ instruction-tuned
	// модели (включая gemma-4-it, который не эмитит нативные <think> блоки).
	if currentConfig != nil && currentConfig.EnableReasoning {
		system = injectThinkingInstruction(system)
	}

	// Применяем chat template из GGUF
	prompt, err := backend.ApplyChatTemplate(modelName, system, msgsToBridge(msgs), true)
	if err == nil && prompt != "" {
		logger.Get().Debugw("buildChatPrompt: used GGUF chat template",
			"model", modelName, "prompt_len", len(prompt))
		return prompt, nil
	}
	if err != nil && err != bridge.ErrNoChatTemplate {
		// For gemma models (gemma4 tokenizer), chat template is not supported by bridge yet.
		// Downgrade to Debug since manual prompt assembly handles it correctly.
		if isGemmaModel(modelName) {
			logger.Get().Debugw("buildChatPrompt: GGUF chat template unavailable for gemma model, using manual fallback",
				"model", modelName, "error", err)
		} else {
			logger.Get().Warnw("buildChatPrompt: ApplyChatTemplate failed, falling back to manual",
				"model", modelName, "error", err)
		}
	}

	// Fallback: ?????? ??????
	logger.Get().Debugw("buildChatPrompt: using manual prompt assembly (no GGUF chat template)",
		"model", modelName)
	// Round 11: для fallback path — НЕ модифицируем msgs (chat template
	// здесь не применяется, только ручная сборка). Чтобы thinking
	// instruction сработал, модель должна получить system промпт через
	// buildChatPromptFromMessages. Это делает prepended system message
	// в msgs (если её ещё нет). См. injectThinkingIntoMessages.
	if currentConfig != nil && currentConfig.EnableReasoning {
		msgs = injectThinkingIntoMessages(msgs)
	}
	return buildChatPromptFromMessages(msgs, modelName), nil
}

// Round 11 (2026-07-28) Soft Prompt Injection — две helper-функции.
//
// enable_thinking native C-bridge support требует common::chat
// миграцию (отложено). В soft mode добавляем "thinking instruction"
// к system промпту, что работает для ЛЮБОЙ instruction-tuned модели.

// injectThinkingInstruction возвращает system промпт с prepended
// thinking instruction. Если исходный system пустой — возвращает
// только instruction. Если есть — instruction + "\n\n" + original.
//
// Round 17.1 fix (2026-07-31): добавлена инструкция "Wrap your reasoning
// in <think>...</think> tags". Без этого модели без нативного thinking
// (qwen3-instruct, gemma-4-it, custom fine-tunes типа
// Qwen3.6-35B-A3B-Uncensored-HauhauCS-Aggressive) эмитят reasoning
// КАК ОБЫЧНЫЙ ТЕКСТ — парсер SplitReasoningContent ищет `<think>` теги,
// не находит, кладёт reasoning в `content` вместо `reasoning_content`.
// OpenWebUI показывает reasoning как ответ — выглядит как "неполный ответ".
//
// С инструкцией "use <think> tags" модель ВЫНУЖДЕНА эмитить `<think>` блок,
// парсер split'ит корректно, OpenWebUI показывает reasoning в своём UI.
func injectThinkingInstruction(system string) string {
	const thinkingInstruction = "Before answering, use detailed step-by-step thinking. " +
		"Reason about the problem carefully, consider different angles, " +
		"show your work, then provide a clear final answer. " +
		"Structure your response: first explain your reasoning, then give the answer. " +
		"IMPORTANT: Wrap your step-by-step reasoning inside <think>...</think> tags. " +
		"Your final answer (the user-facing response) should be OUTSIDE the </think> tag."
	if system == "" {
		return thinkingInstruction
	}
	return thinkingInstruction + "\n\n" + system
}

// injectThinkingIntoMessages prepended system message с thinking
// instruction к msgs. Если system message уже есть — конкатенирует.
// Используется для fallback path (buildChatPromptFromMessages) где
// system промпт собирается руками, а не через chat template.
//
// Round 17.1 fix (2026-07-31): добавлена инструкция "Wrap your reasoning
// in <think>...</think> tags" (см. injectThinkingInstruction).
func injectThinkingIntoMessages(msgs []chatMessage) []chatMessage {
	thinking := "Before answering, use detailed step-by-step thinking. " +
		"Reason about the problem carefully, consider different angles, " +
		"show your work, then provide a clear final answer. " +
		"Structure your response: first explain your reasoning, then give the answer. " +
		"IMPORTANT: Wrap your step-by-step reasoning inside <think>...</think> tags. " +
		"Your final answer (the user-facing response) should be OUTSIDE the </think> tag."

	// Ищем существующий system message
	for i, m := range msgs {
		if m.Role == "system" {
			msgs[i].Content = thinking + "\n\n" + m.Content
			return msgs
		}
	}
	// Нет system — prepended
	systemMsg := chatMessage{Role: "system", Content: thinking}
	return append([]chatMessage{systemMsg}, msgs...)
}

// extractSystemFromMessages ????????? system ????????? ?? ?????? ?????????.
func extractSystemFromMessages(msgs []chatMessage) string {
	for _, m := range msgs {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// msgsToBridge ???????????? []chatMessage ? []bridge.ChatMessage.
// ??? assistant-????????? ? ToolCalls ??????????? tool_calls ? content,
// ??? ??? bridge.ApplyChatTemplate ?? ????? ?????????? ???? ??? tool_calls.
// ??? tool-????????? content ?????????? ??? ???? (??? ???????? ????????? ??????).
func msgsToBridge(msgs []chatMessage) []bridge.ChatMessage {
	result := make([]bridge.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		// ???? assistant ????????? ???????? tool_calls ? ??????????? ?? ? content
			// Round 6 #2: wrap tool_calls as JSON array [{...},{...}] in Hermes/Qwen
		// format, which llama.cpp chat_template recognizes as structured
		// tool_call (not plain-JSON). bridge.ChatMessage has no native ToolCalls
		// field (C-bridge limitation), so we serialize into Content.
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			tcJSON, err := json.Marshal(m.ToolCalls)
			if err == nil && len(tcJSON) > 0 {
				toolCallsBlock := "[" + string(tcJSON) + "]"
				if content != "" {
					content = content + "\n" + toolCallsBlock
				} else {
					content = toolCallsBlock
				}
			}
		}

		result = append(result, bridge.ChatMessage{
			Role:    m.Role,
			Content: content,
		})
	}
	return result
}

// buildChatPromptFromMessages ? ???????? prompt ?? ??????? ?????????.
// ???????????? tool-????????? ? assistant-????????? ? ToolCalls ??? function calling.
func buildChatPromptFromMessages(msgs []chatMessage, modelName string) string {
	var promptBuilder strings.Builder
	isGemma := isGemmaModel(modelName)

	for _, msg := range msgs {
		switch msg.Role {
		case "system":
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>system\n")
			} else {
				promptBuilder.WriteString("<|system|>\n")
			}
			promptBuilder.WriteString(msg.Content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		case "user":
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>user\n")
			} else {
				promptBuilder.WriteString("<|user|>\n")
			}
			promptBuilder.WriteString(msg.Content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		case "assistant":
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>model\n")
			} else {
				promptBuilder.WriteString("<|assistant|>\n")
			}
			// ???? ???? tool_calls, ??????????? ?? ? content
			// Round 6 #2: wrap tool_calls as JSON array in Hermes/Qwen format
			// (see msgsToBridge comment for rationale).
			content := msg.Content
			if len(msg.ToolCalls) > 0 {
				tcJSON, err := json.Marshal(msg.ToolCalls)
				if err == nil && len(tcJSON) > 0 {
					toolCallsBlock := "[" + string(tcJSON) + "]"
					if content != "" {
						content = content + "\n" + toolCallsBlock
					} else {
						content = toolCallsBlock
					}
				}
			}
			promptBuilder.WriteString(content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		case "tool":
			// ??????????? tool-?????????: ??? ??????????? : ?????????
			content := msg.Content
			if msg.Name != "" {
				content = "tool_call_result(" + msg.Name + "): " + content
			} else if msg.ToolCallID != "" {
				content = "tool_call_result(" + msg.ToolCallID + "): " + content
			}
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>tool\n")
			} else {
				promptBuilder.WriteString("<|tool|>\n")
			}
			promptBuilder.WriteString(content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		}
	}
	// ????????? ?????? ??? ?????? ??????????
	if isGemma {
		promptBuilder.WriteString("<start_of_turn>model\n")
	} else {
		promptBuilder.WriteString("<|assistant|>\n")
	}

	return promptBuilder.String()
}

// writeChatStreamResponse ? streaming ????? ? ??????? NDJSON ??? /api/chat.
func writeChatStreamResponse(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
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
	var outputBuf strings.Builder
	start := time.Now()
	createdAt := time.Now().UTC().Format(time.RFC3339)

	// 2026-06-25: ?????? snapshot ????? streaming (????? ??????????? ?????????).
	var streamErr error
	defer func() {
		// Round 5 Fix 3: пробрасываем r для детекции client_disconnect.
		recordLastPromptFromErrorWithContext(modelName, "/api/chat", prompt, &params, false, streamErr, r)
	}()

	// 2026-07-01: reasoning-?????? (??? reasoning-???????).
	rcParser := NewReasoningStreamState()
	rcIsReasoning := IsReasoningModel(modelName)

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		outputBuf.WriteString(token)

		// 2026-07-01: ??? reasoning-??????? ????? ????? ?? (reasoning, content) ?
		// ?????? ??????????? NDJSON-????? ? message.reasoning / message.content.
		if rcIsReasoning {
			reasoningDelta, contentDelta := rcParser.Feed(token)
			if reasoningDelta != "" {
				rcChunk := map[string]interface{}{
					"model":      modelName,
					"created_at": createdAt,
					"message": map[string]interface{}{
						"role":      "assistant",
						"reasoning": reasoningDelta,
					},
					"done": false,
				}
				rcJSON, _ := json.Marshal(rcChunk)
				fmt.Fprintf(w, "%s\n", rcJSON)
				flusher.Flush()
			}
			if contentDelta != "" {
				ccChunk := map[string]interface{}{
					"model":      modelName,
					"created_at": createdAt,
					"message": map[string]string{
						"role":    "assistant",
						"content": contentDelta,
					},
					"done": false,
				}
				ccJSON, _ := json.Marshal(ccChunk)
				fmt.Fprintf(w, "%s\n", ccJSON)
				flusher.Flush()
			}
			return true
		}

		chunk := map[string]interface{}{
			"model":      modelName,
			"created_at": createdAt,
			"message": map[string]string{
				"role":    "assistant",
				"content": token,
			},
			"done": false,
		}
		jsonData, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", jsonData)
		flusher.Flush()
		return true
	}

	// /api/chat endpoint: tools ?? ?????????????? (Ollama-???), reload ???????? ??? n_ctx overflow.
	streamErr = generateStreamWithRamFallback(modelName, prompt, params, callback, false)
	if streamErr != nil {
		maybeRestartOnMemorySlotError(streamErr, modelName)
		// ??????????? ????????? reload-loop-limit (HTTP 413 ? NDJSON-?????).
		if rllErr, ok := streamErr.(*ReloadLoopLimitError); ok {
			errChunk := map[string]interface{}{
				"model":           modelName,
				"created_at":      createdAt,
				"message":         map[string]string{"role": "assistant", "content": ""},
				"done":            true,
				"done_reason":     "error",
				"error":           rllErr.Error(),
				"code":            "reload_loop_limit",
				"attempts":        rllErr.Count,
				"elapsed_seconds": rllErr.Elapsed.Seconds(),
				"http_status":     http.StatusRequestEntityTooLarge,
			}
			errJSON, _ := json.Marshal(errChunk)
			w.Header().Set("X-CppWorker-Error", "reload_loop_limit")
			w.Header().Set("X-HTTP-Status", "413")
			fmt.Fprintf(w, "%s\n", errJSON)
			flusher.Flush()
			return
		}
		errChunk := map[string]interface{}{
			"model":      modelName,
			"created_at": createdAt,
			"message":    map[string]string{"role": "assistant", "content": ""},
			"done":       true,
			"error":      streamErr.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}

	fullOutput := outputBuf.String()
	duration := time.Since(start)

	// 2026-06-24: ???????? ?? ?????? ????? ????? cleanFinalContent (gemma antiprompt).
	if strings.TrimSpace(cleanFinalContent(fullOutput)) == "" {
		errChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"message":     map[string]string{"role": "assistant", "content": ""},
			"done":        true,
			"done_reason": "error",
			"error":       "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, prompt too long, or model issue.",
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}

	cleanedOutput := cleanFinalContent(fullOutput)
	doneChunk := map[string]interface{}{
		"model":          modelName,
		"created_at":     createdAt,
		"message":        map[string]string{"role": "assistant", "content": cleanedOutput},
		"done":           true,
		"done_reason":     "stop",
		"total_duration": duration.Nanoseconds(),
		"eval_count":     0,
		"eval_duration":  duration.Nanoseconds(),
	}
	doneJSON, _ := json.Marshal(doneChunk)
	fmt.Fprintf(w, "%s\n", doneJSON)
	flusher.Flush()
}

// writeChatStreamResponseWithTools ? streaming-????? ? ?????????? tool calling.
//
// ? ??????? ?? writeChatStreamResponse, ??? ???????:
//  1. ??????? ?????? ??? ?????? ????? generateStreamWithRamFallback (real-time feedback).
//  2. ??????????? ??????????? ?????? output ? ?????.
//  3. ????? ?????????? ????????? ????????? tool_calls ????? parseToolCallsFromOutput.
//  4. ???? ???? tool_calls ? ??????? content ? ?????? ????????? ???? ? done:true,
//     done_reason:"tool_calls" ? message.tool_calls.
//  5. ???? ??? ? ?????? ??????? done:true ????.
//
// ??? ?????? ???, ??? ??????? OpenWebUI (stream=true + tools) ??????? non-streaming
// JSON-????? ? ?? ??? ????????? ??? ?????????? (????? "???????? ???????????" ??? ??????).
func writeChatStreamResponseWithTools(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
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
	// Round 6 Fix 5: do NOT flush headers immediately — buffer the response.
	// Previously every prose token was sent on the wire and then the final
	// chunk tried to set content="" with tool_calls — clients got garbage
	// (raw JSON in chat). New approach: callback ONLY writes to outputBuf,
	// no flushes. After inference, parse tool_calls and emit one final
	// chunk (either tool_calls or content). Heartbeat goroutine keeps
	// the connection alive during long generations.
	flusher.Flush()
	ctx := r.Context()
	start := time.Now()
	createdAt := time.Now().UTC().Format(time.RFC3339)

	// Buffer full output for final tool_calls detection.
	var outputBuf strings.Builder

	// Round 6 Fix 5: heartbeat goroutine prevents idle-timeout during
	// long generations. SSE comment ": keepalive\n\n" is ignored by
	// Ollama/OpenWebUI clients but prevents TCP idle disconnect.
	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}()
	defer func() {
		close(heartbeatStop)
		select {
		case <-heartbeatDone:
		case <-time.After(50 * time.Millisecond):
		}
	}()

	// Snapshot for chat streaming (debug endpoint).
	var streamErr error
	defer func() {
		recordLastPromptFromErrorWithContext(modelName, "/api/chat", prompt, &params, true, streamErr, r)
	}()

	// Round 6 Fix 5: callback ONLY buffers. Prose is NEVER sent to the wire
	// because it might be part of a tool_call (Gemma-4 / Qwen3 stream prose
	// before tool_call). Without buffering, clients see raw JSON in chat.
	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		outputBuf.WriteString(token)
		return true
	}

	// /api/chat streaming with tools: full buffer, single final response.
	streamErr = generateStreamWithRamFallback(modelName, prompt, params, callback, false)

	// ???? ?? ????? streaming ????????? ?????? ? ?????? ????????? chunk ? error,
	// ??? ? writeChatStreamResponse. Tool calls ? ???? ?????? ?? ???????????.
	if streamErr != nil {
		maybeRestartOnMemorySlotError(streamErr, modelName)
		// ??????????? ????????? reload-loop-limit: HTTP-?????? 413 ? NDJSON-?????
		// ? error+code+model+attempts. OpenWebUI ?????? error ? ???????? ??????
		// ? ???????? ?????????? (?????? ???????????? ????? unload+reload).
		if rllErr, ok := streamErr.(*ReloadLoopLimitError); ok {
			errChunk := map[string]interface{}{
				"model":           modelName,
				"created_at":      createdAt,
				"message":         map[string]string{"role": "assistant", "content": ""},
				"done":            true,
				"done_reason":     "error",
				"error":           rllErr.Error(),
				"code":            "reload_loop_limit",
				"attempts":        rllErr.Count,
				"elapsed_seconds": rllErr.Elapsed.Seconds(),
				"http_status":     http.StatusRequestEntityTooLarge,
			}
			errJSON, _ := json.Marshal(errChunk)
			// ????? ?????? HTTP-?????? ? response (best-effort: ???? streaming ??? ????? ? http.status ????? ?????????????? ????????, ?? ??? ? NDJSON ??? ????? ????? ???????).
			w.Header().Set("X-CppWorker-Error", "reload_loop_limit")
			w.Header().Set("X-HTTP-Status", "413")
			fmt.Fprintf(w, "%s\n", errJSON)
			flusher.Flush()
			return
		}
		errChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"message":     map[string]string{"role": "assistant", "content": ""},
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
	duration := time.Since(start)

	// ???????? ?????????? tool_calls ?? ?????????? ??????.
	toolCalls := parseToolCallsFromOutput(fullOutput)

	// 2026-06-24: ???????? ?? ?????? ????? ????? cleanFinalContent (gemma antiprompt).
	if len(toolCalls) == 0 && strings.TrimSpace(cleanFinalContent(fullOutput)) == "" {
		errChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"message":     map[string]string{"role": "assistant", "content": ""},
			"done":        true,
			"done_reason": "error",
			"error":       "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, prompt too long, or model issue.",
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}

	finalChunk := map[string]interface{}{
		"model":          modelName,
		"created_at":     createdAt,
		"message":        map[string]interface{}{"role": "assistant"},
		"done":           true,
		"total_duration": duration.Nanoseconds(),
		"eval_count":     0,
		"eval_duration":  duration.Nanoseconds(),
	}

	if len(toolCalls) > 0 {
		// Tool calls ?????????? ? ?????? message.tool_calls ? ??????? content.
		// ??? ????????: OpenWebUI ??? `done_reason: "tool_calls"` ???????
		// ??? content ??????, ? tool_calls ???????? ?????? ???????.
		msg := finalChunk["message"].(map[string]interface{})
		msg["content"] = ""
		msg["tool_calls"] = toolCalls
		finalChunk["done_reason"] = "tool_calls"
		logger.Get().Infow("writeChatStreamResponseWithTools: detected tool_calls in streamed output",
			"model", modelName, "tool_calls_count", len(toolCalls))
	} else {
		// ??? tool calls ? ??????? ????????? ?????. ????????: ??????????
		// ?????? output ? ????????? ?????, ????? OpenWebUI ????? ??????
		// content ? ????????? ?????? ("???? ???????? ??????, ?????? ???").
		//
		// ?????? ????? ??? bug: ????????? ???? ???? content: "", ????
		// ????? ??? ??? ????????? ? streaming chunks. OpenWebUI ?????
		// done:true ????? content ?? ?????????? ????? (?? ??????????
		// ?? streaming chunks), ??????? ????? ?????? ?????.
		//
		// ?????? ??????????? ???????: ??????? ????????? ????????? ??????,
		// ??????? ?????? ????? ??????? (</tool_call>, [TOOL_CALLS], ? ?.?.).
		msg := finalChunk["message"].(map[string]interface{})
		msg["content"] = cleanFinalContent(fullOutput)
		finalChunk["done_reason"] = "stop"
		logger.Get().Debugw("writeChatStreamResponseWithTools: text response (no tool_calls)",
			"model", modelName, "content_len", len(msg["content"].(string)))
	}

	doneJSON, _ := json.Marshal(finalChunk)
	fmt.Fprintf(w, "%s\n", doneJSON)
	flusher.Flush()
}

