// nameless_call_r431_test.go — R431 (2026-11-05): вызов без поля `name`
// восстанавливается по `id` (`call_<инструмент>`) и вложенным аргументам.
//
// ФИКСТУРА — снятый живьём сырой вывод Cline в VS Code (gemma-4-E4B-it, стенд,
// 2026-11-05 04:27 UTC, /app/diag/RAW-20261105-042704.122-markup_stripped.txt):
// модель перечислила план прозой и в конце выдала
//
//	[TOOL_CALLS][{"id":"call_read_files","type":"function","function":{"files":["TASK.md"]}}]<channel|>
//
// Разбор до фикса давал 0 вызовов, клиент показывал «COMPLETED» и файл не
// создавался.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// toolsR431 — подмножество инструментов Cline, объявленное в том запросе.
func toolsR431() []openAITool {
	mk := func(name string, params string) openAITool {
		return openAITool{Type: "function", Function: openAIFunction{Name: name, Parameters: json.RawMessage(params)}}
	}
	return []openAITool{
		mk("read_files", `{"type":"object","properties":{"files":{"type":"array","items":{"type":"string"}}},"required":["files"]}`),
		mk("editor", `{"type":"object","properties":{"path":{"type":"string"},"new_text":{"type":"string"}},"required":["path","new_text"]}`),
		mk("run_commands", `{"type":"object","properties":{"commands":{"type":"array","items":{"type":"string"}}},"required":["commands"]}`),
		mk("ask_question", `{"type":"object","properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"}}}}`),
	}
}

// fixtureNamelessCallR431 — фрагмент живого вывода (план + вызов без name).
const fixtureNamelessCallR431 = "Начну с чтения файла `TASK.md`.\n\n" +
	`[TOOL_CALLS][{"id":"call_read_files","type":"function","function":{"files":["TASK.md"]}}]<channel|>`

// TestR431_NamelessCallFromRealCapture — живой вывод разбирается в вызов
// read_files с аргументом TASK.md (до фикса: 0 вызовов).
func TestR431_NamelessCallFromRealCapture(t *testing.T) {
	calls := parseToolCallsForRequest(fixtureNamelessCallR431, toolsR431())
	if len(calls) != 1 {
		t.Fatalf("вызовов=%d, ожидался 1 (форма из живого захвата не разобрана)", len(calls))
	}
	if calls[0].Function.Name != "read_files" {
		t.Fatalf("имя=%q, ожидалось read_files", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "TASK.md") {
		t.Fatalf("аргументы потеряли файл: %q", calls[0].Function.Arguments)
	}
}

// TestR431_NameMustBeDeclared — имя из `id` принимается только если инструмент
// объявлен в запросе: иначе вызов не выполнится у клиента, а «угадывать»
// инструмент по id нельзя.
func TestR431_NameMustBeDeclared(t *testing.T) {
	onlyEditor := []openAITool{toolsR431()[1]}
	if calls := parseToolCallsForRequest(fixtureNamelessCallR431, onlyEditor); len(calls) != 0 {
		t.Fatalf("вызов необъявленного инструмента принят: %+v", calls)
	}
}

// TestR431_NameAsKeyForm — вариант, где имя инструмента стоит ключом объекта.
func TestR431_NameAsKeyForm(t *testing.T) {
	raw := `[TOOL_CALLS][{"id":"call_1","type":"function","function":{"read_files":{"files":["README.md"]}}}]`
	calls := parseToolCallsForRequest(raw, toolsR431())
	if len(calls) != 1 || calls[0].Function.Name != "read_files" {
		t.Fatalf("форма с именем-ключом не разобрана: %+v", calls)
	}
}

// TestR431_ArgumentsObjectForm — аргументы отдельным полем `arguments`, имени нет.
func TestR431_ArgumentsObjectForm(t *testing.T) {
	raw := `{"id":"call_editor","type":"function","function":{"arguments":{"path":"a.txt","new_text":"ok"}}}`
	calls := parseToolCallsForRequest(raw, toolsR431())
	if len(calls) != 1 || calls[0].Function.Name != "editor" {
		t.Fatalf("форма с arguments не разобрана: %+v", calls)
	}
	if !strings.Contains(calls[0].Function.Arguments, "new_text") {
		t.Fatalf("аргументы потеряны: %q", calls[0].Function.Arguments)
	}
}

// TestR431_NoNameNoID — имени нет ни в id, ни ключом: не угадываем.
func TestR431_NoNameNoID(t *testing.T) {
	raw := `{"type":"function","function":{"files":["TASK.md"]}}`
	if calls := parseToolCallsForRequest(raw, toolsR431()); len(calls) != 0 {
		t.Fatalf("вызов без имени принят: %+v", calls)
	}
}

// TestR431_NormalOpenAIFormUntouched — полноценный вызов (name внутри function)
// этой веткой не переписывается.
func TestR431_NormalOpenAIFormUntouched(t *testing.T) {
	raw := `{"function":{"name":"editor","arguments":{"path":"a.txt","new_text":"ok"}}}`
	if call := namelessCallFromObjectR431(raw, declaredToolNamesR431(toolsR431())); call != nil {
		t.Fatalf("R431 перехватил полноценный вызов: %+v", call)
	}
	if calls := parseToolCallsForRequest(raw, toolsR431()); len(calls) != 1 || calls[0].Function.Name != "editor" {
		t.Fatalf("полноценный вызов перестал разбираться: %+v", calls)
	}
}

// TestR431_TrailingNumericIDSuffix — `call_editor_1` → `editor`.
func TestR431_TrailingNumericIDSuffix(t *testing.T) {
	declared := declaredToolNamesR431(toolsR431())
	for _, id := range []string{"call_editor", "call_editor_1", "call_editor-2", "editor"} {
		if got := declaredToolFromCallIDR431(id, declared); got != "editor" {
			t.Errorf("declaredToolFromCallIDR431(%q) = %q, ожидалось editor", id, got)
		}
	}
	if got := declaredToolFromCallIDR431("call_unknown_tool", declared); got != "" {
		t.Errorf("неизвестный id дал имя %q", got)
	}
}

// TestR431_ProseWithoutCallStaysProse — обычный текст с планом не превращается
// в вызов (иначе агент выполнит выдуманный инструмент).
func TestR431_ProseWithoutCallStaysProse(t *testing.T) {
	raw := "Вот план:\n1. Прочитать TASK.md.\n2. Создать файлы."
	if calls := parseToolCallsForRequest(raw, toolsR431()); len(calls) != 0 {
		t.Fatalf("проза принята за вызов: %+v", calls)
	}
}
