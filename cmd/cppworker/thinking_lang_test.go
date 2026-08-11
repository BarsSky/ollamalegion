// thinking_lang_test.go — Round 32 #12 (2026-08-11): unit tests
// для language detection и language-aware thinking instruction.
//
// Покрывает:
//   - detectPrimaryLanguage: RU/EN/other detection, edge cases
//   - thinkingInstructionFor: правильный текст для каждого языка
//   - injectThinkingInstructionWithLang: prefix + append semantics
//   - injectThinkingIntoMessagesWithLang: prepend vs append system
//   - isLangCodeValid: валидация LangCode enum
//
// Все тесты идемпотентны и не зависят от внешнего состояния.
package main

import (
	"strings"
	"testing"
)

// TestDetectPrimaryLanguage_English — English content → "en".
func TestDetectPrimaryLanguage_English(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "What is the capital of France? Please answer in English."},
		{Role: "assistant", Content: "The capital of France is Paris."},
	}
	got := detectPrimaryLanguage(msgs)
	if got != LangEN {
		t.Errorf("expected LangEN for English conversation, got %s", got)
	}
}

// TestDetectPrimaryLanguage_Russian — Russian content → "ru".
// Это основной кейс из Round 32 #11 — пользователь жалуется на
// mixed RU/EN output, потому что soft prompt был на английском.
func TestDetectPrimaryLanguage_Russian(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: "You are a helpful assistant."}, // English system ignored
		{Role: "user", Content: "Привет! Расскажи кратко о космосе."},
		{Role: "assistant", Content: "Космос — это невероятно огромная и захватывающая тема."},
	}
	got := detectPrimaryLanguage(msgs)
	if got != LangRU {
		t.Errorf("expected LangRU for Russian conversation, got %s", got)
	}
}

// TestDetectPrimaryLanguage_RussianWithTechnicalTerms — Russian
// conversation с английскими technical terms (HTML, CSS) — должна
// определяться как Russian, потому что Cyrillic >> Latin.
func TestDetectPrimaryLanguage_RussianWithTechnicalTerms(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "Распиши подробно код html страницы о путешествии в космосе. Используй CSS3 и JavaScript."},
		{Role: "assistant", Content: "Это очень большой запрос. Создаю страницу с использованием HTML5, CSS3 для стилей и JavaScript для интерактивности."},
	}
	got := detectPrimaryLanguage(msgs)
	if got != LangRU {
		t.Errorf("expected LangRU for Russian + technical terms, got %s", got)
	}
}

// TestDetectPrimaryLanguage_FewLetters — менее 10 букв → fallback
// на English (безопасный дефолт).
func TestDetectPrimaryLanguage_FewLetters(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "Hi"}, // 2 letters
	}
	got := detectPrimaryLanguage(msgs)
	if got != LangEN {
		t.Errorf("expected LangEN fallback for very short text, got %s", got)
	}
}

// TestDetectPrimaryLanguage_EmptyMsgs — нет сообщений.
func TestDetectPrimaryLanguage_EmptyMsgs(t *testing.T) {
	got := detectPrimaryLanguage(nil)
	if got != LangEN {
		t.Errorf("expected LangEN fallback for nil msgs, got %s", got)
	}
}

// TestDetectPrimaryLanguage_OnlySystem — только system message
// (не считается — scan user/assistant only).
func TestDetectPrimaryLanguage_OnlySystem(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: "Привет мир"}, // Russian system, but ignored
	}
	got := detectPrimaryLanguage(msgs)
	if got != LangEN {
		t.Errorf("expected LangEN fallback when only system message present, got %s", got)
	}
}

// TestDetectPrimaryLanguage_OnlyEmojis — эмодзи без букв.
func TestDetectPrimaryLanguage_OnlyEmojis(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "🚀🌟✨"},
	}
	got := detectPrimaryLanguage(msgs)
	if got != LangEN {
		t.Errorf("expected LangEN fallback for emoji-only message, got %s", got)
	}
}

