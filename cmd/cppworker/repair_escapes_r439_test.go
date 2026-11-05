// repair_escapes_r439_test.go — R439 (2026-11-05): невалидные escape-
// последовательности в аргументах вызова чинятся, а не «заворачиваются в path».
//
// ФИКСТУРА — живой вызов Cline+gemma-4 (скриншот оператора): модель записала
// Windows-путь одинарными обратными слэшами, JSON стал невалидным, и весь блок
// аргументов уезжал в первый скалярный параметр. Клиент отвечал
// «Invalid input: expected string, received undefined → at new_text».
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// editorToolsR439 — схема editor в порядке Cline (path, old_text, new_text).
func editorToolsR439() []openAITool {
	return []openAITool{{Type: "function", Function: openAIFunction{
		Name: "editor",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"path":{"type":"string"},"old_text":{"type":"string"},"new_text":{"type":"string"}},` +
			`"required":["path","new_text"]}`),
	}}}
}

// TestR439_WindowsPathKeepsAllArguments — главный случай: путь и содержимое
// остаются на своих местах.
func TestR439_WindowsPathKeepsAllArguments(t *testing.T) {
	raw := `{"path":"d:\cline-e2e-web\index.html","new_text":"<!DOCTYPE html>\n<html lang=\"ru\">\n</html>"}`
	calls := coerceToolCallsToSchema([]openAIToolCall{{Type: "function",
		Function: openAIFunctionCall{Name: "editor", Arguments: raw}}}, editorToolsR439())
	if len(calls) != 1 {
		t.Fatalf("вызовов=%d", len(calls))
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v (%q)", err, calls[0].Function.Arguments)
	}
	path, _ := args["path"].(string)
	if !strings.Contains(path, "cline-e2e-web") || strings.Contains(path, `"path"`) {
		t.Fatalf("path испорчен: %q", path)
	}
	newText, ok := args["new_text"].(string)
	if !ok || !strings.Contains(newText, "DOCTYPE html") {
		t.Fatalf("new_text потерян (%T): %v", args["new_text"], args["new_text"])
	}
	// Одиночные слэши пути восстановлены как литеральные.
	if !strings.Contains(path, `d:\cline-e2e-web\index.html`) {
		t.Fatalf("путь Windows не восстановлен: %q", path)
	}
}

// TestR439_ValidJSONUntouched — корректный JSON не переписывается.
func TestR439_ValidJSONUntouched(t *testing.T) {
	raw := `{"path":"d:\\cline-e2e-web\\index.html","new_text":"<html>\n</html>"}`
	if repaired, changed := repairInvalidJSONEscapesR439(raw); changed {
		t.Fatalf("валидный JSON изменён: %q", repaired)
	}
	calls := coerceToolCallsToSchema([]openAIToolCall{{Type: "function",
		Function: openAIFunctionCall{Name: "editor", Arguments: raw}}}, editorToolsR439())
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v", err)
	}
	if !strings.Contains(args["path"].(string), "cline-e2e-web") {
		t.Fatalf("path = %v", args["path"])
	}
	if !strings.Contains(args["new_text"].(string), "html") {
		t.Fatalf("new_text = %v", args["new_text"])
	}
}

// TestR439_PlainPathStillWrapped — значение, которое вообще не JSON
// (`d:\file.txt`), по-прежнему уходит в скалярный параметр (поведение R113).
func TestR439_PlainPathStillWrapped(t *testing.T) {
	calls := coerceToolCallsToSchema([]openAIToolCall{{Type: "function",
		Function: openAIFunctionCall{Name: "editor", Arguments: `d:\cline-e2e-web\index.html`}}}, editorToolsR439())
	if len(calls) != 1 {
		t.Fatalf("вызовов=%d", len(calls))
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v (%q)", err, calls[0].Function.Arguments)
	}
	if _, ok := args["path"]; !ok {
		t.Fatalf("значение не попало в path: %v", args)
	}
}

// TestR439_InvalidEscapeInPath — недопустимые escape в пути тоже чинятся.
//
// ВНИМАНИЕ: `\t` и `\f` — ВАЛИДНЫЕ JSON-escape (табуляция, form feed), поэтому
// для проверки берём действительно недопустимые `\U` и `\x`.
func TestR439_InvalidEscapeInPath(t *testing.T) {
	raw := `{"path":"C:\Users\aleks\file.txt"}`
	repaired, changed := repairInvalidJSONEscapesR439(raw)
	if !changed {
		t.Fatalf("невалидные escape не починены: %q", raw)
	}
	if !json.Valid([]byte(repaired)) {
		t.Fatalf("после починки всё ещё невалидно: %q", repaired)
	}
}
