// escaped_call_markup_r430_test.go — R430 (2026-11-04): переэкранированная
// разметка вызова (`\"function\":{\"name\":…`) не попадает в видимый ответ.
//
// ФИКСТУРА — живой вывод gemma-4-E4B-it в прогоне Cline-запроса через балансер
// (сборка R429): вызов пришёл с экранированными кавычками у ключей и без баланса
// скобок, ни один разбор его не увидел, и 3441 байт из 3442 уехали клиенту как
// «ответ» (Cline сохранил их в истории).
package main

import (
	"strings"
	"testing"
)

// fixtureEscapedCallR430 — та самая форма (сокращённое содержимое файла).
const fixtureEscapedCallR430 = `The previous attempt to create ` + "`index.html`" + ` failed because I was in "PLAN MODE".` +
	"\r\n\r\n" + `Starting with **index.html**. (The content generated in the previous turn is correct for this file).` +
	"<channel|>[\r\n" +
	`[[{\"function\":{\"name\":\"editor\",\"arguments\":{\"path\":\"d:\\\\cline-e2e-web\\\\index.html\",` +
	`\"new_text\":\"<!-- Cluster Monitor --\n<!DOCTYPE html>\n<html lang=\"ru\">\n</html>\"}]`

// TestR430_EscapedMarkupIsDetected — разметка опознаётся как разметка (до фикса
// false → не срабатывали ни пояснение R428, ни захват сырого вывода R427).
func TestR430_EscapedMarkupIsDetected(t *testing.T) {
	if !looksLikeEscapedToolCallR430(fixtureEscapedCallR430) {
		t.Fatal("переэкранированная разметка вызова не опознана")
	}
	if !looksLikeToolCallMarkup(fixtureEscapedCallR430) {
		t.Fatal("looksLikeToolCallMarkup не видит переэкранированную разметку")
	}
}

// TestR430_PlainProseWithMentionedKeyIsNotMarkup — контроль: упоминание одного
// ключа в прозе (модель объясняет формат) не считается разметкой вызова.
func TestR430_PlainProseWithMentionedKeyIsNotMarkup(t *testing.T) {
	cases := []string{
		`Чтобы вызвать файл, пришлите {\"name\": \"editor\"} в аргументах.`,
		`Формат ответа — JSON-объект с полем name.`,
		`Файл index.html создан, дальше создаю styles.css.`,
	}
	for _, s := range cases {
		if looksLikeEscapedToolCallR430(s) {
			t.Errorf("ложное срабатывание на прозе: %q", s)
		}
	}
}

// TestR430_VisibleTailStopsBeforeEscapedMarkup — клиенту уходит только проза;
// ни ключей вызова, ни HTML в ней нет.
func TestR430_VisibleTailStopsBeforeEscapedMarkup(t *testing.T) {
	prose := `The previous attempt to create ` + "`index.html`" + ` failed because I was in "PLAN MODE".` + "\r\n\r\n"
	tail, cut := visibleTailR429(fixtureEscapedCallR430, len(prose))
	if strings.Contains(tail, `\"function\"`) || strings.Contains(tail, "DOCTYPE") || strings.Contains(tail, "editor(") {
		t.Fatalf("в видимом хвосте осталась разметка вызова: %q", tail)
	}
	if !strings.Contains(tail, "Starting with **index.html**") {
		t.Fatalf("проза перед вызовом потеряна: %q", tail)
	}
	if cut <= len(prose) {
		t.Fatalf("позиция реза не сдвинулась: cut=%d prose=%d", cut, len(prose))
	}
}

// TestR430_MarkupStartCoversWrapper — рез начинается с ОБЁРТКИ вызова (`[`),
// а не с первого ключа: иначе в ответе остаётся «[[{\» — обрывок разметки.
func TestR430_MarkupStartCoversWrapper(t *testing.T) {
	s := "проза. [\r\n" + `[[{\"function\":{\"name\":\"editor\"` + `\"arguments\":{}}`
	pos := escapedCallMarkupStartR430(s)
	if pos < 0 {
		t.Fatal("начало разметки не найдено")
	}
	if s[pos] != '[' && s[pos] != '{' {
		t.Fatalf("начало разметки не на скобке: %q", s[pos:])
	}
	if before := s[:pos]; strings.ContainsAny(before, "[{") {
		t.Fatalf("в прозу попала обёртка вызова: %q", before)
	}
}
