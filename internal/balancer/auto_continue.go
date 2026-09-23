// auto_continue.go — R60.21: detect truncated LLM responses and
// auto-retry with "continue" prompt to recover from model-side
// antiprompt emission (Qwen3-Instruct emits <|im_end|> mid-code).
//
// Problem (R60.20, 2026-09-08): Qwen3-Instruct-2507-q4km has a tendency
// to emit ChatML-EOS <|im_end|> token mid-code generation after ~800-1000
// tokens. C-bridge sees it as antiprompt match (c/bridge/bridge.c:1823-1840)
// and stops the stream. Client receives done_reason=stop with content like:
//
//	```python
//	import numpy as np
//	v0 = 50.0
//	... (model stops at `t = ...`)
//	← NO closing ```
//
// Streaming is lossless (cppworker + balancer faithfully pass through
// what model generated). The closing backticks were never generated —
// model decided "I'm done" mid-code.
//
// R60.21 fix: balancer-level detection + auto-retry:
//  1. After done_reason=stop, check final content with detectIncompleteResponse
//  2. If incomplete, automatically send "continue" request to upstream
//  3. Concatenate the continuation to original content
//  4. Return combined response to client
//
// R60.50 fix (2026-09-12): original PerformAutoContinue included the full
// original conversation history (User: "Привет распиши..." + Assistant: truncated).
// This caused Qwen3-Instruct to RESTART with "Привет! Конечно, вот..." instead
// of continuing from the truncation point. The model interprets the original
// user message as a fresh prompt and greets back.
//
// R60.50 fix: the continuation request contains ONLY a single user message
// with the truncated content embedded + explicit "continue from here" instruction.
// The model has no original user context to "restart" from, so it MUST continue.
// Operator opt-in via LB_AUTO_CONTINUE_ON_TRUNCATION env var (default: off).
package balancer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// truncateReasons — why we think response is truncated.
// Exposed for logging + diagnostics.
const (
	truncateReasonUnclosedCodeBlock = "unclosed_code_block"
	truncateReasonMidLineCutoff     = "mid_line_cutoff"
)

// truncateReasonEmpty — used when content is empty.
const truncateReasonEmpty = "empty"

// detectIncompleteResponse — pure function. Returns true if content
// looks truncated (would benefit from auto-continue retry).
//
// Heuristics (in priority order):
//  1. Odd ``` count → unclosed code block (R60.20 main pattern)
//  2. Last non-empty line ends with continuation char (=, (, {, [, ,, :)
//     → mid-line cutoff (R60.20 secondary pattern)
//
// Both heuristics are conservative — false positives are cheap
// (just one extra API call), false negatives are user-visible
// (response cut off, user sees the bug).
func DetectIncompleteResponse(content string) bool {
	if content == "" {
		return false // empty is empty, not truncated
	}
	// 1. Unclosed code block: odd number of ```
	backtickCount := strings.Count(content, "```")
	if backtickCount%2 != 0 {
		return true
	}
	// 2. Mid-line cutoff: check last non-empty line.
	// Strip trailing whitespace/newlines first.
	trimmed := strings.TrimRight(content, " \t\n\r")
	if trimmed == "" {
		return false
	}
	// Find last line.
	lastNewline := strings.LastIndex(trimmed, "\n")
	var lastLine string
	if lastNewline == -1 {
		lastLine = trimmed
	} else {
		lastLine = trimmed[lastNewline+1:]
	}
	// rstrip last line too — otherwise trailing whitespace
	// masks continuation chars like "= " at the end.
	lastLine = strings.TrimRight(lastLine, " \t")
	// Skip if last line is a "normal" closing — e.g. ends with punctuation
	// that signals completion.
	if endsWithCompleteStatement(lastLine) {
		return false
	}
	// Check for continuation chars: =, (, {, [, ,, :
	// (only the LAST char after rstrip, since the test cases have
	// "t_values = " with trailing space which the rstrip removes)
	if len(lastLine) > 0 {
		switch lastLine[len(lastLine)-1] {
		case '=', '(', '{', '[', ',', ':':
			return true
		}
	}
	// If last non-empty line is just whitespace or single char, also incomplete.
	if strings.TrimSpace(lastLine) == "" {
		return true
	}
	// Check if last line is unusually long (probably incomplete sentence/code).
	// This catches "very long line without terminal punctuation".
	if len(lastLine) > 200 && !endsWithCompleteStatement(lastLine) {
		return true
	}
	return false
}

// endsWithCompleteStatement — heuristic: does this line LOOK complete?
// Punctuation: . ! ? " ' ` ) ] } — or wrapped in code block fence ```.
// Also: empty line (just \n) is considered complete.
func endsWithCompleteStatement(line string) bool {
	if line == "" {
		return true
	}
	last := rune(line[len(line)-1])
	// Common statement terminators.
	switch last {
	case '.', '!', '?', ')', ']', '}', '"', '\'', '`':
		return true
	}
	// Chinese/Japanese full-stop (in case the response is in CJK)
	if last == '。' || last == '!' || last == '?' {
		return true
	}
	return false
}

