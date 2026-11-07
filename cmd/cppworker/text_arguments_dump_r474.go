// text_arguments_dump_r474.go — R474 (2026-11-07): вызов инструмента, выданный
// ТЕКСТОМ, больше не уезжает клиенту как ответ.
//
// ЖИВОЙ ДЕФЕКТ (Cline через балансер, Qwen3.8-9B, /api/chat + 6 инструментов).
// Сводка воркера того хода:
//
//	final_content_len=7095, tool_calls=0, file_body_instead_of_call=false
//
// Клиент получил ~7 КБ JavaScript-кода как ответ ассистента, а в конце —
// сериализованные аргументы вызова: `…", "path":"d:\cline-e2e-web\public\app.js"}]}}`.
// Файл не создан. Следом Cline сообщил «text part BrCl8dU4AzpNgHkf not found» —
// это его собственная реакция на такой ответ (внутренняя привязка частей
// сообщения), а не наша ошибка передачи.
//
// ПОЧЕМУ ПРЕЖНИЕ ДЕТЕКТОРЫ НЕ СРАБОТАЛИ:
//   - `looksLikeToolCallMarkup` ищет обёртки вызова (<tool_call>, [TOOL_CALLS],
//     call:NAME(, переэкранированные ключи) — здесь обёртки нет вовсе;
//   - R457 (`looksLikeFileBodyR457`) смотрит НАЧАЛО текста (DOCTYPE/html/fence/
//     package main/shebang) — здесь текст начинается с кода (`ctx.strokeStyle = …`);
//   - разбор вызовов (стратегии R113/R119/R246/R428/R441) требует имени функции
//     или обёртки — здесь есть только ХВОСТ аргументов.
//
// ПРАВИЛО: если ответ заканчивается закрытием JSON-структуры, в хвосте ключ
// `"path"` стоит на позиции ключа JSON-объекта (перед ним `{`, `,`, `[` или
// пробельный символ), и при этом текст содержит код, это попытка вызова, а не
// ответ. Требовать рядом второй ключ нельзя: в живом выводе 7 КБ содержимого
// файла отодвигают `"content"`/`"new_text"` далеко за пределы хвоста, и такой
// вывод оставался нераспознанным (это и случилось на стенде). Такой текст клиенту не
// отдаём: он не выполнит файл, зато запомнит его как ответ и следующий ход
// потеряет контекст. Вместо него — честное пояснение (что случилось, какой
// инструмент и путь) и запись в диагностику с reason `arguments_as_text`.
//
// ЧЕГО НЕ ДЕЛАЕМ: не пытаемся РАЗОБРАТЬ такой вывод в вызов (прецедент R430):
// починить = угадать, что модель имела в виду, а цена ошибки — файл с чужим
// содержимым. Не трогаем и «просто объяснения с кодом»: нужен именно хвост
// аргументов, иначе ответы на вопросы «покажи код» остались бы без кода.
package main

import (
	"fmt"
	"strings"
)

// dumpedArgsMinLenR474 — ниже этого размера текст не считаем выгрузкой файла:
// короткие фрагменты кода в ответе — норма.
const dumpedArgsMinLenR474 = 1200

// dumpedArgsCodeMarkersR474 — признаки кода, а не прозы: без них хвост
// аргументов в обычном объяснении (например, в статье про JSON) давал бы
// ложное срабатывание.
var dumpedArgsCodeMarkersR474 = []string{
	"function ", "=>", "const ", "let ", "document.", "ctx.", "class ", "</",
}

