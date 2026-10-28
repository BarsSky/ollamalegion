// anthropic_r247.go — R247, этап B плана
// (docs/plans/2026-10-26-clients-agents-contract-r247.md).
//
// ЗАЧЕМ. Anthropic-native клиенты (Claude Code, `anthropic` SDK, часть IDE-плагинов)
// ходят в `POST /v1/messages` и НЕ умеют переключаться на OpenAI-режим. До R247
// такой запрос получал 404 — клиент вообще не работал с нашим стендом.
//
// КАК. Трансляция в обе стороны поверх уже существующей OpenAI-поверхности:
//
//	/v1/messages (Anthropic)  ->  /v1/chat/completions (наш же балансер)  ->  ответ
//
// Внутренний вызов идёт на себя же (self-proxy), поэтому Anthropic-клиент
// автоматически получает ВСЁ, что уже работает: ремонт аргументов, strict,
// tool-историю по шаблону модели, retry, admission-очередь, сессии.
//
// Что транслируется: system (строка/блоки), messages[].content (text/image/
// tool_use/tool_result), tools[].input_schema -> parameters, tool_choice
// (auto/any/tool), max_tokens, stop_sequences, temperature/top_p, stream.
// Ответ: content-блоки text/tool_use, stop_reason (end_turn/tool_use/max_tokens),
// usage (input_tokens/output_tokens); SSE — события Anthropic
// (message_start, content_block_start/delta/stop, message_delta, message_stop).
//
// НЕ транслируется (осознанно, см. план §11): thinking-блоки с signature,
// cache_control, `previous_message_id`-подобного состояния у Anthropic нет.
package balancer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ============================================================
// 1. Запрос Anthropic -> OpenAI
// ============================================================

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Source    json.RawMessage `json:"source,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content json.RawMessage         `json:"content"`
	Blocks  []anthropicContentBlock `json:"-"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type anthropicMessagesRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	Metadata      json.RawMessage    `json:"metadata,omitempty"`
}

// anthropicTextOf — текст из строки ИЛИ массива блоков (system/tool_result).
func anthropicTextOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "" || b.Type == "text" {
				if strings.TrimSpace(b.Text) != "" {
					parts = append(parts, b.Text)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// anthropicBlocksOf — блоки content сообщения (строка -> один text-блок).
func anthropicBlocksOf(raw json.RawMessage) []anthropicContentBlock {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []anthropicContentBlock{{Type: "text", Text: s}}
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	return nil
}

// anthropicToOpenAIRequest — тело Anthropic -> тело OpenAI chat/completions.
func anthropicToOpenAIRequest(body []byte) ([]byte, error) {
	var req anthropicMessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid Anthropic request: %w", err)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("model is required")
	}

	type openAIMsg map[string]interface{}
	messages := make([]openAIMsg, 0, len(req.Messages)+1)

	if sys := anthropicTextOf(req.System); strings.TrimSpace(sys) != "" {
		messages = append(messages, openAIMsg{"role": "system", "content": sys})
	}

	for _, msg := range req.Messages {
		blocks := anthropicBlocksOf(msg.Content)
		switch msg.Role {
		case "assistant":
			var textParts []string
			var toolCalls []map[string]interface{}
			for _, b := range blocks {
				switch b.Type {
				case "", "text":
					if strings.TrimSpace(b.Text) != "" {
						textParts = append(textParts, b.Text)
					}
				case "tool_use":
					args := "{}"
					if len(b.Input) > 0 {
						args = string(b.Input)
					}
					toolCalls = append(toolCalls, map[string]interface{}{
						"id":   b.ID,
						"type": "function",
						"function": map[string]interface{}{
							"name":      b.Name,
							"arguments": args,
						},
					})
				}
			}
			out := openAIMsg{"role": "assistant", "content": strings.Join(textParts, "\n\n")}
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
				if len(textParts) == 0 {
					out["content"] = nil
				}
			}
			messages = append(messages, out)
		default: // user: может содержать tool_result блоки + обычный текст
			var textParts []string
			var toolResults []openAIMsg
			for _, b := range blocks {
				switch b.Type {
				case "", "text":
					if strings.TrimSpace(b.Text) != "" {
						textParts = append(textParts, b.Text)
					}
				case "tool_result":
					text := anthropicTextOf(b.Content)
					isErr := false
					if len(b.Content) > 0 {
						var probe struct {
							IsError bool `json:"is_error"`
						}
						_ = json.Unmarshal(b.Content, &probe)
						isErr = probe.IsError
					}
					if isErr {
						text = "Error: " + text
					}
					toolResults = append(toolResults, openAIMsg{
						"role":         "tool",
						"tool_call_id": b.ToolUseID,
						"content":      text,
					})
				case "image":
					// Изображения отдаём OpenAI-формой; политика «модель не vision»
					// решается на этапе D (иначе это было бы молчаливое отбрасывание).
					messages = append(messages, openAIMsg{
						"role": "user",
						"content": []map[string]interface{}{{
							"type": "image_url",
							"image_url": map[string]string{
								"url": anthropicImageURL(b.Source),
							},
						}},
					})
				}
			}
			// Порядок важен: сначала результаты инструментов (они отвечают на
			// tool_calls предыдущего assistant), затем пользовательский текст.
			messages = append(messages, toolResults...)
			if len(textParts) > 0 {
				messages = append(messages, openAIMsg{"role": "user", "content": strings.Join(textParts, "\n\n")})
			}
		}
	}

	out := map[string]interface{}{
		"model":      req.Model,
		"messages":   messages,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  rawOrDefault(t.InputSchema, `{"type":"object"}`),
				},
			})
		}
		out["tools"] = tools
		if choice := anthropicToolChoice(req.ToolChoice); choice != nil {
			out["tool_choice"] = choice
		}
	}
	return json.Marshal(out)
}

