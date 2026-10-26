// tools_stream_r83.go — R83/v56 (2026-10-01): потоковая отдача content на
// tools-пути /api/chat.
//
// ЗАЧЕМ. Раньше writeChatStreamResponseWithTools буферизовал ответ целиком и
// отдавал его ОДНИМ финальным чанком (Round 6 Fix 5: так проще распарсить
// tool_calls и не показать клиенту сырой JSON). Цена — клиент не видел ни одного
// токена, пока модель генерирует: на стенде он получал только
// {"keepalive":true} раз в 15 секунд. На медленном инстансе (5-9 tok/s,
// 35/42 слоя) Cline успевал отвалиться по своему таймауту и показывал
// «Model returned empty response», хотя модель продолжала работать
// (debug/last-stream: tokens_sent=0, bytes_written=380 за 306 с).
//
// КАК НЕ СЛОМАТЬ TOOL CALL. Держим неотправленным окно toolsStreamHoldback
// байт: маркер начала tool call (самый длинный — "<|tool_call|>", 13 байт)
// физически не успевает уйти клиенту, пока он не собран целиком. Плюс два
// предохранителя:
//   - если первый непробельный символ ответа — '{' или '[', отдача не начинается
//     вовсе: парсер (стратегия 4) трактует такой JSON как tool_calls, а показать
//     его в content значит и не вызвать инструмент, и напечатать мусор;
//   - как только маркер найден, дальнейший content не отдаётся — буфер целиком
//     уходит в parseToolCallsFromOutput.
//
// Отключается через CPPWORKER_TOOLS_STREAM_CONTENT=false — тогда работает
// прежнее поведение (один финальный чанк со всем ответом).
package main

import (
	"os"
	"strings"
	"unicode/utf8"
)

// toolsStreamHoldback — сколько байт ответа держим неотправленными.
// Больше самого длинного маркера (<|tool_call|> = 13) с запасом на то, что
// маркер придёт по частям и с соседним текстом в одном токене.
const toolsStreamHoldback = 32

// toolsStreamMarkers — начала маркеров tool call в текстовом виде. Список шире,
// чем trailingToolTokens: сюда входят и открывающие маркеры (у gemma-4 это
// "<tool_call|>", у Hermes/Qwen — "<tool_call>").
var toolsStreamMarkers = []string{
	"<tool_call>",
	"<tool_call|>",
	"<|tool_call|>",
	// R109 (2026-10-13): нативный открывающий тег gemma-4 — БЕЗ пайпа на конце.
	// Живой случай (Cline CLI + gemma-4, stream=true): вывод был
	// "<|tool_call>call:run_commands{...}<tool_call|>", маркера в списке не было,
	// поэтому блок не удерживался и уехал клиенту как content — Cline показал
	// вызов инструмента текстом (hadToolCalls=false).
	"<|tool_call>",
	"</tool_call>",
	"<|python_tag|>",
	"[TOOL_CALLS]",
	"[TOOL_CALL]",
	// R246 (2026-10-26): диалект gemma tool_code. Без этих маркеров блок
	// уезжал клиенту дельтами как обычный текст (живой дефект: DeepSeek
	// Harness + gemma-4, 25 инструментов — «инструмент не вызван»).
	"[tool_code]",
	"```tool_code",
}

