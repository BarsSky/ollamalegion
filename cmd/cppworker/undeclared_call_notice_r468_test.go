//go:build llama_stub

// undeclared_call_notice_r468_test.go — R468 (2026-11-06): «вызов не выполнен»
// называет НАСТОЯЩУЮ причину.
//
// ЖИВОЙ АРТЕФАКТ (стенд, /app/diag/RAW-20261106-153057.913-markup_stripped.txt,
// Cline + gemma-4-E4B, /api/chat):
//
//	<|tool_call>call:editor{new_text:<|"|>…код…<|"|>,path:<|"|>d:\cline-e2e-web\app.js<|"|>}<tool_call|>
//
// В запросе было объявлено 5 инструментов —
// read_files, search_codebase, fetch_web_content, ask_question, run_commands —
// БЕЗ editor (это Plan mode Cline). Парсер вызов РАЗОБРАЛ, R428 его отбросил, а
// клиенту уходило «Модель вызвала инструмент в нераспознанном формате» —
// ложная причина: формат распознан.
package main

import (
	"strings"
	"testing"
)

// liveNativeEditorCallR468 — вызов из живого артефакта, сокращённый до
// существенного (имя + оба аргумента + обёртка). Форма та же: native gemma,
// разделители <|"|>.
const liveNativeEditorCallR468 = `<|tool_call>call:editor{new_text:<|"|>function initialize() {
    updateMetrics();
    renderModels(DEMO_DATA.metrics.models);
    drawBarChart();
    console.log("Dashboard initialized successfully.");
}
document.addEventListener('DOMContentLoaded', initialize);<|"|>,path:<|"|>d:\cline-e2e-web\app.js<|"|>}<tool_call|>`

// clinePlanModeToolsR468 — ровно те 5 инструментов, что были объявлены в живом
// запросе (из unparsed-tool-calls.jsonl: tool_names).
func clinePlanModeToolsR468() []openAITool {
	names := []string{"read_files", "search_codebase", "fetch_web_content", "ask_question", "run_commands"}
	out := make([]openAITool, 0, len(names))
	for _, n := range names {
		out = append(out, openAITool{Type: "function", Function: openAIFunction{Name: n}})
	}
	return out
}

func clineActModeToolsR468() []openAITool {
	return append(clinePlanModeToolsR468(),
		openAITool{Type: "function", Function: openAIFunction{Name: "editor"}})
}

// TestR468_RecognizedButUndeclaredIsNotUnparsed — ровно живой случай: вызов
// распознан, инструмент не объявлен. Текст обязан это сказать.
func TestR468_RecognizedButUndeclaredIsNotUnparsed(t *testing.T) {
	plan := clinePlanModeToolsR468()

	// Поведение фильтра не меняется: вызов отброшен (выполнять необъявленный нельзя).
	if got := parseToolCallsFromOutputWithTools(liveNativeEditorCallR468, plan); len(got) != 0 {
		t.Fatalf("вызов необъявленного инструмента выполнять нельзя, получено %d вызовов", len(got))
	}
	// Но причина названа верно.
	names := undeclaredCallNamesR468(liveNativeEditorCallR468, plan)
	if len(names) != 1 || names[0] != "editor" {
		t.Fatalf("undeclaredCallNames = %v, want [editor]", names)
	}
	notice := undeclaredCallNoticeR468(names, plan)
	if notice == "" || notice == unparsedToolCallNoticeR430 {
		t.Fatalf("для случая «не объявлен» отдан прежний (ложный) текст: %q", notice)
	}
	for _, want := range []string{"editor", "read_files", "не объявлен"} {
		if !strings.Contains(notice, want) {
			t.Errorf("в тексте нет %q: %s", want, notice)
		}
	}
	if strings.Contains(notice, "нераспознан") {
		t.Errorf("текст снова говорит про «нераспознанный формат», а вызов распознан: %s", notice)
	}
}

// TestR468_DeclaredToolParses — при объявленном editor вызов исполняется, и
// «необъявленных» нет: тот же текст, другой состав tools[].
func TestR468_DeclaredToolParses(t *testing.T) {
	act := clineActModeToolsR468()

	calls := parseToolCallsFromOutputWithTools(liveNativeEditorCallR468, act)
	if len(calls) != 1 || calls[0].Function.Name != "editor" {
		t.Fatalf("вызов editor при объявленном инструменте должен разбираться: %v", calls)
	}
	if names := undeclaredCallNamesR468(liveNativeEditorCallR468, act); len(names) != 0 {
		t.Fatalf("undeclaredCallNames = %v, want пусто (editor объявлен)", names)
	}
	if args := calls[0].Function.Arguments; !strings.Contains(args, "initialize") || !strings.Contains(args, "app.js") {
		t.Errorf("аргументы потеряли содержимое: %.200s", args)
	}
}

// TestR468_NoToolsNoGuess — без tools[] фильтровать нечего: «необъявленных» нет,
// значит и текста про необъявленный инструмент быть не должно.
func TestR468_NoToolsNoGuess(t *testing.T) {
	if names := undeclaredCallNamesR468(liveNativeEditorCallR468, nil); len(names) != 0 {
		t.Fatalf("без tools[] список необъявленных должен быть пуст: %v", names)
	}
}

// TestR468_PlainTextHasNoUndeclared — обычный текст (без разметки вызова) не
// порождает ни списка, ни пояснения.
func TestR468_PlainTextHasNoUndeclared(t *testing.T) {
	text := "Готово: файл app.js создан, график рисуется."
	if names := undeclaredCallNamesR468(text, clinePlanModeToolsR468()); len(names) != 0 {
		t.Fatalf("на прозе найден вызов: %v", names)
	}
}
