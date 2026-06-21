// handlers_openai.go — OpenAI-compatible /v1/ endpoints (/v1/chat/completions,
// /v1/completions, /v1/embeddings, /v1/models).
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// OpenAI-compatible /v1/ handlers
// ============================================================

// openAICompletionRequest — структура запроса OpenAI /v1/completions
type openAICompletionRequest struct {
	Model            string   `json:"model"`
	Prompt           string   `json:"prompt"`
	Suffix           string   `json:"suffix,omitempty"`
	MaxTokens        int      `json:"max_tokens,omitempty"`
	Temperature      float64  `json:"temperature,omitempty"`
	TopP             float64  `json:"top_p,omitempty"`
	N                int      `json:"n,omitempty"`
	Stream           bool     `json:"stream,omitempty"`
	Echo             bool     `json:"echo,omitempty"`
	Stop             []string `json:"stop,omitempty"`
	PresencePenalty  float64  `json:"presence_penalty,omitempty"`
	FrequencyPenalty float64  `json:"frequency_penalty,omitempty"`
	Seed             int      `json:"seed,omitempty"`
	// NumCtx — per-request переопределение n_ctx (OpenAI-совместимый).
	// 0 = использовать n_ctx модели. > 0 → C-bridge pre-flight check.
	// > effective n_ctx → C-bridge вернёт informative ошибку с предложением
	// перезагрузить модель через /api/models/reload.
	NumCtx int `json:"num_ctx,omitempty"`
}

// openAIChatCompletionRequest — структура запроса OpenAI /v1/chat/completions
type openAIChatCompletionRequest struct {
	Model       string              `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature float64             `json:"temperature,omitempty"`
	TopP        float64             `json:"top_p,omitempty"`
	N           int                 `json:"n,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
	Stop        []string            `json:"stop,omitempty"`
	Seed        int                 `json:"seed,omitempty"`
	// NumCtx — per-request переопределение n_ctx (OpenAI-совместимый).
	// 0 = использовать n_ctx модели. > 0 → C-bridge pre-flight check.
	NumCtx int `json:"num_ctx,omitempty"`
	// Tools — определения инструментов для function calling (OpenAI / Ollama совместимый формат).
	Tools []openAITool `json:"tools,omitempty"`
	// ToolChoice — управление выбором инструмента ("auto", "none", "required", или {"type":"function","function":{"name":"..."}}).
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
}

type openAIChatMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// applyCppCtxHeader — backwards-compat алиас для ApplyCppCtxHeader из nctx_clamp.go.
// TODO(refactoring): заменить все вызовы на ApplyCppCtxHeader и удалить этот alias.
func applyCppCtxHeader(r *http.Request, params *bridge.GenerationParams) {
	ApplyCppCtxHeader(r, params)
}

// openAIToChatMessage converts an openAIChatMessage slice to chatMessage slice
// for use with the canonical buildChatPrompt from handlers_chat.go.
// Сохраняет tool_calls, tool_call_id, name для поддержки function calling.
func openAIToChatMessage(msgs []openAIChatMessage) []chatMessage {
	result := make([]chatMessage, 0, len(msgs))
	for _, m := range msgs {
		cm := chatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		if len(m.ToolCalls) > 0 {
			cm.ToolCalls = make([]openAIToolCall, len(m.ToolCalls))
			copy(cm.ToolCalls, m.ToolCalls)
		}
		result = append(result, cm)
	}
	return result
}

