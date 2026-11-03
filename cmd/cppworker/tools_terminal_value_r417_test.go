// tools_terminal_value_r417_test.go — R417 (2026-11-03): пояснение «только
// размышления» не подставляется, если распознан tool_calls.
//
// ЖИВОЙ СЛУЧАЙ (Qwen3.8-9B, /api/chat + tools, stream:false):
//
//	content: "Модель сгенерировала только размышления (136 симв.), видимого
//	          ответа нет (бюджет ответа исчерпан)…"
//	tool_calls: [read_files]
//
// Клиент сохраняет content как сообщение ассистента — рядом с вызовом
// инструмента в истории остаётся предупреждение на русском. Вызов инструмента
// и есть ответ хода; «пустого ответа» здесь нет.
package main

import (
	"strings"
	"testing"
)

// TestR417_ToolCallSuppressesEmptyAnswerNotice — toolCalls > 0 → content пустой.
func TestR417_ToolCallSuppressesEmptyAnswerNotice(t *testing.T) {
	raw := "<|channel>thought\nнадо прочитать TASK.md\n<channel|>"
	if got := terminalContentR417("", raw, 1); got != "" {
		t.Fatalf("content = %q, want пусто: при tool_calls пояснение не подставляется", got)
	}
}

// TestR417_NoToolCallKeepsNotice — без tool_calls поведение прежнее (R314/R394):
// пустой видимый ответ + размышления → пояснение.
func TestR417_NoToolCallKeepsNotice(t *testing.T) {
	raw := "<|channel>thought\nрассуждаю долго\n<channel|>"
	got := terminalContentR417("", raw, 0)
	if strings.TrimSpace(got) == "" {
		t.Fatal("без tool_calls пояснение потерялось — клиент получит пустой ответ")
	}
}

// TestR417_VisibleContentUntouched — видимый текст не переписывается ни в одном
// случае.
func TestR417_VisibleContentUntouched(t *testing.T) {
	raw := "<|channel>thought\nрассуждаю\n<channel|>"
	for _, calls := range []int{0, 1, 3} {
		if got := terminalContentR417("Готово.", raw, calls); got != "Готово." {
			t.Fatalf("tool_calls=%d: content = %q, want «Готово.»", calls, got)
		}
	}
}

// TestR417_NonStreamToolCallSuppressesNotice — не-потоковый /api/chat: при
// tool_calls пояснение не подставляется.
func TestR417_NonStreamToolCallSuppressesNotice(t *testing.T) {
	if got := nonStreamContentR417("", 1, 154); got != "" {
		t.Fatalf("content = %q, want пусто при tool_calls (stream:false)", got)
	}
	// Без вызова — прежнее поведение: пояснение вместо пустоты.
	if got := nonStreamContentR417("", 0, 154); strings.TrimSpace(got) == "" {
		t.Fatal("без tool_calls пояснение потерялось (stream:false)")
	}
	// Видимый ответ не трогаем.
	if got := nonStreamContentR417("Готово.", 1, 154); got != "Готово." {
		t.Fatalf("content = %q, want «Готово.»", got)
	}
}
