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
// Chat types (Ollama /api/chat)
// ============================================================

type chatMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
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
		// For streaming with tools: buffer full response, then check for tool_calls
		if len(req.Tools) > 0 {
				toolStart := time.Now()
			result, err := generateWithRamFallback(req.Model, prompt, params)
			if err != nil {
				statusCode := http.StatusInternalServerError
				if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
					statusCode = http.StatusBadRequest
				}
				writeCppWorkerErrorWithBridgeInfo(w, statusCode, "chat generate (tool) failed", err)
				return
			}
			duration := time.Since(toolStart)
			toolCalls := parseToolCallsFromOutput(result.Output)
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
			if len(toolCalls) > 0 {
				resp.Message.ToolCalls = toolCalls
				resp.Message.Content = ""
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		writeChatStreamResponse(w, r, req.Model, prompt, params)
		return
	}

	start := time.Now()
	result, err := generateWithRamFallback(req.Model, prompt, params)
	if err != nil {
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
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()
	ctx := r.Context()
	start := time.Now()
	createdAt := time.Now().UTC().Format(time.RFC3339)

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
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

	if err := generateStreamWithRamFallback(modelName, prompt, params, callback); err != nil {
		maybeRestartOnMemorySlotError(err, modelName)
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

	duration := time.Since(start)
	doneChunk := map[string]interface{}{
		"model":      modelName,
		"created_at": createdAt,
		"message":    map[string]string{"role": "assistant", "content": ""},
		"done":       true,
		"total_duration": duration.Nanoseconds(),
		"eval_count":     0,
		"eval_duration":  duration.Nanoseconds(),
	}
	doneJSON, _ := json.Marshal(doneChunk)
	fmt.Fprintf(w, "%s\n", doneJSON)
	flusher.Flush()
}