// buildNaiveChatPrompt — fallback when GGUF has no chat template (or bridge_apply_chat_template fails).
// Строит простой chat-формат, совместимый с gemma-style instruction-tuned моделями.
// Если в имени модели встречается "gemma" — используется формат <start_of_turn>user/model<end_of_turn>,
// иначе — формат <|user|>...<|assistant|>, оба совместимы с большинством chat-моделей llama.cpp.
// Поддерживает tool-сообщения и assistant-сообщения с tool_calls.
func buildNaiveChatPrompt(msgs []openAIChatMessage, modelName string) string {
	isGemma := strings.Contains(strings.ToLower(modelName), "gemma")
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>system\n%s<end_of_turn>\n", m.Content)
			} else {
				fmt.Fprintf(&sb, "<|system|>\n%s<|end|>\n", m.Content)
			}
		case "user":
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>user\n%s<end_of_turn>\n", m.Content)
			} else {
				fmt.Fprintf(&sb, "<|user|>\n%s<|end|>\n", m.Content)
			}
		case "assistant":
			content := m.Content
			// Если есть tool_calls, сериализуем их в content
			if len(m.ToolCalls) > 0 {
				tcJSON, err := json.Marshal(m.ToolCalls)
				if err == nil {
					if content != "" {
						content += "\n" + string(tcJSON)
					} else {
						content = string(tcJSON)
					}
				}
			}
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>model\n%s<end_of_turn>\n", content)
			} else {
				fmt.Fprintf(&sb, "<|assistant|>\n%s<|end|>\n", content)
			}
		case "tool":
			content := m.Content
			if m.Name != "" {
				content = "tool_call_result(" + m.Name + "): " + content
			} else if m.ToolCallID != "" {
				content = "tool_call_result(" + m.ToolCallID + "): " + content
			}
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>tool\n%s<end_of_turn>\n", content)
			} else {
				fmt.Fprintf(&sb, "<|tool|>\n%s<|end|>\n", content)
			}
		}
	}
	if isGemma {
		sb.WriteString("<start_of_turn>model\n")
	} else {
		sb.WriteString("<|assistant|>\n")
	}
	return sb.String()
}

// augmentSystemWithTools injects tool definitions into the system message.
// Если system-сообщения нет — создаёт новое.
func augmentSystemWithTools(msgs []chatMessage, tools []openAITool) []chatMessage {
	if len(tools) == 0 {
		return msgs
	}
	toolsPrompt := buildToolsSystemPrompt(tools)
	if toolsPrompt == "" {
		return msgs
	}
	// Ищем существующее system-сообщение и дополняем его
	for i := range msgs {
		if msgs[i].Role == "system" {
			msgs[i].Content = msgs[i].Content + "\n\n" + toolsPrompt
			return msgs
		}
	}
	// Нет system-сообщения — создаём новое в начале
	result := make([]chatMessage, 0, len(msgs)+1)
	result = append(result, chatMessage{Role: "system", Content: toolsPrompt})
	result = append(result, msgs...)
	return result
}

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

	if err := ensureModelLoaded(req.Model); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return
		}
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	// Конвертируем openAIChatMessage → chatMessage и инжектим определения tools
	chatMsgs := openAIToChatMessage(req.Messages)
	if len(req.Tools) > 0 {
		chatMsgs = augmentSystemWithTools(chatMsgs, req.Tools)
	}

	// Собираем промпт из сообщений с применением chat template из GGUF.
	prompt, err := buildChatPrompt(chatMsgs, req.Model)
	var usedNaive bool
	if err != nil {
		logger.Get().Warnw("chat template bridge failed, falling back to naive prompt",
			"model", req.Model, "error", err)
		prompt = buildNaiveChatPrompt(req.Messages, req.Model)
		if prompt == "" {
			writeError(w, http.StatusInternalServerError, "build prompt failed: "+err.Error())
			return
		}
		usedNaive = true
	}

	params := bridge.DefaultGenerationParams()
	if req.Temperature > 0 {
		params.Temperature = float32(req.Temperature)
	}
	if req.TopP > 0 {
		params.TopP = float32(req.TopP)
	}
	if req.MaxTokens > 0 {
		params.NPredict = req.MaxTokens
	}
	if req.Seed != 0 {
		params.Seed = req.Seed
	}
	if req.NumCtx > 0 {
		params.NCtxOverride = req.NumCtx
	}
	applyCppCtxHeader(r, &params)

	// Antiprompts: дефолтные для формата промпта + пользовательские req.Stop.
	defaultAP := defaultAntipromptsForModel(req.Model)
	if len(defaultAP) > 0 {
		params.Antiprompts = append(params.Antiprompts, defaultAP...)
	}
	if !usedNaive {
		if isGemmaModel(req.Model) {
			params.Antiprompts = append(params.Antiprompts, []string{"<end_of_turn>", "<start_of_turn>user"}...)
		}
	}
	for _, s := range req.Stop {
		if s != "" {
			params.Antiprompts = append(params.Antiprompts, s)
		}
	}
	logger.Get().Debugw("handleV1ChatCompletions: antiprompts",
		"model", req.Model, "used_naive", usedNaive,
		"antiprompts_count", len(params.Antiprompts),
		"antiprompts", params.Antiprompts)

	// ============================================================
	// Streaming
	// ============================================================
	if req.Stream {
		if len(req.Tools) > 0 {
			// Если есть tools — буферизируем полный ответ, чтобы распарсить tool_calls
			// перед отправкой SSE. Это trade-off: теряем real-time streaming,
			// но получаем корректные tool_calls + finish_reason.
			result, err := generateWithRamFallback(req.Model, prompt, params)
			if err != nil {
				statusCode := http.StatusInternalServerError
				if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
					statusCode = http.StatusBadRequest
				}
				writeCppWorkerErrorWithBridgeInfo(w, statusCode, "chat completions (tool) failed", err)
				return
			}
			if calls := parseToolCallsFromOutput(result.Output); len(calls) > 0 {
				writeToolCallsStream(w, req.Model, chatID(), time.Now().Unix(), calls)
			} else {
				// Нет tool_calls — стримим как обычный текст
				writeStaticTextStream(w, req.Model, chatID(), time.Now().Unix(), result.Output)
			}
			return
		}
		// Без tools — обычный real-time streaming
		writeOpenAIChatStream(w, r, req.Model, prompt, params)
		return
	}

	// ============================================================
	// Non-streaming
	// ============================================================
	start := time.Now()
	chatID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	result, err := generateWithRamFallback(req.Model, prompt, params)
	if err != nil {
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "chat completions failed", err)
		return
	}
	durationMs := time.Since(start).Milliseconds()

	// Пытаемся распарсить tool_calls из выходного текста (если были tools)
	var hasToolCalls bool
	var toolCalls []openAIToolCall
	if len(req.Tools) > 0 {
		toolCalls = parseToolCallsFromOutput(result.Output)
		hasToolCalls = len(toolCalls) > 0
	}

	// Строим message
	message := map[string]interface{}{
		"role": "assistant",
	}
	if hasToolCalls {
		message["content"] = nil
		message["tool_calls"] = toolCalls
	} else {
		message["content"] = result.Output
	}

	finishReason := "stop"
	if hasToolCalls {
		finishReason = "tool_calls"
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion",
		"created": created,
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
		"duration_ms": durationMs,
	})
}

