package main

import (
	"encoding/json"
	"testing"
)

// TestR439_StringWrappedArgsWithWindowsPath — аргументы пришли СТРОКОЙ с
// внутривалидным (после починки escape) объектом: все поля должны сохраниться.
func TestR439_StringWrappedArgsWithWindowsPath(t *testing.T) {
	// Модель положила объект в строку и записала путь одинарными слэшами.
	inner := `{"new_text":"<!DOCTYPE html><html lang=\"ru\"></html>","path":"d:\cline-e2e-web\index.html"}`
	raw, err := json.Marshal(inner) // аргументы = JSON-СТРОКА
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	calls := coerceToolCallsToSchema([]openAIToolCall{{Type: "function",
		Function: openAIFunctionCall{Name: "editor", Arguments: string(raw)}}}, editorToolsR439())
	if len(calls) != 1 {
		t.Fatalf("вызовов=%d", len(calls))
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON: %v (%q)", err, calls[0].Function.Arguments)
	}
	if _, ok := args["path"].(string); !ok {
		t.Fatalf("path потерян: %v", args)
	}
	nt, ok := args["new_text"].(string)
	if !ok || nt == "" || nt == calls[0].Function.Arguments {
		t.Fatalf("new_text испорчен: %v", args["new_text"])
	}
}
