// thinking_lang.go — Round 32 #12 (2026-08-11): language detection
// + language-specific thinking instructions for soft prompt path.
//
// Проблема (Round 32 #11 follow-up): cppworker's soft prompt для
// EnableReasoning=true инжектит АНГЛИЙСКУЮ thinking instruction
// ("Before answering, use detailed step-by-step thinking...") в
// system prompt. Когда user пишет на русском (или другом не-англ
// языке), gemma-4/Qwen-style модель реагирует multilingual output'ом:
// mixed RU/EN tokens на границах слов ("космическийvoyage",
// "чистый (структура)" без "HTML", "сти" вместо "стиль").
// Это поведение модели при mixed-language prompt, не bug cppworker'а,
// но смягчается language-specific instruction.
//
// Решение:
//   1. detectPrimaryLanguage(msgs) — сканирует user/assistant content,
//      считает Cyrillic vs Latin буквы, возвращает "ru"/"en"/"other".
//   2. thinkingInstructionFor(lang) — возвращает thinking instruction
//      на соответствующем языке. "other" использует universal short
//      version без явного языка.
//
// detectPrimaryLanguage берёт последние user/assistant сообщения
// (самое свежее = релевантное для генерации), считает символы:
//
//	Cyrillic (U+0400..U+04FF) — русский, украинский, болгарский и т.п.
//	Latin (U+0020..U+007E) — английский, испанский, немецкий и т.п.
//
// Если Cyrillic > Latin * 2 → "ru"
// Иначе если Latin > 0 → "en"
// Иначе → "other"
//
// Безопасный fallback: если ничего не распознано (только спецсимволы,
// эмодзи, цифры) — "en" (текущее поведение). Это гарантирует, что
// дефолтный English instruction остаётся для edge cases.
package main

import (
	"strings"
	"unicode"
)

// LangCode — detected primary language of conversation.
// Используется для выбора language-specific thinking instruction.
type LangCode string

const (
	LangEN    LangCode = "en"    // English (или другой Latin-script язык)
	LangRU    LangCode = "ru"    // Russian (Cyrillic-script)
	LangOther LangCode = "other" // нераспознанный (CJK, арабский, etc.)
)

// detectPrimaryLanguage — определение основного языка conversation
// по последним user/assistant messages. Возвращает "ru"/"en"/"other".
//
// Пропускает system messages (они часто на английском от OpenWebUI и
// не отражают язык диалога). Сканирует user/assistant только.
//
// Подсчёт идёт по СОДЕРЖИМОМУ символов (rune), не по байтам —
// иначе Cyrillic в UTF-8 даст 2 байта на букву и исказит пропорцию.
func detectPrimaryLanguage(msgs []chatMessage) LangCode {
	cyrillicCount := 0
	latinCount := 0
	scannedMsgs := 0

	// Берём последние 4 user/assistant messages (свежие = релевантные).
	// Если меньше — сканируем все.
	const maxScan = 4
	const minTotalForDetection = 10 // минимум букв чтобы делать вывод

	for i := len(msgs) - 1; i >= 0 && scannedMsgs < maxScan; i-- {
		m := msgs[i]
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		scannedMsgs++
		for _, r := range m.Content {
			switch {
			case unicode.Is(unicode.Cyrillic, r):
				cyrillicCount++
			case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
				latinCount++
			}
		}
	}

	total := cyrillicCount + latinCount
	if total < minTotalForDetection {
		// Мало букв — нельзя надёжно определить язык.
		// Дефолт — English (текущее поведение для edge cases).
		return LangEN
	}

	// Cyrillic must be at least 2x Latin для уверенного "ru" —
	// иначе Latin может быть из технических терминов (HTML, CSS, JSON)
	// в русском тексте.
	if cyrillicCount > latinCount*2 {
		return LangRU
	}
	if latinCount > 0 {
		return LangEN
	}
	return LangOther
}

