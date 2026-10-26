// tool_schema_validate_r247_test.go — R247, этап A: проверка аргументов по
// ОБЪЯВЛЕННОЙ схеме для инструментов с strict:true.
//
// ЖИВОЙ КОНТЕКСТ. Клиенты structured-output (OpenAI SDK, LangChain, Pydantic-AI,
// Continue) объявляют `strict:true` и считают невалидным ответ, где аргументы не
// соответствуют схеме. До R247 ремонт был эвристическим, и часть нарушений
// уезжала клиенту: он отвечал ошибкой инструмента, агент тратил ход.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func strictTool(t *testing.T, name string, schema string) openAITool {
	t.Helper()
	return openAITool{
		Type: "function",
		Function: openAIFunction{
			Name:       name,
			Parameters: json.RawMessage(schema),
			Strict:     true,
		},
	}
}

func callR247(name, args string) openAIToolCall {
	return openAIToolCall{Type: "function", Function: openAIFunctionCall{Name: name, Arguments: args}}
}

// TestR247_StrictSchemaDetection — strict читается из function.strict и из схемы.
func TestR247_StrictSchemaDetection(t *testing.T) {
	fromField := strictTool(t, "a", `{"type":"object","properties":{}}`)
	if len(strictToolSchemas([]openAITool{fromField})) != 1 {
		t.Error("strict из function.strict не распознан")
	}
	fromSchema := openAITool{
		Type: "function",
		Function: openAIFunction{
			Name:       "b",
			Parameters: json.RawMessage(`{"type":"object","strict":true,"properties":{}}`),
		},
	}
	if len(strictToolSchemas([]openAITool{fromSchema})) != 1 {
		t.Error("strict из схемы параметров не распознан")
	}
	plain := openAITool{Type: "function", Function: openAIFunction{Name: "c", Parameters: json.RawMessage(`{"type":"object"}`)}}
	if len(strictToolSchemas([]openAITool{plain})) != 0 {
		t.Error("не-strict инструмент попал в strict-набор")
	}
}

// TestR247_StrictViolations — таблица нарушений по схеме.
func TestR247_StrictViolations(t *testing.T) {
	schema := `{
		"type":"object","additionalProperties":false,
		"properties":{
			"mode":{"type":"string","enum":["fast","safe"]},
			"level":{"type":"integer"},
			"tags":{"type":"array","items":{"type":"string"}},
			"nested":{"type":"object","additionalProperties":false,
				"properties":{"kind":{"type":"string"}},"required":["kind"]}
		},
		"required":["mode"]
	}`
	tool := strictTool(t, "set_mode", schema)

	cases := []struct {
		name string
		args string
		want []string // подстроки ожидаемых нарушений; пусто = валидно
	}{
		{"валидно", `{"mode":"safe"}`, nil},
		{"валидно с вложенным", `{"mode":"fast","nested":{"kind":"x"},"tags":["a"],"level":2}`, nil},
		{"нет обязательного", `{}`, []string{"mode: required field missing"}},
		{"enum", `{"mode":"turbo"}`, []string{"mode: значение вне enum"}},
		{"тип integer", `{"mode":"safe","level":1.5}`, []string{"level: expected integer"}},
		{"тип integer из строки", `{"mode":"safe","level":"2"}`, []string{"level: expected integer"}},
		{"элемент массива", `{"mode":"safe","tags":["a",3]}`, []string{"tags[1]: expected string"}},
		{"лишнее поле", `{"mode":"safe","extra":1}`, []string{"extra: additional property not allowed"}},
		{"вложенное лишнее", `{"mode":"safe","nested":{"kind":"x","bad":1}}`, []string{"bad: additional property not allowed"}},
		{"вложенное обязательное", `{"mode":"safe","nested":{}}`, []string{"nested.kind: required field missing"}},
		{"не JSON", `not-json`, []string{"arguments is not valid JSON"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strictToolCallViolations([]openAIToolCall{callR247("set_mode", tc.args)}, []openAITool{tool})
			if len(tc.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("ожидалась валидность, получены нарушения: %v", got)
				}
				return
			}
			joined := strings.Join(got, " | ")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("нет нарушения %q в %q", want, joined)
				}
			}
		})
	}
}