// buildContinuePrompt — construct the FULL "continue" user message
// sent to model in auto-retry. R60.50: this is the ONLY message in the
// continuation request (we don't include original user/assistant history
// to prevent the model from "restarting" with a greeting).
//
// Must:
//  1. Reference the partial content (so model knows where to resume)
//  2. Explicitly ask to complete the code block
//  3. Forbid greeting/restarting (R60.50 fix)
//  4. Include the truncated text so the model can resume
//
// Returns the user message content. Caller wraps it in a single user
// message — no other messages in the request body.
func BuildContinuePrompt(partialContent string, reason string) string {
	var sb strings.Builder
	sb.WriteString("Ты — ассистент, который пишет ответ. Твой предыдущий ответ был прерван на середине. ")
	sb.WriteString("Продолжи ТОЧНО с того места, где остановился. ")
	switch reason {
	case truncateReasonUnclosedCodeBlock:
		sb.WriteString("Кодовый блок остался незазакрытым (нет финальной строки ```). ")
		sb.WriteString("Сгенерируй ТОЛЬКО продолжение кода, начиная с последнего выведенного символа. ")
		sb.WriteString("ОБЯЗАТЕЛЬНО закрой блок тремя обратными апострофами (```) в конце.")
	case truncateReasonMidLineCutoff:
		sb.WriteString("Последняя строка кода обрезана посередине. ")
		sb.WriteString("Сгенерируй ТОЛЬКО продолжение с места обрыва. ")
		sb.WriteString("Не повторяй уже написанное.")
	default:
		sb.WriteString("Сгенерируй ТОЛЬКО продолжение с места обрыва. ")
		sb.WriteString("Не повторяй уже написанное.")
	}
	// R60.50: explicit anti-greeting / anti-restart.
	sb.WriteString("\n\nВАЖНО:\n")
	sb.WriteString("- НЕ здоровайся заново (\"Привет\", \"Конечно\", и т.п.).\n")
	sb.WriteString("- НЕ повторяй уже написанное.\n")
	sb.WriteString("- Сразу продолжай с места обрыва.\n")
	// R60.50: forbid emitting literal ``` inside code blocks (Qwen3-Instruct antipattern
	// that breaks OpenWebUI markdown rendering). Use \\\` instead, or skip.
	sb.WriteString("- НЕ используй символы ``` внутри кода — они закрывают markdown блок раньше времени.\n")
	sb.WriteString("  Если нужен обратный апостроф внутри кода, замени на одинарный ` или экранируй.\n")

	// Include last 400 chars of partial content as context anchor.
	// Increased from 200 to 400 for better resumption context.
	anchor := partialContent
	if len(anchor) > 400 {
		anchor = anchor[len(anchor)-400:]
	}
	sb.WriteString("\n\nПоследние символы твоего ответа (для контекста):\n```\n...")
	sb.WriteString(anchor)
	sb.WriteString("\n```\n\nСгенерируй ТОЛЬКО продолжение:")
	return sb.String()
}

// truncateReason is exposed for logging.
func TruncateReason(content string) string {
	if content == "" {
		return truncateReasonEmpty
	}
	if strings.Count(content, "```")%2 != 0 {
		return truncateReasonUnclosedCodeBlock
	}
	trimmed := strings.TrimRight(content, " \t\n\r")
	if trimmed == "" {
		return truncateReasonEmpty
	}
	lastNewline := strings.LastIndex(trimmed, "\n")
	var lastLine string
	if lastNewline == -1 {
		lastLine = trimmed
	} else {
		lastLine = trimmed[lastNewline+1:]
	}
	if endsWithCompleteStatement(lastLine) {
		return ""
	}
	// R65d (2026-09-20): раньше здесь стоял `break` ПОСЛЕ switch, то есть
	// безусловный выход из for на ПЕРВОЙ итерации. Проверялся только первый
	// символ строки: для "function(" (первый символ 'f') функция возвращала "",
	// хотя последний символ '(' — явный признак обрыва. Из-за этого детектор
	// обрыва (и авто-продолжение R60.21, и метрика «ответ обрезан») срабатывал
	// только когда строка НАЧИНАЛАСЬ с '=' '(' '{' '[' ',' ':'.
	//
	// Правильная проверка — последний значимый символ строки.
	//
	// R66b (2026-09-22): набор символов был слишком широким и давал ложные
	// срабатывания на ЛЕГИТИМНЫХ ответах — авто-продолжение дёргало модель
	// повторно и дублировало контент:
	//   * '|' — последняя строка markdown-таблицы;
	//   * '/' — URL или путь в конце строки;
	//   * '*' — закрывающий символ **markdown-выделения**;
	//   * '-' — строка-разделитель или элемент списка;
	//   * '<' — HTML/шаблон, который не обязан закрываться в той же строке.
	// Теперь набор разделён: «сильные» символы (строка физически не может
	// корректно ими заканчиваться) и «слабые» (оператороподобные), которые
	// проверяются с учётом контекста строки. Кавычки/бэктики/звёздочки —
	// только при НЕПАРНОМ количестве в строке (незакрытая строка/выделение).
	if isMidLineCutoff(lastLine) {
		return truncateReasonMidLineCutoff
	}
	return ""
}

