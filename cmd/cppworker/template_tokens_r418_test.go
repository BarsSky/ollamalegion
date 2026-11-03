// template_tokens_r418_test.go — R418 (2026-11-03): токены шаблона Qwen/ChatML
// не попадают в видимый ответ.
//
// ЖИВОЙ СЛУЧАЙ. Qwen3.8-9B, /api/chat, prompt ~12k токенов повторяющегося
// текста: модель начала повторять промпт и вернула content
// «ок\n<|user|>\nОтветь одним словом: нет. Кон». `trailingToolTokens` чистит
// только хвостовые токены — `<|user|>` в середине уехал клиенту.
package main

import (
	"strings"
	"testing"
)

const r418LiveContent = "ок\n<|user|>\nОтветь одним словом: нет. Кон"

// TestR418_TemplateTokenStrippedFromFinalContent — живой текст: токен в середине
// вырезается, остальной текст сохраняется.
func TestR418_TemplateTokenStrippedFromFinalContent(t *testing.T) {
	got := cleanFinalContent(r418LiveContent)
	if strings.Contains(got, "<|user|>") {
		t.Fatalf("cleanFinalContent оставил токен шаблона: %q", got)
	}
	if !strings.Contains(got, "ок") || !strings.Contains(got, "Кон") {
		t.Fatalf("cleanFinalContent потерял текст ответа: %q", got)
	}
}

// TestR418_TemplateTokenStrippedFromStreamDelta — потоковая дельта.
func TestR418_TemplateTokenStrippedFromStreamDelta(t *testing.T) {
	got := stripServiceTokensKeepSpaces("привет<|im_start|>мир<|assistant|>!")
	if strings.Contains(got, "<|") {
		t.Fatalf("stripServiceTokensKeepSpaces оставил токен шаблона: %q", got)
	}
	if got != "приветмир!" {
		t.Fatalf("got %q, want «приветмир!»", got)
	}
}

// TestR418_SplitTokenHeldBack — недособранный токен в хвосте удерживается, чтобы
// вторая половина не пришла отдельным чанком как текст.
func TestR418_SplitTokenHeldBack(t *testing.T) {
	cases := map[string]int{
		"ответ <|us":     len("<|us"),
		"ответ <|user":   len("<|user"),
		"ответ <|im_sta": len("<|im_sta"),
		"обычный хвост":  0,
	}
	for in, want := range cases {
		if got := templateTurnTokenSuffixPrefixR418(in); got != want {
			t.Errorf("%q: удержано %d, ожидалось %d", in, got, want)
		}
	}
	// R414-хелпер обязан учитывать и токены шаблона.
	if got := toolsStreamMarkerPrefixHoldbackR414("ответ <|us"); got != len("<|us") {
		t.Errorf("toolsStreamMarkerPrefixHoldbackR414 = %d, want %d", got, len("<|us"))
	}
}