// TestThinkingInstructionFor_RU — Russian instruction содержит
// "Перед ответом" и явное указание отвечать на русском.
// Round 32 #13 (2026-08-11): также проверяет наличие CODE RULES.
func TestThinkingInstructionFor_RU(t *testing.T) {
	inst := thinkingInstructionFor(LangRU)
	if !strings.Contains(inst, "Перед ответом") {
		t.Error("RU instruction missing 'Перед ответом'")
	}
	if !strings.Contains(inst, "русском") {
		t.Error("RU instruction missing 'русском' (отвечай на русском)")
	}
	if !strings.Contains(inst, "<reasoning>") {
		t.Error("RU instruction missing <reasoning> tag instruction")
	}
	if !strings.Contains(inst, "</reasoning>") {
		t.Error("RU instruction missing </reasoning> tag instruction")
	}
	// Round 32 #13: code block rules
	if !strings.Contains(inst, "fenced") {
		t.Error("RU instruction missing 'fenced' (code block rule)")
	}
	if !strings.Contains(inst, "html") {
		t.Error("RU instruction missing 'html' (example language)")
	}
	if !strings.Contains(inst, "ЗАПРЕЩЕНО") {
		t.Error("RU instruction missing 'ЗАПРЕЩЕНО' (anti-pattern rule)")
	}
}

// TestThinkingInstructionFor_EN — English instruction содержит
// "Before answering" и tag wrapper.
// Round 32 #13: также проверяет наличие CODE RULES.
func TestThinkingInstructionFor_EN(t *testing.T) {
	inst := thinkingInstructionFor(LangEN)
	if !strings.Contains(inst, "Before answering") {
		t.Error("EN instruction missing 'Before answering'")
	}
	if !strings.Contains(inst, "<reasoning>") {
		t.Error("EN instruction missing <reasoning> tag instruction")
	}
	// Round 32 #13: code block rules
	if !strings.Contains(inst, "fenced") {
		t.Error("EN instruction missing 'fenced' (code block rule)")
	}
	if !strings.Contains(inst, "MUST be") {
		t.Error("EN instruction missing 'MUST be' (emphasis on code block requirement)")
	}
}

// TestThinkingInstructionFor_Other — universal short version.
// Round 32 #13: также проверяет наличие CODE RULES в universal version.
func TestThinkingInstructionFor_Other(t *testing.T) {
	inst := thinkingInstructionFor(LangOther)
	if !strings.Contains(inst, "<reasoning>") {
		t.Error("Other instruction missing <reasoning> tag")
	}
	if !strings.Contains(inst, "OUTSIDE") {
		t.Error("Other instruction missing 'OUTSIDE' (final answer outside tag)")
	}
	// Round 32 #13: code block rules even in universal short
	if !strings.Contains(inst, "fenced") {
		t.Error("Other instruction missing 'fenced' (code block rule)")
	}
	// Universal НЕ должен быть слишком длинным (efficient token use)
	if len(inst) > 600 {
		t.Errorf("Universal instruction too long: %d chars (max 600 after Round 32 #13 additions)", len(inst))
	}
}

// TestInjectThinkingInstructionWithLang_EmptySystem — пустой system,
// возвращает только instruction.
func TestInjectThinkingInstructionWithLang_EmptySystem(t *testing.T) {
	got := injectThinkingInstructionWithLang("", LangRU)
	if !strings.HasPrefix(got, "Перед ответом") {
		t.Errorf("expected RU instruction prefix, got: %q", got[:50])
	}
	if !strings.Contains(got, "<reasoning>") {
		t.Error("expected <reasoning> tag in output")
	}
}

// TestInjectThinkingInstructionWithLang_NonEmptySystem — non-empty
// system, instruction PREPENDED.
func TestInjectThinkingInstructionWithLang_NonEmptySystem(t *testing.T) {
	got := injectThinkingInstructionWithLang("You are a pirate.", LangRU)
	// RU instruction должна идти ПЕРЕД оригинальным system
	if !strings.HasPrefix(got, "Перед ответом") {
		t.Errorf("expected RU instruction at start, got: %q", got[:80])
	}
	if !strings.Contains(got, "You are a pirate.") {
		t.Error("expected original system content preserved at end")
	}
	// Должен быть separator между ними
	if !strings.Contains(got, "\n\n") {
		t.Error("expected \\n\\n separator between instruction and system")
	}
}