// isMidLineCutoff — true, если последняя строка выглядит оборванной посередине
// конструкции. См. комментарий в TruncateReason: разделение на сильные и слабые
// признаки сделано, чтобы не дёргать auto-continue на легитимных ответах.
func isMidLineCutoff(lastLine string) bool {
	ch := lastRune(lastLine)
	if ch == 0 {
		return false
	}
	switch ch {
	// СИЛЬНЫЕ: корректный ответ почти никогда не заканчивается этими символами.
	case '=', '(', '{', '[', ',', ':', '\\':
		return true
	// '*' — звёздочка: закрывает markdown-выделение (**жирный**), но также
	// является оператором умножения. Различаем по чётности: 4 звёздочки в
	// "**очень важно**" — завершённое выделение, одна в "total *" — обрыв.
	case '*':
		return strings.Count(lastLine, "*")%2 == 1
	// СЛАБЫЕ операторы: сами по себе легальны в конце строки (markdown-таблица,
	// URL, путь, HTML-тег), поэтому проверяем контекст.
	case '+', '-', '/', '&', '|', '<':
		return !looksLikeCompleteNonCodeLine(lastLine, ch)
	}
	// Кавычки и бэктики сюда не доходят: endsWithCompleteStatement() выше
	// считает строку, заканчивающуюся на " ' `, завершённой (кавычка может
	// быть легальным концом цитаты). Это осознанный консервативный выбор —
	// лучше не дёрнуть auto-continue на незакрытой кавычке, чем дублировать
	// корректные ответы.
	return false
}

// looksLikeCompleteNonCodeLine — true для строк, которые ЛЕГАЛЬНО заканчиваются
// на оператороподобный символ:
//   - строка markdown-таблицы: начинается с '|' либо состоит из |, -, :, пробелов
//     ("| a | b |", "|---|---|");
//   - URL или путь: содержит "://" или (для '/') не содержит пробелов;
//   - HTML/XML-строка: содержит '<' и '>' (тег закрыт где-то в строке).
func looksLikeCompleteNonCodeLine(line string, ch rune) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	// Markdown-таблица: | a | b |
	if strings.HasPrefix(t, "|") {
		return true
	}
	// Разделитель таблицы без ведущего '|': ---|---
	if isMarkdownTableSeparator(t) {
		return true
	}
	// URL
	if strings.Contains(t, "://") {
		return true
	}
	// Путь: проверяем ПОСЛЕДНИЙ токен строки (путь может стоять после текста:
	// "Файлы лежат в /usr/local/share/models/"). Одиночный "/" — это оператор
	// деления в коде, а не путь, поэтому требуем длину > 1.
	if ch == '/' {
		lastTok := t
		if i := strings.LastIndexAny(t, " \t"); i >= 0 {
			lastTok = t[i+1:]
		}
		if len(lastTok) > 1 && strings.Contains(lastTok, "/") {
			return true
		}
	}
	// HTML/XML: '<' встречается вместе с '>' в той же строке — тег закрыт.
	if ch == '<' && strings.Contains(t, ">") {
		return true
	}
	return false
}

// isMarkdownTableSeparator — "|---|---|", "--- | ---", ":--:" и т.п.
func isMarkdownTableSeparator(t string) bool {
	if t == "" {
		return false
	}
	hasDash := false
	for _, r := range t {
		switch r {
		case '-':
			hasDash = true
		case '|', ':', ' ':
			// допустимые символы разделителя
		default:
			return false
		}
	}
	return hasDash
}

// lastRune возвращает последний rune строки или 0, если строка пуста.
// Нужен отдельный хелпер, потому что range по строке даёт индексы байтов,
// а нам важен последний СИМВОЛ (UTF-8-безопасно, без аллокации среза).
func lastRune(s string) rune {
	if s == "" {
		return 0
	}
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

// IsAutoContinueOnTruncationEnabled — checks if LB_AUTO_CONTINUE_ON_TRUNCATION
// env var is set. Operator opt-in feature.
//
// Default: false (off). Set to "1", "true", "yes" to enable.
func IsAutoContinueOnTruncationEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_AUTO_CONTINUE_ON_TRUNCATION")))
	return v == "1" || v == "true" || v == "yes"
}

