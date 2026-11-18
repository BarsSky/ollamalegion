//go:build llama_stub

// decode_error_tail_r521_test.go — R521 (2026-11-19, §8.3.5): ошибка декода
// обязана нести последние строки лога llama.cpp.
//
// ЖИВОЙ ФАКТ (архив 2026-11-18): клиент получал «llama_decode failed … code 1»,
// хотя в логе llama.cpp рядом стоял CUDA out-of-memory. Мост строки
// перехватывал (только для раскладки слоёв), а в ответ они не попадали.
package cppbackend

import (
	"errors"
	"strings"
	"testing"
)

func TestR521_DecodeErrorCarriesLlamaTail(t *testing.T) {
	err := errors.New("llama_decode failed for prompt batch: prompt_tokens=79675")
	lines := []string{
		"load_tensors: offloaded 65/65 layers to GPU",
		"ggml_backend_cuda_buffer_type_alloc_buffer: allocating 4096.00 MiB on device 0",
		"CUDA error: out of memory",
	}
	got := decodeErrorWithTail(err, lines)
	if got == nil {
		t.Fatalf("ошибка исчезла")
	}
	msg := got.Error()
	for _, want := range []string{"llama_decode failed", "[llama.cpp] последние строки", "CUDA error: out of memory"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("в ошибке нет %q; текст:\n%s", want, msg)
		}
	}
}

func TestR521_NonDecodeErrorIsUntouched(t *testing.T) {
	err := errors.New("context canceled")
	if got := decodeErrorWithTail(err, []string{"CUDA error: out of memory"}); got.Error() != err.Error() {
		t.Fatalf("к не-декодной ошибке приклеили хвост: %s", got.Error())
	}
}

func TestR521_NoTailSaysLogUnavailable(t *testing.T) {
	err := errors.New("llama_decode failed for prompt batch")
	msg := decodeErrorWithTail(err, nil).Error()
	if !strings.Contains(msg, "лог недоступен") {
		t.Fatalf("без строк ошибка обязана признаться, что лога нет; текст:\n%s", msg)
	}
	// И это признание не должно появляться у не-декодных ошибок.
	if got := decodeErrorWithTail(errors.New("n_ctx exceeds"), nil); strings.Contains(got.Error(), "лог недоступен") {
		t.Fatalf("к 413-подобной ошибке приклеили «лог недоступен»: %s", got.Error())
	}
}

func TestR521_TailIsBounded(t *testing.T) {
	long := strings.Repeat("x", 1000)
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = long
	}
	msg := decodeErrorWithTail(errors.New("llama_decode failed"), lines).Error()
	if len(msg) > decodeErrorMaxBytes+64 {
		t.Fatalf("текст ошибки не ограничен: %d байт (предел %d)", len(msg), decodeErrorMaxBytes)
	}
	if !strings.Contains(msg, "лог обрезан") {
		t.Fatalf("обрезанный текст не помечен: %s", msg[:80])
	}
}
