// tools_stream_markers_r414.go — R414 (2026-11-03): служебные маркеры
// размышлений не должны утекать в видимый content потокового ответа.
//
// ЖИВОЙ ДЕФЕКТ (Cline в VS Code, провайдер Ollama, gemma-4, сессия
// 1793704404258_l9pcu): последняя дельта видимого ответа пришла как
//
//	"…and finally run the check command.<channel|>"
//
// то есть закрывающий маркер канала gemma-4 уехал клиенту как обычный текст.
// Пользователь видит в чате служебный токен, а Cline сохраняет его в истории —
// следующий ход получает испорченный контекст.
//
// ПОЧЕМУ НЕ ЛОВИЛОСЬ РАНЬШЕ. ReasoningStreamState (reasoning_content.go)
// вырезает маркеры и удерживает недособранные теги, но tools-путь
// (/api/chat + tools, см. writeChatStreamResponseWithTools) отдаёт content не
// через этот парсер: он режет сырой буфер `buf[emittedLen:cut]` и держит только
// окно toolsStreamHoldback (маркеры вызова инструмента / JSON). Закрывающий
// маркер канала в это окно не входит и уходил как есть.
//
// РЕШЕНИЕ. Те же два правила, что у ReasoningStreamState, но на байтовом срезе:
//  1. хвост, который может быть началом маркера (`<chan`), удерживается
//     до следующего чанка (toolsStreamMarkerPrefixHoldbackR414);
//  2. первый ПОЛНЫЙ маркер в дельте не отдаётся — текст до него отдаётся,
//     маркер пропускается (toolsStreamMarkerScanR414).
package main

import "strings"

// toolsStreamMarkerScanR414 — первый служебный маркер размышлений (открывающий
// или закрывающий) в дельте content.
//
// Возвращает позицию байта, длину маркера и признак находки. Позиция -1, если
// маркеров нет.
func toolsStreamMarkerScanR414(s string) (pos, markerLen int, found bool) {
	if s == "" {
		return -1, 0, false
	}
	pos = -1
	for _, pair := range thinkTagPairs {
		for _, m := range []string{pair.open, pair.close} {
			if m == "" {
				continue
			}
			if i := strings.Index(s, m); i >= 0 && (pos < 0 || i < pos) {
				pos, markerLen = i, len(m)
			}
		}
	}
	if pos < 0 {
		return -1, 0, false
	}
	return pos, markerLen, true
}

// toolsStreamMarkerPrefixHoldbackR414 — длина хвоста s, который является
// НАЧАЛОМ служебного маркера (например `<chan`), но короче его.
//
// Такой хвост нельзя отдавать: следующий чанк достроит маркер, а «отозвать»
// уже отправленный текст нельзя. 0 = хвост безопасен.
func toolsStreamMarkerPrefixHoldbackR414(s string) int {
	if s == "" {
		return 0
	}
	maxLen := maxThinkTagLen() - 1
	if maxLen > len(s) {
		maxLen = len(s)
	}
	for n := maxLen; n >= 1; n-- {
		if isProperTagPrefix(s[len(s)-n:]) {
			return n
		}
	}
	return 0
}