// IsContinuationARegeneration — R60.55 (2026-09-13): detects when the
// model's "continuation" is actually a regeneration of the original
// response from scratch (chat-model antipattern).
//
// Models like Qwen3-Instruct / Gemma-4 emit a "continue" prompt → they
// respond with a FRESH greeting + intro + (duplicate) code, starting
// with the same text as the original response.
//
// Heuristic (R60.55 v2): check if the continuation STARTS with a greeting
// pattern (Russian "Привет! Конечно" / English "Sure, here's..."). Real
// continuations start with code/text (e.g., "  const x = ..."). This
// is more reliable than substring matching (which falsely flagged real
// continuations that share substrings with original).
//
// Returns true if the continuation looks like a regeneration (should be
// suppressed to avoid duplicate responses in OpenWebUI).
func IsContinuationARegeneration(originalContent, continuationContent string) bool {
	if continuationContent == "" || originalContent == "" {
		return false
	}
	trimmed := strings.TrimLeft(continuationContent, " \t\n\r")
	if len(trimmed) < 10 {
		return false
	}
	// Strip common chat-model preamble patterns first.
	preambles := []string{
		"Sure, here's the continuation:",
		"Continuing from where I left off:",
		"Of course, here's the continuation:",
		"Продолжаю:",
		"Конечно, продолжаю:",
		"Sure! Here's the continuation:",
		// R65b (2026-09-15): more preamble patterns found in Qwen3-Instruct
		// generation after auto-continue prompt.
		"Хочешь добавить",
		"Продолжим",
		"Давайте продолжим",
		"Let's continue",
	}
	for _, p := range preambles {
		if strings.HasPrefix(trimmed, p) {
			// R65b: any preamble match is itself a sign of regeneration.
			// The model wouldn't emit "Sure, here's the continuation:" if it
			// were truly continuing mid-stream — it's restarting the response.
			return true
		}
	}
	// R65b (2026-09-15): Detect ChatML token leak — a strong signal that
	// the model entered role-play mode (assistant simulating a multi-turn
	// conversation in its own output). Symptom: "<|im_start|>assistant"
	// or "<|im_end|>" appears as LITERAL TEXT inside the continuation
	// content. Real continuations never contain these tokens.
	if strings.Contains(continuationContent, "<|im_start|>") ||
		strings.Contains(continuationContent, "<|im_end|>") {
		return true
	}
	// Check first 80 chars for greeting markers.
	// Russian: "Привет! Конечно" / "Привет!" / "Конечно,"
	// English: "Sure!" / "Of course" / "Certainly"
	// These are the typical chat-model "yes, I'll help" patterns
	// that appear at the start of regenerated responses.
	prefix := trimmed
	if len(prefix) > 80 {
		prefix = prefix[:80]
	}
	greetingPatterns := []string{
		"Привет! Конечно",
		"Привет! С удовольствием",
		"Здравствуйте",
		"Привет! Хорошо",
		"Sure!",
		"Of course,",
		"Certainly,",
		"Here's",
		// R65b (2026-09-15): common Qwen3-Instruct chat-model restart
		// patterns observed when continuation prompt included original
		// user message context.
		"Конечно!",
		"Ниже —",
		"Ниже представлен",
		"Below is",
	}
	for _, g := range greetingPatterns {
		if strings.HasPrefix(prefix, g) {
			return true
		}
	}
	// R66d (2026-09-23): similarity-проверка, ОБЕЩАННАЯ в docstring R60.55
	// («suppress the continuation if it's >85% similar to the original»), но так
	// и не реализованная — до этого проверялись только приветствия/преамбулы/
	// ChatML-утечка. Из-за этого проходил дубликат «слово-в-слово»: модель
	// (Qwen3.8-27B на A10, reasoning выключен) перегенерировала ответ с ТОГО ЖЕ
	// текста, без приветствия, и клиент (OpenWebUI) показывал ответ дважды.
	//
	// Признаки регенерации:
	//  1) длинный общий префикс с оригиналом — настоящий continue начинается
	//     там, где оригинал оборвался, а не с его начала;
	//  2) начало continuation уже присутствует в оригинале (модель повторила
	//     фрагмент);
	//  3) оригинал целиком является префиксом continuation (модель начала
	//     заново и продолжила дальше — симптом «A + (A+B)»).
	return isRegenerationBySimilarity(originalContent, continuationContent)
}

// minRegenerationPrefixRunes — минимальная длина общего префикса (в рунах),
// при которой continuation считается перегенерацией. 40 рун ≈ одна короткая
// фраза: реальный continue почти никогда не повторяет начало ответа дословно.
const minRegenerationPrefixRunes = 40

// minRegenerationContainmentRunes — минимальная длина совпадающего фрагмента
// для проверок «continuation уже есть в оригинале» / «оригинал — префикс
// continuation». Порог защищает от ложных срабатываний на коротких строках:
// 60 рун ≈ 8-10 слов дословного совпадения — это уже дубликат, а не случайное
// совпадение формулировки.
const minRegenerationContainmentRunes = 60