// chatID генерирует уникальный ID чата.
func chatID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}

// writeToolCallsStream отправляет SSE-поток с tool_calls (без content).
// Формат: один chunk с delta.tool_calls, затем chunk с finish_reason="tool_calls".
func writeToolCallsStream(w http.ResponseWriter, modelName, chatID string, created int64, calls []openAIToolCall) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Chunk 1: роль ассистента (без content, с tool_calls)
	firstChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"role":       "assistant",
					"content":    nil,
					"tool_calls": calls,
				},
				"finish_reason": nil,
			},
		},
	}
	jsonData, _ := json.Marshal(firstChunk)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	flusher.Flush()

	// Chunk 2: завершение с finish_reason="tool_calls"
	stopChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "tool_calls",
			},
		},
	}
	stopJSON, _ := json.Marshal(stopChunk)
	fmt.Fprintf(w, "data: %s\n\n", stopJSON)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// writeStaticTextStream отправляет SSE-поток с фиксированным текстом
// (используется когда tools запросили буферизированный ответ).
func writeStaticTextStream(w http.ResponseWriter, modelName, chatID string, created int64, content string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Отправляем весь контент одним chunk
	chunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]string{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": nil,
			},
		},
	}
	jsonData, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	flusher.Flush()

	// Сигнал завершения
	stopChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "stop",
			},
		},
	}
	stopJSON, _ := json.Marshal(stopChunk)
	fmt.Fprintf(w, "data: %s\n\n", stopJSON)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// writeOpenAIChatStream — streaming ответ в формате SSE для /v1/chat/completions.
