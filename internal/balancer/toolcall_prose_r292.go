package balancer

// toolcall_prose_r292.go — R292 (2026-10-29): текст, который лишь ПОХОЖ на
// начало tool-вызова, не должен «замирать» в буфере.
//
// ЖИВАЯ ЖАЛОБА (второй стенд): «последний вывод обернулся в план вместе с
// предстоящими размышлениями и ждёт, пока буфер заполнится, чтобы выдать
// результат целиком — не печатает потоково».
//
// ПРИЧИНА. Кросс-чанковый детектор текстового tool-вызова (R114/R266) держит
// содержимое в `contentBuffer`, пока не поймёт, что это вызов: триггером служит
// ПЕРВЫЙ символ чанка (`[`, `{name`, `<tool_call>`, `<|tool|>`, `[TOOL_CALLS]`).
// Дальше текст копится до закрывающей `]`/`}` ЛИБО до предела буфера
// (`LB_TOOL_CALL_BUFFER_BYTES`, по умолчанию 64 КиБ) — и до этого момента
// клиенту не уходит НИЧЕГО. Если модель начала ответ с `[` (маркдаун-список,
// массив в тексте, цитата JSON в объяснении), клиент видел «замер», а потом
// всё сразу — это и есть живая жалоба.
//
// РЕШЕНИЕ. Держать текст «на всякий случай» можно только пока есть ПРИЗНАКИ
// вызова. Если за отведённое время (LB_TOOLCALL_PROSE_GRACE_MS, дефолт 300 мс)
// или объём (LB_TOOLCALL_PROSE_MAX_BYTES, дефолт 512 байт) признаков не
// появилось — это проза, и её надо отдать клиенту, а накопление для этого
// стрима прекратить (иначе следующая порция снова уйдёт в буфер).
//
// ПОЧЕМУ НЕ «ПРОСТО ВЫКЛЮЧИТЬ БУФЕР». Настоящий текстовый tool-вызов (qwen,
// gemma, hermes) приходит размазанным по чанкам: если отдавать его клиенту
// текстом, инструмент не вызовется вовсе (это отдельный живой дефект R266).
// Поэтому: настоящий вызов содержит ключи JSON (`"name":`, `"parameters":`, …)
// практически сразу — по ним и различаем.

import (
	"strings"
	"time"

	"ollama-loadbalancer/internal/settings"
)

const (
	defaultToolCallProseBytesR292 = 512
	defaultToolCallProseGraceMS   = 300
	minToolCallProseBytesR292     = 64
)

// toolCallStrongMarkersR292 — признаки НАСТОЯЩЕГО tool-вызова в накопленном
// тексте. Все — в нижнем регистре, сравнение регистронезависимое.
//
// Список намеренно узкий: чем больше «похожих» маркеров, тем чаще настоящий
// вызов уезжал бы клиенту текстом. Ключи JSON (`"name":`, `"parameters":`,
// `"arguments":`, `"function":`) появляются в вызове почти сразу после начала
// объекта, поэтому этого достаточно.
var toolCallStrongMarkersR292 = []string{
	`"name":`,
	`"parameters":`,
	`"arguments":`,
	`"tool_calls":`,
	`"function":`,
	`<tool_call`,
	`<|tool|`,
	`<|tool_call|`,
	`[tool_calls]`,
	`tool_call_result(`,
}

// toolCallProseEscapeBytes — предел, после которого подозрительный текст
// считается прозой (значение из реестра настроек: override → env → дефолт).
func toolCallProseEscapeBytes() int {
	if v, ok := settings.HotInt("LB_TOOLCALL_PROSE_MAX_BYTES"); ok && v >= minToolCallProseBytesR292 {
		return int(v)
	}
	return defaultToolCallProseBytesR292
}

// toolCallProseGrace — сколько держать подозрительный текст без признаков
// вызова. 0 = отдавать сразу, как только пришёл чанк (агрессивный стрим).
func toolCallProseGrace() time.Duration {
	if v, ok := settings.HotInt("LB_TOOLCALL_PROSE_GRACE_MS"); ok && v >= 0 {
		return time.Duration(v) * time.Millisecond
	}
	return defaultToolCallProseGraceMS * time.Millisecond
}

// contentBufferLooksLikeToolCallR292 — есть ли в накопленном тексте признаки
// настоящего tool-вызова (а не прозы, начавшейся с `[` или `{`).
func contentBufferLooksLikeToolCallR292(buf string) bool {
	if buf == "" {
		return false
	}
	lower := strings.ToLower(buf)
	for _, m := range toolCallStrongMarkersR292 {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// shouldFlushToolBufferAsProseR292 — пора ли отдать буфер клиенту как текст.
//
// Основное правило: признаки вызова есть → держим (это настоящий вызов).
// Признаков нет → держим не дольше grace и не больше bytes; иначе отдаём.
// Так «замирание» ограничено сверху и не зависит от того, как часто cppworker
// присылает чанки (при непрерывной генерации проверка «простаивает» не сработает
// — поэтому нужен ещё и объёмный порог).
func shouldFlushToolBufferAsProseR292(buf string, since time.Time) bool {
	if buf == "" {
		return false
	}
	if contentBufferLooksLikeToolCallR292(buf) {
		return false
	}
	if len(buf) >= toolCallProseEscapeBytes() {
		return true
	}
	grace := toolCallProseGrace()
	if grace <= 0 {
		// Грация выключена: держим только до первого «непонятного» чанка,
		// но всё же не выбрасываем текст — отдадим его клиенту.
		return true
	}
	return !since.IsZero() && time.Since(since) >= grace
}
