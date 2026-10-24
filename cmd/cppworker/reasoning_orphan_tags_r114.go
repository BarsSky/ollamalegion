// reasoning_orphan_tags_r114.go — R114b (2026-10-13): маркеры размышлений,
// оставшиеся в ВИДИМОМ тексте, вырезаются.
//
// ЖИВАЯ ЖАЛОБА оператора (Cline, gemma-4-E4B, стенд 192.0.2.20:18080):
// «в ответе перемешка с ответом и маркером </think>, а также периодически
// возникают формы вызова инструментов, словно парсинг идёт не так».
//
// Разбор сохранённой сессии Cline подтвердил две вещи:
//
//  1. текст ответа содержал ОБРЫВОК JSON вызова инструмента
//     («…I consider the task complete. [{"id":"call_submit_and_exit","type":
//     "function","function":{"name":"submit_and_exit","arguments":{"summary":
//     "The directory was listed, …» — обрыв ровно там, где сработало окно
//     удержания (см. toolsStreamHoldsTailStart: теперь удержание начинается на
//     САМОМ '{'/'[' , а не на ключах внутри объекта);
//  2. в потоке встречались одиночные маркеры размышлений.
//
// Одиночный маркер — это след парного разбора: SplitReasoningContent вырезает
// БЛОКИ (open…close), а тег, оставшийся без пары (модель начала блок в промпте
// шаблона, оборвалась по max_tokens, или тег пришёл от другого шаблона), в
// видимый текст попадал как обычные символы. Клиент показывает такой текст
// пользователю — «ответ с </think>».
package main

import "strings"

// orphanReasoningTags — маркеры размышлений и служебные токены каналов, которые
// не должны попадать в видимый ответ. Список собирается из thinkTagPairs
// (единый источник пар) плюс одиночные токены каналов.
func orphanReasoningTags() []string {
	tags := []string{
		// Одиночные токены каналов (в парах их нет как отдельных строк).
		"<channel|>", "<|channel>",
		"<|turn>", "<turn|>",
		"<|start_header_id|>", "<|end_header_id|>",
		"<|eot_id|>", "<|end|>",
	}
	seen := map[string]bool{}
	for _, t := range tags {
		seen[t] = true
	}
	for _, p := range thinkTagPairs {
		for _, t := range []string{p.open, p.close} {
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			tags = append(tags, t)
		}
	}
	return tags
}

// removeReasoningTagMarkers вырезает ТОЛЬКО сами маркеры размышлений, сохраняя
// текст и ПРОБЕЛЫ вокруг них.
//
// Вынесено из stripOrphanReasoningTags, потому что потоковой дельте TrimSpace
// противопоказан (см. stripReasoningTagsInStreamDelta), а список маркеров и
// порядок их вырезания должны оставаться ОДНИМИ для обеих чисток.
//
// Длинные маркеры обрабатываются первыми, чтобы «<|channel>thought» не оставил
// после себя «thought» от короткого «<|channel>».
func removeReasoningTagMarkers(text string) string {
	tags := orphanReasoningTags()
	// Сортируем по убыванию длины (вставками — список короткий).
	for i := 1; i < len(tags); i++ {
		for j := i; j > 0 && len(tags[j]) > len(tags[j-1]); j-- {
			tags[j], tags[j-1] = tags[j-1], tags[j]
		}
	}
	out := text
	for _, t := range tags {
		if strings.Contains(out, t) {
			out = strings.ReplaceAll(out, t, "")
		}
	}
	return out
}

// stripOrphanReasoningTags удаляет маркеры размышлений из видимого текста.
//
// Вырезаются ТОЛЬКО сами маркеры: текст вокруг них сохраняется. Это осознанно —
// если размышления уже утекли (например, парсер не увидел открывающий тег),
// вырезать вместе с ними весь текст означало бы потерять ответ. Убираем то, что
// пользователю показывать нельзя: технические теги.
//
// Длинные маркеры обрабатываются первыми, чтобы «<|channel>thought» не оставил
// после себя «thought» от короткого «<|channel>».
func stripOrphanReasoningTags(text string) string {
	if text == "" {
		return text
	}
	out := removeReasoningTagMarkers(text)
	// После вырезания маркера «<|channel>thought» мог остаться его текстовый
	// суффикс в начале строки («thought\n») — это тоже не ответ.
	out = strings.TrimLeft(out, " \t")
	for _, suffix := range []string{"thought\n", "thought\r\n", "thought ", "analysis\n", "analysis\r\n", "analysis "} {
		if strings.HasPrefix(out, suffix) {
			out = strings.TrimPrefix(out, suffix)
			break
		}
	}
	return strings.TrimSpace(out)
}