func writeOpenAIChatStream(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	chatID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	tokenDone := make(chan struct{})
	defer close(tokenDone)

	keepaliveInterval := 15 * time.Second

	var writeMu sync.Mutex
	safeFlush := func() {
		writeMu.Lock()
		defer writeMu.Unlock()
		flusher.Flush()
	}
	safeFprintf := func(format string, a ...interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		fmt.Fprintf(w, format, a...)
	}

	keepaliveDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		defer close(keepaliveDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-tokenDone:
				return
			case <-ticker.C:
				safeFprintf(": keepalive\n\n")
				safeFlush()
			}
		}
	}()

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"delta": map[string]string{
						"role":    "assistant",
						"content": token,
					},
					"finish_reason": nil,
				},
			},
		}
		jsonData, _ := json.Marshal(chunk)
		safeFprintf("data: %s\n\n", jsonData)
		safeFlush()
		return true
	}
	if err := generateStreamWithRamFallback(modelName, prompt, params, callback); err != nil {
		maybeRestartOnMemorySlotError(err, modelName)
		errChunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         map[string]string{},
					"finish_reason": "error",
				},
			},
			"error": err.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		safeFprintf("data: %s\n\n", errJSON)
		safeFprintf("data: [DONE]\n\n")
		safeFlush()
		return
	}

	stopChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         map[string]string{},
				"finish_reason": "stop",
			},
		},
	}
	stopJSON, _ := json.Marshal(stopChunk)
	safeFprintf("data: %s\n\n", stopJSON)
	safeFprintf("data: [DONE]\n\n")
	safeFlush()
}

// handleV1Completions — OpenAI-совместимый /v1/completions endpoint.
func handleV1Completions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req openAICompletionRequest
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

	if err := ensureModelLoaded(req.Model); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return
		}
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	params := bridge.DefaultGenerationParams()
	if req.Temperature > 0 {
		params.Temperature = float32(req.Temperature)
	}
	if req.TopP > 0 {
		params.TopP = float32(req.TopP)
	}
	if req.MaxTokens > 0 {
		params.NPredict = req.MaxTokens
	}
	if req.PresencePenalty > 0 {
		params.PresencePenalty = float32(req.PresencePenalty)
	}
	if req.FrequencyPenalty > 0 {
		params.FrequencyPenalty = float32(req.FrequencyPenalty)
	}
	if req.Seed != 0 {
		params.Seed = req.Seed
	}
	if req.NumCtx > 0 {
		params.NCtxOverride = req.NumCtx
	}
	applyCppCtxHeader(r, &params)

	if req.Stream {
		writeOpenAICompletionStream(w, r, req.Model, req.Prompt, params)
		return
	}

	start := time.Now()
	result, err := generateWithRamFallback(req.Model, req.Prompt, params)
	if err != nil {
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "completions failed", err)
		return
	}
	durationMs := time.Since(start).Milliseconds()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":      fmt.Sprintf("cmpl-%d", time.Now().UnixNano()),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{
				"text":          result.Output,
				"index":         0,
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
		"duration_ms": durationMs,
	})
}

// writeOpenAICompletionStream — streaming ответ в формате SSE для /v1/completions.
func writeOpenAICompletionStream(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
	completionID := fmt.Sprintf("cmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		chunk := map[string]interface{}{
			"id":      completionID,
			"object":  "text_completion",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"text":          token,
					"index":         0,
					"finish_reason": nil,
				},
			},
		}
		jsonData, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", jsonData)
		flusher.Flush()
		return true
	}
	if err := generateStreamWithRamFallback(modelName, prompt, params, callback); err != nil {
		maybeRestartOnMemorySlotError(err, modelName)
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
			"error": err.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "data: %s\n\n", errJSON)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	stopChunk := map[string]interface{}{
		"id":      completionID,
		"object":  "text_completion",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"text":          "",
				"index":         0,
				"finish_reason": "stop",
			},
		},
	}
	stopJSON, _ := json.Marshal(stopChunk)
	fmt.Fprintf(w, "data: %s\n\n", stopJSON)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// handleV1Embeddings — OpenAI-совместимый /v1/embeddings endpoint.
func handleV1Embeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}
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
		"object": "list",
		"data": []map[string]interface{}{
			{
				"object":    "embedding",
				"index":     0,
				"embedding": embeddings,
			},
		},
		"model": req.Model,
		"usage": map[string]interface{}{
			"prompt_tokens": 0,
			"total_tokens":  0,
		},
	})
}

// handleV1Models — OpenAI-совместимый /v1/models endpoint.
func handleV1Models(w http.ResponseWriter, r *http.Request) {
	models := backend.ListModels()
	openaiModels := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		openaiModels = append(openaiModels, map[string]interface{}{
			"id":       m.Name,
			"object":   "model",
			"created":  m.LoadedAt.Unix(),
			"owned_by": "ollamalegion",
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   openaiModels,
	})
}
