// anthropic_r247_test.go — R247, этап B: трансляция Anthropic <-> OpenAI.
//
// Приёмка этапа по плану: текст и инструмент, второй ход (tool_result),
// корректная последовательность SSE-событий Anthropic, форма ошибок.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR247_AnthropicRequestToOpenAI — запрос Anthropic переводится целиком.
func TestR247_AnthropicRequestToOpenAI(t *testing.T) {
	body := []byte(`{
		"model":"gemma-4",
		"max_tokens":512,
		"stream":true,
		"temperature":0.2,
		"system":[{"type":"text","text":"You are a coder."}],
		"stop_sequences":["</done>"],
		"tools":[
			{"name":"read","description":"Read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}
		],
		"tool_choice":{"type":"tool","name":"read"},
		"messages":[
			{"role":"user","content":"Read go.mod"},
			{"role":"assistant","content":[
				{"type":"text","text":"Reading."},
				{"type":"tool_use","id":"toolu_1","name":"read","input":{"file_path":"go.mod"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"module x"}]},
				{"type":"text","text":"Now name it"}
			]}
		]
	}`)
	out, err := anthropicToOpenAIRequest(body)
	if err != nil {
		t.Fatalf("трансляция запроса упала: %v", err)
	}
	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	if req["model"] != "gemma-4" {
		t.Errorf("model = %v", req["model"])
	}
	if req["stream"] != true {
		t.Errorf("stream = %v", req["stream"])
	}
	if _, ok := req["max_tokens"]; !ok {
		t.Error("max_tokens потерян")
	}
	msgs := req["messages"].([]interface{})
	if len(msgs) != 5 {
		t.Fatalf("сообщений %d, ожидалось 5 (system, user, assistant, tool, user-текст): %+v", len(msgs), msgs)
	}
	first := msgs[0].(map[string]interface{})
	if first["role"] != "system" || !strings.Contains(first["content"].(string), "coder") {
		t.Errorf("system не передан: %+v", first)
	}
	assistant := msgs[2].(map[string]interface{})
	calls, ok := assistant["tool_calls"].([]interface{})
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_use не стал tool_calls: %+v", assistant)
	}
	call := calls[0].(map[string]interface{})
	fn := call["function"].(map[string]interface{})
	if fn["name"] != "read" {
		t.Errorf("имя вызова = %v", fn["name"])
	}
	if fn["arguments"] != `{"file_path":"go.mod"}` {
		t.Errorf("arguments = %v, ожидалась JSON-строка", fn["arguments"])
	}
	toolMsg := msgs[3].(map[string]interface{})
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "toolu_1" {
		t.Errorf("tool_result не стал role=tool: %+v", toolMsg)
	}
	if !strings.Contains(toolMsg["content"].(string), "module x") {
		t.Errorf("результат инструмента потерян: %+v", toolMsg)
	}
	tools := req["tools"].([]interface{})
	tool := tools[0].(map[string]interface{})
	params := tool["function"].(map[string]interface{})["parameters"].(map[string]interface{})
	if params["required"] == nil {
		t.Errorf("input_schema не стал parameters: %+v", params)
	}
	choice := req["tool_choice"].(map[string]interface{})
	if choice["type"] != "function" {
		t.Errorf("tool_choice не форсирует функцию: %+v", choice)
	}
}

