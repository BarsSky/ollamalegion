// ollama_toolcall_shape_r295_test.go — R295 (2026-10-29).
//
// Проверяется КОНТРАКТ OLLAMA-ПОВЕРХНОСТИ (порт 18080, /api/chat):
//  1. `function.arguments` — ОБЪЕКТ, а не строка (иначе Cline с ollama-провайдером
//     не может применить вызов; на /v1 строка остаётся правильной — это OpenAI);
//  2. `done:true` ставит только терминатор: чанк с content+finish_reason и
//     reasoning+finish_reason терминальным НЕ является (живая жалоба «получаю
//     флаг о завершении», после которого ещё идут кадры);
//  3. error-чанк посреди генерации не подменяет ход «концом ответа»: клиент
//     видит причину и получает один терминальный кадр.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// argsObjectOf — достать function.arguments как значение (для проверки типа).
func argsObjectOf(t *testing.T, raw []byte) (interface{}, map[string]interface{}) {
	t.Helper()
	var chunk map[string]interface{}
	if err := json.Unmarshal(raw, &chunk); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, string(raw))
	}
	msg, _ := chunk["message"].(map[string]interface{})
	tcs, _ := msg["tool_calls"].([]interface{})
	if len(tcs) == 0 {
		t.Fatalf("в чанке нет message.tool_calls: %s", string(raw))
	}
	tc, _ := tcs[0].(map[string]interface{})
	fn, _ := tc["function"].(map[string]interface{})
	return fn["arguments"], tc
}

// TestR295_ToolCallArgumentsAreObject_Stream — дельта role+tool_calls от воркера
// (аргументы строкой, форма OpenAI) → в NDJSON аргументы ОБЪЕКТ.
func TestR295_ToolCallArgumentsAreObject_Stream(t *testing.T) {
	chunk := []byte(`{"id":"c1","object":"chat.completion.chunk","model":"gemma-4","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_read","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"go.mod\",\"lines\":10}"}}]},"finish_reason":null}]}`)

	out := translateOpenAISSEDataToOllama("/api/chat", chunk, "gemma-4", nil, time.Time{}, time.Time{}, "")
	if len(out) == 0 {
		t.Fatal("перевод вернул пусто")
	}
	args, tc := argsObjectOf(t, out)

	if _, isString := args.(string); isString {
		t.Fatalf("R295: arguments ушли СТРОКОЙ (%v) — Ollama-клиент не применит вызов", args)
	}
	obj, ok := args.(map[string]interface{})
	if !ok {
		t.Fatalf("R295: arguments должны быть объектом, got %T", args)
	}
	if obj["path"] != "go.mod" {
		t.Errorf("R295: аргументы потеряли значения: %v", obj)
	}
	if got, _ := obj["lines"].(float64); got != 10 {
		t.Errorf("R295: lines = %v, want 10", obj["lines"])
	}
	if tc["type"] != "function" {
		t.Errorf("R295: type = %v, want function", tc["type"])
	}
	if tc["id"] != "call_read" {
		t.Errorf("R295: id вызова потерян: %v", tc["id"])
	}
}