// anthropicImageURL — источник картинки Anthropic в URL OpenAI-формы.
func anthropicImageURL(source json.RawMessage) string {
	if len(source) == 0 {
		return ""
	}
	var src struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(source, &src); err != nil {
		return ""
	}
	if src.URL != "" {
		return src.URL
	}
	if src.Data != "" {
		media := src.MediaType
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + src.Data
	}
	return ""
}

// anthropicToolChoice — tool_choice Anthropic -> OpenAI.
//
//	{"type":"auto"} -> "auto"; {"type":"any"} -> "required";
//	{"type":"tool","name":"read"} -> {"type":"function","function":{"name":"read"}}
func anthropicToolChoice(raw json.RawMessage) interface{} {
	if len(raw) == 0 {
		return nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil
	}
	switch choice.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		if choice.Name == "" {
			return "required"
		}
		return map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": choice.Name},
		}
	}
	return nil
}

func rawOrDefault(raw json.RawMessage, def string) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return json.RawMessage(def)
	}
	return raw
}

// ============================================================
// 2. Ответ OpenAI -> Anthropic (non-stream)
// ============================================================

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openAIResponseMessage struct {
	Content   string `json:"content"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

type openAIChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string                `json:"finish_reason"`
		Message      openAIResponseMessage `json:"message"`
	} `json:"choices"`
	Usage openAIUsage `json:"usage"`
}

// anthropicStopReason — finish_reason OpenAI -> stop_reason Anthropic.
func anthropicStopReason(finish string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_use"
	}
	switch finish {
	case "length":
		return "max_tokens"
	case "stop", "":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	}
	return "end_turn"
}

// anthropicMessageID — id сообщения Anthropic (форма msg_…).
func anthropicMessageID(openAIID string) string {
	id := strings.TrimSpace(openAIID)
	if id == "" {
		return "msg_" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + id
}

// openAIToAnthropicResponse — non-stream ответ.
func openAIToAnthropicResponse(body []byte, model string) ([]byte, error) {
	var resp openAIChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("invalid upstream response: %w", err)
	}
	var choice = struct {
		FinishReason string
		Message      openAIResponseMessage
	}{}
	if len(resp.Choices) > 0 {
		choice.FinishReason = resp.Choices[0].FinishReason
		choice.Message = resp.Choices[0].Message
	}
	content := make([]map[string]interface{}, 0, 1+len(choice.Message.ToolCalls))
	if strings.TrimSpace(choice.Message.Content) != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": choice.Message.Content})
	}
	for _, call := range choice.Message.ToolCalls {
		content = append(content, map[string]interface{}{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Function.Name,
			"input": argumentsAsObject(call.Function.Arguments),
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]interface{}{"type": "text", "text": ""})
	}
	out := map[string]interface{}{
		"id":            anthropicMessageID(resp.ID),
		"type":          "message",
		"role":          "assistant",
		"model":         anthropicFirstNonEmpty(resp.Model, model),
		"content":       content,
		"stop_reason":   anthropicStopReason(choice.FinishReason, len(choice.Message.ToolCalls) > 0),
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
		},
	}
	return json.Marshal(out)
}

// argumentsAsObject — JSON-строка аргументов в объект (для tool_use.input).
func argumentsAsObject(args string) interface{} {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return map[string]interface{}{}
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
		return obj
	}
	// Аргументы не объект: Anthropic требует object; отдаём пустой и не врём
	// клиенту-валидатору. Текст остаётся в ответе как есть (см. content).
	return map[string]interface{}{}
}

func anthropicFirstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ============================================================
// 3. SSE OpenAI -> события Anthropic
// ============================================================

// writeAnthropicErrorEvent — событие error в форме Anthropic.
func writeAnthropicErrorEvent(w io.Writer, flush func(), errType, message string) {
	payload := map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	}
	writeAnthropicSSE(w, "error", payload)
	if flush != nil {
		flush()
	}
}

// writeAnthropicSSE — одно событие SSE в формате Anthropic.
func writeAnthropicSSE(w io.Writer, event string, payload map[string]interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

// anthropicStreamForwarder — читает OpenAI SSE и пишет события Anthropic.
type anthropicStreamForwarder struct {
	w        io.Writer
	flush    func()
	model    string
	msgID    string
	started  bool
	textOpen bool
	textIdx  int
	nextIdx  int
	usage    openAIUsage
	finish   string
	calls    []struct{ id, name, args string }
	callIdx  map[string]int
}

// forwardOpenAISSEToAnthropic — построчная трансляция потока.
func forwardOpenAISSEToAnthropic(r io.Reader, w io.Writer, flush func(), model string) error {
	f := &anthropicStreamForwarder{w: w, flush: flush, model: model, callIdx: map[string]int{}}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *openAIUsage `json:"usage"`
			Error interface{}  `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			f.finishStream()
			writeAnthropicErrorEvent(w, flush, "api_error", fmt.Sprintf("%v", chunk.Error))
			return nil
		}
		f.ensureStart(anthropicFirstNonEmpty(chunk.ID, f.msgID))
		if chunk.Usage != nil {
			f.usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if content := choice.Delta.Content; content != "" {
			f.emitText(content)
		}
		for _, tc := range choice.Delta.ToolCalls {
			key := tc.ID
			if key == "" && tc.Index != nil {
				key = fmt.Sprintf("idx%d", *tc.Index)
			}
			pos, seen := f.callIdx[key]
			if !seen {
				pos = len(f.calls)
				f.callIdx[key] = pos
				f.calls = append(f.calls, struct{ id, name, args string }{id: tc.ID})
			}
			if tc.Function.Name != "" {
				f.calls[pos].name = tc.Function.Name
			}
			if tc.ID != "" {
				f.calls[pos].id = tc.ID
			}
			f.calls[pos].args += tc.Function.Arguments
		}
		if choice.FinishReason != "" {
			f.finish = choice.FinishReason
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		f.finishStream()
		writeAnthropicErrorEvent(w, flush, "api_error", "upstream stream error: "+err.Error())
		return nil
	}
	f.finishStream()
	return nil
}

