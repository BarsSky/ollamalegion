// tools_stream_struct_ws_r429_test.go — R429 (2026-11-04): начало JSON-структуры
// вызова распознаётся и тогда, когда после скобки идёт перевод строки.
//
// ЖИВОЙ ДЕФЕКТ (gemma-4 + Cline, сборка R429): модель поставила между '[' и
// '[[{"function"…' ПЕРЕВОД СТРОКИ, прежняя проверка требовала значимый символ
// сразу, и первая скобка уезжала клиенту дельтой — в видимом ответе оставался
// хвост вызова из одного символа:
//
//	…соответствовать требованиям к типографике.  [
//	[[{"function":{"name":"editor",…
package main

import (
	"strings"
	"testing"
)

// TestR429_JSONStructAcrossWhitespace — скобка + пробельные + структура: удержание
// начинается с ПЕРВОЙ скобки, а не со второй.
func TestR429_JSONStructAcrossWhitespace(t *testing.T) {
	cases := []struct {
		name string
		buf  string
	}{
		{"перевод строки между [", "требования к типографике.  [\n[[{\"function\":{\"name\":\"editor\""},
		{"пробелы между [", "Готово. [ \t [[{\"name\":\"editor\""},
		{"перевод строки между {", "Ответ: {\n\"function\":{\"name\":\"editor\"}}"},
		{"массив строк через перевод строки", "Значения: [\n\"a\", \"b\"]"},
	}
	for _, tc := range cases {
		pos, ok := toolsStreamJSONStructPos(tc.buf, len(tc.buf))
		if !ok {
			t.Errorf("%s: структура не найдена в %q", tc.name, tc.buf)
			continue
		}
		// Позиция обязана указывать на первую скобку вызова, а проза до неё — не
		// содержать скобок: иначе клиент снова получит хвост разметки.
		if pos >= len(tc.buf) || (tc.buf[pos] != '{' && tc.buf[pos] != '[') {
			t.Errorf("%s: позиция %d не указывает на скобку", tc.name, pos)
		}
		if before := tc.buf[:pos]; strings.ContainsAny(before, "[{") {
			t.Errorf("%s: в прозе осталась скобка: %q", tc.name, before)
		}
	}
}

// TestR429_JSONStructNotFooledByProse — контроль: скобки в обычном тексте не
// должны останавливать поток (иначе ответ обрывался бы на первой же '[').
func TestR429_JSONStructNotFooledByProse(t *testing.T) {
	cases := []string{
		"список: [ 1, 2, 3 ]",
		"вижу [ текст в скобках ] и всё",
		"Используйте { для группировки",
		"Файлов четыре: alpha, beta, gamma и README.",
		"массив [ ] пустой",
	}
	for _, buf := range cases {
		if pos, ok := toolsStreamJSONStructPos(buf, len(buf)); ok {
			t.Errorf("ложное удержание на позиции %d в %q", pos, buf)
		}
	}
}
