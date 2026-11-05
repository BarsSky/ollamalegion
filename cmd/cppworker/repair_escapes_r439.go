// repair_escapes_r439.go — R439 (2026-11-05): невалидные escape-последовательности
// в JSON-аргументах вызова чинятся ДО того, как аргументы считаются «не JSON».
//
// ЖИВОЙ ДЕФЕКТ (Cline в VS Code + gemma-4, скриншот оператора). Модель выдала
// вызов editor с Windows-путём, записанным ОДИНАРНЫМИ обратными слэшами:
//
//	{"path":"d:\cline-e2e-web\index.html","new_text":"<!DOCTYPE html>…"}
//
// В JSON `\c` и `\i` — недопустимые escape-последовательности, поэтому строка
// невалидна целиком. Дальше срабатывал старый fallback «не JSON вообще (например
// Windows-путь)»: ВЕСЬ блок аргументов заворачивался в первый скалярный параметр
// схемы (`path`), и клиент получал
//
//	{"path":"{\"path\":\"d:\\\\cline…\",\"new_text\":\"…\"}"}
//
// — то есть `new_text` исчезал, а `path` содержал весь вызов. Cline отвечал
// `Invalid input: expected string, received undefined → at new_text`, повторял
// шесть раз (каждая попытка добавляла ещё один уровень вложенности в `path`) и
// останавливал задачу. Ровно этот текст и был на скриншоте.
//
// ЧТО ДЕЛАЕМ. Перед отказом от разбора проходим строку и удваиваем те обратные
// слэши, которые не образуют валидную JSON-escape-последовательность. Получив
// валидный JSON, отдаём его обычному конвейеру приведения по схеме — аргументы
// остаются на своих местах (`path` = путь, `new_text` = содержимое файла).
//
// ПОЧЕМУ НЕЛЬЗЯ ЧИНИТЬ ВСЛЕПУЮ ВСЁ: одиночный обратный слэш ВНЕ строки и
// валидные escape-последовательности (`\"`, `\\`, `\n`, `\uXXXX`) не трогаем —
// иначе поедет уже корректный JSON.
package main

import "strings"

// repairInvalidJSONEscapesR439 — удвоить невалидные escape-последовательности.
//
// Возвращает исправленную строку и признак «что-то изменили». Если менять
// нечего, второй результат false: вызывающий не тратит попытку разбора.
func repairInvalidJSONEscapesR439(raw string) (string, bool) {
	if !strings.Contains(raw, `\`) {
		return raw, false
	}
	var b strings.Builder
	b.Grow(len(raw) + 16)
	inString, changed := false, false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			b.WriteByte(c)
			continue
		}
		switch c {
		case '"':
			inString = false
			b.WriteByte(c)
		case '\\':
			if i+1 >= len(raw) {
				// Одинокий слэш в конце строки — не escape; удваиваем.
				b.WriteString(`\\`)
				changed = true
				continue
			}
			next := raw[i+1]
			if isJSONEscapeCharR439(next) {
				b.WriteByte(c)
				b.WriteByte(next)
				i++
				if next == 'u' {
					// \uXXXX: четыре шестнадцатеричные цифры обязательны.
					if !hasFourHexR439(raw, i+1) {
						changed = true
					}
				}
				continue
			}
			// Недопустимый escape (\c, \i, \O …) — это литеральный обратный слэш.
			b.WriteString(`\\`)
			b.WriteByte(next)
			i++
			changed = true
		default:
			b.WriteByte(c)
		}
	}
	if !changed {
		return raw, false
	}
	return b.String(), true
}

// isJSONEscapeCharR439 — символ, допустимый сразу после обратного слэша в JSON.
func isJSONEscapeCharR439(c byte) bool {
	switch c {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't', 'u':
		return true
	}
	return false
}

// hasFourHexR439 — идут ли с позиции from четыре шестнадцатеричные цифры.
func hasFourHexR439(s string, from int) bool {
	if from+4 > len(s) {
		return false
	}
	for i := from; i < from+4; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
