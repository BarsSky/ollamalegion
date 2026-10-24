//go:build llama_stub

// writer_matrix_helpers_r219_test.go — общие проверки контракта SSE-писателей.
package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func newToolCall(id, name, args string) openAIToolCall {
	var c openAIToolCall
	c.ID = id
	c.Type = "function"
	c.Function.Name = name
	c.Function.Arguments = args
	return c
}

// verifyWriterContract проверяет КОНТРАКТ потока независимо от конкретного
// случая: role ровно один раз, reasoning/content только в своих полях,
// finish_reason последнего чанка — ожидаемый, поток завершён [DONE].
func verifyWriterContract(t *testing.T, raw, wantRsn, wantCnt, wantFinish string) {
	t.Helper()
	roleCount, content, reasoning, finish := r216ParseSSE(t, raw)

	if got := reasoning.String(); got != wantRsn {
		t.Errorf("reasoning_content = %q, want %q", got, wantRsn)
	}
	if got := content.String(); got != wantCnt {
		t.Errorf("content = %q, want %q", got, wantCnt)
	}
	if roleCount != 1 {
		t.Errorf("role=assistant пришла %d раз, want 1", roleCount)
	}
	if len(finish) == 0 || finish[len(finish)-1] != wantFinish {
		t.Errorf("finish_reason = %v, want %q", finish, wantFinish)
	}
	if !strings.Contains(raw, "[DONE]") {
		t.Error("поток не завершён [DONE]")
	}
	// Служебные маркеры не должны попасть в видимый канал.
	for _, tag := range []string{"<think>", "</think>", "<|channel>", "<channel|>"} {
		if strings.Contains(content.String(), tag) {
			t.Errorf("в content остался маркер %q: %q", tag, content.String())
		}
	}
}
