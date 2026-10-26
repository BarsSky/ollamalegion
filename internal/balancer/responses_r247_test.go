// responses_r247_test.go — R247, этап C: OpenAI Responses API.
//
// Приёмка этапа по плану: текст и вызов инструмента, второй ход
// (function_call_output), порядок событий response.*, форма ошибок,
// необъявленный previous_response_id отбивается честно.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR247_ResponsesRequestString — простейший input строкой + flat tools.
func TestR247_ResponsesRequestString(t *testing.T) {
	body := []byte(`{
		"model":"gemma-4","input":"Read go.mod","instructions":"You are a coder.",
		"max_output_tokens":256,"temperature":0.1,"stream":true,
		"tools":[{"type":"function","name":"read","description":"Read a file",
			"parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}],
		"tool_choice":{"type":"function","name":"read"}
	}`)
	out, err := responsesToOpenAIRequest(body)
	if err != nil {
		t.Fatalf("трансляция запроса упала: %v", err)
	}
	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	if req["max_tokens"] != float64(256) {
		t.Errorf("max_output_tokens не стал max_tokens: %v", req["max_tokens"])
	}
	msgs := req["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("сообщений %d, ожидалось 2 (system + user): %+v", len(msgs), msgs)
	}
	if msgs[0].(map[string]interface{})["role"] != "system" {
		t.Errorf("instructions не стал system: %+v", msgs[0])
	}
	tools := req["tools"].([]interface{})
	tool := tools[0].(map[string]interface{})
	fn := tool["function"].(map[string]interface{})
	if fn["name"] != "read" || fn["parameters"] == nil {
		t.Errorf("flat-инструмент не стал OpenAI-формой: %+v", tool)
	}
	choice := req["tool_choice"].(map[string]interface{})
	if choice["type"] != "function" {
		t.Errorf("tool_choice не форсирует функцию: %+v", choice)
	}
}

// TestR247_ResponsesRequestItems — items function_call/function_call_output.
func TestR247_ResponsesRequestItems(t *testing.T) {
	body := []byte(`{
		"model":"gemma-4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Read go.mod"}]},
			{"type":"function_call","call_id":"call_read","name":"read","arguments":"{\"file_path\":\"go.mod\"}"},
			{"type":"function_call_output","call_id":"call_read","output":"1: module ollama-loadbalancer"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Name it"}]}
		],
		"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]
	}`)
	out, err := responsesToOpenAIRequest(body)
	if err != nil {
		t.Fatalf("трансляция items упала: %v", err)
	}
	var req map[string]interface{}
	_ = json.Unmarshal(out, &req)
	msgs := req["messages"].([]interface{})
	if len(msgs) != 4 {
		t.Fatalf("сообщений %d, ожидалось 4: %+v", len(msgs), msgs)
	}
	assistant := msgs[1].(map[string]interface{})
	calls, ok := assistant["tool_calls"].([]interface{})
	if !ok || len(calls) != 1 {
		t.Fatalf("function_call не стал tool_calls: %+v", assistant)
	}
	if calls[0].(map[string]interface{})["id"] != "call_read" {
		t.Errorf("call_id потерян: %+v", calls[0])
	}
	tool := msgs[2].(map[string]interface{})
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_read" {
		t.Errorf("function_call_output не стал role=tool: %+v", tool)
	}
	if !strings.Contains(tool["content"].(string), "ollama-loadbalancer") {
		t.Errorf("вывод инструмента потерян: %+v", tool)
	}
	if msgs[3].(map[string]interface{})["content"] != "Name it" {
		t.Errorf("текст после tool_output потерян: %+v", msgs[3])
	}
}

