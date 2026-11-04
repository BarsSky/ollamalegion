// visible_tail_r429.go — R429 (2026-11-04): хвост, который добирается до
// клиента перед финальным чанком tool_calls, обрезается по НАЧАЛУ разметки
// вызова — тело вызова больше не уезжает в content.
//
// ЖИВОЙ ДЕФЕКТ (снят прогоном Cline-запроса через балансер, gemma-4-E4B-it +
// 25 инструментов, сборка R428). Вызов РАСПОЗНАН (done_reason=tool_calls,
// tool_calls=1, editor), но клиент одновременно получил в content ВСЁ тело
// вызова — 3254 байта:
//
//	…Начинаю с создания **index.html**.<channel|>[
//	[[{"function":{"name":"editor","arguments":{"path":"d:\\cline-e2e-web\\index.html",
//	"new_text": "<!DOCTYPE html>\n…</html>"}}]]
//
// Причина — не парсер и не окно удержания: потоковые предохранители (2b/2c,
// tools_stream_r83.go) как раз остановили отдачу на `[[{`, и `emittedLen`
// встал ровно перед вызовом. Тело вызова уехало ОДНОЙ дельтой из хвостового
// добирания R248 (handlers_chat.go): «раз tool_calls найдены и что-то уже
// отдано, доберём остаток прозы», — а добор брал `fullOutput[emittedLen:]`
// целиком и чистил только `stripToolCallMarkup`. Эта чистка знает теговые
// обёртки и ЗАВЕРШЁННЫЕ JSON-объекты; незавершённый (или с экранированными
// кавычками внутри `new_text`) JSON вызова она не снимает — и вместе с ним
// уходил `<channel|>`.
//
// Цена дефекта: Cline сохранял HTML всего файла в истории как «ответ
// ассистента» — испорченный контекст следующего хода и лишние токены на каждом
// следующем запросе.
//
// РЕШЕНИЕ. Добор хвоста не «чистит» разметку, а РЕЖЕТ по её началу: если в
// непереданном остатке есть разметка вызова (тег, JSON-форма или питоновская
// форма `call:NAME(`), клиенту уходит только проза ДО неё. Всё, что после, —
// тело вызова, его место в message.tool_calls, а не в content.
package main

import "strings"

// toolCallJSONShapeMarkersR429 — начала JSON-форм вызова, которые парсер
// понимает без теговой обёртки (стратегии «raw array» и «openai object»).
var toolCallJSONShapeMarkersR429 = []string{
	`"tool_calls"`,
	`"tool_call"`,
	`{"function"`,
	`[{"`,
	`[[{"`,
}

// toolCallMarkupStartR429 — позиция ПЕРВОГО байта разметки вызова в s
// (-1 — разметки нет).
//
// Проверки идут от самых специфичных к общим:
//  1. теговые маркеры — тот же список, что у потокового удержания
//     (toolsStreamMarkers), чтобы стрим и добор хвоста считали началом вызова
//     одно и то же место;
//  2. заголовки JSON-форм вызова;
//  3. начало любой JSON-структуры по правилу предохранителя 2c
//     (toolsStreamJSONStructPos) — модель кодирует вызов и как `{"…`, и как
//     `[{"…`, и как `[[{"…`;
//  4. питоновская форма вызова gemma `call:NAME(` (R428).
func toolCallMarkupStartR429(s string) int {
	if s == "" {
		return -1
	}
	best := -1
	note := func(p int) {
		if p >= 0 && (best < 0 || p < best) {
			best = p
		}
	}
	for _, m := range toolsStreamMarkers {
		note(strings.Index(s, m))
	}
	for _, m := range toolCallJSONShapeMarkersR429 {
		note(strings.Index(s, m))
	}
	if p, ok := toolsStreamJSONStructPos(s, len(s)); ok {
		note(p)
	}
	note(gemmaParenCallStartR429(s))
	// R430: переэкранированные ключи (`\"function\"`) — тоже разметка вызова,
	// иначе её тело уезжает в content как «проза» (живой прогон gemma-4 + Cline).
	note(escapedCallMarkupStartR430(s))
	return best
}

// gemmaParenCallStartR429 — позиция питоновской формы вызова (`call:editor(…`)
// в s, -1 если её нет.
//
// Форма опознаётся строго: префикс (`call:`/`tool:`/`function:`), имя
// идентификатором и открывающая скобка. Просто `call:` в прозе (например
// «function call: см. выше») не совпадёт.
func gemmaParenCallStartR429(s string) int {
	if s == "" {
		return -1
	}
	best := -1
	skipSpace := func(i int) int {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		return i
	}
	for _, prefix := range []string{"call:", "tool:", "function:"} {
		from := 0
		for {
			i := strings.Index(s[from:], prefix)
			if i < 0 {
				break
			}
			start := from + i
			from = start + len(prefix)
			p := skipSpace(from)
			nameStart := p
			for p < len(s) && (isIdentStart(s[p]) || (p > nameStart && isIdentPart(s[p]))) {
				p++
			}
			if p == nameStart {
				continue
			}
			p = skipSpace(p)
			if p < len(s) && s[p] == '(' {
				if best < 0 || start < best {
					best = start
				}
				break
			}
		}
	}
	return best
}

// visibleTailR429 — видимый хвост ответа, которым можно закрыть прозу перед
// финальным чанком tool_calls, и позиция в буфере, до которой всё уже отдано.
//
// Возвращает текст ОТ emittedLen до начала разметки вызова (разметка и всё за
// ней отбрасывается), очищенный от служебных токенов и одиночных маркеров
// размышлений, и АБСОЛЮТНУЮ позицию начала разметки (len(fullOutput), если её
// не видно). Пустая строка — отдавать нечего (весь остаток и есть вызов).
func visibleTailR429(fullOutput string, emittedLen int) (string, int) {
	if emittedLen < 0 || emittedLen >= len(fullOutput) {
		return "", emittedLen
	}
	seg := fullOutput[emittedLen:]
	cut := toolCallMarkupStartR429(seg)
	if cut < 0 {
		// Разметки в остатке не видно (вызов распознан по форме, которую скан не
		// знает). Тогда отдаём остаток как прозу — это прежнее поведение R248, но
		// с той разницей, что чистки ниже всё равно снимут известные обёртки.
		cut = len(seg)
	}
	cutAbs := emittedLen + cut
	tail := seg[:cut]
	tail = stripOrphanReasoningTags(sanitizeStreamText(tail))
	tail = stripToolCallMarkup(tail)
	// Последний рубеж: если после чисток разметка вызова всё ещё видна в
	// начале — режем по ней (cleanFinalContent не знает всех форм, а риск
	// показать тело вызова дороже, чем потерять прозу перед ним).
	if p := toolCallMarkupStartR429(tail); p >= 0 {
		tail = tail[:p]
		cutAbs = emittedLen + p
	}
	return strings.TrimSpace(tail), cutAbs
}
