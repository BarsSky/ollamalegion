package balancer

import (
	"encoding/json"
	"testing"
)

// R248 (2026-10-27): живой дефект из матрицы инструментов — gemma-4 вложила
// весь объект аргументов в строковое поле path, экранировав кавычки, и вызов
// уходил битым: «search_files: нет обязательного поля regex».
func TestR248_SalvageNestedGarbledArgs(t *testing.T) {
	schema := &toolParamSchema{
		Type:     "object",
		Required: []string{"path", "regex"},
		Properties: map[string]*toolParamSchema{
			"path":  {Type: "string"},
			"regex": {Type: "string"},
		},
	}
	garbled := `{"path":"{\"path\":\"{\".\"\",\"regex\":\"package main\"}"}`
	fixed, changed := repairToolArguments(garbled, schema)
	if !changed {
		t.Fatalf("аргументы не починены: %s", garbled)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(fixed), &got); err != nil {
		t.Fatalf("post-repair не JSON: %v (%s)", err, fixed)
	}
	if got["regex"] != "package main" {
		t.Fatalf("regex = %v, ожидалось 'package main' (args=%s)", got["regex"], fixed)
	}
	if _, ok := got["path"]; !ok {
		t.Fatalf("path потерян (args=%s)", fixed)
	}
	if s, _ := got["path"].(string); s == "" || s[0] == '{' {
		t.Fatalf("path остался мусором: %q", s)
	}
}

// R248: корректные аргументы не трогаем.
func TestR248_SalvageKeepsValidArgs(t *testing.T) {
	schema := &toolParamSchema{
		Type:     "object",
		Required: []string{"path", "regex"},
		Properties: map[string]*toolParamSchema{
			"path":  {Type: "string"},
			"regex": {Type: "string"},
		},
	}
	valid := `{"path":".","regex":"package main"}`
	fixed, changed := repairToolArguments(valid, schema)
	if changed && fixed != valid {
		t.Fatalf("валидные аргументы изменены: %s -> %s", valid, fixed)
	}
}
