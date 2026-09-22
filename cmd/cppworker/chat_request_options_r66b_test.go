package main

// chat_request_options_r66b_test.go — R66b (2026-09-22): тесты обработки
// per-request полей Ollama /api/chat `think` и `format`, которые раньше
// принимались и молча игнорировались.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEffectiveReasoningEnabled — приоритет per-request think над глобальным
// config.EnableReasoning.
func TestEffectiveReasoningEnabled(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name          string
		override      *bool
		configEnabled bool
		want          bool
	}{
		{"no override, config on", nil, true, true},
		{"no override, config off", nil, false, false},
		{"think=true overrides config off", &yes, false, true},
		{"think=false overrides config on", &no, true, false},
		{"think=true with config on", &yes, true, true},
		{"think=false with config off", &no, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveReasoningEnabled(chatPromptOptions{ReasoningOverride: tc.override}, tc.configEnabled)
			if got != tc.want {
				t.Errorf("effectiveReasoningEnabled = %v, ожидалось %v", got, tc.want)
			}
		})
	}
}

// TestJSONSchemaText — извлечение схемы из поля format.
func TestJSONSchemaText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"plain json", `"json"`, ""},
		{"empty string", `""`, ""},
		{"nil", ``, ""},
		{"object schema", `{"type":"object","properties":{"a":{"type":"string"}}}`, `{"type":"object","properties":{"a":{"type":"string"}}}`},
		{"array schema", `[{"type":"string"}]`, `[{"type":"string"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jsonSchemaText(json.RawMessage(tc.raw))
			if got != tc.want {
				t.Errorf("jsonSchemaText(%q) = %q, ожидалось %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestInjectJSONInstruction — инструкция должна требовать валидный JSON и
// содержать схему, если она передана.
func TestInjectJSONInstruction(t *testing.T) {
	base := injectJSONInstruction("Ты — ассистент.", LangEN, "")
	if !strings.Contains(base, "VALID JSON") {
		t.Errorf("нет требования валидного JSON: %s", base)
	}
	if !strings.Contains(base, "Ты — ассистент.") {
		t.Errorf("исходный system-промпт потерян: %s", base)
	}

	schema := `{"type":"object","properties":{"name":{"type":"string"}}}`
	withSchema := injectJSONInstruction("", LangRU, schema)
	if !strings.Contains(withSchema, schema) {
		t.Errorf("схема не попала в инструкцию: %s", withSchema)
	}
	if !strings.Contains(withSchema, "JSON") {
		t.Errorf("нет упоминания JSON: %s", withSchema)
	}
}

// TestStripJSONFence — снятие markdown-обёртки вокруг JSON-ответа.
func TestStripJSONFence(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain json untouched", `{"a":1}`, `{"a":1}`},
		{"fenced with lang", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"fenced without lang", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"unclosed fence", "```json\n{\"a\":1}", `{"a":1}`},
		{"one line fenced", "```json {\"a\":1}```", `{"a":1}`},
		{"short explanation prefix", "Вот результат:\n```json\n{\"a\":1}\n```", `{"a":1}`},
		{"array fenced", "```json\n[1,2,3]\n```", `[1,2,3]`},
		{"nested braces and strings", "```json\n{\"a\":\"}{ not a brace\",\"b\":{\"c\":1}}\n```", `{"a":"}{ not a brace","b":{"c":1}}`},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripJSONFence(tc.in)
			if got != tc.want {
				t.Errorf("stripJSONFence(%q) = %q, ожидалось %q", tc.in, got, tc.want)
			}
			// Результат не должен содержать markdown-заборов и должен быть
			// валидным JSON (кроме пустого входа).
			if strings.Contains(got, "```") {
				t.Errorf("в результате остался markdown-забор: %q", got)
			}
			if got != "" && !json.Valid([]byte(got)) {
				t.Errorf("результат не валидный JSON: %q", got)
			}
		})
	}
}

// TestUpsertSystemMessage — R66b: итоговый system (с JSON/thinking-инструкциями)
// обязан попадать в messages, иначе Jinja-шаблон его не увидит.
func TestUpsertSystemMessage(t *testing.T) {
	t.Run("prepends when absent", func(t *testing.T) {
		msgs := []chatMessage{{Role: "user", Content: "hi"}}
		out := upsertSystemMessage(msgs, "SYS")
		if len(out) != 2 || out[0].Role != "system" || out[0].Content != "SYS" {
			t.Fatalf("ожидался system-месседж в начале, получено %+v", out)
		}
		if out[1].Role != "user" || out[1].Content != "hi" {
			t.Errorf("исходное сообщение потеряно: %+v", out)
		}
		if msgs[0].Role != "user" || len(msgs) != 1 {
			t.Errorf("исходный срез не должен мутироваться: %+v", msgs)
		}
	})

	t.Run("replaces existing", func(t *testing.T) {
		msgs := []chatMessage{
			{Role: "system", Content: "OLD"},
			{Role: "user", Content: "hi"},
		}
		out := upsertSystemMessage(msgs, "NEW")
		if len(out) != 2 {
			t.Fatalf("количество сообщений изменилось: %+v", out)
		}
		if out[0].Content != "NEW" {
			t.Errorf("system не заменён: %+v", out[0])
		}
	})

	t.Run("empty system is a no-op", func(t *testing.T) {
		msgs := []chatMessage{{Role: "user", Content: "hi"}}
		out := upsertSystemMessage(msgs, "   ")
		if len(out) != 1 || out[0].Role != "user" {
			t.Errorf("пустой system не должен добавлять сообщение: %+v", out)
		}
	})
}

// TestThinkEnabledFromRaw_Values — think принимает bool и строковые уровни.
func TestThinkEnabledFromRaw_Values(t *testing.T) {
	cases := []struct {
		raw      string
		enabled  bool
		explicit bool
	}{
		{`true`, true, true},
		{`false`, false, true},
		{`"true"`, true, true},
		{`"low"`, true, true},
		{`"medium"`, true, true},
		{`"high"`, true, true},
		{`"off"`, false, true},
		{`"none"`, false, true},
		{``, false, false},
		{`null`, false, false},
	}
	for _, tc := range cases {
		en, ex := thinkEnabledFromRaw(json.RawMessage(tc.raw))
		if en != tc.enabled || ex != tc.explicit {
			t.Errorf("thinkEnabledFromRaw(%q) = (%v,%v), ожидалось (%v,%v)",
				tc.raw, en, ex, tc.enabled, tc.explicit)
		}
	}
}