// toolsStreamContentEnabled — включена ли потоковая отдача content на tools-пути.
// По умолчанию включена; CPPWORKER_TOOLS_STREAM_CONTENT=false возвращает прежнее
// поведение (весь ответ одним чанком) — аварийный рубильник на случай клиента,
// который не умеет собирать дельты.
func toolsStreamContentEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_TOOLS_STREAM_CONTENT"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// toolsStreamJSONStructPos — R114b (2026-10-13): позиция начала JSON-структуры в
// непереданном хвосте буфера.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ПРОВЕРКА. Прежние предохранители смотрели на НАЧАЛО всего
// ответа (toolsStreamStartsWithJSON) и на ключи ВНУТРИ JSON-объекта. Живой
// дефект из сохранённой сессии Cline: модель написала прозу, затем JSON вызова, и
// клиент получил
//
//	…I consider the task complete. [{"id":"call_submit_and_exit","type":
//	"function","function":{"name":"submit_and_exit","arguments":{"summary":
//	"The directory was listed, …
//
// — ОБРЫВОК вызова в теле ответа. Причина: объект начался раньше, чем в буфере
// появился ключ ("arguments"), а окно удержания — 32 байта, поэтому начало JSON
// успело уйти дельтой.
//
// Правило: как только в непереданном хвосте видно НАЧАЛО JSON-структуры
// ('{' или '[', за которым сразу идёт '"', '{' или '['), дельты прекращаем.
// Требование к следующему символу отсекает одиночные фигурные скобки в прозе.
//
// Возвращает (позицию, true). window — размер проверяемой зоны (окно удержания +
// текущий токен).
func toolsStreamJSONStructPos(buf string, window int) (int, bool) {
	if buf == "" {
		return 0, false
	}
	limit := len(buf) - window
	if limit < 0 {
		limit = 0
	}
	tail := buf[limit:]
	for i := 0; i+1 < len(tail); i++ {
		switch tail[i] {
		case '{':
			if n := tail[i+1]; n == '"' || n == '{' || n == '[' {
				return limit + i, true
			}
		case '[':
			if n := tail[i+1]; n == '{' || n == '[' || n == '"' {
				return limit + i, true
			}
		}
	}
	// Последний непереданный символ — '{' или '[': структура началась, следующий
	// символ ещё не сгенерирован. Держим, чтобы не выпустить её первой байт.
	if n := len(tail); n > 0 {
		if last := tail[n-1]; last == '{' || last == '[' {
			return limit + n - 1, true
		}
	}
	// Хвост ЦЕЛИКОМ начинается с '{'/'[' (например «{», «[{"…).
	trimmed := strings.TrimLeft(tail, " \t\r\n")
	if trimmed != "" && (trimmed[0] == '{' || trimmed[0] == '[') {
		return len(buf) - len(trimmed), true
	}
	return 0, false
}

// toolsStreamHoldsTailStart — R114b (2026-10-13): нужно ли удержать дельты из-за
// содержимого непереданного хвоста (кроме JSON-структуры — её ищет
// toolsStreamJSONStructPos).
//
// Условие: в хвосте появился ОТКРЫВАЮЩИЙ маркер размышлений. Блок, начавшийся
// ПОСЛЕ уже отданной прозы, — тоже не ответ: его место в message.thinking.
//
// Возвращает причину удержания и true, если удерживать нужно.
func toolsStreamHoldsTailStart(tail string) (string, bool) {
	if tail == "" {
		return "", false
	}
	trimmed := strings.TrimLeft(tail, " \t\r\n")
	if trimmed == "" {
		return "", false
	}
	if containsReasoningOpenMarker(trimmed) {
		return "открывающий маркер размышлений в хвосте", true
	}
	// R114b: служебный токен шаблона в хвосте — тоже не текст ответа: живой
	// случай «other coding task.\n<end_of_turn>» в content.
	//
	// ВАЖНО: проверяем только токен ЦЕЛИКОМ. Проверка «хвост заканчивается
	// началом токена» (tailEndsWithServiceTokenPrefix) здесь НЕ применяется: она
	// удерживала обычную прозу, за которой шёл маркер вызова (`…файл. <tool_`),
	// и ломала потоковую отдачу прозы (тест TestR83v56_ToolCallDoesNotLeakIntoContent).
	// Незавершённый токен доберётся следующим чанком и будет вырезан в
	// emitContentDelta (stripServiceTokensKeepSpaces) — то есть целиком в content
	// он не попадёт.
	if tailContainsServiceToken(trimmed) {
		return "служебный токен в хвосте", true
	}
	return "", false
}

// toolsStreamStartsWithJSON — ответ начинается с '{' или '[' (после пробелов).
// Такой ответ парсер может распознать как tool_calls целиком, поэтому дельтами
// его не отдаём.
func toolsStreamStartsWithJSON(buf string) bool {
	trimmed := strings.TrimLeft(buf, " \t\r\n")
	if trimmed == "" {
		return false
	}
	return trimmed[0] == '{' || trimmed[0] == '['
}

// toolsStreamJSONCallKeys — ключи JSON-объекта, по которым он опознаётся как
// ВЫЗОВ инструмента (а не как обычный JSON в ответе модели).
var toolsStreamJSONCallKeys = []string{
	`"tool_call"`, `"tool_calls"`, `"tool_name"`, `"function"`, `"arguments"`,
}

