// visible_tail_r429_test.go — R429 (2026-11-04): тело вызова не попадает в
// content через добор хвоста перед финальным чанком tool_calls.
//
// ФИКСТУРЫ — из живых прогонов Cline-запроса через балансер (gemma-4-E4B-it,
// 25 инструментов, сборка R428), а не выдуманные:
//   - питоновская форма `call:editor(…)` без закрывающего тега (raw1.txt);
//   - JSON-массив вызова после `<channel|>` (content-gemma-…-11.txt).
package main

import (
	"strings"
	"testing"
)

// fixtureFlushParenR429 — проза + вызов gemma в питоновской форме.
const fixtureFlushParenR429 = `I have a clear plan. Начинаю с создания **index.html**.` +
	`<|tool_call>call:editor(new_text: "<!DOCTYPE html>\n<html lang=\"ru\"><body>…</body></html>", ` +
	`path: "d:\\cline-e2e-web\\index.html"}]`

// fixtureFlushJSONR429 — проза, закрывающий маркер канала и JSON-массив вызова.
const fixtureFlushJSONR429 = `Начинаю с создания **index.html**.` +
	"<channel|>[\n" +
	`[[{"function":{"name":"editor","arguments":{"path":"d:\\cline-e2e-web\\index.html",` +
	`"new_text": "<!DOCTYPE html>\n<html lang=\"ru\"><body>…</body></html>"}}]]`

// TestR429_FlushTailExcludesParenCallBody — питоновская форма: клиенту уходит
// проза, тело вызова — нет.
func TestR429_FlushTailExcludesParenCallBody(t *testing.T) {
	// Проза, которая уже ушла дельтами до маркера вызова.
	prose := `I have a clear plan. `
	got, _ := visibleTailR429(fixtureFlushParenR429, len(prose))
	if want := "Начинаю с создания **index.html**."; got != want {
		t.Fatalf("visibleTailR429 = %q, ожидалось %q", got, want)
	}
	for _, leak := range []string{"call:", "editor(", "<!DOCTYPE", "<|tool_call>", "<html"} {
		if strings.Contains(got, leak) {
			t.Fatalf("в видимом хвосте осталась разметка вызова: %q в %q", leak, got)
		}
	}
}

// TestR429_FlushTailExcludesJSONCallBody — JSON-массив вызова после
// `<channel|>`: отдавать нечего, тело вызова и маркер канала вырезаны.
func TestR429_FlushTailExcludesJSONCallBody(t *testing.T) {
	prose := `Начинаю с создания **index.html**.`
	got, _ := visibleTailR429(fixtureFlushJSONR429, len(prose))
	for _, leak := range []string{"<channel", "[", "{", "editor", "<!DOCTYPE"} {
		if strings.Contains(got, leak) {
			t.Fatalf("в видимом хвосте осталась разметка вызова: %q в %q", leak, got)
		}
	}
	if got != "" {
		t.Fatalf("ожидалось пусто (весь остаток — вызов), получено %q", got)
	}
}

// TestR429_FlushTailKeepsProseBeforeCall — проза, не успевшая уйти дельтами,
// обязана дойти до клиента (иначе ответ обрывается посреди фразы).
func TestR429_FlushTailKeepsProseBeforeCall(t *testing.T) {
	full := "Готово. Проверяю результат: файлы созданы, " + fixtureFlushParenR429
	emitted := len("Готово. Проверяю результат: ")
	got, _ := visibleTailR429(full, emitted)
	want := "файлы созданы, I have a clear plan. Начинаю с создания **index.html**."
	if got != want {
		t.Fatalf("visibleTailR429 = %q, ожидалось %q", got, want)
	}
}

// TestR429_FlushTailWithoutMarkup — разметки в остатке нет: отдаём остаток
// целиком (это прежнее поведение R248, его терять нельзя).
func TestR429_FlushTailWithoutMarkup(t *testing.T) {
	full := "Создаю файл. Осталось проверить сборку."
	got, _ := visibleTailR429(full, len("Создаю файл. "))
	if want := "Осталось проверить сборку."; got != want {
		t.Fatalf("visibleTailR429 = %q, ожидалось %q", got, want)
	}
}

// TestR429_FlushTailNoRemainder — остатка нет: ничего не отдаём (иначе клиент
// получит дубль уже отправленного текста).
func TestR429_FlushTailNoRemainder(t *testing.T) {
	full := "короткий ответ"
	if got, _ := visibleTailR429(full, len(full)); got != "" {
		t.Fatalf("visibleTailR429 = %q, ожидалось пусто", got)
	}
	if got, _ := visibleTailR429(full, len(full)+10); got != "" {
		t.Fatalf("visibleTailR429(emittedLen за концом) = %q, ожидалось пусто", got)
	}
}

// TestR429_ToolCallMarkupStart — границы распознавания начала разметки вызова:
// теги, JSON-формы, питоновская форма, и КОНТРОЛЬ: обычная проза с «call:» без
// скобок не должна считаться вызовом (иначе добор хвоста съедал живой текст).
func TestR429_ToolCallMarkupStart(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"tag gemma", `проза <|tool_call>call:editor(new_text: "x"}`, len("проза ")},
		{"tag hermes", `проза <tool_call>{"name":"editor"}`, len("проза ")},
		{"python tag", `проза <|python_tag|>{"name":"editor"}`, len("проза ")},
		{"tool_code", "проза [tool_code]editor(...)", len("проза ")},
		{"json array", `проза [{"function":{"name":"editor"}}]`, len("проза ")},
		{"json double array", `проза [[{"function":{"name":"editor"}}]]`, len("проза ")},
		{"json object", `проза {"function":{"name":"editor"}}`, len("проза ")},
		{"tool_calls key", `проза {"tool_calls":[{"id":"1"}]}`, len("проза ")},
		{"paren form", `проза call:editor(new_text: "x")`, len("проза ")},
		{"paren form with space", `проза call: editor(new_text: "x")`, len("проза ")},
		{"prose only", "Всё готово, файлы созданы.", -1},
		{"prose with call colon", "See the function call: it is described above.", -1},
		{"code sample without call", "```js\nconst a = 1;\n```", -1},
	}
	for _, tt := range tests {
		if got := toolCallMarkupStartR429(tt.in); got != tt.want {
			t.Errorf("%s: toolCallMarkupStartR429 = %d, ожидалось %d", tt.name, got, tt.want)
		}
	}
}

// TestR429_FlushTailSanitizesReasoningMarker — одиночный `<channel|>` внутри
// прозы перед вызовом не должен доехать до клиента как текст ответа.
func TestR429_FlushTailSanitizesReasoningMarker(t *testing.T) {
	full := "Создаю файл.<channel|>"
	got, _ := visibleTailR429(full, 0)
	if got != "Создаю файл." {
		t.Fatalf("visibleTailR429 = %q, ожидалось %q", got, "Создаю файл.")
	}
}