// looksLikeDumpedArgumentsR474 — выведены ли аргументы вызова текстом.
//
// Возвращает понятную подсказку (путь/имя инструмента) для текста пояснения.
func looksLikeDumpedArgumentsR474(text string) (hint, toolName string, ok bool) {
	if len(text) < dumpedArgsMinLenR474 {
		return "", "", false
	}
	// Хвост — там, где сериализованные аргументы: последние 600 символов.
	tailStart := len(text) - 600
	if tailStart < 0 {
		tailStart = 0
	}
	tail := text[tailStart:]

	// Ключ "path" обязан стоять на позиции ключа JSON-объекта: перед ним
	// открывающая скобка, запятая, скобка массива или пробельный символ.
	// Простое «"path" где-то в хвосте» слишком слабо — так выглядит и проза
	// про JSON (см. TestR474_JsonInProseIsNotTouched).
	if !dumpedArgsKeyPositionR474(tail, `"path"`) {
		return "", "", false
	}
	// Хвост обязан выглядеть как закрытие JSON-структуры.
	trimmed := strings.TrimRight(tail, " \t\r\n")
	if !strings.HasSuffix(trimmed, "}") && !strings.HasSuffix(trimmed, "}}") &&
		!strings.HasSuffix(trimmed, "]}}") && !strings.HasSuffix(trimmed, "}]}") {
		return "", "", false
	}
	// И это должно быть похоже на код, а не на прозу с примером JSON.
	codeHits := 0
	for _, m := range dumpedArgsCodeMarkersR474 {
		if strings.Contains(text, m) {
			codeHits++
		}
	}
	if codeHits < 2 {
		return "", "", false
	}

	toolName = dumpedToolNameR474(text)
	hint = dumpedPathR474(tail)
	if hint == "" {
		hint = dumpedPathR474(text)
	}
	return hint, toolName, true
}

// dumpedArgsKeyPositionR474 — встречается ли key на позиции ключа JSON-объекта:
// сразу после `{`, `,`, `[` или пробельного символа (в сериализованном виде
// перед ключом бывает перевод строки с отступом).
func dumpedArgsKeyPositionR474(text, key string) bool {
	for idx := 0; idx < len(text); {
		rel := strings.Index(text[idx:], key)
		if rel < 0 {
			return false
		}
		pos := idx + rel
		if pos == 0 || isDumpedArgsKeySepR474(text[pos-1]) {
			return true
		}
		idx = pos + len(key)
	}
	return false
}

// isDumpedArgsKeySepR474 — символы, после которых ключ JSON-объекта законен.
func isDumpedArgsKeySepR474(c byte) bool {
	switch c {
	case '{', ',', '[', ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

// dumpedToolNameR474 — имя инструмента из хвоста, если оно там есть.
func dumpedToolNameR474(text string) string {
	for _, name := range []string{"editor", "write_to_file", "create_file", "edit_file", "apply_patch"} {
		if strings.Contains(text, `"`+name+`"`) || strings.Contains(text, "call:"+name) {
			return name
		}
	}
	return ""
}

// dumpedPathR474 — значение "path" из хвоста (для понятного пояснения).
func dumpedPathR474(text string) string {
	const key = `"path":"`
	idx := strings.LastIndex(text, key)
	if idx < 0 {
		// Вариант с пробелом после двоеточия.
		const key2 = `"path": "`
		idx = strings.LastIndex(text, key2)
		if idx < 0 {
			return ""
		}
		text = text[idx+len(key2):]
	} else {
		text = text[idx+len(key):]
	}
	end := strings.IndexAny(text, `"`)
	if end <= 0 {
		return ""
	}
	path := text[:end]
	if len(path) > 200 {
		path = path[:200] + "…"
	}
	return path
}

// dumpedArgumentsNoticeR474 — что видит клиент вместо 7 КБ кода.
func dumpedArgumentsNoticeR474(toolName, hint string) string {
	if toolName == "" {
		toolName = "инструмент записи файла"
	}
	if hint == "" {
		hint = "аргументы инструмента"
	}
	return fmt.Sprintf(
		"Модель вывела вызов инструмента ТЕКСТОМ вместо вызова (%s, %s) — файл не создан. "+
			"Этот текст не является ответом и не является вызовом. "+
			"На следующем ходу вызови инструмент %s и передай аргументы полем вызова, "+
			"а не текстом ответа.",
		toolName, hint, toolName)
}