// minRegenerationOriginalRunes — минимальная длина оригинала (в нормализованных
// рунах), при которой вообще применяются similarity-проверки.
//
// Зачем порог: на КОРОТКИХ фрагментах кода/текста продолжение законно совпадает
// с оригиналом по форме (например, оригинал оборвался на
// «const height = parseFloat(document.getElementById('height').value);», а
// continuation начинается с той же строки кода в другом месте). Подавлять такое
// «продолжение» — значит молча терять контент, что хуже дубликата. Дубликат
// слово-в-слово, из-за которого пришла жалоба (Qwen3.8-27B на A10), — это
// длинный ответ, он порог проходит.
const minRegenerationOriginalRunes = 120

// normalizeForCompare — приводит текст к виду, устойчивому к форматированию:
// нижний регистр + схлопывание любых пробельных последовательностей в один
// пробел. Нужна, чтобы сравнение не зависело от переносов строк/отступов.
func normalizeForCompare(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// commonPrefixRunes — длина общего префикса двух строк в рунах.
func commonPrefixRunes(a, b string) int {
	ar, br := []rune(a), []rune(b)
	n := len(ar)
	if len(br) < n {
		n = len(br)
	}
	i := 0
	for i < n && ar[i] == br[i] {
		i++
	}
	return i
}

// longestPrefixPresentIn — длина самого длинного префикса cont (в рунах),
// который целиком встречается в haystack. Возвращает 0, если даже minRunes рун
// не найдены.
//
// Используется вместо фиксированного окна: continuation может повторять
// предложение оригинала и уходить в новый текст уже в середине этого
// предложения, поэтому проверяем префиксы убывающей длины (от всей доступной
// длины до minRunes), а не одно «окно» фиксированного размера.
func longestPrefixPresentIn(cont []rune, haystack string, minRunes int) int {
	limit := len(cont)
	if limit > maxRegenerationProbeRunes {
		limit = maxRegenerationProbeRunes
	}
	// Убывающие длины: первая найденная — самая длинная.
	for l := limit; l >= minRunes; l-- {
		if strings.Contains(haystack, string(cont[:l])) {
			return l
		}
	}
	return 0
}

// maxRegenerationProbeRunes — верхняя граница окна поиска повторённого
// фрагмента. Ограничение нужно, чтобы `strings.Contains` не гонялся по очень
// длинному continuation.
const maxRegenerationProbeRunes = 160

// isRegenerationBySimilarity — R66d: определяет, что continuation является
// повторной генерацией того же ответа, а не его продолжением.
//
// Работает на нормализованном тексте (регистр + пробелы), поэтому не зависит от
// markdown-отступов и переносов строк.
func isRegenerationBySimilarity(originalContent, continuationContent string) bool {
	orig := normalizeForCompare(originalContent)
	cont := normalizeForCompare(continuationContent)
	if len(orig) == 0 || len(cont) == 0 {
		return false
	}
	origRunes, contRunes := []rune(orig), []rune(cont)

	// На коротких оригиналах similarity-проверки не применяем: продолжение
	// законно повторяет форму короткого фрагмента (см. minRegenerationOriginalRunes).
	if len(origRunes) < minRegenerationOriginalRunes {
		return false
	}

	// 1) Длинный общий префикс.
	if commonPrefixRunes(orig, cont) >= minRegenerationPrefixRunes {
		return true
	}

	// 2) Начало continuation уже встречается в оригинале — модель повторила
	// фрагмент, который клиент уже получил.
	//
	// Ищем самый длинный префикс continuation, который целиком есть в оригинале:
	// сравнивать фиксированные 80 рун нельзя — continuation может повторять
	// предложение оригинала и уже в его середине уходить в новый текст
	// (пример: «...повторяется моделью. И дальше модель продолжает...»).
	if shared := longestPrefixPresentIn(contRunes, orig, minRegenerationContainmentRunes); shared > 0 {
		return true
	}

	// 3) Оригинал целиком повторён в начале continuation (A → A+B).
	if len(origRunes) >= minRegenerationContainmentRunes && len(contRunes) > len(origRunes) {
		if strings.HasPrefix(cont, orig) {
			return true
		}
	}

	return false
}

// IsAutoLoadAsyncEnabled — R60.33 (2026-09-10): если true (default),
// auto-load запускается в goroutine и balancer сразу возвращает
// 503+Retry-After (вместо sync wait 3 мин). Это решает проблему
// OpenWebUI/Cline timeout 60-120s на cold start (sync load = 3 мин
// → connection aborted → JSON parse error).
//
// Set LB_AUTO_LOAD_ASYNC=0 для legacy sync поведения (long-running
// requests, debug).
func IsAutoLoadAsyncEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_AUTO_LOAD_ASYNC")))
	// Default = true. Только explicit "0"/"false"/"no" отключают.
	if v == "0" || v == "false" || v == "no" {
		return false
	}
	return true
}

