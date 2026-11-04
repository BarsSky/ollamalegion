//go:build llama_stub

// gemma_paren_calls_r428_test.go — R428 (2026-11-04): нативный вызов gemma в
// форме `call:NAME(key: value, …)`.
//
// ФИКСТУРЫ — РЕАЛЬНЫЕ байты, снятые диагностикой R427 из живой сессии Cline
// (gemma-4-E4B-it, 25 инструментов): см. /app/diag/RAW-*.txt.
//
// Контракт:
//  1. форма `<|tool_call>call:editor(new_text: "…", path: "…")` распознаётся,
//     включая вариант с забытой `)` и хвостовым мусором `}]`;
//  2. строковые значения с экранированием (`\"`, `\\n`, `\\\\`) и юникодом
//     сохраняются байт-в-байт;
//  3. обычный текст (без маркера) не превращается в вызов;
//  4. имя вызова, которого нет в tools[], не уезжает клиенту как tool_call
//     (R428: alias write_file → editor, иначе вызов отбрасывается).

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// fixtureGemmaParenBroken — точная форма из захвата: круглые скобки, нет
// закрывающего тега, в конце мусор `"}]`, путь с двойными бэкслешами.
const fixtureGemmaParenBroken = `<|tool_call>call:editor(new_text: "<!-- Cluster Monitor - Static Web Page --\n<!DOCTYPE html>\n<html lang=\"ru\" data-theme=\"light\">\n<head>\n    <meta charset=\"UTF-8\">\n    <title>Монитор кластера</title>\n</head>\n<body><h1>Монитор кластера</h1></body>\n</html>", path: "d:\\cline-e2e-web\\index.html"}]`

// fixtureGemmaParenClosed — аккуратная форма с закрывающей скобкой.
const fixtureGemmaParenClosed = `Сейчас создам файл.
<|tool_call>call:editor(new_text: "HELLO", path: "C:\\Ollama\\cline-r426\\hello.txt")`

func parseFixture(t *testing.T, raw string) []openAIToolCall {
	t.Helper()
	all := []openAITool{{Type: "function", Function: openAIFunction{Name: "editor"}}, {Type: "function", Function: openAIFunction{Name: "read_files"}}}
	return parseToolCallsFromOutputWithTools(raw, all)
}

func TestR428_GemmaParenCallWithCloseTag(t *testing.T) {
	calls := parseFixture(t, fixtureGemmaParenClosed)
	if len(calls) != 1 {
		t.Fatalf("ожидался 1 вызов, получено %d: %+v", len(calls), calls)
	}
	if calls[0].Function.Name != "editor" {
		t.Fatalf("имя = %q, ожидалось editor", calls[0].Function.Name)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("аргументы не JSON: %v (%q)", err, calls[0].Function.Arguments)
	}
	if args["new_text"] != "HELLO" {
		t.Errorf("new_text = %v, ожидалось HELLO", args["new_text"])
	}
	if args["path"] != `C:\Ollama\cline-r426\hello.txt` {
		t.Errorf("path = %v", args["path"])
	}
}

// TestR428_GemmaParenCallLiveFixture — та самая сломанная форма из сессии.
func TestR428_GemmaParenCallLiveFixture(t *testing.T) {
	calls := parseFixture(t, fixtureGemmaParenBroken)
	if len(calls) != 1 {
		t.Fatalf("ожидался 1 вызов из живого захвата, получено %d (форма с забытой `)` и хвостом `}]`)", len(calls))
	}
	if calls[0].Function.Name != "editor" {
		t.Fatalf("имя = %q, ожидалось editor", calls[0].Function.Name)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("аргументы не JSON: %v", err)
	}
	text, _ := args["new_text"].(string)
	if !strings.Contains(text, "<!DOCTYPE html>") || !strings.Contains(text, "Монитор кластера") {
		t.Errorf("содержимое файла потеряно/искажено: %q", text)
	}
	if !strings.Contains(text, "\n") {
		t.Errorf("переводы строк потеряны: %q", text)
	}
	if args["path"] != `d:\cline-e2e-web\index.html` {
		t.Errorf("path = %v", args["path"])
	}
}

// TestR428_PlainFunctionLikeTextIsNotCall — без маркера вызова нет.
func TestR428_PlainFunctionLikeTextIsNotCall(t *testing.T) {
	for _, out := range []string{
		"Вызови editor(new_text: \"x\", path: \"y\") чтобы создать файл.",
		"Функция read_files(files) читает файлы.",
	} {
		if calls := parseFixture(t, out); len(calls) != 0 {
			t.Errorf("текст %q принят за вызов: %+v", out, calls)
		}
	}
}

// TestR428_UndeclaredToolIsNotPassedThrough — 9B зовёт write_file, у Cline его
// нет: такой вызов не должен уезжать клиенту как есть. Ожидаем alias → editor.
func TestR428_UndeclaredToolIsNotPassedThrough(t *testing.T) {
	raw := `[{"name":"write_file","arguments":{"path":"C:\\Ollama\\x\\a.txt","text":"HI"}}]`
	calls := parseFixture(t, raw)
	if len(calls) != 1 {
		t.Fatalf("ожидался 1 вызов (alias write_file → editor), получено %d: %+v", len(calls), calls)
	}
	if calls[0].Function.Name != "editor" {
		t.Fatalf("имя = %q, ожидалось editor (alias)", calls[0].Function.Name)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("аргументы не JSON: %v", err)
	}
	if args["new_text"] != "HI" {
		t.Errorf("text не переименован в new_text: %+v", args)
	}
	if args["path"] == nil {
		t.Errorf("path потерян: %+v", args)
	}
}

// TestR428_UnknownToolDroppedWhenNoAlias — имя не из tools[] и alias не
// применим → вызов отбрасывается, а не отдаётся клиенту как несуществующий.
func TestR428_UnknownToolDroppedWhenNoAlias(t *testing.T) {
	raw := `[{"name":"frobnicate","arguments":{"x":1}}]`
	if calls := parseFixture(t, raw); len(calls) != 0 {
		t.Fatalf("неизвестный инструмент должен быть отброшен, получено %+v", calls)
	}
}

// TestR428_TailDoesNotLeakReasoningMarker — D: хвост ответа не должен нести
// маркер размышлений. Живой захват: ответ gemma-4 заканчивался
// «Создаю файл `index.html`:<channel|>», и маркер осел в истории Cline.
func TestR428_TailDoesNotLeakReasoningMarker(t *testing.T) {
	in := "Создаю файл `index.html`:<channel|>"
	got := stripOrphanReasoningTags(sanitizeStreamText(in))
	if strings.Contains(got, "<channel|>") {
		t.Fatalf("маркер остался в хвосте: %q", got)
	}
	if !strings.Contains(got, "Создаю файл") {
		t.Fatalf("полезный текст потерян: %q", got)
	}
}