// thinkingInstructionFor — language-specific thinking instruction.
//
// Round 32 #13 (2026-08-11): добавлены explicit code block rules
// во все версии. Live test с gemma-4 (Round 32 #12) показал что
// модель путает структуру code block'ов: explanations внутри
// ```code```, HTML/CSS смешаны, иногда только close без open.
// Добавление явного правила "ВСЕГДА в fenced blocks, НИКОГДА inline"
// решает это на уровне instruction.
//
// Структура каждой инструкции:
//  1. Краткое указание "think step by step" на языке диалога
//  2. Ссылка на язык ответа (важно — иначе модель отвечает на языке
//     инструкции, а не на языке вопроса)
//  3. Требование обернуть рассуждение в <reasoning>...</reasoning>
//  4. Указание что финальный ответ ВНЕ тега
//  5. (Round 32 #13) Explicit code block rules: fenced blocks
//     only, language specified, never inline
//
// Universal ("other") version — минимальная, на английском (token
// efficient). Используется для CJK/арабского/etc. где перевод
// сложно сделать в runtime.
func thinkingInstructionFor(lang LangCode) string {
	switch lang {
	case LangRU:
		return "Перед ответом используй подробное пошаговое рассуждение. " +
			"Тщательно продумай задачу, рассмотри разные точки зрения, " +
			"покажи свою работу, затем дай чёткий финальный ответ. " +
			"Отвечай на русском языке. " +
			"ВАЖНО: Оберни пошаговое рассуждение в теги <reasoning>...</reasoning>. " +
			"Твой финальный ответ должен быть ВНЕ тега </reasoning>. " +
			"ПРАВИЛА ДЛЯ КОДА: Весь многострочный код (HTML, CSS, JavaScript, Python и т.д.) " +
			"ОБЯЗАТЕЛЬНО оборачивай в fenced code blocks с указанием языка: " +
			"```html\n[код]\n``` или ```css\n[код]\n```. " +
			"ЗАПРЕЩЕНО использовать inline backticks (`code`) для многострочного кода. " +
			"ЗАПРЕЩЕНО смешивать explanations с кодом внутри code block — " +
			"explanations ВСЕГДА снаружи, code ВСЕГДА внутри ```."
	case LangEN:
		return "Before answering, use detailed step-by-step thinking. " +
			"Reason about the problem carefully, consider different angles, " +
			"show your work, then provide a clear final answer. " +
			"IMPORTANT: Wrap your step-by-step reasoning inside <reasoning>...</reasoning> tags. " +
			"Your final answer (the user-facing response) should be OUTSIDE the </reasoning> tag. " +
			"CODE RULES: ALL multi-line code (HTML, CSS, JavaScript, Python, etc.) " +
			"MUST be wrapped in fenced code blocks with the language specified: " +
			"```html\n[code]\n``` or ```css\n[code]\n```. " +
			"NEVER use inline backticks (`code`) for multi-line code. " +
			"NEVER mix explanations with code inside a code block — " +
			"explanations ALWAYS outside, code ALWAYS inside ```."
	default: // LangOther — universal short
		return "Think step by step before answering. " +
			"Wrap your step-by-step reasoning in <reasoning>...</reasoning> tags. " +
			"Your final answer must be OUTSIDE the </reasoning> tag. " +
			"CODE RULES: All multi-line code MUST be in fenced blocks (```lang\ncode\n```). " +
			"NEVER use inline backticks for multi-line code. " +
			"Explanations outside, code inside. " +
			"Respond in the same language as the user's question."
	}
}

// injectThinkingInstructionWithLang — language-aware version.
//
// Round 32 #12 (2026-08-11): вынесена в отдельную функцию.
// Использует thinkingInstructionFor(lang) вместо hardcoded English.
func injectThinkingInstructionWithLang(system string, lang LangCode) string {
	instruction := thinkingInstructionFor(lang)
	if system == "" {
		return instruction
	}
	return instruction + "\n\n" + system
}

// injectThinkingIntoMessagesWithLang — language-aware version
// для fallback path (buildChatPromptFromMessages).
func injectThinkingIntoMessagesWithLang(msgs []chatMessage, lang LangCode) []chatMessage {
	thinking := thinkingInstructionFor(lang)
	for i, m := range msgs {
		if m.Role == "system" {
			msgs[i].Content = thinking + "\n\n" + m.Content
			return msgs
		}
	}
	systemMsg := chatMessage{Role: "system", Content: thinking}
	return append([]chatMessage{systemMsg}, msgs...)
}

// String representation for logging.
func (l LangCode) String() string {
	switch l {
	case LangRU:
		return "ru"
	case LangEN:
		return "en"
	default:
		return "other"
	}
}

// isLangCodeValid — sanity check для unit tests.
func isLangCodeValid(s string) bool {
	return s == string(LangEN) || s == string(LangRU) || s == string(LangOther)
}

// trimNonEmpty — helper для тестов: returns "yes" если s содержит substr.
func trimNonEmpty(s, substr string) bool {
	return strings.Contains(s, substr)
}
