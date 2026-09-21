//go:build llama_stub

// toolcall_nested_args_r65d_test.go — R65d (2026-09-20): регрессия для
// tool_calls с ВЛОЖЕННЫМИ аргументами и для типа function.arguments.
//
// Два дефекта, найденных аудитом:
//
//  1. hermesToolCallRegex использовал ленивую `\{.*?\}+`, поэтому для
//     аргументов с вложенными объектами
//     {"name":"edit","arguments":{"file":"a","opts":{"x":1,"y":2}}}
//     матч обрывался на первой внутренней '}' → JSON невалиден →
//     parseHermesToOpenAI возвращал nil → TOOL CALL ТЕРЯЛСЯ МОЛЧА.
//     Такие аргументы типичны для агентных сценариев (Cline/Roo): правка
//     файлов, многошаговые планы, вложенные параметры.
//
//  2. function.arguments оставался JSON-объектом, а не строкой. OpenAI-клиенты
//     (openai-python, openai-node) делают json.loads(arguments) и на объекте
//     получают TypeError. Cppworker для этого имеет stringifyArguments,
//     балансер его обходил.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// assertArgumentsIsString проверяет, что у tool call поле
// function.arguments — строка с валидным JSON внутри.
func assertArgumentsIsString(t *testing.T, tc interface{}) {
	t.Helper()
	m, ok := tc.(map[string]interface{})
	if !ok {
		t.Fatalf("tool call is not a map: %T", tc)
	}
	fn, ok := m["function"].(map[string]interface{})
	if !ok {
		t.Fatalf("tool call has no function object: %v", m)
	}
	args, ok := fn["arguments"]
	if !ok {
		t.Fatalf("function has no arguments: %v", fn)
	}
	s, isString := args.(string)
	if !isString {
		t.Fatalf("function.arguments must be a JSON STRING (openai-python calls json.loads), got %T: %v",
			args, args)
	}
	if !json.Valid([]byte(s)) {
		t.Errorf("function.arguments is not valid JSON: %q", s)
	}
}

// TestR65d_HermesToolCall_NestedArguments_Preserved — вложенные объекты
// в аргументах не должны обрывать парсинг.
func TestR65d_HermesToolCall_NestedArguments_Preserved(t *testing.T) {
	content := `<tool_call>{"name":"edit_file","arguments":{"file":"main.go","opts":{"backup":true,"encoding":"utf-8"},"edits":[{"line":1,"text":"a"}]}}</tool_call>`

	calls, remaining, found := detectHermesToolCallsInContent(content)
	if !found {
		t.Fatalf("tool call with nested arguments NOT detected (regression). content=%q", content)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d: %v", len(calls), calls)
	}

	tc := calls[0].(map[string]interface{})
	fn := tc["function"].(map[string]interface{})
	if got := fn["name"]; got != "edit_file" {
		t.Errorf("function.name = %v, want edit_file", got)
	}
	assertArgumentsIsString(t, calls[0])

	// Проверяем, что вложенные данные ДОЕХАЛИ, а не потерялись при обрезке.
	args := fn["arguments"].(string)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("cannot parse arguments: %v (%s)", err, args)
	}
	opts, ok := parsed["opts"].(map[string]interface{})
	if !ok {
		t.Fatalf("nested object 'opts' lost: %v", parsed)
	}
	if opts["encoding"] != "utf-8" {
		t.Errorf("nested value lost: opts.encoding = %v, want utf-8", opts["encoding"])
	}
	edits, ok := parsed["edits"].([]interface{})
	if !ok || len(edits) != 1 {
		t.Errorf("nested array 'edits' lost or truncated: %v", parsed["edits"])
	}

	if strings.Contains(remaining, "edit_file") {
		t.Errorf("tool call JSON should be removed from remaining content, got %q", remaining)
	}
}

// TestR65d_HermesToolCall_MultipleNestedCalls — несколько tool calls с
// вложенными аргументами в одном ответе (параллельный function calling,
// типичный для Cline/Roo).
func TestR65d_HermesToolCall_MultipleNestedCalls(t *testing.T) {
	content := `<tool_call>{"name":"read_file","arguments":{"path":"a.go"}}</tool_call>` +
		`<tool_call>{"name":"write_file","arguments":{"path":"b.go","meta":{"mode":420,"tags":["x","y"]}}}</tool_call>`

	calls, _, found := detectHermesToolCallsInContent(content)
	if !found {
		t.Fatal("multiple tool calls NOT detected")
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d: %v", len(calls), calls)
	}
	for i, tc := range calls {
		assertArgumentsIsString(t, tc)
		_ = i
	}
	// Второй вызов содержит вложенный объект — проверяем, что он цел.
	second := calls[1].(map[string]interface{})["function"].(map[string]interface{})
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(second["arguments"].(string)), &parsed); err != nil {
		t.Fatalf("second call arguments invalid: %v", err)
	}
	if _, ok := parsed["meta"].(map[string]interface{}); !ok {
		t.Errorf("nested 'meta' lost in second call: %v", parsed)
	}
}