func (f *anthropicStreamForwarder) ensureStart(id string) {
	if f.started {
		return
	}
	f.started = true
	if id != "" {
		f.msgID = id
	}
	writeAnthropicSSE(f.w, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            anthropicMessageID(f.msgID),
			"type":          "message",
			"role":          "assistant",
			"model":         f.model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	if f.flush != nil {
		f.flush()
	}
}

func (f *anthropicStreamForwarder) emitText(text string) {
	f.ensureStart("")
	if !f.textOpen {
		f.textIdx = f.nextIdx
		f.nextIdx++
		writeAnthropicSSE(f.w, "content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         f.textIdx,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})
		f.textOpen = true
	}
	writeAnthropicSSE(f.w, "content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": f.textIdx,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	})
	if f.flush != nil {
		f.flush()
	}
}

// finishStream — закрыть открытые блоки и отдать message_delta/message_stop.
func (f *anthropicStreamForwarder) finishStream() {
	f.ensureStart("")
	if f.textOpen {
		writeAnthropicSSE(f.w, "content_block_stop", map[string]interface{}{
			"type": "content_block_stop", "index": f.textIdx,
		})
		f.textOpen = false
	}
	for _, call := range f.calls {
		idx := f.nextIdx
		f.nextIdx++
		writeAnthropicSSE(f.w, "content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    call.id,
				"name":  call.name,
				"input": map[string]interface{}{},
			},
		})
		args := strings.TrimSpace(call.args)
		if args == "" {
			args = "{}"
		}
		writeAnthropicSSE(f.w, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": args},
		})
		writeAnthropicSSE(f.w, "content_block_stop", map[string]interface{}{
			"type": "content_block_stop", "index": idx,
		})
	}
	writeAnthropicSSE(f.w, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": anthropicStopReason(f.finish, len(f.calls) > 0), "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": f.usage.CompletionTokens},
	})
	writeAnthropicSSE(f.w, "message_stop", map[string]interface{}{"type": "message_stop"})
	if f.flush != nil {
		f.flush()
	}
}

