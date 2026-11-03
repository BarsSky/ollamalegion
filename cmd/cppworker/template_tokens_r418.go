// template_tokens_r418.go — R418 (2026-11-03): токены шаблона Qwen/ChatML не
// должны попадать в видимый ответ.
//
// ЖИВОЙ СЛУЧАЙ (Qwen3.8-9B, /api/chat, prompt ~12k токенов повторяющегося
// текста): модель начала повторять промпт и вернула в content
//
//	"ок\n<|user|>\nОтветь одним словом: нет. Кон"
//
// `trailingToolTokens` (tool_calls.go) чистит только ХВОСТОВЫЕ служебные токены,
// поэтому `<|user|>` в СЕРЕДИНЕ текста уехал клиенту как обычный текст. Тот же
// класс, что R414 (`<channel|>`), но для токенов шаблона.
//
// Здесь — один список и две операции: вырезание из готового видимого текста
// (cleanFinalContent) и вырезание из потоковых дельт
// (stripServiceTokensKeepSpaces). Удержание недособранного токена в хвосте —
// в R414 (toolsStreamMarkerPrefixHoldbackR414 теперь знает и эти токены).
package main

import "strings"

// templateTurnTokensR418 — токены шаблонов Qwen (ChatML) и родственных,
// которые не являются текстом ответа.
//
// ВАЖНО: список не должен содержать обычные слова/фрагменты, иначе вырежется
// часть ответа. Только служебные обёртки с угловыми скобками.
var templateTurnTokensR418 = []string{
	"<|im_start|>",
	"<|im_end|>",
	"<|user|>",
	"<|assistant|>",
	"<|system|>",
	"<|endoftext|>",
	// Варианты без второй палки (модели иногда теряют её при обрыве).
	"<|user>",
	"<|assistant>",
	"<|system>",
}

// stripTemplateTurnTokensR418 вырезает токены шаблона из видимого текста.
func stripTemplateTurnTokensR418(s string) string {
	if s == "" {
		return s
	}
	for _, tok := range templateTurnTokensR418 {
		if strings.Contains(s, tok) {
			s = strings.ReplaceAll(s, tok, "")
		}
	}
	return s
}

// templateTurnTokenSuffixPrefixR418 — длина хвоста s, который является началом
// токена шаблона (например `<|us`), но короче его. Нужен потоковому пути:
// такой хвост отдавать нельзя, следующий чанк достроит токен.
func templateTurnTokenSuffixPrefixR418(s string) int {
	if s == "" {
		return 0
	}
	best := 0
	for _, tok := range templateTurnTokensR418 {
		for n := 1; n < len(tok); n++ {
			if n > len(s) {
				break
			}
			if strings.HasSuffix(s, tok[:n]) && n > best {
				best = n
			}
		}
	}
	return best
}
