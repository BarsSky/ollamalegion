//go:build llama_stub

// tool_history_callformat_r475_test.go — R475 (2026-11-07): история вызовов
// показывается модели в форме, которую требует шаблон.
//
// ЖИВОЙ ДЕФЕКТ: в промпте Qwen3.8 (Cline, 6 инструментов) все 13 вызовов в
// истории были JSON-дампом `[[{…}]]` в тексте assistant-сообщения, хотя шаблон
// модели предписывает `<tool_call><function=…><parameter=…>`. Модель повторяла
// дамп: сначала всем блоком (7 КБ кода уехали ответом), затем — командой в
// fence вместо вызова run_commands.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// qwenCapsR475 — возможности шаблона Qwen3 (фрагмент реального
// tokenizer.chat_template из GGUF: `<function=` + `<parameter=`).
func qwenCapsR475() templateToolCaps {
	return analyzeTemplateToolCaps(strings.Join([]string{
		"{%- if message.role == \"tool\" %}",
		"{{- '<|im_start|>user\\n<tool_response>\\n' }}",
		"{%- if message.tool_calls and message.tool_calls is iterable %}",
		"{{- '<tool_call>\\n<function=' + tool_call.name + '>\\n' }}",
		"{{- '<parameter=' + args_name + '>\\n' }}",
		"{{- '</function>\\n</tool_call>' }}",
	}, "\n"))
}

// gemmaCapsR475 — шаблон без формы `<function=…>`: там остаётся прежний дамп.
func gemmaCapsR475() templateToolCaps {
	return analyzeTemplateToolCaps(strings.Join([]string{
		"{%- for message in messages %}",
		"{%- if message['role'] != 'tool' %}",
		"{{ message['content'] }}",
		"{%- if message['tool_calls'] %}{{ message['tool_calls'] }}{% endif %}",
		"{%- endif %}{%- endfor %}",
	}, "\n"))
}

func editorCallR475(t *testing.T) ollamaToolCall {
	t.Helper()
	return ollamaToolCall{
		ID:   "call_editor",
		Type: "function",
		Function: ollamaToolCallFunction{
			Name:      "editor",
			Arguments: json.RawMessage(`{"insert_line":132,"new_text":"function delay(ms = 300) {\n  return new Promise(resolve => setTimeout(resolve, ms));\n}","path":"d:\\cline-e2e-web\\public\\app.js"}`),
		},
	}
}

// TestR475_FunctionTagCapsDetected — форма шаблона определяется по его тексту.
func TestR475_FunctionTagCapsDetected(t *testing.T) {
	if !qwenCapsR475().FunctionTagCalls {
		t.Fatal("шаблон с <function=/<parameter= не распознан как форма <tool_call>")
	}
	if gemmaCapsR475().FunctionTagCalls {
		t.Fatal("шаблон без <function= ошибочно распознан как форма <tool_call>")
	}
}

// TestR475_HistoryShowsNativeCallForm — в истории вызов отдаётся в форме
// шаблона: <tool_call> + <function=editor> + <parameter=…>, без `[[{`.
func TestR475_HistoryShowsNativeCallForm(t *testing.T) {
	msgs := []chatMessage{{Role: "assistant", ToolCalls: []ollamaToolCall{editorCallR475(t)}}}
	history := buildLegacyToolHistory(msgs, qwenCapsR475())
	if len(history) != 1 {
		t.Fatalf("ожидалось одно сообщение, получено %d", len(history))
	}
	content := history[0].Content
	for _, want := range []string{"<tool_call>", "<function=editor>", "<parameter=insert_line>", "</function>", "</tool_call>"} {
		if !strings.Contains(content, want) {
			t.Errorf("в форме вызова нет %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "[[{") {
		t.Errorf("в истории остался JSON-дамп [[{:\n%s", content)
	}
	// Значение строки — без JSON-кавычек, иначе шаблон покажет модели \"…\".
	if !strings.Contains(content, "function delay(ms = 300) {") {
		t.Errorf("тело аргумента экранировано:\n%s", content)
	}
}

// TestR475_ArgsOrderPreserved — порядок параметров тот же, что прислал клиент.
func TestR475_ArgsOrderPreserved(t *testing.T) {
	msgs := []chatMessage{{Role: "assistant", ToolCalls: []ollamaToolCall{editorCallR475(t)}}}
	content := buildLegacyToolHistory(msgs, qwenCapsR475())[0].Content
	idxLine := strings.Index(content, "<parameter=insert_line>")
	idxText := strings.Index(content, "<parameter=new_text>")
	idxPath := strings.Index(content, "<parameter=path>")
	if idxLine < 0 || idxText < 0 || idxPath < 0 {
		t.Fatalf("не все параметры в форме вызова:\n%s", content)
	}
	if !(idxLine < idxText && idxText < idxPath) {
		t.Errorf("порядок параметров изменён (insert_line=%d, new_text=%d, path=%d)", idxLine, idxText, idxPath)
	}
}

// TestR475_LegacyFormKeptWithoutFunctionTags — шаблоны без формы <function=…
// продолжают получать прежний дамп: менять их поведение незачем.
func TestR475_LegacyFormKeptWithoutFunctionTags(t *testing.T) {
	msgs := []chatMessage{{Role: "assistant", ToolCalls: []ollamaToolCall{editorCallR475(t)}}}
	content := buildLegacyToolHistory(msgs, gemmaCapsR475())[0].Content
	if !strings.Contains(content, "[[{") {
		t.Fatalf("прежняя форма дампа потеряна:\n%s", content)
	}
}

// TestR475_UnparsableArgsFallBack — аргументы не объектом (например строка)
// не превращаются в битую форму шаблона: остаётся прежний дамп.
func TestR475_UnparsableArgsFallBack(t *testing.T) {
	calls := []ollamaToolCall{{
		Function: ollamaToolCallFunction{Name: "editor", Arguments: json.RawMessage(`"not-an-object"`)},
	}}
	msgs := []chatMessage{{Role: "assistant", ToolCalls: calls}}
	content := buildLegacyToolHistory(msgs, qwenCapsR475())[0].Content
	if strings.Contains(content, "<function=editor>") {
		t.Fatalf("битые аргументы ушли в форму шаблона:\n%s", content)
	}
	if !strings.Contains(content, "[[{") {
		t.Fatalf("нет фолбэка на прежний дамп:\n%s", content)
	}
}