// ============================================================
// 4. Self-proxy: вызов собственного /v1/chat/completions
// ============================================================

// selfProxyClient — клиент для внутренних вызовов. Без общего таймаута: поток
// генерации ограничен дедлайнами самого балансера и отменой клиента.
var selfProxyClient = &http.Client{
	// R253.1: Timeout НЕ ставим — self-proxy обслуживает SSE-стримы (таймаут
	// оборвал бы длинный ответ). Ограничиваем только число соединений и простой.
	Transport: &http.Transport{
		Proxy:               nil,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 32,
		MaxConnsPerHost:     32,
		IdleConnTimeout:     60 * time.Second,
	},
}

// selfProxyBaseURL — адрес собственной OpenAI-поверхности.
// LB_SELF_BASE_URL — явное переопределение; иначе LB_PORT (в контейнере прокси
// слушает 0.0.0.0:LB_PORT) или 18080.
func selfProxyBaseURL(r *http.Request) string {
	if base := strings.TrimSpace(os.Getenv("LB_SELF_BASE_URL")); base != "" {
		return strings.TrimRight(base, "/")
	}
	port := strings.TrimSpace(os.Getenv("LB_PORT"))
	if port == "" {
		port = "18080"
	}
	scheme := "http"
	if r != nil && r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://127.0.0.1:" + port
}

// selfProxyPost — POST в собственную OpenAI-поверхность.
//
// R253.1: пробрасываются те же заголовки, что у preflight-диалога (общий помощник
// selfProxyPostRequest). Раньше уходили только токен/Authorization/X-Request-ID,
// поэтому внутренний запрос выглядел пришедшим с 127.0.0.1: другой sessionID,
// вторая строка в панели, разделённый кошелёк admission-очереди — регресс R252.
func selfProxyPost(ctx context.Context, incoming *http.Request, path string, body []byte) (*http.Response, error) {
	return selfProxyPostRequest(ctx, incoming, path, body, "")
}

// ============================================================
// 5. Обработчики /v1/messages
// ============================================================

// anthropicErrorTypeForStatus — тип ошибки Anthropic по HTTP-коду.
func anthropicErrorTypeForStatus(status int) string {
	switch {
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusServiceUnavailable, status == http.StatusBadGateway:
		return "overloaded_error"
	}
	return "api_error"
}

// writeAnthropicError — JSON-ошибка в форме Anthropic.
func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    anthropicErrorTypeForStatus(status),
			"message": message,
		},
	})
}

// handleAnthropicMessages — POST /v1/messages.
func (lr *LlamaCppRouter) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := readRequestBodyLimited(r)
	if err != nil {
		if err == errBodyTooLarge {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req anthropicMessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	openAIBody, err := anthropicToOpenAIRequest(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := selfProxyPost(r.Context(), r, "/v1/chat/completions", openAIBody)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "upstream call failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		if resp.StatusCode != http.StatusOK {
			upstream, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			writeAnthropicError(w, resp.StatusCode, strings.TrimSpace(string(upstream)))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		w.WriteHeader(http.StatusOK)
		flush()
		_ = forwardOpenAISSEToAnthropic(resp.Body, w, flush, req.Model)
		return
	}

	upstream, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		writeAnthropicError(w, resp.StatusCode, strings.TrimSpace(string(upstream)))
		return
	}
	out, err := openAIToAnthropicResponse(upstream, req.Model)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// handleAnthropicCountTokens — POST /v1/messages/count_tokens.
//
// Anthropic SDK использует его для планирования контекста. Точного токенайзера
// нужной модели у балансера нет (модель живёт в воркере), поэтому отдаём
// КОНСЕРВАТИВНУЮ оценку и помечаем её в заголовке — лучше оценка, чем 404 и
// отказ клиента работать.
func (lr *LlamaCppRouter) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := readRequestBodyLimited(r)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req anthropicMessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	chars := len([]rune(anthropicTextOf(req.System)))
	for _, msg := range req.Messages {
		for _, block := range anthropicBlocksOf(msg.Content) {
			chars += len([]rune(block.Text))
			chars += len([]rune(anthropicTextOf(block.Content)))
			chars += len(block.Input)
		}
	}
	for _, tool := range req.Tools {
		chars += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}
	// 4 символа на токен — верхняя граница для латиницы, нижняя для кириллицы.
	tokens := chars / 3
	if tokens < 1 {
		tokens = 1
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Token-Count-Estimate", "chars/3")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": tokens})
}
