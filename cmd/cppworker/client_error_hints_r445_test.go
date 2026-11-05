package main

import (
	"strings"
	"testing"
)

// toolMsgR445 — сообщение-результат инструмента в форме Cline.
func toolMsgR445(content string) chatMessage {
	return chatMessage{Role: "tool", Content: content}
}

// TestR445_OldTextHint — отказ «файл существует без old_text» даёт инструкцию
// прочитать файл и передать old_text; повтор вызова назван бесполезным.
func TestR445_OldTextHint(t *testing.T) {
	t.Setenv("CPPWORKER_CLIENT_HINTS", "")
	msgs := []chatMessage{
		{Role: "user", Content: "создай index.html"},
		toolMsgR445(`{"error":"Editor operation failed: d:\\x\\index.html already exists, but ` + "`old_text`" + ` was omitted. To edit an existing file, set ` + "`old_text`" + ` … Do not re-send this call unchanged.","success":false}`),
	}
	hint := clientErrorHintsR445(msgs)
	if hint == "" {
		t.Fatal("подсказка не сформирована")
	}
	for _, want := range []string{"already exists", "read_files", "old_text", "do not repeat"} {
		if !strings.Contains(strings.ToLower(hint), strings.ToLower(want)) {
			t.Errorf("в подсказке нет %q:\n%s", want, hint)
		}
	}
}

// TestR445_TooLargeHint — отказ по размеру даёт инструкцию дробить на вызовы.
func TestR445_TooLargeHint(t *testing.T) {
	t.Setenv("CPPWORKER_CLIENT_HINTS", "")
	msgs := []chatMessage{toolMsgR445(
		`{"error":"Editor input too large: new_text was 7036 characters, exceeding the recommended limit of 6000. Split the edit into smaller tool calls …"}`)}
	hint := clientErrorHintsR445(msgs)
	if !strings.Contains(hint, "6000") || !strings.Contains(strings.ToLower(hint), "split") {
		t.Fatalf("подсказка про лимит не сформирована: %q", hint)
	}
}

// TestR445_BothHints — обе ошибки в истории дают обе инструкции, один блок.
func TestR445_BothHints(t *testing.T) {
	t.Setenv("CPPWORKER_CLIENT_HINTS", "")
	msgs := []chatMessage{
		toolMsgR445(`{"error":"Editor operation failed: d:\\x\\index.html already exists, but ` + "`old_text`" + ` was omitted."}`),
		toolMsgR445(`{"error":"Editor input too large: new_text was 7036 characters, exceeding the recommended limit of 6000."}`),
	}
	hint := clientErrorHintsR445(msgs)
	if strings.Count(hint, "IMPORTANT") != 1 {
		t.Fatalf("блок подсказки должен быть один: %q", hint)
	}
	if !strings.Contains(hint, "read_files") || !strings.Contains(hint, "6000") {
		t.Fatalf("нет обеих инструкций: %q", hint)
	}
}

// TestR445_NoHintsForNormalTools — обычный результат инструмента подсказок не даёт.
func TestR445_NoHintsForNormalTools(t *testing.T) {
	t.Setenv("CPPWORKER_CLIENT_HINTS", "")
	msgs := []chatMessage{toolMsgR445(`{"query":"d:\\x\\TASK.md","result":"# Задача …","success":true}`)}
	if hint := clientErrorHintsR445(msgs); hint != "" {
		t.Fatalf("подсказка на нормальный результат: %q", hint)
	}
}

// TestR445_DisabledByEnv — рубильник.
func TestR445_DisabledByEnv(t *testing.T) {
	t.Setenv("CPPWORKER_CLIENT_HINTS", "0")
	msgs := []chatMessage{toolMsgR445(`already exists, but old_text was omitted`)}
	if hint := clientErrorHintsR445(msgs); hint != "" {
		t.Fatalf("подсказки включены при CPPWORKER_CLIENT_HINTS=0: %q", hint)
	}
}
