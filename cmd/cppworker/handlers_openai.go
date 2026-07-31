// handlers_openai.go — OpenAI-compatible /v1/ endpoints (/v1/chat/completions,
// /v1/completions, /v1/embeddings, /v1/models).
package main

import (
	"encoding/json"

	"errors"
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
//
// IMPORTANT: Temperature, TopP, PresencePenalty, FrequencyPenalty используют
// *float64 — см. комментарий к openAIChatCompletionRequest (Round 16 follow-up
// fix). nil = использовать дефолт cppworker; *0.0 = explicit 0 от клиента.
type openAICompletionRequest struct {
	Model     string   `json:"model"`
	Prompt    string   `json:"prompt"`
	Suffix    string   `json:"suffix,omitempty"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	// Sampling params — *float64 для различения "unset" vs "explicit 0".
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	N                int      `json:"n,omitempty"`
	Stream           bool     `json:"stream,omitempty"`
	Echo             bool     `json:"echo,omitempty"`
	Stop             []string `json:"stop,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Seed             int      `json:"seed,omitempty"`
	// NumCtx — per-request переопределение n_ctx (OpenAI-совместимый).
	// 0 = использовать n_ctx модели. > 0 → C-bridge pre-flight check.
	// > effective n_ctx → C-bridge вернёт informative ошибку с предложением
	// перезагрузить модель через /api/models/reload.
	NumCtx int `json:"num_ctx,omitempty"`
	// StreamOptions — параметры стриминга (Round 15, 2026-07-29: теперь и
	// для legacy /v1/completions endpoint). include_usage добавляет usage chunk
	// в финальный SSE чанк — требуется Cline/прочим IDE-агентам для трекинга
	// контекста. По дефолту ВКЛЮЧЕНО (см. includeUsageEffective ниже).
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
}

// openAIChatCompletionRequest — структура запроса OpenAI /v1/chat/completions
//
// IMPORTANT (Round 16 follow-up fix, 2026-07-30): поля с optional zero values
// (Temperature, TopP) используют *float64 чтобы отличить "не задано клиентом"
// (nil → использовать дефолт cppworker 0.7/0.9) от "клиент явно задал 0"
// (*0.0 → greedy/no-top_p). Раньше использовался float64 + `if > 0` — это
// ИГНОРИРОВАЛО temperature=0 от клиента (Cline/Aider/Continue все шлют
// temperature=0 для tool calls) и подставляло 0.7 по умолчанию → не-greedy
// sampling, недетерминированные tool calls. См. _audit_test_temp0_2.py для repro.
type openAIChatCompletionRequest struct {
	Model     string              `json:"model"`
	Messages  []openAIChatMessage `json:"messages"`
	MaxTokens int                 `json:"max_tokens,omitempty"`
	// Temperature / TopP — pointer types: nil = не задано, *0.0 = explicit 0.
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	N           int      `json:"n,omitempty"`
	Stream      bool     `json:"stream,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	Seed        int      `json:"seed,omitempty"`
	// NumCtx — per-request переопределение n_ctx (OpenAI-совместимый).
	// 0 = использовать n_ctx модели. > 0 → C-bridge pre-flight check.
	NumCtx int `json:"num_ctx,omitempty"`
	// Tools — определения инструментов для function calling (OpenAI / Ollama совместимый формат).
	Tools []openAITool `json:"tools,omitempty"`
	// ToolChoice — управление выбором инструмента ("auto", "none", "required", или {"type":"function","function":{"name":"..."}}).
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	// StreamOptions — параметры стриминга (include_usage добавляет usage в финальный SSE чанк).
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
}

// openAIStreamOptions — подмножество OpenAI `stream_options`.
// Используется Cline для отслеживания контекстного окна (usage-чанк в SSE).
type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type openAIChatMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
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

	// InFlight counter: защищает активные запросы от reload-обрыва.
	if req.Model != "" && backend.InFlight() != nil {
		backend.InFlight().Inc(req.Model)
		defer backend.InFlight().Dec(req.Model)
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
	// Round 16 follow-up fix (2026-07-30): *float64 — nil = use default, *0.0 = greedy/no-top_p.
	// Раньше `if req.Temperature > 0` ИГНОРИРОВАЛО temperature=0 (Cline tool calls) → использовался
	// дефолт 0.7 → не-greedy sampling, недетерминированные tool calls.
	if req.Temperature != nil {
		params.Temperature = float32(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = float32(*req.TopP)
	}
	if req.MaxTokens > 0 {
		// 2026-07-01: для reasoning-моделей (qwen3.5, deepseek-r1, gemma-4) поднимаем
		// дефолт n_predict до DefaultNPredictReasoning, иначе модель обрывает генерацию
		// сразу после <think>...</think> (completion_tokens=0).
		params.NPredict = ResolveNPredict(req.MaxTokens, req.Model)
	} else {
		// 2026-07-01: req.MaxTokens==0 → используем дефолт cppworker (2048), но для
		// reasoning-моделей повышаем до DefaultNPredictReasoning (8192).
		params.NPredict = ResolveNPredict(0, req.Model)
	}
	if req.Seed != 0 {
		params.Seed = req.Seed
	}
	if req.NumCtx > 0 {
		params.NCtxOverride = req.NumCtx
	}
	// Bug fix (Round 4): прокидываем HasTools=true чтобы увеличить резерв
	// токенов под tools-prompt в n_ctx clamp. Без этого tools-запросы
	// получают 1024 токен резерва (как обычные чаты), и при 5+ tools с
	// длинными описаниями n_ctx overflow → reload-loop → 413 "n_ctx_too_large".
	ApplyCppCtxHeaderWithOptions(r, &params, CppCtxApplyOptions{
		HasTools: len(req.Tools) > 0,
	})

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
			hasTools := len(req.Tools) > 0
			result, err := generateWithRamFallback(req.Model, prompt, params, hasTools)
			// 2026-06-25: snapshot для /v1/chat/completions (tools path).
			// Round 16 P2 fix (2026-07-30): closure pattern (consistent с streaming
			// path line 686). Раньше `defer recordLastPromptFromError(... err)`
			// captures err by value at defer time — fragile к refactoring (если
			// кто-то переставит defer выше err, тихо получит nil всегда).
			// Closure form гарантирует что err захватывается при return.
			defer func() {
				recordLastPromptFromError(req.Model, "/v1/chat/completions", prompt, &params, hasTools, err)
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
			if calls := parseToolCallsFromOutput(result.Output); len(calls) > 0 {
				// Round 15 (2026-07-29): default includeUsage=true (кроме явного
				// include_usage=false в stream_options). OpenAI/стандарт говорит
				// "по умолчанию false", но Cline/Roo/Aider/Continue и другие IDE-агенты
				// ВСЕГДА хотят usage в финальном чанке для отслеживания контекстного
				// окна. Без usage chunk клиент не знает prompt_tokens и не может
				// предупредить о превышении контекста — что и наблюдал пользователь
				// на локальной машине. Совместимость: клиент может явно opt-out
				// через "stream_options": {"include_usage": false}.
				includeUsage := req.StreamOptions == nil || req.StreamOptions.IncludeUsage
				writeToolCallsStream(w, req.Model, chatID(), time.Now().Unix(), calls, prompt, includeUsage)
			} else {
				// Нет tool_calls — стримим как обычный текст
				includeUsage := req.StreamOptions == nil || req.StreamOptions.IncludeUsage
				writeStaticTextStream(w, req.Model, chatID(), time.Now().Unix(), result.Output, prompt, includeUsage)
			}
			return
		}
		// Без tools — обычный real-time streaming
		includeUsage := req.StreamOptions == nil || req.StreamOptions.IncludeUsage
		writeOpenAIChatStream(w, r, req.Model, prompt, params, includeUsage)
		return
	}

	// ============================================================
	// Non-streaming
	// ============================================================
	start := time.Now()
	chatID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	hasTools := len(req.Tools) > 0
	result, err := generateWithRamFallback(req.Model, prompt, params, hasTools)
	// 2026-06-25: snapshot для /v1/chat/completions (non-stream path).
	// Round 16 P2 fix (2026-07-30): closure form (см. tools path выше).
	defer func() {
		recordLastPromptFromError(req.Model, "/v1/chat/completions", prompt, &params, hasTools, err)
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
		toolCalls = parseToolCallsFromOutput(result.Output)
		hasToolCalls = len(toolCalls) > 0
	}

	// Строим message
	message := map[string]interface{}{
		"role": "assistant",
	}
	if hasToolCalls {
		// Round 6 #4+#10: for reasoning models we still want to expose
		// reasoning_content alongside tool_calls. SplitReasoningContent
		// peels off ``...`` block from output; remaining content goes
		// into tool_calls via parser above.
		if IsReasoningModel(req.Model) {
			r, c, has := SplitReasoningContent(result.Output)
			if has {
				message["reasoning_content"] = r
				// c is normally empty when tool_calls cover the rest,
				// but keep it as plain content if non-empty (some models
				// stream prose before tool_call even with tools present).
				if c != "" {
					message["content"] = c
				} else {
					message["content"] = nil
				}
			} else {
				message["content"] = nil
			}
		} else {
			message["content"] = nil
		}
		message["tool_calls"] = toolCalls
	} else {
		if IsReasoningModel(req.Model) {
			r, c, has := SplitReasoningContent(result.Output)
			if has {
				message["content"] = c
				message["reasoning_content"] = r
			} else {
				message["content"] = result.Output
			}
		} else {
			message["content"] = result.Output
		}
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
		"usage":       buildUsage(prompt, result.Output, req.Model),
		"duration_ms": durationMs,
	})
}

// chatID генерирует уникальный ID чата.
func chatID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}

// writeToolCallsStream отправляет SSE-поток с tool_calls (без content).
// Формат: один chunk с delta.tool_calls, затем chunk с finish_reason="tool_calls".
//
// ВАЖНО: каждый tool_call эмитится с полем `index` — это требование OpenAI
// streaming spec. Без `index` Cline/Roo не могут правильно склеить
// инкрементальные deltas аргументов и интерпретируют tool_call как
// невалидный ("Invalid API Response").
//
// Параметр includeUsage включает финальный usage-чанк (после finish_reason
// но до [DONE]) — соответствует OpenAI stream_options.include_usage=true.
func writeToolCallsStream(w http.ResponseWriter, modelName, chatID string, created int64, calls []openAIToolCall, prompt string, includeUsage bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// OpenAI streaming spec требует поле `index` для каждого tool_call,
	// чтобы клиент мог склеивать инкрементальные deltas по index.
	// helper indexedToolCalls добавляет индексы 0..N-1.
	indexed := indexedToolCalls(calls)

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
					"tool_calls": indexed,
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
	flusher.Flush()

	// Chunk 3 (опционально): usage блок, если includeUsage=true.
	// Для tool_calls output completion=JSON аргументы функций.
	// Тут output = serialized tool_calls (см. caller), используем как есть.
	if includeUsage {
		writeOpenAIUsageChunk(w, flusher, chatID, created, modelName, prompt, serializeToolCallsForUsage(indexed), includeUsage)
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// serializeToolCallsForUsage склеивает tool_calls в строку для подсчёта
// completion_tokens. Подсчёт токенов на JSON-сериализации близок к реальному,
// т.к. модель сгенерировала этот JSON-текст.
func serializeToolCallsForUsage(calls []map[string]interface{}) string {
	b, err := json.Marshal(calls)
	if err != nil {
		return ""
	}
	return string(b)
}

// writeStaticTextStream отправляет SSE-поток с фиксированным текстом
// (используется когда tools запросили буферизированный ответ).
//
// Параметр includeUsage добавляет финальный usage-чанк перед [DONE]
// (OpenAI stream_options.include_usage=true). Используется Cline/Roo для
// корректного трекинга контекстного окна.
func writeStaticTextStream(w http.ResponseWriter, modelName, chatID string, created int64, content, prompt string, includeUsage bool) {
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
	flusher.Flush()

	// Финальный usage chunk (если include_usage=true) — перед [DONE].
	// OpenAI stream_options.include_usage=true → клиент получает prompt/completion/total_tokens.
	writeOpenAIUsageChunk(w, flusher, chatID, created, modelName, prompt, content, includeUsage)

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// writeOpenAIChatStream — streaming ответ в формате SSE для /v1/chat/completions.
// writeOpenAIChatStream — streaming ответ в формате SSE для /v1/chat/completions.
//
// КРИТИЧЕСКИ ВАЖНО: финальный SSE чанк ДОЛЖЕН содержать полный output в delta.content,
// а не пустую строку. Иначе OpenWebUI после done:true видит пустой content и завершает
// сессию ("один источник найден, ответа нет"). Также поддерживаем детекцию tool_calls
// в выходном тексте — если модель сгенерировала <tool_call>...</tool_call> (Hermes/Gemma-4/Qwen),
// эмитим tool_calls в финальном чанке и обнуляем content.
func writeOpenAIChatStream(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams, includeUsage bool) {
	// Round 15 (2026-07-29): debug-логирование для диагностики missing usage chunk.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	// 2026-06-24: preflight n_ctx check BEFORE WriteHeader(200).
	// Once WriteHeader(200) is called, we can't change status code mid-stream.
	// The HTTP client would see "200 OK" with whatever body we wrote after
	// (which it would interpret as malformed SSE response).
	if err := clampNPredictToFitContext(modelName, prompt, &params); err != nil {
		// Returns *PromptExceedsNCtxError → HTTP 413 with actionable advice.
		var perr *PromptExceedsNCtxError
		if errors.As(err, &perr) {
			writePromptExceedsNCtxResponse(w, perr)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
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

	// 2026-06-25: snapshot для /v1/chat/completions streaming.
	var streamErr error
	defer func() {
		// Round 5 Fix 3: проброс r в новую context-aware версию —
		// при client_disconnect err==nil, без ctx этого не видно.
		recordLastPromptFromErrorWithContext(modelName, "/v1/chat/completions", prompt, &params, false, streamErr, r)
	}()

	// Round 6 #7: configurable heartbeat via env. Default 15s for OpenAI SSE.
	keepaliveInterval := getHeartbeatInterval(15 * time.Second)

	// === 2026-06-26 BUGFIX: safeStreamWriter ===
	//
	// Раньше использовались локальные safeFprintf/safeFlush, которые
	// НЕ проверяли ctx.Done() и НЕ возвращали write error. После обрыва
	// клиента cppworker продолжал писать в мёртвый socket, получая
	// "broken pipe" без логирования (issue «обрыв ответа без каких-либо
	// ошибок»). safeStreamWriter:
	//   - проверяет ctx.Done() перед каждой операцией;
	//   - логирует первый write error;
	//   - записывает snapshot в /api/v1/cppworker/debug/last-stream;
	//   - потокобезопасен.
	sw := newSafeStreamWriter(w, r, "writeOpenAIChatStream", modelName)

	// === 2026-06-26 BUGFIX: явный stopCh для keepalive goroutine ===
	//
	// Старая версия использовала defer close(tokenDone) — это создавало
	// race: горутина могла выйти по `<-ctx.Done()` ДО того, как defer
	// main-функции выполнится, и если бы потом defer пытался закрыть уже
	// закрытый канал, был бы panic. Также при коротких стримах (<15s)
	// defer мог закрыть tokenDone ДО того, как горутина успеет среагировать.
	// Решение: явный stopCh, который закрывается в defer с wg.Wait().
	stopCh := make(chan struct{})
	var keepaliveWG sync.WaitGroup

	// Накапливаем полный output параллельно для финального чанка + tool_calls detection.
	// Это критично: иначе финальный SSE чанк имел бы пустой delta.content,
	// и OpenWebUI считал бы ответ пустым после done:true.
	var outputBuf strings.Builder

	// 2026-07-01: reasoning-парсер. Для reasoning-моделей (qwen3.5, deepseek-r1,
	// gemma-4 и т.п.) разделяем токены на (reasoning, content) и эмитим
	// отдельный `delta.reasoning_content` в SSE — это позволяет OpenWebUI/Cline
	// корректно отображать think-блок и видимый ответ раздельно.
	rsParser := NewReasoningStreamState()
	rsIsReasoning := IsReasoningModel(modelName)
	rsSentHeader := false

	keepaliveWG.Add(1)
	go func() {
		defer keepaliveWG.Done()
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-ticker.C:
				if !sw.Writef(": keepalive\n\n") || !sw.Flush() {
					// Client disconnected — выходим.
					return
				}
			}
		}
	}()

	// Гарантируем остановку keepalive goroutine и ожидание её завершения
	// при любом выходе (нормальный return, panic, disconnect).
	defer func() {
		close(stopCh)
		keepaliveWG.Wait()
	}()

	// writeReasoningChunk — низкоуровневый helper, эмитит ОДИН SSE чанк с указанным
	// delta (map[string]interface{}). Возвращает false при обрыве клиента / writer broken.
	writeReasoningChunk := func(delta map[string]interface{}) bool {
		if sw.IsBroken() {
			return false
		}
		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   modelName,
			"choices": []map[string]interface{}{
				{
					"index":         0,
					"delta":         delta,
					"finish_reason": nil,
				},
			},
		}
		jsonData, _ := json.Marshal(chunk)
		if !sw.Writef("data: %s\n\n", jsonData) {
			return false
		}
		if !sw.Flush() {
			return false
		}
		return true
	}

	callback := func(token string) bool {
		// Безопасная проверка: если контекст отвалился или writer сломан —
		// прекращаем генерацию (callback должен вернуть false).
		if sw.IsBroken() {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		default:
		}
		// Накапливаем output для финального чанка.
		outputBuf.WriteString(token)

		// 2026-07-01: reasoning-парсер — разделяем токены на (reasoning, content)
		// для reasoning-моделей (qwen3.5/qwen3.6/deepseek-r1/gemma-4/...). Эмитим
		// отдельный `delta.reasoning_content` в SSE, что позволяет OpenWebUI/Cline
		// корректно отображать think-блок и видимый ответ раздельно.
		if rsIsReasoning {
			reasoningDelta, contentDelta := rsParser.Feed(token)

			// Header-chunk: первый чанк содержит role=assistant. Отправляется ОДИН раз
			// до первого reasoning/content delta. Это упрощает клиент-парсер.
			if !rsSentHeader {
				rsSentHeader = true
				if !writeReasoningChunk(map[string]interface{}{"role": "assistant"}) {
					return false
				}
			}

			ok := true
			// Сначала отправляем reasoning_delta (если есть), затем content_delta.
			// Каждый — отдельный SSE чанк с одним непустым полем.
			if reasoningDelta != "" {
				if !writeReasoningChunk(map[string]interface{}{"reasoning_content": reasoningDelta}) {
					ok = false
				}
			}
			if ok && contentDelta != "" {
				if !writeReasoningChunk(map[string]interface{}{"content": contentDelta}) {
					ok = false
				}
			}
			// Каждый входной token = 1 токен генерации, независимо от того, на какие
			// части он был разделён.
			sw.IncTokens(1)
			return ok
		}

		// Non-reasoning путь: legacy-логика с одним SSE чанком и content=token.
		delta := map[string]interface{}{"role": "assistant", "content": token}
		if !writeReasoningChunk(delta) {
			return false
		}
		sw.IncTokens(1)
		return true
	}
	// writeOpenAIChatStream вызывается только когда tools НЕ заданы (для tools идёт буферизированный путь в handleV1ChatCompletions). Поэтому reload при n_ctx overflow разрешён.
	streamErr = generateStreamWithRamFallback(modelName, prompt, params, callback, false)
	if streamErr != nil {
		maybeRestartOnMemorySlotError(streamErr, modelName)
		// Специальная обработка reload-disabled-for-tools (HTTP 413 в SSE-чанке).
		if rdtErr, ok := streamErr.(*ReloadDisabledForToolsError); ok {
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
				"error":      rdtErr.Error(),
				"code":       "tools_reload_disabled",
				"suggestion": "Reduce the number of tools, chat history length, or increase n_ctx in the model profile.",
			}
			errJSON, _ := json.Marshal(errChunk)
			sw.Writef("data: %s\n\n", errJSON)
			sw.Writef("data: [DONE]\n\n")
			sw.Flush()
			return
		}
		// Специальная обработка reload-loop-limit (HTTP 413 в SSE-чанке).
		if rllErr, ok := streamErr.(*ReloadLoopLimitError); ok {
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
			"error": streamErr.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		sw.Writef("data: %s\n\n", errJSON)
		sw.Writef("data: [DONE]\n\n")
		sw.Flush()
		return
	}

	// ============================================================
	// Финальный чанк — содержит ПОЛНЫЙ output или tool_calls.
	// ============================================================
	// Раньше здесь был критичный bug: финальный SSE чанк имел пустой delta.content,
	// OpenWebUI после done:true видел пустой content и завершал сессию.
	fullOutput := outputBuf.String()
	toolCalls := parseToolCallsFromOutput(fullOutput)

	// 2026-06-24: проверка на пустой ответ после cleanFinalContent (gemma antiprompt).
	// Если tool_calls нет И cleaned output пустой → error-чанк.
	if len(toolCalls) == 0 && strings.TrimSpace(cleanFinalContent(fullOutput)) == "" {
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
			"error": "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, prompt too long, or model issue.",
		}
		errJSON, _ := json.Marshal(errChunk)
		sw.Writef("data: %s\n\n", errJSON)
		sw.Writef("data: [DONE]\n\n")
		sw.Flush()
		return
	}

	finalDelta := map[string]interface{}{}
	finishReason := "stop"

	if len(toolCalls) > 0 {
		// Tool calls обнаружены — эмитим их в финальном чанке с finish_reason="tool_calls".
		// Каждый tool_call должен иметь `index` per OpenAI streaming spec —
		// иначе Cline/Roo/openai-python SDK не могут правильно склеить инкремент.
		finalDelta["role"] = "assistant"
		finalDelta["content"] = nil
		finalDelta["tool_calls"] = indexedToolCalls(toolCalls)
		finishReason = "tool_calls"
	} else {
		// Plain-text ответ.
		// 2026-06-29: токены уже стримились инкрементально через callback
		// (см. тело цикла выше, строки 624-660 — каждый токен улетает отдельным
		// SSE-чанком с delta.content). Финальный чанк содержит только role +
		// finish_reason="stop", иначе клиент видит ДУБЛЬ: сначала N стриминговых
		// чанков с токенами, потом ещё один с ПОЛНЫМ текстом. OpenAI API это
		// допускает (финальный chunk может иметь пустой delta.content).
		finalDelta["role"] = "assistant"
		finalDelta["content"] = nil
	}

	stopChunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"delta":         finalDelta,
				"finish_reason": finishReason,
			},
		},
	}
	stopJSON, _ := json.Marshal(stopChunk)
	sw.Writef("data: %s\n\n", stopJSON)

	// Финальный usage chunk (если include_usage=true) — перед [DONE].
	// OpenAI stream_options.include_usage=true → клиент получает prompt/completion/total_tokens.
	// Для tool_calls output completion_tokens считаем по JSON-сериализации calls;
	// для текстового output completion_tokens = sw.Tokens() (atomic counter, лояльный).
	if includeUsage {
		var completionText string
		if len(toolCalls) > 0 {
			completionText = serializeToolCallsForUsage(indexedToolCalls(toolCalls))
		} else {
			completionText = fullOutput
		}
		emitOpenAIUsageChunkSafe(sw, chatID, created, modelName, prompt, completionText, true)
	}

	sw.Writef("data: [DONE]\n\n")
	sw.Flush()
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

	// InFlight counter: защищает активные запросы от reload-обрыва.
	if req.Model != "" && backend.InFlight() != nil {
		backend.InFlight().Inc(req.Model)
		defer backend.InFlight().Dec(req.Model)
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
	// Round 16 follow-up fix (2026-07-30): *float64 — nil = use default, *0.0 = explicit 0.
	if req.Temperature != nil {
		params.Temperature = float32(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = float32(*req.TopP)
	}
	if req.MaxTokens > 0 {
		// 2026-07-01: для reasoning-моделей (qwen3.5, deepseek-r1, gemma-4) поднимаем
		// дефолт n_predict до DefaultNPredictReasoning, иначе модель обрывает генерацию
		// сразу после <think>...</think> (completion_tokens=0).
		params.NPredict = ResolveNPredict(req.MaxTokens, req.Model)
	} else {
		// 2026-07-01: req.MaxTokens==0 → дефолт cppworker (2048), для reasoning-моделей
		// повышаем до DefaultNPredictReasoning (8192).
		params.NPredict = ResolveNPredict(0, req.Model)
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
		writeOpenAICompletionStream(w, r, req.Model, req.Prompt, params, includeUsage)
		return
	}

	start := time.Now()
	// /v1/completions (legacy text completions) — tools не поддерживаются,
	// reload разрешён при n_ctx overflow.
	result, err := generateWithRamFallback(req.Model, req.Prompt, params, false)
	// 2026-06-25: snapshot для /v1/completions.
	// Round 16 P2 fix (2026-07-30): closure form (consistent со всеми другими).
	defer func() {
		recordLastPromptFromError(req.Model, "/v1/completions", req.Prompt, &params, false, err)
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

	// 2026-07-01: для reasoning-моделей (qwen3.5/qwen3.6/deepseek-r1/gemma-4)
	// разделяем output на (reasoning_content, text) на верхнем уровне ответа.
	// Это позволяет OpenWebUI/Cline отображать think-блок и видимый ответ раздельно.
	textValue := result.Output
	reasoningValue := ""
	if IsReasoningModel(req.Model) {
		r, c, has := SplitReasoningContent(result.Output)
		if has {
			textValue = c
			reasoningValue = r
		}
	}

	resp := map[string]interface{}{
		"id":      fmt.Sprintf("cmpl-%d", time.Now().UnixNano()),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{
				"text":          textValue,
				"index":         0,
				"finish_reason": "stop",
			},
		},
		"usage":       buildUsage(req.Prompt, result.Output, req.Model),
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
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	// 2026-06-24: preflight n_ctx check BEFORE WriteHeader.
	if err := clampNPredictToFitContext(modelName, prompt, &params); err != nil {
		var perr *PromptExceedsNCtxError
		if errors.As(err, &perr) {
			writePromptExceedsNCtxResponse(w, perr)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
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
	rcParser := NewReasoningStreamState()
	rcIsReasoning := IsReasoningModel(modelName)

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
		if _, err := fmt.Fprintf(w, "data: %s\n\n", jsonData); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		outputBuf.WriteString(token)

		// 2026-07-01: reasoning-парсер — для reasoning-моделей разделяем токен на
		// (reasoning, text) и эмитим отдельный `reasoning_content` в SSE.
		if rcIsReasoning {
			reasoningDelta, textDelta := rcParser.Feed(token)
			return writeCompletionChunk(textDelta, reasoningDelta)
		}

		return writeCompletionChunk(token, "")
	}
	// writeOpenAICompletionStream — /v1/completions, tools не поддерживаются.
	streamErr = generateStreamWithRamFallback(modelName, prompt, params, callback, false)
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
			fmt.Fprintf(w, "data: %s\n\n", errJSON)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
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
		fmt.Fprintf(w, "data: %s\n\n", errJSON)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	// Финальный чанк с полным output (раньше был пустой text — клиент видел пустой ответ).
	// 2026-06-24: проверка на пустой ответ после cleanFinalContent (gemma antiprompt).
	fullOutput := outputBuf.String()
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
		fmt.Fprintf(w, "data: %s\n\n", errJSON)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
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
	fmt.Fprintf(w, "data: %s\n\n", stopJSON)
	flusher.Flush()

	// Round 15 (2026-07-29): финальный usage chunk (если includeUsage=true)
	// — перед [DONE]. OpenAI stream_options.include_usage=true. Cline/прочие
	// IDE-агенты читают prompt_tokens из этого чанка и трекают контекст.
	writeOpenAIUsageChunk(w, flusher, completionID, created, modelName, prompt, fullOutput, includeUsage)

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
			"prompt_tokens": buildPromptTokens(req.Model, req.Input),
			"total_tokens":  buildPromptTokens(req.Model, req.Input),
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

// ============================================================
// Streaming helpers
// ============================================================

// indexedToolCalls конвертирует []openAIToolCall в []map с полем `index` per OpenAI
// streaming spec. Без `index` клиенты (Cline/Roo/openai-python) не могут правильно
// склеить инкрементальные deltas tool_calls и считают ответ невалидным.
// Каждый элемент получает уникальный индекс в порядке массива.
func indexedToolCalls(calls []openAIToolCall) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(calls))
	for i, c := range calls {
		out = append(out, map[string]interface{}{
			"index": i,
			"id":    c.ID,
			"type":  c.Type,
			"function": map[string]interface{}{
				"name":      c.Function.Name,
				"arguments": c.Function.Arguments,
			},
		})
	}
	return out
}

// openAITokenUsage — счётчики токенов для OpenAI usage.
type openAITokenUsage struct {
	PromptTokens     int
	CompletionTokens int
}

// writeOpenAIUsageChunk эмитит в поток SSE chunk с блоком `usage` если запрошено.
// Соответствует OpenAI streaming spec (https://platform.openai.com/docs/api-reference/chat-streaming).
//
// Использование:
//   - includeUsage == true → chunk с полем `usage` (только в финальном чанке).
//   - includeUsage == false → no-op (legacy-режим, не мешаем старым клиентам).
//
// chunkID, created, modelName — копируются из вызывающего запроса (или
// генерируются заново). Безопасно для буферизованной и реальной передачи.
func writeOpenAIUsageChunk(w http.ResponseWriter, flusher http.Flusher, chunkID string, created int64, modelName string, prompt, output string, includeUsage bool) {
	if !includeUsage {
		return
	}
	usage := openAITokenUsage{
		PromptTokens:     countTokensSafe(modelName, prompt),
		CompletionTokens: countTokensSafe(modelName, output),
	}
	chunk := map[string]interface{}{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{},
		"usage": map[string]interface{}{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.PromptTokens + usage.CompletionTokens,
		},
	}
	jsonData, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
	if flusher != nil {
		flusher.Flush()
	}
}

// emitOpenAIUsageChunkSafe — то же что writeOpenAIUsageChunk, но использует
// safeStreamWriter (для уже-стартовавшего streaming). Используется в
// writeOpenAIChatStream где WriteHeader уже вызван и балансером может
// уже идти инкрементальный стрим.
//
// Возвращает false если writer broken (клиент отвалился) — позволяет
// caller прервать дальнейшую работу.
func emitOpenAIUsageChunkSafe(sw *safeStreamWriter, chunkID string, created int64, modelName, prompt, output string, includeUsage bool) {
	if !includeUsage {
		return
	}
	usage := openAITokenUsage{
		PromptTokens:     countTokensSafe(modelName, prompt),
		CompletionTokens: countTokensSafe(modelName, output),
	}
	chunk := map[string]interface{}{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   modelName,
		"choices": []map[string]interface{}{},
		"usage": map[string]interface{}{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.PromptTokens + usage.CompletionTokens,
		},
	}
	jsonData, _ := json.Marshal(chunk)
	sw.Writef("data: %s\n\n", jsonData)
}

// ============================================================
// Usage helpers
// ============================================================

// buildUsage возвращает OpenAI-совместимый блок `usage` для /v1/chat/completions
// и /v1/completions. Использует реальный токенайзер модели (backend.CountTokens);
// если он недоступен — fallback на ~4 символа/токен. Cline/Roo/OpenWebUI читают
// `total_tokens` для трекинга заполненности контекстного окна.
//
// Параметры:
//   - prompt     — входные сообщения (для chat) или prompt (для completions);
//   - output     — полный output модели (result.Output);
//   - modelName  — для выбора токенайзера.
//
// Возвращает map с ключами prompt_tokens, completion_tokens, total_tokens,
// совместимыми со спецификацией OpenAI.
func buildUsage(prompt, output, modelName string) map[string]interface{} {
	promptTokens := countTokensSafe(modelName, prompt)
	completionTokens := countTokensSafe(modelName, output)
	totalTokens := promptTokens + completionTokens
	return map[string]interface{}{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      totalTokens,
	}
}

// buildPromptTokens считает prompt_tokens для embeddings (только входной текст).
// Используется в /v1/embeddings.
func buildPromptTokens(modelName, input string) int {
	return countTokensSafe(modelName, input)
}

// countTokensSafe считает токены, безопасно обрабатывая backend==nil (в unit-тестах).
// Fallback: ~4 символа на токен, что соответствует эвристике для
// BPE-токенизаторов llama.cpp/Gemma. Для CJK и кириллицы это занижает
// реальный счёт, но лучше чем 0 — клиент хотя бы видит порядок.
func countTokensSafe(modelName, text string) int {
	if text == "" {
		return 0
	}
	if backend == nil {
		return len([]rune(text)) / 4
	}
	if _, err := backend.GetModel(modelName); err != nil {
		return len([]rune(text)) / 4
	}
	return backend.CountTokens(modelName, text)
}


