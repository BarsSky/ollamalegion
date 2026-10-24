// reasoning_orphan_tags_r114_test.go — R114b (2026-10-13).
//
// ЖИВАЯ ЖАЛОБА оператора (Cline, gemma-4-E4B, стенд 192.0.2.20:18080):
// «в ответе перемешка с ответом и маркером </think>, а также периодически
// возникают формы вызова инструментов, словно парсинг идёт не так… остаётся след
// [[ от применения инструмента».
//
// Разбор сохранённой сессии Cline (файл .messages.json) дал точный вид дефекта:
//
//	"I have listed the files … I consider the task complete.\n[{\"id\":
//	 \"call_submit_and_exit\",\"type\":\"function\",\"function\":{\"name\":
//	 \"submit_and_exit\",\"arguments\":{\"summary\":\"The directory was listed,"
//
// — то есть в content уехал ОБРЫВОК JSON вызова, оборванный на границе окна
// удержания (32 байта): ключ "arguments" оказался дальше от конца буфера, а
// начало объекта уже было отправлено дельтой.
package main

import (
	"strings"
	"testing"
)

// TestR114b_JSONStructHoldCatchesCallStart — начало JSON-структуры в хвосте
// удерживается, даже если ключи внутри ещё не появились.
func TestR114b_JSONStructHoldCatchesCallStart(t *testing.T) {
	cases := []struct {
		name string
		buf  string
		want bool
	}{
		{
			name: "начало вызова Cline (id/type/function)",
			buf:  "I consider the task complete. [{\"id\":\"call_submit_and_exit\",\"type\":\"function\",\"function\":{\"name\":\"submit_and_exit\"",
			want: true,
		},
		{
			name: "двойной массив [[",
			buf:  "Ответ готов. [[{\"id\":\"call_1\",\"type\":\"function\"",
			want: true,
		},
		{
			name: "просто объект с ключом",
			buf:  "Результат: {\"count\": 3}",
			want: true,
		},
		{
			name: "хвост начинается со скобки (ключ ещё не пришёл)",
			buf:  "Готово. {",
			want: true,
		},
		{
			name: "фигурная скобка в прозе — не JSON",
			buf:  "Используйте { для группировки",
			want: false,
		},
		{
			name: "обычная проза",
			buf:  "Файлов четыре: alpha, beta, gamma и README.",
			want: false,
		},
	}
	for _, tc := range cases {
		pos, ok := toolsStreamJSONStructPos(tc.buf, len(tc.buf))
		if ok != tc.want {
			t.Errorf("%s: toolsStreamJSONStructPos = (%d,%v), ожидалось %v", tc.name, pos, ok, tc.want)
			continue
		}
		if ok {
			// Позиция обязана указывать на саму скобку и стоять до конца строки.
			if pos >= len(tc.buf) || (tc.buf[pos] != '{' && tc.buf[pos] != '[') {
				t.Errorf("%s: позиция %d не указывает на '{'/'[': %q", tc.name, pos, tc.buf[pos:])
			}
			// Всё, что ДО позиции — проза, её отдаём клиенту как дельту.
			if before := tc.buf[:pos]; strings.ContainsAny(before, "{") {
				t.Errorf("%s: в прозе до удержания осталась скобка: %q", tc.name, before)
			}
		}
	}
}

// TestR114b_JSONStructHoldKeepsProsePrefix — проза перед вызовом уходит дельтой,
// а сам JSON — нет (именно этого не хватало: клиент получал «…task complete. [{»).
func TestR114b_JSONStructHoldKeepsProsePrefix(t *testing.T) {
	buf := "I consider the task complete. [{\"id\":\"call_submit_and_exit\""
	pos, ok := toolsStreamJSONStructPos(buf, len(buf))
	if !ok {
		t.Fatal("начало JSON-структуры не найдено")
	}
	prose := buf[:pos]
	if !strings.Contains(prose, "I consider the task complete.") {
		t.Fatalf("проза потеряна: %q", prose)
	}
	if strings.ContainsAny(prose, "[{") {
		t.Fatalf("в прозу попал JSON: %q", prose)
	}
}