// IsNCtxReloadAsyncEnabled — R60.47 (2026-09-11): если true (default),
// n_ctx auto-reload (triggered when cppworker returns 400 prompt_too_long)
// запускается в goroutine и balancer СРАЗУ возвращает 503+Retry-After.
//
// Pre-R60.47: handleNCtxReloadActual делал sync DoReload с
// `?wait=true&waitTimeoutSec=300` — блокировал HTTP handler до 5 минут.
// OpenWebUI/Cline timeout 30-60s → клиент cancel-ил соединение → "empty
// response" / "Unexpected token" / EOF. Retry приводил к cascade.
//
// Post-R60.47: balancer возвращает 503+Retry-After за <50ms, клиент
// retry-ит через Retry-After, к этому моменту reload обычно завершён.
//
// Set LB_NCTX_RELOAD_ASYNC=0 для legacy sync reload+retry поведения
// (single round-trip, клиент получает финальный ответ за один запрос).
func IsNCtxReloadAsyncEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_NCTX_RELOAD_ASYNC")))
	// Default = true. Только explicit "0"/"false"/"no" отключают.
	if v == "0" || v == "false" || v == "no" {
		return false
	}
	return true
}

// GetAutoContinueMaxTokens — env override for max tokens in the
// continue request. Default 1024 (enough for code completion).
// Set LB_AUTO_CONTINUE_MAX_TOKENS=2048 to allow longer continuations.
func GetAutoContinueMaxTokens() int {
	v := strings.TrimSpace(os.Getenv("LB_AUTO_CONTINUE_MAX_TOKENS"))
	if v == "" {
		return 1024
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return 1024
}

// GetAutoContinueChatPolicy — R60.55 (2026-09-13): controls whether to
// auto-continue when the upstream is a chat model (Qwen3-Instruct,
// Gemma-4, etc.). Chat models can't truly "continue" — when given a
// continue prompt, they regenerate a full response with greeting.
//
// Options:
//   - "always" — always auto-continue (legacy behavior; produces duplicates
//     for chat models)
//   - "never" — never auto-continue (user gets incomplete response)
//   - "smart" — auto-continue, but suppress the continuation if it's >85%
//     similar to the original (treats as regeneration)
//
// Default: "smart" (R60.55). Set LB_AUTO_CONTINUE_CHAT_POLICY=always
// to restore legacy behavior, =never to disable entirely.
func GetAutoContinueChatPolicy() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_AUTO_CONTINUE_CHAT_POLICY")))
	switch v {
	case "always", "never", "smart":
		return v
	}
	return "smart"
}

// GetAutoContinueTimeout — env override for the auto-continue
// request's http.Client.Timeout. R60.25 fix: bumped default from
// 60s to 10 minutes because long-form generation (code, math
// articles, RAG-context) routinely exceeds 60s, and the original
// main request's timeout has already expired by the time we get
// here (so the model's load is fresh + warm but slow to first
// byte). Set LB_AUTO_CONTINUE_TIMEOUT_SEC=600 to raise to 10min.
// Set LB_AUTO_CONTINUE_TIMEOUT_SEC=0 to fall back to the previous
// 60s default (NOT recommended for long code).
func GetAutoContinueTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("LB_AUTO_CONTINUE_TIMEOUT_SEC"))
	if v == "" {
		return 600 * time.Second // R60.25 default: 10 minutes
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	return 600 * time.Second
}

