// responses_r247.go — R247, этап C плана
// (docs/plans/2026-10-26-clients-agents-contract-r247.md).
//
// ЗАЧЕМ. Новые OpenAI-клиенты (Codex-style, часть SDK-обёрток) ходят только в
// `POST /v1/responses` и не умеют chat/completions. До R247 такой запрос получал
// 404 — клиент вообще не работал с нашим стендом.
//
// КАК. Та же схема, что у этапа B: трансляция в обе стороны поверх собственной
// `/v1/chat/completions` через self-proxy. Клиент автоматически получает весь уже
// работающий слой (ремонт аргументов, strict, tool-историю, повторы, admission).
//
// Транслируется: `input` (строка или items: message / function_call /
// function_call_output), `instructions`, `tools[]` плоской формы
// ({type:"function",name,parameters,strict}), `tool_choice`, `max_output_tokens`,
// `temperature`, `top_p`, `stream`.
// Ответ: объект `response` с `output[]` (message/output_text и function_call) и
// `output_text`; события `response.created`, `output_item.added`,
// `output_text.delta/done`, `function_call_arguments.delta/done`,
// `output_item.done`, `response.completed`.
package balancer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ============================================================
// 1. Запрос Responses -> OpenAI chat/completions
// ============================================================

type responsesInputItem struct {
	Type    string          `json:"type,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	CallID  string          `json:"call_id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Args    string          `json:"arguments,omitempty"`
	Output  json.RawMessage `json:"output,omitempty"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

type responsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       json.RawMessage `json:"instructions,omitempty"`
	MaxOutputTokens    int             `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Tools              []responsesTool `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
}

// hasResponsesImagePart — есть ли в content часть-изображение (input_image).
func hasResponsesImagePart(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	return strings.Contains(string(raw), "input_image") || strings.Contains(string(raw), "\"type\":\"image\"")
}

func responsesTextOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var out []string
		for _, p := range parts {
			if p.Text != "" {
				out = append(out, p.Text)
			}
		}
		return strings.Join(out, "\n\n")
	}
	return ""
}

// responsesToolChoice — tool_choice Responses -> OpenAI.
//
//	"auto"/"none"/"required" -> как есть;
//	{"type":"function","name":"read"} -> {"type":"function","function":{"name":"read"}}
func responsesToolChoice(raw json.RawMessage) interface{} {
	if len(raw) == 0 {
		return nil
	}
	var word string
	if err := json.Unmarshal(raw, &word); err == nil {
		switch word {
		case "auto", "none", "required":
			return word
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	if obj.Name == "" {
		return nil
	}
	return map[string]interface{}{
		"type":     "function",
		"function": map[string]interface{}{"name": obj.Name},
	}
}

// responsesToOpenAIRequest — тело Responses -> тело chat/completions.
func responsesToOpenAIRequest(body []byte) ([]byte, error) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid Responses request: %w", err)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("model is required")
	}
	messages := make([]map[string]interface{}, 0, 4)
	if instr := responsesTextOf(req.Instructions); strings.TrimSpace(instr) != "" {
		messages = append(messages, map[string]interface{}{"role": "system", "content": instr})
	}

	// input: строка или массив items.
	var inputStr string
	if err := json.Unmarshal(req.Input, &inputStr); err == nil {
		if strings.TrimSpace(inputStr) != "" {
			messages = append(messages, map[string]interface{}{"role": "user", "content": inputStr})
		}
	} else {
		var items []responsesInputItem
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, fmt.Errorf("invalid input: %w", err)
		}
		for _, item := range items {
			switch item.Type {
			case "function_call":
				args := strings.TrimSpace(item.Args)
				if args == "" {
					args = "{}"
				}
				messages = append(messages, map[string]interface{}{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []map[string]interface{}{{
						"id":   item.CallID,
						"type": "function",
						"function": map[string]interface{}{
							"name":      item.Name,
							"arguments": args,
						},
					}},
				})
			case "function_call_output":
				messages = append(messages, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": item.CallID,
					"content":      responsesTextOf(item.Output),
				})
			default: // message / без type
				role := item.Role
				if role == "" {
					role = "user"
				}
				// R247/D: части input_image нельзя терять молча — переводим в
				// image_url, дальше общий обработчик честно ответит 415.
				if hasResponsesImagePart(item.Content) {
					messages = append(messages, map[string]interface{}{
						"role":    "user",
						"content": []map[string]interface{}{{"type": "image_url", "image_url": map[string]string{"url": ""}}},
					})
				}
				text := responsesTextOf(item.Content)
				if strings.TrimSpace(text) == "" {
					continue
				}
				messages = append(messages, map[string]interface{}{"role": role, "content": text})
			}
		}
	}

	out := map[string]interface{}{
		"model":    req.Model,
		"messages": messages,
		"stream":   req.Stream,
	}
	if req.MaxOutputTokens > 0 {
		out["max_tokens"] = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "" && t.Type != "function" {
				continue // встроенные инструменты (web_search и т.п.) не транслируются
			}
			fn := map[string]interface{}{
				"name":       t.Name,
				"parameters": rawOrDefault(t.Parameters, `{"type":"object"}`),
			}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.Strict {
				fn["strict"] = true
			}
			tools = append(tools, map[string]interface{}{"type": "function", "function": fn})
		}
		if len(tools) > 0 {
			out["tools"] = tools
			if choice := responsesToolChoice(req.ToolChoice); choice != nil {
				out["tool_choice"] = choice
			}
		}
	}
	return json.Marshal(out)
}

// ============================================================
// 2. Ответ OpenAI -> объект response
// ============================================================

// responsesID — id в форме соответствующего объекта OpenAI (resp_/msg_/fc_).
func responsesID(prefix, openAIID string) string {
	id := strings.TrimSpace(openAIID)
	if id == "" {
		return prefix + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if strings.HasPrefix(id, prefix) {
		return id
	}
	return prefix + id
}

// openAIToResponsesObject — non-stream ответ chat/completions -> response.
func openAIToResponsesObject(body []byte, model string) ([]byte, error) {
	var resp openAIChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("invalid upstream response: %w", err)
	}
	var finish string
	msg := openAIResponseMessage{}
	if len(resp.Choices) > 0 {
		finish = resp.Choices[0].FinishReason
		msg = resp.Choices[0].Message
	}
	respID := responsesID("resp_", resp.ID)
	output := make([]interface{}, 0, 2)
	text := strings.TrimSpace(msg.Content)
	if text != "" {
		output = append(output, map[string]interface{}{
			"type":   "message",
			"id":     responsesID("msg_", resp.ID),
			"role":   "assistant",
			"status": "completed",
			"content": []map[string]interface{}{{
				"type":        "output_text",
				"text":        msg.Content,
				"annotations": []interface{}{},
			}},
		})
	}
	for _, call := range msg.ToolCalls {
		output = append(output, map[string]interface{}{
			"type":      "function_call",
			"id":        responsesID("fc_", call.ID),
			"call_id":   call.ID,
			"name":      call.Function.Name,
			"arguments": call.Function.Arguments,
			"status":    "completed",
		})
	}
	_ = finish
	out := map[string]interface{}{
		"id":          respID,
		"object":      "response",
		"created_at":  time.Now().Unix(),
		"status":      "completed",
		"model":       anthropicFirstNonEmpty(resp.Model, model),
		"output":      output,
		"output_text": msg.Content,
		"usage": map[string]int{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
			"total_tokens":  resp.Usage.TotalTokens,
		},
		"error": nil,
	}
	return json.Marshal(out)
}

// ============================================================
// 3. SSE: chat/completions -> события Responses
// ============================================================

// responsesStreamWriter — события Responses с растущим sequence_number.
type responsesStreamWriter struct {
	w        io.Writer
	flush    func()
	model    string
	respID   string
	seq      int
	msgIndex int
	msgID    string
	textOpen bool
	textBuf  strings.Builder
	finish   string
	usage    openAIUsage
	calls    []struct{ id, name, args string }
	callIdx  map[string]int
}

func (s *responsesStreamWriter) event(name string, payload map[string]interface{}) {
	s.seq++
	payload["type"] = name
	payload["sequence_number"] = s.seq
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data)
	if s.flush != nil {
		s.flush()
	}
}

// baseResponse — «скелет» объекта response для событий created/completed.
func (s *responsesStreamWriter) baseResponse(status string) map[string]interface{} {
	return map[string]interface{}{
		"id":         s.respID,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      s.model,
		"output":     []interface{}{},
	}
}

// forwardOpenAISSEToResponses — построчная трансляция потока.
func forwardOpenAISSEToResponses(r io.Reader, w io.Writer, flush func(), model string) error {
	s := &responsesStreamWriter{w: w, flush: flush, model: model, callIdx: map[string]int{}}
	s.respID = responsesID("resp_", "")
	s.msgID = responsesID("msg_", "")
	s.event("response.created", map[string]interface{}{"response": s.baseResponse("in_progress")})

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), streamMaxLineBytes()) // R264: один лимит строки на все пути (R259)
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
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
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
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			s.usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.Delta.Content != "" {
			s.emitText(choice.Delta.Content)
		}
		for _, tc := range choice.Delta.ToolCalls {
			key := tc.ID
			if key == "" && tc.Index != nil {
				key = fmt.Sprintf("idx%d", *tc.Index)
			}
			pos, seen := s.callIdx[key]
			if !seen {
				pos = len(s.calls)
				s.callIdx[key] = pos
				s.calls = append(s.calls, struct{ id, name, args string }{id: tc.ID})
			}
			if tc.ID != "" {
				s.calls[pos].id = tc.ID
			}
			if tc.Function.Name != "" {
				s.calls[pos].name = tc.Function.Name
			}
			s.calls[pos].args += tc.Function.Arguments
		}
		if choice.FinishReason != "" {
			s.finish = choice.FinishReason
		}
	}
	s.finishStream()
	return nil
}

// emitText — текст ответа как item message + content_part output_text.
func (s *responsesStreamWriter) emitText(delta string) {
	if !s.textOpen {
		s.textOpen = true
		s.event("response.output_item.added", map[string]interface{}{
			"output_index": s.msgIndex,
			"item": map[string]interface{}{
				"type":    "message",
				"id":      s.msgID,
				"role":    "assistant",
				"status":  "in_progress",
				"content": []interface{}{},
			},
		})
		s.event("response.content_part.added", map[string]interface{}{
			"item_id":       s.msgID,
			"output_index":  s.msgIndex,
			"content_index": 0,
			"part":          map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
		})
	}
	s.textBuf.WriteString(delta)
	s.event("response.output_text.delta", map[string]interface{}{
		"item_id":       s.msgID,
		"output_index":  s.msgIndex,
		"content_index": 0,
		"delta":         delta,
	})
}

// finishStream — закрыть item'ы и отдать response.completed.
func (s *responsesStreamWriter) finishStream() {
	output := make([]interface{}, 0, 2)
	if s.textOpen {
		text := s.textBuf.String()
		s.event("response.output_text.done", map[string]interface{}{
			"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0, "text": text,
		})
		s.event("response.content_part.done", map[string]interface{}{
			"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
			"part": map[string]interface{}{"type": "output_text", "text": text, "annotations": []interface{}{}},
		})
		s.event("response.output_item.done", map[string]interface{}{
			"output_index": s.msgIndex,
			"item": map[string]interface{}{
				"type": "message", "id": s.msgID, "role": "assistant", "status": "completed",
				"content": []map[string]interface{}{{"type": "output_text", "text": text, "annotations": []interface{}{}}},
			},
		})
		output = append(output, map[string]interface{}{
			"type": "message", "id": s.msgID, "role": "assistant", "status": "completed",
			"content": []map[string]interface{}{{"type": "output_text", "text": text, "annotations": []interface{}{}}},
		})
		s.msgIndex++
	}
	for _, call := range s.calls {
		idx := s.msgIndex
		s.msgIndex++
		itemID := responsesID("fc_", call.id)
		s.event("response.output_item.added", map[string]interface{}{
			"output_index": idx,
			"item": map[string]interface{}{
				"type": "function_call", "id": itemID, "call_id": call.id,
				"name": call.name, "arguments": "", "status": "in_progress",
			},
		})
		args := strings.TrimSpace(call.args)
		if args == "" {
			args = "{}"
		}
		s.event("response.function_call_arguments.delta", map[string]interface{}{
			"item_id": itemID, "output_index": idx, "delta": args,
		})
		s.event("response.function_call_arguments.done", map[string]interface{}{
			"item_id": itemID, "output_index": idx, "arguments": args,
		})
		s.event("response.output_item.done", map[string]interface{}{
			"output_index": idx,
			"item": map[string]interface{}{
				"type": "function_call", "id": itemID, "call_id": call.id,
				"name": call.name, "arguments": args, "status": "completed",
			},
		})
		output = append(output, map[string]interface{}{
			"type": "function_call", "id": itemID, "call_id": call.id,
			"name": call.name, "arguments": args, "status": "completed",
		})
	}
	completed := s.baseResponse("completed")
	completed["output"] = output
	completed["usage"] = map[string]int{
		"input_tokens":  s.usage.PromptTokens,
		"output_tokens": s.usage.CompletionTokens,
		"total_tokens":  s.usage.TotalTokens,
	}
	s.event("response.completed", map[string]interface{}{"response": completed})
}

// ============================================================
// 4. Обработчик /v1/responses
// ============================================================

// writeResponsesError — ошибка в форме OpenAI Responses (`error`).
func writeResponsesError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
			"param":   nil,
			"code":    nil,
		},
	})
}

// handleOpenAIResponses — POST /v1/responses.
func (lr *LlamaCppRouter) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeResponsesError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	body, err := readRequestBodyLimited(r)
	if err != nil {
		if err == errBodyTooLarge {
			writeResponsesError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
			return
		}
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return
	}
	if req.PreviousResponseID != "" {
		// Полное состояние диалога у нас не хранится: честный отказ лучше
		// молчаливого «продолжения» без истории (план §11, вопрос 3).
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error",
			"previous_response_id is not supported: send the full input history instead")
		return
	}
	openAIBody, err := responsesToOpenAIRequest(body)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	resp, err := selfProxyPost(r.Context(), r, "/v1/chat/completions", openAIBody)
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "api_error", "upstream call failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		if resp.StatusCode != http.StatusOK {
			upstream, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			writeResponsesError(w, resp.StatusCode, "api_error", strings.TrimSpace(string(upstream)))
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
		_ = forwardOpenAISSEToResponses(resp.Body, w, flush, req.Model)
		return
	}

	upstream, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		writeResponsesError(w, resp.StatusCode, "api_error", strings.TrimSpace(string(upstream)))
		return
	}
	out, err := openAIToResponsesObject(upstream, req.Model)
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// bytesTrimmed — маленький помощник для сравнений тела (используется в тестах).
func bytesTrimmed(b []byte) string {
	return strings.TrimSpace(string(bytes.TrimSpace(b)))
}