// stripReasoningTagsInStreamDelta — снять маркеры размышлений с ПОТОКОВОЙ
// дельты видимого текста.
//
// ЗАЧЕМ. stripOrphanReasoningTags вызывался только из cleanFinalContent
// (tool_calls.go) — то есть на НЕ-потоковых путях. В стриме дельта content
// формируется в callback'ах writeOpenAIChatStream (handlers_openai.go, emit
// delta.content) и writeChatStreamResponse (handlers_chat.go, message.content) и
// уходила клиенту сырой: живой дефект оператора — строка видимого ответа
// начиналась с «Let me retry the command. </think>» (скриншот Cline).
//
// Режем маркеры ТЕМ ЖЕ списком orphanReasoningTags, что и центральная чистка
// (единый источник — thinkTagPairs): парная форма open…close снимается ЦЕЛИКОМ,
// одиночный маркер (открывающий или закрывающий) вырезается по месту. Это
// согласовано с финальным done-чанком: клиент склеивает дельты и сравнивает их с
// done-content, который уже прошёл cleanFinalContent.
//
// ПРОБЕЛЫ НЕ ТРОГАЕМ (в отличие от stripOrphanReasoningTags, которая делает
// TrimSpace): клиент склеивает дельты, и срезанный пробел на границе склеил бы
// слова («список» + « файлов» → «списокфайлов»). Поэтому финальный TrimSpace
// здесь не нужен, а пробелы внутри удержанного текста остаются.
func stripReasoningTagsInStreamDelta(delta string) string {
	if delta == "" {
		return delta
	}
	out := delta
	// Парные формы — первыми: иначе одиночный open был бы вырезан раньше, а тело
	// пары осталось бы в видимом тексте (тело открытого блока кормит
	// reasoning-канал парсера, не content).
	for _, p := range thinkTagPairs {
		if p.open == "" || p.close == "" {
			continue
		}
		for strings.Contains(out, p.open) {
			openIdx := strings.Index(out, p.open)
			closeIdx := strings.Index(out[openIdx+len(p.open):], p.close)
			if closeIdx < 0 {
				break
			}
			out = out[:openIdx] + out[openIdx+len(p.open)+closeIdx+len(p.close):]
		}
	}
	// Одиночные маркеры (в том числе `</think>` без открывающего — prefill-канал
	// шаблона). Порядок — как в парсере (длинные теги первыми), чтобы
	// «<|channel>thought» не оставил суффикс от короткого «<|channel>».
	return removeReasoningTagMarkers(out)
}

// stripServiceTokensKeepSpaces — убрать служебные токены шаблона, НЕ трогая
// пробелы.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ. stripKnownServiceTokens (stream_sanitize.go) делает
// TrimSpace, а для ПОТОКОВОЙ дельты это недопустимо: клиент склеивает дельты, и
// срезанный пробел на границе склеит слова («список» + « файлов» → «списокфайлов»).
// Здесь вырезаются только сами токены.
func stripServiceTokensKeepSpaces(s string) string {
	if s == "" {
		return s
	}
	for _, tok := range trailingToolTokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if strings.Contains(s, tok) {
			s = strings.ReplaceAll(s, tok, "")
		}
	}
	return s
}

// tailEndsWithServiceTokenPrefix — хвост заканчивается НАЧАЛОМ служебного токена
// (например «<end_of_»).
//
// ЗАЧЕМ. Токен приходит по частям: если отдать дельту с его началом, вторая
// половина придёт следующим чанком и клиент увидит обрывок токена как текст
// (живой случай: «other coding task.\n<end_of_turn>» в content). Держим хвост,
// пока токен либо допишется целиком (и будет вырезан), либо окажется обычным
// текстом.
func tailEndsWithServiceTokenPrefix(s string) bool {
	if s == "" {
		return false
	}
	for _, tok := range trailingToolTokens {
		tok = strings.TrimSpace(tok)
		for i := 1; i < len(tok); i++ {
			if strings.HasSuffix(s, tok[:i]) {
				return true
			}
		}
	}
	return false
}

// tailContainsServiceToken — в хвосте есть служебный токен целиком.
func tailContainsServiceToken(s string) bool {
	if s == "" {
		return false
	}
	for _, tok := range trailingToolTokens {
		tok = strings.TrimSpace(tok)
		if tok != "" && strings.Contains(s, tok) {
			return true
		}
	}
	return false
}

// containsReasoningOpenMarker — в тексте есть ОТКРЫВАЮЩИЙ маркер размышлений.
// Нужно потоковому удержанию: блок размышлений, начавшийся ПОСЛЕ того, как часть
// прозы уже ушла дельтами, всё равно не должен стримиться как ответ.
func containsReasoningOpenMarker(text string) bool {
	if text == "" {
		return false
	}
	for _, p := range thinkTagPairs {
		if p.open != "" && strings.Contains(text, p.open) {
			return true
		}
	}
	return false
}

// promptEndsWithReasoningOpen — ПРОМПТ заканчивается открывающим маркером
// размышлений: значит, канал уже открыт шаблоном (prefill), и вывод модели
// начинается ВНУТРИ размышлений, а закрывающий маркер придёт из генерации.
//
// ЗАЧЕМ. В этом случае в выводе нет открывающего тега, поэтому потоковое
// удержание «ответ начинается с размышлений» не срабатывало: тело размышлений
// уходило клиенту дельтами как ответ, а вместе с ним и `</think>` / `<channel|>`
// (живая жалоба оператора). Проверяем сам факт prefill по НАШЕМУ промпту —
// это свойство шаблона, а не догадка по имени модели.
func promptEndsWithReasoningOpen(prompt string) bool {
	// Пробелы и переводы строк в конце промпта не значимы: шаблон может
	// заканчиваться «<|channel>thought\n».
	tail := strings.TrimRight(prompt, " \t\r\n")
	for _, p := range thinkTagPairs {
		if p.open == "" {
			continue
		}
		if strings.HasSuffix(tail, strings.TrimRight(p.open, "\n")) {
			return true
		}
	}
	return false
}