// chatMessage — minimal struct for chat message. Both Ollama and
// OpenAI APIs use this format for the "messages" field.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// PerformAutoContinue — sends a "continue" request to upstream when
// truncation is detected. Returns the continuation content (string)
// and the new eval_count (if known).
//
// Algorithm (R60.50 — fixed from R60.21):
//  1. Parse originalBody just to extract the model name.
//  2. Build a SINGLE-MESSAGE user request containing:
//     - The truncated content as context anchor (last 400 chars)
//     - Explicit "continue from here, don't greet/restart" instruction
//  3. Re-marshal and POST non-streaming request to upstream.
//  4. Read response, extract content, return.
//
// R60.50 fix: original implementation sent FULL conversation history
// (User: original + Assistant: truncated + User: continue). The model
// saw the original user message ("Привет распиши...") and RESTARTED with
// a greeting instead of continuing. The fix sends ONLY a single user
// message with the truncated content embedded — the model has no original
// user context to "restart" from.
//
// Returns ("", 0, nil) on any error — caller treats as "no continuation".
// This means failed auto-continue doesn't break the response, just
// doesn't add anything.
//
// Caller is responsible for emitting the continuation to the client
// (as separate NDJSON/SSE chunks, then a final done chunk).
func PerformAutoContinue(
	upstreamURL string,
	originalBody []byte,
	accumulatedContent string,
	truncationReason string,
	httpClient *http.Client,
	timeout time.Duration,
) (continuationContent string, evalCount int, err error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if timeout <= 0 {
		// R60.25: default auto-continue timeout raised to 10 minutes
		// (env override LB_AUTO_CONTINUE_TIMEOUT_SEC). 60s was the
		// old default and too short for long-form code/RAG that the
		// main request also took >60s to produce.
		timeout = GetAutoContinueTimeout()
	}

	// 1. Parse original body to extract model name AND user's first message.
	// R60.55 root-cause fix (2026-09-13): instead of single-user-message continuation
	// (which caused the model to restart with greeting "Привет! Конечно..." — the
	// antiprompt restart antipattern), use MULTI-TURN CHAT continuation: send
	// [user: original_prompt, assistant: partial_content, user: continue instruction].
	// The model sees this as a mid-conversation continuation → no greeting → no
	// duplicate "Привет!" in the response. Eliminates the need for R60.55 smart-policy
	// filter (which was a band-aid hiding the real bug).
	//
	// Parsing strategy: try Ollama /api/chat format first, then OpenAI /v1/chat/completions
	// format, then fall back to single-user-message (legacy) for unknown formats.
	var reqBody struct {
		Model    string        `json:"model"`
		Messages []chatMessage `json:"messages"`
	}
	if err := json.Unmarshal(originalBody, &reqBody); err != nil {
		return "", 0, err
	}
	if reqBody.Model == "" {
		return "", 0, errEmptyMessages
	}

	// Find the user's last message (most recent "user" role in original conversation).
	// This is what we want the model to continue responding to.
	var originalUserPrompt string
	for i := len(reqBody.Messages) - 1; i >= 0; i-- {
		if reqBody.Messages[i].Role == "user" {
			originalUserPrompt = reqBody.Messages[i].Content
			break
		}
	}

	var continuationMessages []chatMessage
	if originalUserPrompt != "" {
		// R60.55 proper fix: multi-turn chat continuation.
		// Model sees: [user: original prompt, assistant: my partial response,
		//              user: "continue from where you stopped"]
		// → model treats as mid-conversation, continues without greeting.
		continuationMessages = []chatMessage{
			{Role: "user", Content: originalUserPrompt},
			{Role: "assistant", Content: accumulatedContent},
			{Role: "user", Content: "Продолжи с того места, где остановился. Без приветствий, без повторов, без перезапуска. Сразу продолжай код/текст."},
		}
	} else {
		// Fallback: no user message found (unusual). Use legacy single-user-message
		// with embedded partial content. R60.55 smart-policy will catch regen if any.
		continuationPrompt := BuildContinuePrompt(accumulatedContent, truncationReason)
		continuationMessages = []chatMessage{
			{Role: "user", Content: continuationPrompt},
		}
	}

	// Force non-streaming for the continue request (simpler to handle).
	// Stream was true for original; we want to read the full response
	// before returning, since we already streamed the original to client.
	// Limit continuation size to prevent runaway.
	//
	// R60.21: cppworker's /v1/chat/completions strict decoder REJECTS
	// unknown fields. Ollama-specific `num_predict` is NOT in the OpenAI
	// spec, so cppworker returns 400 "invalid JSON: unknown field
	// \"num_predict\"". Use only `max_tokens` (OpenAI standard).
	maxTokens := GetAutoContinueMaxTokens()
	newBody, err := json.Marshal(struct {
		Model     string        `json:"model"`
		Messages  []chatMessage `json:"messages"`
		Stream    bool          `json:"stream"`
		MaxTokens int           `json:"max_tokens,omitempty"`
	}{
		Model:     reqBody.Model,
		Messages:  continuationMessages,
		Stream:    false,
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", 0, err
	}

	// 4. POST to upstream (non-streaming)
	url := strings.TrimRight(upstreamURL, "/")
	if !strings.HasSuffix(url, "/v1/chat/completions") &&
		!strings.HasSuffix(url, "/api/chat") {
		// Default: assume OpenAI-compatible /v1/chat/completions for the continue.
		// If the original was /api/chat (Ollama), the user-agent path
		// proxyRequestOpenAI handles translation.
		url = url + "/v1/chat/completions"
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(newBody)))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Read error body for diagnostics
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", 0, fmt.Errorf("upstream status %d: %s", resp.StatusCode, string(errBody))
	}

	// 5. Read response, extract content
	var respBody struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return "", 0, err
	}
	if len(respBody.Choices) == 0 {
		return "", 0, errEmptyChoices
	}
	return respBody.Choices[0].Message.Content, respBody.Usage.CompletionTokens, nil
}

// Sentinel errors for PerformAutoContinue.
var (
	errEmptyMessages = simpleError("empty messages in request body")
	errUpstreamNonOK = simpleError("upstream returned non-OK status")
	errEmptyChoices  = simpleError("upstream response had no choices")
)

