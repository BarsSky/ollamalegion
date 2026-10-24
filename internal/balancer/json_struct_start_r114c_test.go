//go:build llama_stub

// json_struct_start_r114c_test.go — R114c (2026-10-13).
//
// ЖИВОЙ ДЕФЕКТ (Cline → балансер → cppworker, Ollama-путь /api/chat). Балансер
// переупаковывает JSON-вызов, который модель выдала текстом, в message.tool_calls.
// Накопление буфера начиналось только если content ЧАНКА начинался с триггера
// ('[', '<tool_call>', '{name' …), поэтому чанк «проза + вызов» уходил клиенту
// ЦЕЛИКОМ — в теле ответа оказывался обрывок вызова:
//
//	…I consider the task complete. [{"id":"call_submit_and_exit","type":
//	"function","function":{"name":"submit_and_exit","arguments":{"summary":…
//
// Оператор описал это как «периодически возникают формы вызова инструментов» и
// «остаётся след [[ от применения инструмента».
package balancer

import (
	"strings"
	"testing"
)

func TestR114c_JSONStructStartPos_FindsCallStart(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantPos int
		wantOK  bool
	}{
		{
			name:    "проза + массив вызовов (форма Cline)",
			in:      `I consider the task complete. [{"id":"call_submit_and_exit","type":"function"`,
			wantPos: strings.Index(`I consider the task complete. [{"id":"call_submit_and_exit","type":"function"`, `[{"id"`),
			wantOK:  true,
		},
		{
			name:    "двойной массив [[",
			in:      `Готово. [[{"id":"call_1"`,
			wantPos: strings.Index(`Готово. [[{"id":"call_1"`, "[["),
			wantOK:  true,
		},
		{
			name:    "объект с ключом",
			in:      `Результат: {"name":"run_commands","arguments":{}}`,
			wantPos: strings.Index(`Результат: {"name":"run_commands","arguments":{}}`, `{"name"`),
			wantOK:  true,
		},
		{
			name:    "одиночная скобка в конце (ключ ещё не пришёл)",
			in:      `Готово. [`,
			wantPos: len(`Готово. [`) - 1,
			wantOK:  true,
		},
		{
			name:   "скобка в прозе — не JSON",
			in:     `Используйте { для группировки`,
			wantOK: false,
		},
		{
			name:   "обычная проза",
			in:     `Файлов четыре: alpha, beta, gamma, README.`,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		pos, ok := jsonStructStartPos(tc.in)
		if ok != tc.wantOK {
			t.Errorf("%s: jsonStructStartPos(%q) = (%d, %v), ожидалось ok=%v", tc.name, tc.in, pos, ok, tc.wantOK)
			continue
		}
		if ok && pos != tc.wantPos {
			t.Errorf("%s: позиция %d, ожидалась %d", tc.name, pos, tc.wantPos)
		}
		if ok {
			if pos >= len(tc.in) || (tc.in[pos] != '[' && tc.in[pos] != '{') {
				t.Errorf("%s: позиция %d не указывает на скобку: %q", tc.name, pos, tc.in)
			}
			// Всё до позиции — проза, которую можно отдать клиенту сразу.
			if strings.ContainsAny(tc.in[:pos], "{[") {
				t.Errorf("%s: в прозе остались скобки JSON: %q", tc.name, tc.in[:pos])
			}
		}
	}
}

func TestR114c_ProsePrefixIsClean(t *testing.T) {
	in := `I have listed the files and there are 4 files: alpha.txt, beta.txt. ` +
		`[{"id":"call_submit_and_exit","type":"function","function":{"name":"submit_and_exit"}}]`
	pos, ok := jsonStructStartPos(in)
	if !ok {
		t.Fatal("начало JSON-структуры не найдено — обрывок вызова уедет в тело ответа")
	}
	prose := in[:pos]
	jsonPart := in[pos:]
	if !strings.Contains(prose, "I have listed the files") {
		t.Fatalf("проза потеряна: %q", prose)
	}
	if strings.ContainsAny(prose, "{[") {
		t.Fatalf("в прозу попал JSON: %q", prose)
	}
	if !strings.HasPrefix(jsonPart, `[{"id"`) {
		t.Fatalf("накопление начинается не со вызова: %q", jsonPart)
	}
}