// TestR65d_ScanBalancedJSON — юнит-тесты depth-aware сканера.
func TestR65d_ScanBalancedJSON(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		openPos  int
		wantEnd  int
		wantNone bool
	}{
		{"simple object", `{"a":1}`, 0, 6, false},
		{"nested object", `{"a":{"b":2}}`, 0, 12, false},
		{"deeply nested", `{"a":{"b":{"c":[1,2,{"d":3}]}}}`, 0, 30, false},
		{"array", `[1,2,3]`, 0, 6, false},
		{"nested array in object", `{"x":[{"y":1}]}`, 0, 14, false},
		// Скобки внутри строк НЕ должны влиять на глубину.
		{"braces in string", `{"a":"}}}"}`, 0, 10, false},
		{"bracket in string", `{"a":"]]]"}`, 0, 10, false},
		// Экранированная кавычка не должна закрывать строку.
		{"escaped quote", `{"a":"x\"}y"}`, 0, 12, false},
		// Незакрытая конструкция → -1.
		{"unterminated", `{"a":{"b":1}`, 0, 0, true},
		{"not json start", `abc`, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanBalancedJSON(tc.input, tc.openPos)
			if tc.wantNone {
				if got != -1 {
					t.Errorf("scanBalancedJSON(%q) = %d, want -1", tc.input, got)
				}
				return
			}
			if got != tc.wantEnd {
				t.Errorf("scanBalancedJSON(%q) = %d, want %d", tc.input, got, tc.wantEnd)
			}
			// Ключевая инварианта: срез обязан быть валидным JSON.
			if got >= 0 && !json.Valid([]byte(tc.input[tc.openPos:got+1])) {
				t.Errorf("scanned slice is not valid JSON: %q", tc.input[tc.openPos:got+1])
			}
		})
	}
}

// TestR65d_StringifyToolArguments — приведение arguments к строке.
func TestR65d_StringifyToolArguments(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"object", map[string]interface{}{"a": 1.0}, `{"a":1}`},
		{"nested", map[string]interface{}{"a": map[string]interface{}{"b": 1.0}}, `{"a":{"b":1}}`},
		// Строка с валидным JSON — НЕ сериализуем повторно (иначе Cline/Roo
		// получат "невалидный JSON": экранированные кавычки).
		{"already stringified", `{"a":1}`, `{"a":1}`},
		{"empty string", "", "{}"},
		{"whitespace string", "   ", "{}"},
		{"nil", nil, "{}"},
		{"bare number", 42.0, "42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stringifyToolArguments(tc.in)
			if got != tc.want {
				t.Errorf("stringifyToolArguments(%#v) = %q, want %q", tc.in, got, tc.want)
			}
			if !json.Valid([]byte(got)) {
				t.Errorf("result is not valid JSON: %q", got)
			}
		})
	}
}

// TestR65d_OpenAIToolCallShape_IsClientCompatible — финальная проверка формы:
// то, что уходит клиенту, должно проходить проверку как настоящий
// OpenAI tool_call (id/type/function.name/function.arguments-as-string).
func TestR65d_OpenAIToolCallShape_IsClientCompatible(t *testing.T) {
	content := `<tool_call>{"name":"search","arguments":{"query":"nested","filters":{"lang":"go"}}}</tool_call>`
	calls, _, found := detectHermesToolCallsInContent(content)
	if !found || len(calls) == 0 {
		t.Fatal("tool call not detected")
	}

	// Сериализуем как это делает NDJSON-ответ для клиента.
	msg := map[string]interface{}{
		"role":       "assistant",
		"content":    "",
		"tool_calls": calls,
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Разбираем как клиент: имитируем типичный парсер OpenAI SDK.
	var parsed struct {
		ToolCalls []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"` // ДОЛЖНО быть строкой
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("client-side decode failed (arguments not a string?): %v\nraw=%s", err, raw)
	}
	if len(parsed.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(parsed.ToolCalls))
	}
	tc := parsed.ToolCalls[0]
	if tc.Function.Name != "search" {
		t.Errorf("function.name = %q, want search", tc.Function.Name)
	}
	// openai-python делает ровно это:
	var argsMap map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &argsMap); err != nil {
		t.Fatalf("json.loads(arguments) failed — клиент получит TypeError: %v", err)
	}
	if argsMap["query"] != "nested" {
		t.Errorf("arguments round-trip lost data: %v", argsMap)
	}
}