// TestInjectThinkingIntoMessagesWithLang_NoSystem — нет system msg,
// prepended.
func TestInjectThinkingIntoMessagesWithLang_NoSystem(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "Привет!"},
	}
	got := injectThinkingIntoMessagesWithLang(msgs, LangRU)
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if got[0].Role != "system" {
		t.Errorf("expected first msg to be system, got %s", got[0].Role)
	}
	if !strings.Contains(got[0].Content, "Перед ответом") {
		t.Error("prepended system should contain RU instruction")
	}
	if got[1].Role != "user" {
		t.Errorf("expected second msg to be user, got %s", got[1].Role)
	}
}

// TestInjectThinkingIntoMessagesWithLang_HasSystem — есть system,
// instruction CONCATENATED.
func TestInjectThinkingIntoMessagesWithLang_HasSystem(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: "You are a pirate."},
		{Role: "user", Content: "Hello!"},
	}
	got := injectThinkingIntoMessagesWithLang(msgs, LangEN)
	if len(got) != 2 {
		t.Fatalf("expected 2 messages (no prepending), got %d", len(got))
	}
	if !strings.Contains(got[0].Content, "Before answering") {
		t.Error("expected EN instruction in existing system message")
	}
	if !strings.Contains(got[0].Content, "You are a pirate.") {
		t.Error("expected original system content preserved")
	}
}

// TestIsLangCodeValid — sanity check для enum.
func TestIsLangCodeValid(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"en", true},
		{"ru", true},
		{"other", true},
		{"", false},
		{"EN", false}, // case-sensitive
		{"RU", false},
		{"Russian", false},
		{"zh", false},
	}
	for _, c := range cases {
		if got := isLangCodeValid(c.in); got != c.want {
			t.Errorf("isLangCodeValid(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestDetectPrimaryLanguage_RecentMessages — проверяет что скан
// последних 4 messages, а не всех. Если в начале EN (короткое), а в
// конце RU (длинное) — должен выиграть RU. Тест использует короткое
// EN вступление и длинный RU-диалог, чтобы ratio Cyrillic/Latin > 2.
func TestDetectPrimaryLanguage_RecentMessages(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "Hi!"}, // 2 Latin
		{Role: "assistant", Content: "Hello!"}, // 6 Latin
		{Role: "user", Content: "Привет! Расскажи мне о космосе, пожалуйста. " +
			"Меня интересуют планеты, звёзды и галактики. Какие самые интересные факты о Вселенной и далёких мирах? " +
			"Хочу узнать больше про чёрные дыры, квазары и пульсары."},
		{Role: "assistant", Content: "Космос полон удивительных объектов. " +
			"В нашей галактике Млечный Путь более ста миллиардов звёзд. " +
			"Существуют миллиарды других галактик с триллионами планет. " +
			"Астрономы открывают новые объекты каждый день."},
	}
	got := detectPrimaryLanguage(msgs)
	if got != LangRU {
		t.Errorf("expected LangRU (recent messages are Russian), got %s", got)
	}
}

// TestDetectPrimaryLanguage_BoundaryCase — почти равное количество
// Cyrillic и Latin, но Cyrillic должен выиграть (2x rule).
func TestDetectPrimaryLanguage_BoundaryCase(t *testing.T) {
	// 12 Cyrillic vs 5 Latin → ratio 2.4, должно быть "ru"
	msgs := []chatMessage{
		{Role: "user", Content: "Привет мир как дела сегодня HTML CSS JSON"}, // 17 Cyrillic + 9 Latin
	}
	got := detectPrimaryLanguage(msgs)
	if got != LangRU {
		t.Errorf("expected LangRU (Cyrillic > Latin * 2), got %s", got)
	}
}
