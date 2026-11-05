// gemma_native_args_r441_test.go — R441 (2026-11-05): нативное тело вызова gemma
// разбирается сканером (разделители <|"|>), а не переделкой в JSON.
//
// ФИКСТУРА — сырой вывод, снятый диагностикой R440 из живого отказа Cline:
//
//	<|tool_call>call:editor{new_text:<|"|><!DOCTYPE html><html lang="ru">…</html><|"|>,
//	                       path:<|"|>d:\cline-e2e-web\index.html<|"|>}<tool_call|>
//
// Прежний путь (`<|"|>` → `"`, затем JSON) ломался: внутри значения оставались
// настоящие кавычки (`lang="ru"`) и недопустимые escape пути (`\c`, `\i`), из-за
// чего весь блок аргументов уезжал в один параметр и клиент писал
// «Invalid input: expected string, received undefined → at new_text».
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// fixtureGemmaBracesR441 — точная строка из живого лога.
const fixtureGemmaBracesR441 = `<|tool_call>call:editor{new_text:<|"|><!DOCTYPE html><html lang="ru">` +
	`<head><title>T</title></head><body><h1>OK</h1></body></html><|"|>,` +
	`path:<|"|>d:\cline-e2e-web\index.html<|"|>}<tool_call|>`

// editorToolsR441 — схема editor в порядке Cline.
func editorToolsR441() []openAITool {
	return []openAITool{{Type: "function", Function: openAIFunction{
		Name: "editor",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string"},"old_text":{"type":"string"},"new_text":{"type":"string"}},` +
			`"required":["path","new_text"]}`),
	}}}
}

// TestR441_LiveBracesCallKeepsAllArguments — главный случай: путь и содержимое
// файла (с кавычками внутри) на своих местах.
func TestR441_LiveBracesCallKeepsAllArguments(t *testing.T) {
	calls := parseToolCallsForRequest(fixtureGemmaBracesR441, editorToolsR441())
	if len(calls) != 1 || calls[0].Function.Name != "editor" {
		t.Fatalf("вызов не разобран: %+v", calls)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v (%q)", err, calls[0].Function.Arguments)
	}
	path, _ := args["path"].(string)
	if !strings.Contains(path, `cline-e2e-web\index.html`) {
		t.Fatalf("path = %q", path)
	}
	newText, _ := args["new_text"].(string)
	if !strings.Contains(newText, "DOCTYPE html") || !strings.Contains(newText, `lang="ru"`) {
		t.Fatalf("new_text испорчен: %q", newText)
	}
	if strings.Contains(newText, "path") {
		t.Fatalf("в new_text попал весь блок аргументов: %q", newText)
	}
}

// TestR441_NativeArgsDirectly — сканер на теле без обёртки вызова.
func TestR441_NativeArgsDirectly(t *testing.T) {
	body := `new_text:<|"|><h1 class="t">x</h1><|"|>,path:<|"|>d:\a\b.html<|"|>`
	got, ok := gemmaNativeArgsToJSONR441(body)
	if !ok {
		t.Fatalf("тело не разобрано")
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(got), &args); err != nil {
		t.Fatalf("не JSON: %v (%q)", err, got)
	}
	if args["path"] != `d:\a\b.html` {
		t.Fatalf("path = %v", args["path"])
	}
	if args["new_text"] != `<h1 class="t">x</h1>` {
		t.Fatalf("new_text = %v", args["new_text"])
	}
}

// TestR441_ParenFormWithQuotesInContent — скобочная форма с теми же сложностями.
func TestR441_ParenFormWithQuotesInContent(t *testing.T) {
	raw := `<|tool_call>call:editor(new_text:<|"|><p class="x">hi</p><|"|>, path:<|"|>d:\root\i.html<|"|>)`
	calls := parseToolCallsForRequest(raw, editorToolsR441())
	if len(calls) != 1 {
		t.Fatalf("вызовов=%d", len(calls))
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v (%q)", err, calls[0].Function.Arguments)
	}
	if args["new_text"] != `<p class="x">hi</p>` {
		t.Fatalf("new_text = %v", args["new_text"])
	}
	if args["path"] != `d:\root\i.html` {
		t.Fatalf("path = %v", args["path"])
	}
}

// TestR441_CommasInsideContent — запятая внутри значения не режет пару.
func TestR441_CommasInsideContent(t *testing.T) {
	body := `cmd:<|"|>echo a, b, c<|"|>,timeout:30`
	got, ok := gemmaNativeArgsToJSONR441(body)
	if !ok {
		t.Fatal("тело не разобрано")
	}
	var args map[string]interface{}
	_ = json.Unmarshal([]byte(got), &args)
	if args["cmd"] != "echo a, b, c" {
		t.Fatalf("cmd = %v", args["cmd"])
	}
	if args["timeout"] != float64(30) {
		t.Fatalf("timeout = %v (%T)", args["timeout"], args["timeout"])
	}
}

// TestR441_NestedObjectAndSingleQuotes — вложенный объект, одинарные кавычки.
func TestR441_NestedObjectAndSingleQuotes(t *testing.T) {
	body := `opt:{a:'x',b:2},name:<|"|>plain<|"|>`
	got, ok := gemmaNativeArgsToJSONR441(body)
	if !ok {
		t.Fatal("тело не разобрано")
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(got), &args); err != nil {
		t.Fatalf("не JSON: %v (%q)", err, got)
	}
	opt, ok := args["opt"].(map[string]interface{})
	if !ok || opt["a"] != "x" || opt["b"] != float64(2) {
		t.Fatalf("opt = %v", args["opt"])
	}
}

// TestR441_GarbageFallsBack — на мусоре сканер честно отказывается.
func TestR441_GarbageFallsBack(t *testing.T) {
	if _, ok := gemmaNativeArgsToJSONR441(`просто проза без пар`); ok {
		t.Fatal("проза принята за аргументы")
	}
}