// TestR247_AnthropicToolChoiceMapping — auto/any/tool.
func TestR247_AnthropicToolChoiceMapping(t *testing.T) {
	cases := map[string]string{
		`{"type":"auto"}`: `"auto"`,
		`{"type":"any"}`:  `"required"`,
	}
	for in, want := range cases {
		got, _ := json.Marshal(anthropicToolChoice(json.RawMessage(in)))
		if string(got) != want {
			t.Errorf("tool_choice %s -> %s, ожидалось %s", in, got, want)
		}
	}
	got := anthropicToolChoice(json.RawMessage(`{"type":"tool","name":"grep"}`))
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"grep"`) {
		t.Errorf("tool_choice tool не назвал функцию: %s", b)
	}
}

// TestR247_OpenAIToAnthropicText — текстовый ответ и usage.
func TestR247_OpenAIToAnthropicText(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","model":"gemma-4","choices":[{"finish_reason":"stop","message":{"content":"Module is ollama-loadbalancer"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	out, err := openAIToAnthropicResponse(body, "fallback-model")
	if err != nil {
		t.Fatalf("трансляция ответа упала: %v", err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	if resp["type"] != "message" || resp["role"] != "assistant" {
		t.Errorf("форма Anthropic нарушена: %+v", resp)
	}
	if !strings.HasPrefix(resp["id"].(string), "msg_") {
		t.Errorf("id не в форме msg_: %v", resp["id"])
	}
	content := resp["content"].([]interface{})
	if len(content) != 1 || content[0].(map[string]interface{})["type"] != "text" {
		t.Fatalf("content не text-блок: %+v", content)
	}
	usage := resp["usage"].(map[string]interface{})
	if usage["input_tokens"].(float64) != 10 || usage["output_tokens"].(float64) != 5 {
		t.Errorf("usage = %+v", usage)
	}
	if resp["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", resp["stop_reason"])
	}
}

// TestR247_OpenAIToAnthropicToolUse — вызов отдаётся блоком tool_use с input-объектом.
func TestR247_OpenAIToAnthropicToolUse(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-2","model":"gemma-4","choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{"id":"call_read","type":"function","function":{"name":"read","arguments":"{\"file_path\":\"go.mod\"}"}}]}}],"usage":{"prompt_tokens":4,"completion_tokens":7}}`)
	out, _ := openAIToAnthropicResponse(body, "gemma-4")
	var resp map[string]interface{}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	if resp["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, ожидался tool_use", resp["stop_reason"])
	}
	content := resp["content"].([]interface{})
	block := content[0].(map[string]interface{})
	if block["type"] != "tool_use" {
		t.Fatalf("блок не tool_use: %+v", block)
	}
	input := block["input"].(map[string]interface{})
	if input["file_path"] != "go.mod" {
		t.Errorf("input = %+v, ожидался объект с file_path", input)
	}
	if block["id"] != "call_read" || block["name"] != "read" {
		t.Errorf("id/name = %v/%v", block["id"], block["name"])
	}
}

// TestR247_AnthropicSSESequence — порядок событий Anthropic для текста и вызова.
func TestR247_AnthropicSSESequence(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl-3","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-3","choices":[{"delta":{"content":"Module "},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-3","choices":[{"delta":{"content":"is ollama-loadbalancer"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-3","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_read","function":{"name":"read","arguments":"{\"file_path\":\"go.mod\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-3","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"id":"chatcmpl-3","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":11,"total_tokens":20}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var out strings.Builder
	if err := forwardOpenAISSEToAnthropic(strings.NewReader(upstream), &out, nil, "gemma-4"); err != nil {
		t.Fatalf("трансляция SSE упала: %v", err)
	}
	text := out.String()
	// Порядок событий: message_start -> text block -> tool_use block -> message_delta -> message_stop.
	order := []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text"`,
		`"text_delta"`,
		"event: content_block_stop",
		`"type":"tool_use"`,
		`"input_json_delta"`,
		"event: message_delta",
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	}
	pos := 0
	for _, needle := range order {
		idx := strings.Index(text[pos:], needle)
		if idx < 0 {
			t.Fatalf("в потоке нет %q (после позиции %d)\n%s", needle, pos, text)
		}
		pos += idx + len(needle)
	}
	if !strings.Contains(text, `"text":"Module "`) {
		t.Errorf("текст дельты не передан: %s", text)
	}
	if !strings.Contains(text, `"partial_json":"{\"file_path\":\"go.mod\"}"`) {
		t.Errorf("аргументы вызова не переданы в partial_json: %s", text)
	}
	if !strings.Contains(text, `"output_tokens":11`) {
		t.Errorf("usage в message_delta потерян: %s", text)
	}
}

// TestR247_AnthropicErrorShape — форма ошибки Anthropic.
func TestR247_AnthropicErrorShape(t *testing.T) {
	if got := anthropicErrorTypeForStatus(413); got != "request_too_large" {
		t.Errorf("413 -> %q", got)
	}
	if got := anthropicErrorTypeForStatus(503); got != "overloaded_error" {
		t.Errorf("503 -> %q", got)
	}
	if got := anthropicErrorTypeForStatus(400); got != "invalid_request_error" {
		t.Errorf("400 -> %q", got)
	}
}

// TestR247_AnthropicInvalidArguments — не-JSON аргументы не роняют ответ.
func TestR247_AnthropicInvalidArguments(t *testing.T) {
	body := []byte(`{"id":"x","choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"c","function":{"name":"read","arguments":"not-json"}}]}}]}`)
	out, err := openAIToAnthropicResponse(body, "m")
	if err != nil {
		t.Fatalf("ответ с битыми аргументами уронил трансляцию: %v", err)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(out, &resp)
	block := resp["content"].([]interface{})[0].(map[string]interface{})
	input, ok := block["input"].(map[string]interface{})
	if !ok || len(input) != 0 {
		t.Errorf("input = %+v, ожидался пустой объект (клиент не должен падать)", block["input"])
	}
}