// toolsStreamJSONCallPos — R113 (2026-10-13): начало JSON-объекта, похожего на
// вызов инструмента, если он попал в зону удержания.
//
// ЗАЧЕМ. Прежние предохранители держали дельты только когда ВЫВОД НАЧИНАЛСЯ с
// JSON/фенса. Живой прогон Cline-запроса (gemma-4, 25 tools) дал другое: сначала
// шла проза, затем JSON вызова — и он уехал клиенту дельтами. В записи сессии это
// выглядело как «инструмент в теле ответа» (content 2456 символов с сырым JSON).
//
// Правило — по содержимому, без имён моделей: если в удерживаемом окне есть
// JSON-объект с ключом вызова, начиная с его '{' дельты не отдаём (весь вывод
// уйдёт в разбор и в финальный чанк). Частично дописанный ключ в хвосте тоже
// удерживается, иначе начало вызова успело бы уйти дельтой.
//
// window — размер зоны удержания в байтах (toolsStreamHoldback + текущий токен).
func toolsStreamJSONCallPos(buf string, window int) (int, bool) {
	if buf == "" {
		return 0, false
	}
	limit := len(buf) - window
	if limit < 0 {
		limit = 0
	}
	best := -1
	for _, key := range toolsStreamJSONCallKeys {
		idx := strings.LastIndex(buf, key)
		if idx < limit {
			continue
		}
		open := strings.LastIndex(buf[:idx], "{")
		if open < 0 || open < limit {
			continue
		}
		if best < 0 || open < best {
			best = open
		}
	}
	if best >= 0 {
		return best, true
	}
	// Частичный ключ в хвосте: генерация ещё не дописала `"tool_call"` и т.п.
	tailStart := len(buf) - 24
	if tailStart < 0 {
		tailStart = 0
	}
	tail := buf[tailStart:]
	for _, key := range toolsStreamJSONCallKeys {
		for i := 2; i < len(key); i++ {
			if strings.HasSuffix(tail, key[:i]) {
				if open := strings.LastIndex(tail, "{"); open >= 0 {
					return tailStart + open, true
				}
			}
		}
	}
	return 0, false
}

// toolsStreamStartsWithFence — R106d (2026-10-12): ответ начинается с
// markdown-фенса (``` или ```json).
//
// ЖИВОЙ ДЕФЕКТ (Cline → балансер → cppworker, gemma-4 с 18 инструментами):
// модель положила вызов инструмента в ```json-блок, и клиент получил его
// ДЕЛЬТАМИ как обычный текст:
//
//	{"message":{"content":"```j"}}, {"content":"so"}, {"content":"n"},
//	{"content":"\n[\n "}, {"content":"{\n    \"i"}, …
//
// а финальный чанк пришёл с done_reason:"stop" и БЕЗ message.tool_calls —
// инструмент не вызывался, пользователь видел сырой JSON в ответе. Причина:
// предохранитель отдавал дельты, пока первый непробельный символ не '{'/'[',
// а тут первым идёт фенс.
//
// Фенс — такой же признак «дальше структура, а не текст»: при включённых
// инструментах не отдаём дельты вовсе, а в финальном чанке отдаём либо
// tool_calls, либо очищенный content.
func toolsStreamStartsWithFence(buf string) bool {
	return strings.HasPrefix(strings.TrimLeft(buf, " \t\r\n"), "```")
}

// toolsStreamMarkerPos — позиция самого раннего маркера tool call в последних
// tailLen байтах буфера (ok=false — маркера нет).
//
// Возвращаем ПОЗИЦИЮ, а не bool: prose, стоящий перед маркером, обязан уйти
// клиенту. Без этого окно удержания «съедало» до 32 байт полезного текста перед
// tool call (живой пример: «Сейчас прочитаю файл.» → клиент видел «Сейчас »).
//
// Сканируем только хвост, а не весь буфер: маркер не может «появиться» в уже
// проверенной части, а полный проход по буферу на каждый токен — это O(n²).
// tailLen должен быть не меньше toolsStreamHoldback + длины самого длинного
// маркера (вызывающий передаёт toolsStreamHoldback + длину текущего токена).
func toolsStreamMarkerPos(buf string, tailLen int) (int, bool) {
	if tailLen <= 0 {
		return 0, false
	}
	start := len(buf) - tailLen
	if start < 0 {
		start = 0
	}
	// Маркер может начинаться в проверенной части и заканчиваться в хвосте —
	// поэтому расширяем окно на длину самого длинного маркера влево.
	const maxMarker = 13
	if from := start - maxMarker; from > 0 {
		start = from
	} else {
		start = 0
	}
	tail := buf[start:]
	best := -1
	for _, m := range toolsStreamMarkers {
		if p := strings.Index(tail, m); p >= 0 {
			if best < 0 || p < best {
				best = p
			}
		}
	}
	if best < 0 {
		return 0, false
	}
	return start + best, true
}

// toolsStreamRuneSafeCut — сдвигает границу среза влево до начала UTF-8 руны,
// чтобы дельта не разрезала многобайтный символ (иначе клиент получит битый
// символ, а JSON-строка — невалидную последовательность).
func toolsStreamRuneSafeCut(s string, from, to int) int {
	if to > len(s) {
		to = len(s)
	}
	for to > from && to < len(s) && !utf8.RuneStart(s[to]) {
		to--
	}
	return to
}