// TestR114b_StripOrphanThinkClose — одиночный </think> не остаётся в ответе.
func TestR114b_StripOrphanThinkClose(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Ответ готов.</think>", "Ответ готов."},
		{"</think>Ответ готов.", "Ответ готов."},
		{"Размышления</think>Вот ответ", "РазмышленияВот ответ"},
		{"<|channel>thought\n Список файлов готов", "Список файлов готов"},
		{"<channel|>Готово", "Готово"},
		{"<thinking>шум</thinking>", "шум"},
		{"Обычный ответ без маркеров", "Обычный ответ без маркеров"},
	}
	for _, tc := range cases {
		if got := stripOrphanReasoningTags(tc.in); got != tc.want {
			t.Errorf("stripOrphanReasoningTags(%q) = %q, ожидалось %q", tc.in, got, tc.want)
		}
	}
}

// TestR114b_CleanFinalContentStripsThinkTag — центральная чистка видимого текста
// тоже снимает одиночные маркеры (её используют все клиентские пути).
func TestR114b_CleanFinalContentStripsThinkTag(t *testing.T) {
	if got := cleanFinalContent("Файлов четыре.</think>"); got != "Файлов четыре." {
		t.Fatalf("cleanFinalContent оставил маркер: %q", got)
	}
	if got := cleanFinalContent("<|channel>thought\nФайлов четыре."); got != "Файлов четыре." {
		t.Fatalf("cleanFinalContent оставил канал: %q", got)
	}
}

// TestR114b_HoldsTailOnReasoningStart — блок размышлений, начавшийся после
// прозы, не стримится как ответ.
func TestR114b_HoldsTailOnReasoningStart(t *testing.T) {
	for _, tail := range []string{"<think>", "…текст <thinking>", "<|channel>thought\nдумаю", "<|think>"} {
		if _, hold := toolsStreamHoldsTailStart(tail); !hold {
			t.Errorf("хвост %q не удержан, хотя содержит открывающий маркер размышлений", tail)
		}
	}
	for _, tail := range []string{"обычный текст", "Файлов четыре", ""} {
		if reason, hold := toolsStreamHoldsTailStart(tail); hold {
			t.Errorf("хвост %q удержан зря (%s)", tail, reason)
		}
	}
}

// TestR114b_SplitOrphanCloseIsPrefillReasoning — закрывающий маркер без
// открывающего: канал размышлений открыт ШАБЛОНОМ, значит всё до маркера —
// размышления, а не ответ (иначе клиент видит «перемешку» и сам маркер).
func TestR114b_SplitOrphanCloseIsPrefillReasoning(t *testing.T) {
	// gemma: канал открыт шаблоном, модель отдаёт только закрывающий маркер.
	if r, c, has := SplitReasoningContent("думаю о задаче и перебираю варианты<channel|>Вот ответ: 4 файла."); !has ||
		r != "думаю о задаче и перебираю варианты" || c != "Вот ответ: 4 файла." {
		t.Errorf("prefill-канал разобран неверно: reasoning=%q content=%q has=%v", r, c, has)
	}
	// Та же форма для пары <reasoning> (её требует старая инструкция).
	if r, c, has := SplitReasoningContent("рассуждение</reasoning>Ответ"); !has || r != "рассуждение" || c != "Ответ" {
		t.Errorf("orphan </reasoning>: reasoning=%q content=%q has=%v", r, c, has)
	}
	// Маркер в самом начале — это НЕ prefill: резать нечего, ответ начинается сразу.
	// R231: сам маркер при этом не должен доезжать до клиента как видимый текст
	// (живая жалоба Cline «</think> в теле ответа») — вырезаем только маркер.
	if r, c, has := SplitReasoningContent("</think>Только ответ"); has || r != "" || c != "Только ответ" {
		t.Errorf("маркер в начале обработан как prefill: reasoning=%q content=%q has=%v", r, c, has)
	}
	// Обычный текст без маркеров не трогаем.
	if r, c, has := SplitReasoningContent("просто ответ"); has || r != "" || c != "просто ответ" {
		t.Errorf("текст без маркеров изменён: reasoning=%q content=%q has=%v", r, c, has)
	}
}

