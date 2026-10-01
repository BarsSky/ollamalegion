// stream_sanitize.go — R83 (2026-09-26): защита стрима от управляющих символов.
//
// ЗАЧЕМ. Живой инцидент: длинная сессия на 8 GB GPU (Qwen3.8-27B в
// partial_offload), клиент показал под блоком кода
// «Invalid control character at: line 1 column 73 (char 72)» — это ошибка
// JavaScript-парсера JSON на стороне клиента (OpenWebUI). Строка с сырым
// управляющим символом не может прийти из нашего NDJSON: cppworker собирает
// каждую строку через json.Marshal, а он экранирует все C0-контролы в \u00XX,
// и балансер на нативном пути отдаёт строку байт-в-байт.
//
// Но остаётся вторая дорожка: клиент САМ парсит текст модели как JSON
// (артефакты, tool-call аргументы, «JSON mode»). Если квантованная модель на
// длинном контексте выдаёт в тексте сырые контролы (0x00-0x1F, 0x7F), парсер
// клиента на них падает — и падает он не в нашем коде, поэтому мы этого даже не
// видим в логах.
//
// Поэтому чистим текст модели перед отправкой: убираем C0-контролы, КРОМЕ
// структурных \n, \r, \t (их несут markdown и код), плюс DEL. Замена — на
// пробел, а не на удаление: так не склеиваются соседние слова, если модель
// вставила мусорный байт внутри текста.
package main

import "strings"

// sanitizeStreamText удаляет управляющие символы, которые ломают клиентские
// JSON-парсеры, сохраняя \n, \r и \t.
//
// Быстрый путь: если контролов нет (обычный случай), строка возвращается как
// есть — без аллокаций и без изменения уже нормального текста.
func sanitizeStreamText(s string) string {
	if s == "" {
		return s
	}
	clean := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			clean = false
			break
		}
		if c == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\n' || c == '\r' || c == '\t':
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// stripKnownServiceTokens — R83/v51 (2026-10-12): убирает из текста ВСЕ
// известные служебные токены chat-шаблонов (не только хвостовые, как
// cleanFinalContent). Нужна исключительно для диагностики: понять, есть ли в
// сыром выводе модели хоть какое-то настоящее содержимое, прежде чем отдавать
// его клиенту вместо ошибки "empty response".
//
// Пример: gemma эмитит единственный токен "<end_of_turn>" — raw output не пуст,
// но содержания в нём нет, и подсовывать его клиенту как content нельзя.
func stripKnownServiceTokens(s string) string {
	if s == "" {
		return ""
	}
	for _, tok := range trailingToolTokens {
		s = strings.ReplaceAll(s, tok, "")
	}
	return strings.TrimSpace(s)
}

// truncateForLog обрезает строку для лога до n РУН (не байт), чтобы не порвать
// UTF-8 посередине символа.
func truncateForLog(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
