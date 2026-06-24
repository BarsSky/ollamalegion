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

	// Ленивая загрузка модели
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

	// Собираем prompt через GGUF chat template (если доступен)
	// ApplyChatTemplate использует tokenizer.chat_template из GGUF метаданных,
	// что гарантирует корректную токенизацию и избегает segfault при ручной сборке.
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
		genReq.Temperature = *req.Temperature
	}
	if req.MaxTokens != nil {
		genReq.MaxTokens = *req.MaxTokens
	}
	if req.NumCtx != nil && *req.NumCtx > 0 {
		genReq.NumCtx = *req.NumCtx
	}

	params := buildGenerationParams(genReq)
	applyCppCtxHeader(r, &params)
	// Antiprompts для gemma/non-gemma
	params.Antiprompts = append(params.Antiprompts, defaultAntipromptsForModel(req.Model)...)
	logger.Get().Debugw("handleChat: antiprompts",
		"model", req.Model, "count", len(params.Antiprompts),
		"antiprompts", params.Antiprompts)

	if req.Stream {
		// При stream=true && tools!=[] стримим токены как обычно, но накапливаем
		// полный output параллельно. После завершения генерации проверяем tool_calls
		// и эмитим финальный чанк с done:true + message.tool_calls при необходимости.
		// Раньше здесь был bug: cppworker возвращал non-streaming JSON через writeJSON,
		// что ломало OpenWebUI (он ждал NDJSON чанки).
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
	if err != nil {
		// 2026-06-24: PromptExceedsNCtxError → HTTP 413 (см. inference.go).
		if handleInferenceError(w, err) {
			return
		}
		// Специальная обработка reload-loop-limit (HTTP 413 с понятным message).
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
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildChatPrompt использует ApplyChatTemplate из GGUF (предпочтительно) или
// ручную сборку (fallback). Применение GGUF chat template гарантирует
// корректную токенизацию и избегает segfault при ручной сборке.
func buildChatPrompt(msgs []chatMessage, modelName string) (string, error) {
	// Извлекаем system сообщение (если есть)
	system := extractSystemFromMessages(msgs)

	// Сначала пробуем ApplyChatTemplate из GGUF
	prompt, err := backend.ApplyChatTemplate(modelName, system, msgsToBridge(msgs), true)
	if err == nil && prompt != "" {
		logger.Get().Debugw("buildChatPrompt: used GGUF chat template",
			"model", modelName, "prompt_len", len(prompt))
		return prompt, nil
	}
	if err != nil && err != bridge.ErrNoChatTemplate {
		logger.Get().Warnw("buildChatPrompt: ApplyChatTemplate failed, falling back to manual",
			"model", modelName, "error", err)
	}

	// Fallback: ручная сборка
	logger.Get().Debugw("buildChatPrompt: using manual prompt assembly (no GGUF chat template)",
		"model", modelName)
	return buildChatPromptFromMessages(msgs, modelName), nil
}

// extractSystemFromMessages извлекает system сообщение из списка сообщений.
func extractSystemFromMessages(msgs []chatMessage) string {
	for _, m := range msgs {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// msgsToBridge конвертирует []chatMessage в []bridge.ChatMessage.
// Для assistant-сообщений с ToolCalls сериализует tool_calls в content,
// так как bridge.ApplyChatTemplate не имеет отдельного поля для tool_calls.
// Для tool-сообщений content передаётся как есть (уже содержит результат вызова).
func msgsToBridge(msgs []chatMessage) []bridge.ChatMessage {
	result := make([]bridge.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		// Если assistant сообщение содержит tool_calls — сериализуем их в content
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			tcJSON, err := json.Marshal(m.ToolCalls)
			if err == nil {
				if content != "" {
					content += "\n" + string(tcJSON)
				} else {
					content = string(tcJSON)
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

// buildChatPromptFromMessages — собирает prompt из массива сообщений.
// Поддерживает tool-сообщения и assistant-сообщения с ToolCalls для function calling.
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
			// Если есть tool_calls, сериализуем их в content
			content := msg.Content
			if len(msg.ToolCalls) > 0 {
				tcJSON, err := json.Marshal(msg.ToolCalls)
				if err == nil {
					if content != "" {
						content += "\n" + string(tcJSON)
					} else {
						content = string(tcJSON)
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
			// Форматируем tool-сообщение: имя инструмента : результат
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
	// Добавляем маркер для ответа ассистента
	if isGemma {
		promptBuilder.WriteString("<start_of_turn>model\n")
	} else {
		promptBuilder.WriteString("<|assistant|>\n")
	}

	return promptBuilder.String()
}

// writeChatStreamResponse — streaming ответ в формате NDJSON для /api/chat.
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

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		outputBuf.WriteString(token)
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

	// /api/chat endpoint: tools НЕ поддерживаются (Ollama-чат), reload разрешён при n_ctx overflow.
	if err := generateStreamWithRamFallback(modelName, prompt, params, callback, false); err != nil {
		maybeRestartOnMemorySlotError(err, modelName)
		// Специальная обработка reload-loop-limit (HTTP 413 в NDJSON-чанке).
		if rllErr, ok := err.(*ReloadLoopLimitError); ok {
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
			"error":      err.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}

	fullOutput := outputBuf.String()
	duration := time.Since(start)

	// 2026-06-24: проверка на пустой ответ после cleanFinalContent (gemma antiprompt).
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

// writeChatStreamResponseWithTools — streaming-ответ с поддержкой tool calling.
//
// В отличие от writeChatStreamResponse, эта функция:
//  1. Стримит токены как обычно через generateStreamWithRamFallback (real-time feedback).
//  2. Параллельно накапливает полный output в буфер.
//  3. После завершения генерации проверяет tool_calls через parseToolCallsFromOutput.
//  4. Если есть tool_calls — очищает content и эмитит финальный чанк с done:true,
//     done_reason:"tool_calls" и message.tool_calls.
//  5. Если нет — эмитит обычный done:true чанк.
//
// Это решает баг, при котором OpenWebUI (stream=true + tools) получал non-streaming
// JSON-ответ и не мог корректно его обработать (видел "источник использован" без ответа).
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
	flusher.Flush()
	ctx := r.Context()
	start := time.Now()
	createdAt := time.Now().UTC().Format(time.RFC3339)

	// Буферизация полного output для последующего парсинга tool_calls.
	// Используем strings.Builder + мьютекс для безопасности из разных goroutine,
	// хотя callback вызывается синхронно из generateStreamWithRamFallback.
	var outputBuf strings.Builder

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		// Накапливаем output для последующего парсинга tool_calls.
		outputBuf.WriteString(token)

		// Эмитим обычный streaming chunk с content.
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

	// /api/chat streaming fallback: tools не поддерживаются.
	streamErr := generateStreamWithRamFallback(modelName, prompt, params, callback, false)

	// Если во время streaming произошла ошибка — отдаём финальный chunk с error,
	// как в writeChatStreamResponse. Tool calls в этом случае не анализируем.
	if streamErr != nil {
		maybeRestartOnMemorySlotError(streamErr, modelName)
		// Специальная обработка reload-loop-limit: HTTP-статус 413 в NDJSON-чанке
		// с error+code+model+attempts. OpenWebUI увидит error и завершит сессию
		// с понятным сообщением (вместо бесконечного цикла unload+reload).
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
			// Также ставим HTTP-статус в response (best-effort: если streaming уже начат — http.status будет проигнорирован клиентом, но код в NDJSON всё равно будет передан).
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

	// Пытаемся распарсить tool_calls из финального текста.
	toolCalls := parseToolCallsFromOutput(fullOutput)

	// 2026-06-24: проверка на пустой ответ после cleanFinalContent (gemma antiprompt).
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
		// Tool calls обнаружены — эмитим message.tool_calls и очищаем content.
		// Это критично: OpenWebUI при `done_reason: "tool_calls"` ожидает
		// что content пустой, а tool_calls содержит вызовы функций.
		msg := finalChunk["message"].(map[string]interface{})
		msg["content"] = ""
		msg["tool_calls"] = toolCalls
		finalChunk["done_reason"] = "tool_calls"
		logger.Get().Infow("writeChatStreamResponseWithTools: detected tool_calls in streamed output",
			"model", modelName, "tool_calls_count", len(toolCalls))
	} else {
		// Нет tool calls — обычный текстовый ответ. КРИТИЧНО: возвращаем
		// полный output в финальном чанке, иначе OpenWebUI видит пустой
		// content и завершает сессию ("один источник найден, ответа нет").
		//
		// Раньше здесь был bug: финальный чанк имел content: "", хотя
		// текст уже был отправлен в streaming chunks. OpenWebUI после
		// done:true берёт content из финального чанка (не агрегирует
		// из streaming chunks), поэтому видел пустой ответ.
		//
		// Делаем минимальную очистку: удаляем хвостовые служебные токены,
		// которые модель могла эмитить (</tool_call>, [TOOL_CALLS], и т.п.).
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