// TestR114b_PairedBlocksNotAffectedByOrphanRule — если пара нашлась, остаток
// после неё — обычный content (резать его по одинокому маркеру нельзя: это
// потеря ответа).
func TestR114b_PairedBlocksNotAffectedByOrphanRule(t *testing.T) {
	r, c, has := SplitReasoningContent("<think>первое</think>Ответ пользователю")
	if !has || r != "первое" || c != "Ответ пользователю" {
		t.Fatalf("парный блок сломан: reasoning=%q content=%q has=%v", r, c, has)
	}
}

// TestR114b_PromptPrefillDetected — если НАШ промпт заканчивается открывающим
// маркером, вывод модели начинается внутри размышлений (prefill-канал); это
// свойство шаблона, а не догадка по имени модели.
func TestR114b_PromptPrefillDetected(t *testing.T) {
	for _, p := range []string{"<|channel>thought\n", "...диалог...<|channel>thought", "<start_of_turn>model\n<thinking>"} {
		if !promptEndsWithReasoningOpen(p) {
			t.Errorf("prefill не распознан: %q", p)
		}
	}
	for _, p := range []string{"<start_of_turn>model\n", "обычный промпт без канала", ""} {
		if promptEndsWithReasoningOpen(p) {
			t.Errorf("ложный prefill: %q", p)
		}
	}
}

// TestR114b_ThinkingInstructionHasNoForeignTags — наша soft-инструкция не
// навязывает модели чужой тег размышлений (источник одиночных </think>).
func TestR114b_ThinkingInstructionHasNoForeignTags(t *testing.T) {
	for _, lang := range []LangCode{LangRU, LangEN, LangOther} {
		inst := thinkingInstructionFor(lang)
		if strings.Contains(inst, "<reasoning>") || strings.Contains(inst, "</reasoning>") {
			t.Errorf("инструкция %v содержит чужой тег <reasoning>: %q", lang, inst)
		}
		if strings.Contains(inst, "<think") {
			t.Errorf("инструкция %v содержит чужой тег <think>: %q", lang, inst)
		}
	}
}

// TestR114b_EndToEnd_UserSessionShape — форма из живой сессии: проза + JSON
// вызова рядом. После приведения/удержания клиент не должен видеть JSON.
func TestR114b_EndToEnd_UserSessionShape(t *testing.T) {
	output := "I have listed the files in the directory, and there are 4 files: " +
		"alpha.txt, beta.txt, gamma.txt, and README.txt. Since this was a simple " +
		"request to list files and count them, I consider the task complete. " +
		`[{"id":"call_submit_and_exit","type":"function","function":{"name":` +
		`"submit_and_exit","arguments":{"summary":"The directory was listed"}}}]`

	// 1. Потоковое удержание: JSON не уходит дельтой.
	pos, ok := toolsStreamJSONStructPos(output, len(output))
	if !ok {
		t.Fatal("JSON вызова в выводе не распознан — он уехал бы обрывком в content")
	}
	if strings.ContainsAny(output[:pos], "[{") {
		t.Fatalf("в отдаваемой прозе оказался JSON: %q", output[:pos])
	}

	// 2. Разбор: вызов восстанавливается как tool_call (вызов не теряется).
	calls := parseToolCallsForRequest(output, clineToolsR113())
	if len(calls) != 1 || calls[0].Function.Name != "submit_and_exit" {
		t.Fatalf("вызов не восстановлен: %+v", calls)
	}

	// 3. Видимый текст не содержит JSON вызова.
	visible := stripToolCallMarkup(cleanFinalContent(output))
	if strings.Contains(visible, `"function"`) || strings.Contains(visible, "call_submit_and_exit") {
		t.Fatalf("JSON вызова остался в видимом тексте: %q", visible)
	}
	if !strings.Contains(visible, "I have listed the files") {
		t.Fatalf("проза потеряна: %q", visible)
	}
}