type simpleError string

func (e simpleError) Error() string { return string(e) }

// EmitContinuationNDJSON — write the continuation content to client as
// SEPARATE streaming NDJSON chunks (NOT a combined done-chunk with the
// original content concatenated).
//
// R65a (2026-09-15) BUGFIX: original implementation concatenated
//
//	combined := originalContent + continuationContent
//
// and emitted that as ONE final done-chunk. This caused content
// DUPLICATION in OpenWebUI: the client already received originalContent
// via streaming chunks (done:false each), then received a final
// done:chunk with message.content = originalContent + continuationContent.
// OpenWebUI's parser does not consistently replace accumulated content
// with the done-chunk's message.content — many implementations APPEND,
// producing A + (A+B) = "duplicated text" symptom where the model
// response appears twice.
//
// Correct fix: emit continuation as additional done:false streaming
// chunks, then a final done:true chunk. Client accumulates all chunks
// naturally: A1 + A2 + ... + An + B1 + B2 + ... + Bn (final) = correct.
// No duplication, regardless of client's done-chunk handling.
//
// If continuationContent is empty, only the final done-chunk is emitted.
func EmitContinuationNDJSON(
	w http.ResponseWriter,
	apiPath string,
	modelName string,
	originalContent string,
	continuationContent string,
	totalEvalCount int,
	flusher http.Flusher,
) error {
	// R60.50: strip interior ``` that break markdown rendering (Qwen3-Instruct
	// antipattern — emits ``` inside JS code which prematurely closes outer fence).
	continuationContent = FixMarkdownCodeFences(continuationContent)

	writeChunk := func(contentField string, done bool) error {
		var ollamaChunk map[string]interface{}
		if apiPath == "/api/chat" {
			msg := map[string]string{"role": "assistant"}
			if done {
				msg["content"] = ""
			} else {
				msg["content"] = contentField
			}
			ollamaChunk = map[string]interface{}{
				"model":      modelName,
				"created_at": time.Now().UTC().Format(time.RFC3339Nano),
				"done":       done,
				"message":    msg,
			}
			if done {
				ollamaChunk["done_reason"] = "stop"
				ollamaChunk["total_duration"] = int64(0)
				ollamaChunk["eval_count"] = totalEvalCount
			}
		} else if apiPath == "/api/generate" {
			resp := ""
			if !done {
				resp = contentField
			}
			ollamaChunk = map[string]interface{}{
				"model":      modelName,
				"created_at": time.Now().UTC().Format(time.RFC3339Nano),
				"done":       done,
				"response":   resp,
			}
			if done {
				ollamaChunk["done_reason"] = "stop"
				ollamaChunk["total_duration"] = int64(0)
				ollamaChunk["eval_count"] = totalEvalCount
			}
		} else {
			return simpleError("unsupported api path for EmitContinuationNDJSON: " + apiPath)
		}
		out, err := json.Marshal(ollamaChunk)
		if err != nil {
			return err
		}
		out = append(out, '\n')
		if _, err := w.Write(out); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	// Emit continuation as ONE additional streaming chunk (done:false) —
	// if non-empty. This way the client appends it to already-streamed content.
	if continuationContent != "" {
		if err := writeChunk(continuationContent, false); err != nil {
			return err
		}
	}

	// Emit final done:chunk to signal stream completion. Empty content
	// (since the full content has already been streamed + sent as continuation).
	if err := writeChunk("", true); err != nil {
		return err
	}
	return nil
}

// Sanitize for utf8RuneCount — no-op removed, no extra imports needed.

// FixMarkdownCodeFences — R60.50 (2026-09-12): keep the FIRST ``` (for syntax
// highlighting) and replace ALL subsequent ``` with non-fence representation.
// Qwen3-Instruct reflexively emits ``` inside JavaScript template literals /
// comments / strings, which prematurely closes the outer markdown code block
// and breaks OpenWebUI rendering.
//
// Strategy:
//  1. Find first ``` — keep it (preserves code-block syntax highlighting)
//  2. Replace all subsequent ``` with ` ` (single backtick + space +
//     backtick, which is NOT a valid markdown fence)
//
// Trade-off: response still has ONE nice code block (the first one) for
// syntax highlighting. Any additional ``` (which are model antipatterns)
// are converted to non-fences, preserving the character content but
// preventing rendering breaks.
//
// Returns the cleaned content.
func FixMarkdownCodeFences(content string) string {
	if !strings.Contains(content, "```") {
		return content
	}
	firstFence := strings.Index(content, "```")
	if firstFence == -1 {
		return content
	}
	// Keep the first ```, replace all subsequent ones.
	before := content[:firstFence+3]
	after := content[firstFence+3:]
	after = strings.ReplaceAll(after, "```", "` `")
	return before + after
}
