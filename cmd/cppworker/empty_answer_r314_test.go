//go:build llama_stub

// empty_answer_r314_test.go — R314 (2026-10-30): нативная поверхность не отдаёт
// пустой ответ молча, когда весь бюджет ушёл в размышления.
//
// ЖИВОЙ ФАКТ (стенд, gemma-4-E4B, reasoning включён, num_predict=160):
//
//	POST /api/chat → {"content":"", "thinking":"…592 симв…", "done_reason":"length"}
//
// Клиент (Cline через Ollama API) показывает «Model returned empty response», и
// оператор идёт искать поломку — хотя модель работала и рассуждала. Пояснение
// существовало только для OpenAI-поверхности (R261); нативная идёт passthrough'ом.
package main

import (
	"strings"
	"testing"
)

func TestR314_EmptyAnswerNoticeOnlyWhenReasoningExisted(t *testing.T) {
	if got := emptyAnswerNoticeForNativeR314("", 0); got != "" {
		t.Errorf("пусто и без размышлений: %q, ожидалась пустая строка — нечего объяснять", got)
	}
	if got := emptyAnswerNoticeForNativeR314("   ", 0); got != "" {
		t.Errorf("пробелы и без размышлений: %q, ожидалась пустая строка", got)
	}
	if got := emptyAnswerNoticeForNativeR314("Четыре", 592); got != "" {
		t.Errorf("непустой ответ: %q, ожидалась пустая строка — чужой ответ не трогаем", got)
	}

	notice := emptyAnswerNoticeForNativeR314("", 592)
	if notice == "" {
		t.Fatal("пустой content при 592 символах размышлений — клиент снова получит «Model returned empty response»")
	}
	if !strings.Contains(notice, "592") {
		t.Errorf("в пояснении нет числа символов размышлений: %q (оператору нужен факт, а не «пусто»)", notice)
	}
	if strings.Contains(notice, "%d") {
		t.Errorf("в пояснении остался неразвёрнутый формат: %q", notice)
	}
}
