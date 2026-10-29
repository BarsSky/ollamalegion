// auto_continue_guard_r289_test.go — R289 (2026-10-29): 413 «prompt too long» —
// это не обрыв, и повторять запрос бессмысленно.
//
// ЖИВОЙ ДЕФЕКТ (стенд r282, окно 4096, промпт 3791 токенов): апстрим ответил
// 413 `prompt too long`, эвристика обрыва увидела «незакрытую строку» и
// запустила R60.21 auto-continue с причиной `mid_line_cutoff`. Продолжение
// получило тот же 413 — два похода в модель за заведомо обречённым ответом,
// а клиент так и не узнал настоящую причину.
package balancer

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestR289_IsContextOverflowText — что считаем отказом по размеру контекста.
func TestR289_IsContextOverflowText(t *testing.T) {
	overflow := []string{
		"prompt too long",
		"HTTP 413: prompt too long (prompt_tokens=3791)",
		"requested n_ctx=32768 exceeds model's effective n_ctx=8192",
		"context overflow: prompt 3791 > 4096",
		"code 3: prompt too long",
		"PROMPT TOO LONG", // регистр не важен
	}
	for _, s := range overflow {
		if !isContextOverflowText(s) {
			t.Errorf("%q должно распознаваться как отказ по контексту", s)
		}
	}

	notOverflow := []string{
		"",
		"model file not found",
		"connection reset by peer",
		"the assistant talked about prompt handling in general",
	}
	for _, s := range notOverflow {
		if isContextOverflowText(s) {
			t.Errorf("%q не должно распознаваться как отказ по контексту", s)
		}
	}
}

// TestR289_AutoContinueBlockedByContext — решение перед R60.21.
func TestR289_AutoContinueBlockedByContext(t *testing.T) {
	// Ошибка пришла в done-чанке, в тексте ответа её нет.
	if blocked, reason := autoContinueBlockedByContext("code 3: prompt too long", "", ""); !blocked {
		t.Fatal("отказ по контексту обязан блокировать auto-continue")
	} else if !strings.Contains(reason, "prompt too long") {
		t.Fatalf("причина должна называть отказ, got %q", reason)
	}

	// Обычный обрыв: ни один источник не говорит про контекст.
	if blocked, _ := autoContinueBlockedByContext("", "", "```go\nfunc main() {"); blocked {
		t.Fatal("обычный обрыв не должен блокироваться: auto-continue тут уместен")
	}

	// Ошибка попала в content (воркер вписал её в текст) — тоже блокируем.
	if blocked, _ := autoContinueBlockedByContext("", "", "context overflow: prompt 3791 > 4096"); !blocked {
		t.Fatal("ошибка в content обязана блокировать auto-continue")
	}
}

// TestR289_NoticeCarriesNumbers — клиент видит причину и числа, а не повтор.
func TestR289_NoticeCarriesNumbers(t *testing.T) {
	rec := httptest.NewRecorder()
	writeContextTooSmallNotice(rec, "/api/chat", "gemma-4-E4B", 3791, 4096)
	body := rec.Body.String()
	if body == "" {
		t.Fatal("клиент должен получить сообщение, а не пустоту")
	}
	if !strings.Contains(body, "3791") || !strings.Contains(body, "4096") {
		t.Fatalf("в сообщении должны быть числа (промпт и окно): %s", body)
	}
	if !strings.Contains(body, "context") {
		t.Fatalf("сообщение обязано называть причину: %s", body)
	}
}

// TestR289_EstimateMatchesPreflight — числа в сообщении берутся тем же путём,
// что и в preflight (иначе «сколько просили» разошлось бы с решением о росте).
func TestR289_EstimateMatchesPreflight(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("x", 4000) + `"}]}`)
	got := estimatePromptTokensFromBody(body, "/v1/chat/completions")
	meta := ExtractRequestMeta(body, "/v1/chat/completions")
	if meta == nil {
		t.Fatal("ExtractRequestMeta не разобрал тело")
	}
	if got != meta.EstimatedPromptTokens {
		t.Fatalf("оценка %d != preflight %d", got, meta.EstimatedPromptTokens)
	}
	if got <= 0 {
		t.Fatal("оценка промпта должна быть положительной")
	}
}