// TestR295_ToolCallArgumentsAreObject_NonStream — тот же контракт на не-стриме.
func TestR295_ToolCallArgumentsAreObject_NonStream(t *testing.T) {
	body := []byte(`{"id":"x","model":"gemma-4","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_w","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"a.txt\",\"content\":\"hi\"}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)

	out, err := translateOpenAIChatToOllama(body, "gemma-4")
	if err != nil {
		t.Fatalf("translateOpenAIChatToOllama: %v", err)
	}
	args, _ := argsObjectOf(t, out)
	if _, isString := args.(string); isString {
		t.Fatalf("R295 non-stream: arguments ушли строкой (%v)", args)
	}
	if obj, ok := args.(map[string]interface{}); !ok || obj["content"] != "hi" {
		t.Fatalf("R295 non-stream: arguments должны быть объектом с content: %v", args)
	}
}

// TestR295_FlatDetectedCallNormalized — наш текстовый детектор отдаёт «плоскую»
// форму {name, arguments}; на выходе Ollama обязана быть полная форма function.
func TestR295_FlatDetectedCallNormalized(t *testing.T) {
	flat := []interface{}{
		map[string]interface{}{"name": "list_files", "arguments": `{"dir":"."}`},
	}
	out := toOllamaToolCallsObject(flat)
	if len(out) != 1 {
		t.Fatalf("ожидался 1 вызов, got %d", len(out))
	}
	tc := out[0].(map[string]interface{})
	fn, ok := tc["function"].(map[string]interface{})
	if !ok {
		t.Fatalf("R295: плоская форма не приведена к function: %v", tc)
	}
	if fn["name"] != "list_files" {
		t.Errorf("R295: имя потеряно: %v", fn["name"])
	}
	if _, isString := fn["arguments"].(string); isString {
		t.Errorf("R295: аргументы остались строкой: %v", fn["arguments"])
	}
}

// TestR295_InvalidArgumentsJSONKeptAsString — битый JSON аргументов не теряем
// и не роняем: отдаём строкой (клиент увидит вызов и сообщит об ошибке).
func TestR295_InvalidArgumentsJSONKeptAsString(t *testing.T) {
	flat := []interface{}{
		map[string]interface{}{"id": "c1", "function": map[string]interface{}{
			"name": "read_file", "arguments": `{path: broken`,
		}},
	}
	out := toOllamaToolCallsObject(flat)
	fn := out[0].(map[string]interface{})["function"].(map[string]interface{})
	if s, ok := fn["arguments"].(string); !ok || s != `{path: broken` {
		t.Errorf("R295: битые аргументы должны дойти строкой как есть, got %v", fn["arguments"])
	}
}

// TestR295_ContentWithFinishReasonIsNotTerminal — главный живой дефект:
// чанк с content+finish_reason больше не «конец ответа».
func TestR295_ContentWithFinishReasonIsNotTerminal(t *testing.T) {
	chunk := []byte(`{"model":"gemma-4","choices":[{"index":0,"delta":{"content":"финальный блок"},"finish_reason":"stop"}]}`)
	out := translateOpenAISSEDataToOllama("/api/chat", chunk, "gemma-4", nil, time.Time{}, time.Time{}, "")

	var c map[string]interface{}
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatalf("не JSON: %v", err)
	}
	if done, _ := c["done"].(bool); done {
		t.Fatalf("R295: чанк с content+finish_reason не должен быть терминальным: %s", string(out))
	}
	msg, _ := c["message"].(map[string]interface{})
	if msg["content"] != "финальный блок" {
		t.Errorf("R295: содержимое потерялось: %v", msg["content"])
	}
	if c["done_reason"] != nil {
		t.Errorf("R295: done_reason на нефинальном чанке не нужен: %v", c["done_reason"])
	}
}

// TestR295_ReasoningWithFinishReasonIsNotTerminal — то же для reasoning-чанка
// (gemma-4 присылает последний reasoning вместе с finish_reason).
func TestR295_ReasoningWithFinishReasonIsNotTerminal(t *testing.T) {
	chunk := []byte(`{"model":"gemma-4","choices":[{"index":0,"delta":{"reasoning_content":"последняя мысль"},"finish_reason":"stop"}]}`)
	out := translateOpenAISSEDataToOllama("/api/chat", chunk, "gemma-4", nil, time.Time{}, time.Time{}, "")

	var c map[string]interface{}
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatalf("не JSON: %v", err)
	}
	if done, _ := c["done"].(bool); done {
		t.Fatalf("R295: reasoning+finish_reason не должен быть терминальным: %s", string(out))
	}
}

// TestR295_ErrorChunkIsReportedThenTerminated — error посреди генерации:
// сначала кадр с причиной (done:false), затем ровно один терминатор.
func TestR295_ErrorChunkIsReportedThenTerminated(t *testing.T) {
	chunk := []byte(`{"model":"gemma-4","error":"bridge code 2: requested n_ctx=16384 exceeds model's effective n_ctx=8192"}`)
	out := translateOpenAISSEDataToOllama("/api/chat", chunk, "gemma-4", nil, time.Time{}, time.Time{}, "")

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("R295: ожидались 2 строки (причина + терминатор), got %d: %s", len(lines), string(out))
	}

	var first, second map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("первая строка не JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("вторая строка не JSON: %v", err)
	}

	if done, _ := first["done"].(bool); done {
		t.Errorf("R295: кадр с ошибкой не должен быть терминальным: %v", first)
	}
	if first["error"] == nil {
		t.Errorf("R295: причина ошибки потеряна: %v", first)
	}
	if done, _ := second["done"].(bool); !done {
		t.Errorf("R295: второй кадр обязан закрыть поток: %v", second)
	}
	if second["done_reason"] != "error" {
		t.Errorf("R295: done_reason = %v, want error", second["done_reason"])
	}
}
