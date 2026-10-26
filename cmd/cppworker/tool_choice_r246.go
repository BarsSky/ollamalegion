// tool_choice_r246.go — R246 (2026-10-26): вынужденный выбор инструмента
// доезжает до модели.
//
// ЖИВОЙ ДЕФЕКТ (проверка формы запросов разных клиентов, `_diag/clients-compat`
// на стенде): запрос с `tool_choice: {"type":"function","function":{"name":"read"}}`
// и нейтральной репликой «Привет.» возвращал обычный текст и НОЛЬ вызовов.
// Поле `tool_choice` принималось (R66b), но применялось только для `"none"`
// (отключить инструменты); `"required"` и вынужденная функция игнорировались.
//
// ЗАЧЕМ ЭТО КЛИЕНТАМ. Вынужденный вызов используют не «хотелки», а рабочие
// схемы: структурированный вывод LangChain (`with_structured_output`),
// MCP-мосты, плагины OpenAI SDK, сценарии «сначала вызови этот инструмент».
// Для них текст вместо вызова — неверный ответ, а не «модель передумала».
//
// РЕШЕНИЕ. Требование уходит в промпт (soft-инструкция) — работает для ЛЮБОЙ
// модели и любого шаблона, в отличие от нативного `tool_choice` в Jinja,
// которого у нас в C-мосте нет (tools рисуются нашей преамбулой).
package main

import (
	"encoding/json"
	"strings"
)

// toolChoiceDirective — ограничение выбора инструмента из запроса клиента.
type toolChoiceDirective struct {
	// Forced — имя инструмента, который ОБЯЗАН быть вызван ("" — не задано).
	Forced string
	// Required — обязателен любой инструмент (tool_choice:"required").
	Required bool
}

// parseToolChoice разбирает поле tool_choice в любой из форм:
//
//	"auto" | "none" | "required"
//	{"type":"function","function":{"name":"read"}}
//	{"type":"function","function":{"name":"read"}} (Ollama-совместимая форма)
//
// Неизвестные формы = «не задано»: выдумывать ограничение нельзя.
func parseToolChoice(choice json.RawMessage) toolChoiceDirective {
	raw := strings.TrimSpace(string(choice))
	if raw == "" || raw == "null" {
		return toolChoiceDirective{}
	}
	var word string
	if err := json.Unmarshal(choice, &word); err == nil {
		if strings.EqualFold(strings.TrimSpace(word), "required") {
			return toolChoiceDirective{Required: true}
		}
		return toolChoiceDirective{} // auto / none: ограничения нет
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(choice, &obj); err != nil {
		return toolChoiceDirective{}
	}
	name := strings.TrimSpace(obj.Function.Name)
	if name == "" {
		name = strings.TrimSpace(obj.Name)
	}
	if !strings.EqualFold(strings.TrimSpace(obj.Type), "function") || name == "" {
		return toolChoiceDirective{}
	}
	return toolChoiceDirective{Forced: name}
}

// instruction — текст требования для системного промпта ("" — требовать нечего).
func (d toolChoiceDirective) instruction() string {
	switch {
	case d.Forced != "":
		return "YOU MUST call the tool \"" + d.Forced + "\" now. " +
			"Do not answer with text, do not explain: emit the tool call only."
	case d.Required:
		return "YOU MUST call one of the available tools now. " +
			"Do not answer with text, do not explain: emit a tool call only."
	}
	return ""
}

// withToolChoiceInstruction — добавить требование к преамбуле инструментов.
func withToolChoiceInstruction(toolsPrompt string, choice json.RawMessage) string {
	instr := parseToolChoice(choice).instruction()
	if instr == "" {
		return toolsPrompt
	}
	if strings.TrimSpace(toolsPrompt) == "" {
		return instr
	}
	return toolsPrompt + "\n\n" + instr
}
