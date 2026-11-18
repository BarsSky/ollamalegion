// decode_error_tail_r521.go — R521 (2026-11-19): ошибка декода обязана нести
// последние строки лога llama.cpp.
//
// ЖИВОЙ ФАКТ (§8.3.5 плана, архив 2026-11-18): клиент получал
//
//	llama_decode failed for prompt batch: ... — текущий код 1
//
// без единой строки самого llama.cpp, хотя мост эти строки перехватывает
// (c/bridge/bridge.go: goBridgeLogLine → recordLogLine). В логе llama.cpp в этот
// момент стоял CUDA out-of-memory — то есть «code 1» превращался в загадку, и
// оператор искал причину в окне (числа как раз влезали: 79 675 + 16 384 < 131 072).
//
// Здесь — не новый диагноз, а ФАКТ: последние N строк моста приклеиваются к
// ошибке. Если строк нет (llama_stub, ранний сбой) — честно пишем
// «лог недоступен», а не делаем вид, что llama.cpp молчал.
package cppbackend

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/c/bridge"
)

// decodeErrorTailLines — сколько последних строк лога прикладывать к ошибке.
// 12 хватает, чтобы увидеть строку OOM и 2–3 строки контекста перед ней, и мало
// чтобы раздуть ответ: строки llama.cpp короткие, но при 200 строках JSON ответа
// уже нечитаем для клиента.
const decodeErrorTailLines = 12

// decodeErrorMaxBytes — предел суммарного текста ошибки, чтобы диагностика не
// превращалась в простыню (клиенты Cline/OpenWebUI показывают её в диалоге).
const decodeErrorMaxBytes = 4096

// isDecodeFailureMessage — это ошибка декода (или её обёртка)?
//
// Ищем по подстрокам, а не по типу: текст собирается в C-мосте, и типизированной
// ошибки оттуда нет. Зато формулировки стабильны (window_fit.c, bridge.c).
func isDecodeFailureMessage(msg string) bool {
	low := strings.ToLower(msg)
	if low == "" {
		return false
	}
	return strings.Contains(low, "llama_decode failed") ||
		strings.Contains(low, "decode failed") ||
		strings.Contains(low, "llama_decode returned")
}

// appendDecodeTail — добавить хвост лога к тексту ошибки (чистая функция).
//
// Возвращает (текст, добавлено ли). Пустой хвост даёт явное «лог недоступен»:
// оператору важно отличать «llama.cpp ничего не сказал» от «мы не показали».
func appendDecodeTail(msg string, lines []string) (string, bool) {
	if msg == "" {
		return msg, false
	}
	if len(lines) == 0 {
		if !isDecodeFailureMessage(msg) {
			return msg, false
		}
		return msg + "\n[llama.cpp] лог недоступен (строк нет)", true
	}
	var b strings.Builder
	b.WriteString(msg)
	b.WriteString("\n[llama.cpp] последние строки:\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	out := b.String()
	if len(out) > decodeErrorMaxBytes {
		// Обрезаем по границе строки, чтобы не рвать UTF-8 посередине.
		cut := out[:decodeErrorMaxBytes]
		if i := strings.LastIndexByte(cut, '\n'); i > 0 {
			cut = cut[:i]
		}
		out = cut + "\n… (лог обрезан)"
	}
	return out, true
}

// decodeErrorWithTail — appendDecodeTail для настоящего error.
//
// Не-ошибочные значения и ошибки, не похожие на декод, возвращаются как есть:
// добавлять хвост к 413/400 незачем — там причина уже названа числами.
func decodeErrorWithTail(err error, lines []string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if !isDecodeFailureMessage(msg) {
		return err
	}
	withTail, added := appendDecodeTail(msg, lines)
	if !added {
		return err
	}
	return fmt.Errorf("%s", withTail)
}

// decodeErrorTailLive — обёртка над bridge.RecentLogLines (для вызовов в рантайме).
func decodeErrorTailLive() []string {
	return bridge.RecentLogLines(decodeErrorTailLines)
}

// AppendDecodeTailToMessage — приложить хвост лога llama.cpp к тексту ошибки
// (R521, §8.3.5) для слоя HTTP.
//
// ПОЧЕМУ НЕ В diagnoseInferenceFailure. Там менять error нельзя: R224 закрепляет,
// что «не наша» ошибка возвращается ТЕМ ЖЕ значением (identity), иначе failover
// теряет исходную причину. Диагноз остаётся прежним, а хвост добавляется в
// момент формирования ответа клиенту — там, где ошибка больше не классифицируется.
func AppendDecodeTailToMessage(msg string) string {
	withTail, added := appendDecodeTail(msg, decodeErrorTailLive())
	if !added {
		return msg
	}
	return withTail
}
