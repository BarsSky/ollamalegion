//go:build llama_stub

// tools_stream_short_answer_r424_test.go — R424 (2026-11-03): короткий ответ на
// tools-пути обязан уйти ДЕЛЬТОЙ, а не только в финальном чанке.
//
// ЖИВОЙ ДЕФЕКТ (прогон Cline CLI 3.0.74 через балансер, сессия
// conv_1793730119921_sd9evze). Модель ответила ровно «FINAL: needle-7f3a»
// (18 байт) — это меньше окна удержания toolsStreamHoldback=32, поэтому за весь
// ход не ушло ни одной дельты:
//
//	cppworker: streamed_content_bytes=0, final_content_len=18, done_reason=stop
//	Cline CLI: DONE error: text part IiSeqLLDO6Ty5Hjh not found
//
// Балансер по умолчанию вычищает content из done-чанка
// (LB_OLLAMA_DONE_CONTENT=strip), то есть клиент не получал ответа вообще.
// R248 закрыл ту же дырку для «есть tool_calls»; тест ниже фиксирует второй
// случай — обычный короткий текст без вызова инструмента.

package main

import (
	"os"
	"strings"
	"testing"
)

// TestR424_ShortTextAnswerStreamedAsDelta — весь видимый текст уходит дельтой,
// финальный чанк его не дублирует.
func TestR424_ShortTextAnswerStreamedAsDelta(t *testing.T) {
	os.Setenv("CPPWORKER_TOOLS_STREAM_CONTENT", "true")
	defer os.Unsetenv("CPPWORKER_TOOLS_STREAM_CONTENT")

	// 18 байт — целиком внутри окна удержания (32).
	const answer = "FINAL: needle-7f3a"
	defer withStubEmitTokens(t, []string{answer})()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	chunks := postChatWithTools(t, srv.URL)
	streamed, final := joinContentDeltas(t, chunks)

	if got := strings.TrimSpace(streamed); got != strings.TrimSpace(answer) {
		t.Fatalf("склейка дельт = %q, want %q (ответ целиком попал в окно удержания и не был отдан дельтой)", got, answer)
	}
	if got := getContentFromMessage(final); got != "" {
		t.Errorf("финальный чанк дублирует уже отправленный текст: %q", got)
	}
	if got, _ := final["done_reason"].(string); got != "stop" {
		t.Errorf("done_reason = %q, want stop", got)
	}
}