// TestR247_NonStrictToolNotValidated — для не-strict инструментов нарушений нет.
func TestR247_NonStrictToolNotValidated(t *testing.T) {
	tool := openAITool{
		Type:     "function",
		Function: openAIFunction{Name: "plain", Parameters: json.RawMessage(`{"type":"object","required":["x"],"properties":{"x":{"type":"string"}}}`)},
	}
	if got := strictToolCallViolations([]openAIToolCall{callR247("plain", `{}`)}, []openAITool{tool}); len(got) != 0 {
		t.Fatalf("не-strict инструмент проверяется: %v", got)
	}
}

// TestR247_AnyOfAccepted — ветка anyOf принимается, если проходит хоть одна.
func TestR247_AnyOfAccepted(t *testing.T) {
	tool := strictTool(t, "flex", `{"type":"object","properties":{"v":{"anyOf":[{"type":"string"},{"type":"integer"}]}},"required":["v"]}`)
	if got := strictToolCallViolations([]openAIToolCall{callR247("flex", `{"v":5}`)}, []openAITool{tool}); len(got) != 0 {
		t.Fatalf("валидное по anyOf значение отвергнуто: %v", got)
	}
	if got := strictToolCallViolations([]openAIToolCall{callR247("flex", `{"v":true}`)}, []openAITool{tool}); len(got) == 0 {
		t.Fatal("невалидное по anyOf значение принято")
	}
}

// TestR247_RetrySwitchDefaultOn — выключатель повтора по умолчанию включён.
func TestR247_RetrySwitchDefaultOn(t *testing.T) {
	t.Setenv("CPPWORKER_STRICT_ARGS_RETRY", "")
	if !strictArgsRetryEnabled() {
		t.Error("по умолчанию повтор должен быть включён")
	}
	t.Setenv("CPPWORKER_STRICT_ARGS_RETRY", "off")
	if strictArgsRetryEnabled() {
		t.Error("CPPWORKER_STRICT_ARGS_RETRY=off не выключает повтор")
	}
}

// TestR247_AppendToolRepairInstruction — nudge-повтор видит, что именно не приняли.
func TestR247_AppendToolRepairInstruction(t *testing.T) {
	msgs := []chatMessage{{Role: "system", Content: "base"}, {Role: "user", Content: "task"}}
	out := appendToolRepairInstruction(msgs, []string{"read: arguments.file_path: required field missing"})
	if len(out) != 2 {
		t.Fatalf("сообщений %d, ожидалось 2", len(out))
	}
	if !strings.Contains(out[0].Content, "base") {
		t.Errorf("базовый system потерян: %q", out[0].Content)
	}
	if !strings.Contains(out[0].Content, "required field missing") {
		t.Errorf("перечень нарушений не попал в system: %q", out[0].Content)
	}
	if !strings.Contains(out[0].Content, "satisfy the schema exactly") {
		t.Errorf("нет требования схемы: %q", out[0].Content)
	}
	// Исходный срез не мутирован (повтор не должен менять историю запроса).
	if strings.Contains(msgs[0].Content, "required field missing") {
		t.Error("appendToolRepairInstruction мутировал исходные сообщения")
	}
	// Без system-сообщения инструкция вставляется первой.
	noSystem := appendToolRepairInstruction([]chatMessage{{Role: "user", Content: "task"}}, []string{"x"})
	if len(noSystem) != 2 || noSystem[0].Role != "system" {
		t.Errorf("без system инструкция не вставлена первой: %+v", noSystem)
	}
	// Пустые нарушения — без изменений.
	if got := appendToolRepairInstruction(msgs, nil); len(got) != 2 {
		t.Errorf("пустые нарушения изменили историю: %+v", got)
	}
}
