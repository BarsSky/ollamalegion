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
	"</tool_call>",
	"<|python_tag|>",
	"[TOOL_CALLS]",
	"[TOOL_CALL]",
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