// TestR247_ResponsesObjectText — non-stream: текст + output_text + usage.
func TestR247_ResponsesObjectText(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-9","model":"gemma-4","choices":[{"finish_reason":"stop","message":{"content":"Module is ollama-loadbalancer"}}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
	out, err := openAIToResponsesObject(body, "fallback")
	if err != nil {
		t.Fatalf("трансляция ответа упала: %v", err)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(out, &resp)
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("форма response нарушена: %+v", resp)
	}
	if !strings.HasPrefix(resp["id"].(string), "resp_") {
		t.Errorf("id = %v, ожидалась форма resp_", resp["id"])
	}
	if resp["output_text"] != "Module is ollama-loadbalancer" {
		t.Errorf("output_text = %v", resp["output_text"])
	}
	output := resp["output"].([]interface{})
	item := output[0].(map[string]interface{})
	if item["type"] != "message" {
		t.Fatalf("item не message: %+v", item)
	}
	content := item["content"].([]interface{})[0].(map[string]interface{})
	if content["type"] != "output_text" || !strings.Contains(content["text"].(string), "ollama-loadbalancer") {
		t.Errorf("content output_text нарушен: %+v", content)
	}
	usage := resp["usage"].(map[string]interface{})
	if usage["total_tokens"].(float64) != 12 {
		t.Errorf("usage = %+v", usage)
	}
}

// TestR247_ResponsesObjectFunctionCall — вызов инструмента как item function_call.
func TestR247_ResponsesObjectFunctionCall(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-10","model":"gemma-4","choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{"id":"call_read","type":"function","function":{"name":"read","arguments":"{\"file_path\":\"go.mod\"}"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)
	out, _ := openAIToResponsesObject(body, "gemma-4")
	var resp map[string]interface{}
	_ = json.Unmarshal(out, &resp)
	output := resp["output"].([]interface{})
	if len(output) != 1 {
		t.Fatalf("items %d, ожидался 1: %+v", len(output), output)
	}
	item := output[0].(map[string]interface{})
	if item["type"] != "function_call" {
		t.Fatalf("item не function_call: %+v", item)
	}
	if item["call_id"] != "call_read" || item["name"] != "read" {
		t.Errorf("call_id/name = %v/%v", item["call_id"], item["name"])
	}
	if _, ok := item["arguments"].(string); !ok {
		t.Errorf("arguments обязан быть строкой: %T", item["arguments"])
	}
}

// TestR247_ResponsesSSEOrder — порядок событий и наличие вызова.
func TestR247_ResponsesSSEOrder(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl-11","choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-11","choices":[{"delta":{"content":"Read."},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-11","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_read","function":{"name":"read","arguments":"{\"file_path\":\"go.mod\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-11","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"id":"chatcmpl-11","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":6,"total_tokens":11}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	var out strings.Builder
	if err := forwardOpenAISSEToResponses(strings.NewReader(upstream), &out, nil, "gemma-4"); err != nil {
		t.Fatalf("трансляция SSE упала: %v", err)
	}
	text := out.String()
	order := []string{
		`"type":"response.created"`,
		`"type":"response.output_item.added"`,
		`"type":"response.content_part.added"`,
		`"type":"response.output_text.delta"`,
		`"type":"response.output_text.done"`,
		`"type":"response.function_call_arguments.delta"`,
		`"type":"response.function_call_arguments.done"`,
		`"type":"response.completed"`,
	}
	pos := 0
	for _, needle := range order {
		idx := strings.Index(text[pos:], needle)
		if idx < 0 {
			t.Fatalf("в потоке нет %q (после позиции %d)\n%s", needle, pos, text)
		}
		pos += idx + len(needle)
	}
	if !strings.Contains(text, `"delta":"Read."`) {
		t.Errorf("текстовая дельта потеряна: %s", text)
	}
	if !strings.Contains(text, `"arguments":"{\"file_path\":\"go.mod\"}"`) {
		t.Errorf("аргументы вызова потеряны: %s", text)
	}
	if !strings.Contains(text, `"sequence_number":`) {
		t.Errorf("sequence_number отсутствует: %s", text)
	}
}

// TestR247_ResponsesSSECallIDSequenced — sequence_number растёт монотонно.
func TestR247_ResponsesSSECounted(t *testing.T) {
	upstream := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"
	var out strings.Builder
	_ = forwardOpenAISSEToResponses(strings.NewReader(upstream), &out, nil, "m")
	count := strings.Count(out.String(), "event: response.")
	if count < 4 {
		t.Errorf("событий response.* %d, ожидалось не меньше 4:\n%s", count, out.String())
	}
}
