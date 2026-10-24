// tool_args_schema_r113_test.go — R113 (2026-10-13): arguments вызова
// инструмента приводятся к ОБЪЕКТУ по схеме инструмента из запроса.
//
// ЖИВОЙ ДЕФЕКТ (запрос настоящего Cline CLI, gemma-4-E4B-it-Q4_K_M, 25 tools,
// stream=true, стенд 127.0.0.1:18080 → cppworker):
//
//	{"function":{"name":"run_commands","arguments":["ls -A"]}}          // массив
//	{"function":{"name":"list_files","arguments":"c:\\Ollama\\ollamalegion"}} // строка
//
// Клиент (Cline/OpenAI SDK/ollama-python) ждёт объект по схеме инструмента
// (`run_commands` → {"commands":[…]}, `read_files` → {"files":[…]},
// `editor` → {"path":…}). Массив/строка = вызов невыполним: инструмент не
// запускается, Cline показывает вызов текстом и считает задачу выполненной
// (COMPLETE) уже после первой итерации.
//
// ПРАВИЛА (по схеме из запроса, без имён моделей):
//
//	массив  → первый объявленный параметр-массив (required в приоритете);
//	скаляр  → единственный параметр, иначе первая required-строка, иначе первый;
//	объект  → как есть (обёртка {"args":{…}} разворачивается, если ключ не объявлен);
//	строка с JSON внутри → разворачивается;
//	инструмент не объявлен, а значение не объект → вызов отбрасывается.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// toolSchemaR113 — конструктор инструмента со схемой параметров.
//
// Порядок параметров ВАЖЕН и задаётся аргументами (не map!): правило выбора
// параметра для «одинокого» значения опирается на порядок объявления в схеме —
// ровно на тот, что прислал клиент. Реальная схема Cline объявляет `path` первым
// в `editor`, поэтому одиночная строка уходит в `path`, а не в `new_text`.
func toolSchemaR113(name string, required []string, props ...toolSchemaProp) openAITool {
	var b strings.Builder
	b.WriteString(`{"type":"object","properties":{`)
	for i, p := range props {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q:{\"type\":%q}", p.name, p.typ)
	}
	b.WriteString("}")
	if len(required) > 0 {
		quoted := make([]string, 0, len(required))
		for _, r := range required {
			quoted = append(quoted, fmt.Sprintf("%q", r))
		}
		b.WriteString(`,"required":[` + strings.Join(quoted, ",") + `]`)
	}
	b.WriteString("}")
	return openAITool{
		Type:     "function",
		Function: openAIFunction{Name: name, Parameters: json.RawMessage(b.String())},
	}
}

// clineToolsR113 — слепок схем НАСТОЯЩЕГО запроса Cline (порядок как у клиента).
func clineToolsR113() []openAITool {
	return []openAITool{
		toolSchemaR113("run_commands", []string{"commands"}, toolSchemaProp{"commands", "array"}),
		toolSchemaR113("read_files", []string{"files"}, toolSchemaProp{"files", "array"}),
		toolSchemaR113("search_codebase", []string{"queries"}, toolSchemaProp{"queries", "array"}),
		// Порядок из запроса Cline: path, old_text, new_text, insert_line.
		toolSchemaR113("editor", []string{"path", "new_text"},
			toolSchemaProp{"path", "string"},
			toolSchemaProp{"old_text", "string"},
			toolSchemaProp{"new_text", "string"},
			toolSchemaProp{"insert_line", "integer"},
		),
		toolSchemaR113("ask_question", []string{"question", "options"},
			toolSchemaProp{"question", "string"},
			toolSchemaProp{"options", "array"},
		),
	}
}

func argsOf(t *testing.T, call openAIToolCall) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &out); err != nil {
		t.Fatalf("arguments не JSON-ОБЪЕКТ (%q): %v", call.Function.Arguments, err)
	}
	return out
}

// TestR114d_ScalarInArrayParamBecomesArray — скаляр в объявленном массиве
// превращается в массив из одного элемента.
//
// Живой случай (Cline + gemma-4, агентский цикл): модель отдала
// `{"commands":"dir"}`, а схема `run_commands` объявляет `commands: array`.
// Валидатор клиента отверг вызов («expected array, received string») — цикл
// потратил ход на ошибку инструмента.
func TestR114d_ScalarInArrayParamBecomesArray(t *testing.T) {
	calls := parseToolCallsForRequest(`{"name":"run_commands","arguments":{"commands":"dir"}}`, clineToolsR113())
	if len(calls) != 1 {
		t.Fatalf("вызовов: %d, ожидался 1", len(calls))
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments не JSON-объект: %q", calls[0].Function.Arguments)
	}
	cmds, ok := args["commands"].([]interface{})
	if !ok || len(cmds) != 1 || cmds[0] != "dir" {
		t.Fatalf("arguments = %q, ожидалось {\"commands\":[\"dir\"]} (скаляр в поле-массиве)", calls[0].Function.Arguments)
	}
}

// TestR114d_CoerceValueToDeclaredType — приведение по объявленному типу.
func TestR114d_CoerceValueToDeclaredType(t *testing.T) {
	type arr = []interface{}
	cases := []struct {
		name     string
		value    interface{}
		declared string
		want     interface{}
	}{
		{"скаляр в array", "dir", "array", arr{"dir"}},
		{"массив остаётся массивом", arr{"a"}, "array", arr{"a"}},
		{"одиночный элемент из string", arr{"x"}, "string", "x"},
		{"числовая строка в integer", "42", "integer", int64(42)},
		{"числовая строка в number", "1.5", "number", 1.5},
		{"нечисловая строка в integer", "dir", "integer", "dir"},
		{"строка true в boolean", "true", "boolean", true},
		{"строка 0 в boolean", "0", "boolean", false},
		{"неизвестный тип — без изменений", "dir", "", "dir"},
	}
	for _, tc := range cases {
		got := coerceValueToDeclaredType(tc.value, tc.declared)
		if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", tc.want) {
			t.Errorf("%s: coerceValueToDeclaredType(%#v, %q) = %#v, ожидалось %#v",
				tc.name, tc.value, tc.declared, got, tc.want)
		}
	}
}

// TestR113_ArrayArgsGoToDeclaredArrayParam — массив → объявленный параметр-массив.
func TestR113_ArrayArgsGoToDeclaredArrayParam(t *testing.T) {
	calls := []openAIToolCall{{
		Type:     "function",
		Function: openAIFunctionCall{Name: "run_commands", Arguments: `["ls -A"]`},
	}}
	got := coerceToolCallsToSchema(calls, clineToolsR113())
	if len(got) != 1 {
		t.Fatalf("вызовов после приведения: %d, ожидался 1", len(got))
	}
	args := argsOf(t, got[0])
	cmds, ok := args["commands"].([]interface{})
	if !ok || len(cmds) != 1 || cmds[0] != "ls -A" {
		t.Fatalf("arguments = %q, ожидалось {\"commands\":[\"ls -A\"]} — иначе Cline не выполнит вызов",
			got[0].Function.Arguments)
	}
}

// TestR113_MultipleArrayToolsKeepOwnParam — у каждого инструмента свой параметр-массив.
func TestR113_MultipleArrayToolsKeepOwnParam(t *testing.T) {
	cases := []struct {
		tool string
		want string
		args string
	}{
		{"run_commands", "commands", `["dir"]`},
		{"read_files", "files", `[{"path":"a.txt"}]`},
		{"search_codebase", "queries", `["func main"]`},
	}
	for _, tc := range cases {
		got := coerceToolCallsToSchema([]openAIToolCall{{
			Type:     "function",
			Function: openAIFunctionCall{Name: tc.tool, Arguments: tc.args},
		}}, clineToolsR113())
		if len(got) != 1 {
			t.Fatalf("%s: вызов потерян", tc.tool)
		}
		args := argsOf(t, got[0])
		if _, ok := args[tc.want]; !ok {
			t.Errorf("%s: arguments=%q, ожидался параметр %q", tc.tool, got[0].Function.Arguments, tc.want)
		}
	}
}

// TestR113_BareStringGoesToRequiredStringParam — одинокая строка → первый
// required-параметр: модель отдаёт значение, а имя параметра берём из схемы.
func TestR113_BareStringGoesToRequiredStringParam(t *testing.T) {
	got := coerceToolCallsToSchema([]openAIToolCall{{
		Type:     "function",
		Function: openAIFunctionCall{Name: "editor", Arguments: `"C:\\work\\main.go"`},
	}}, clineToolsR113())
	if len(got) != 1 {
		t.Fatal("вызов потерян")
	}
	args := argsOf(t, got[0])
	if args["path"] != `C:\work\main.go` {
		t.Fatalf("arguments=%q, ожидалось {\"path\":\"C:\\\\work\\\\main.go\"}", got[0].Function.Arguments)
	}
}

// TestR113_BareUnquotedStringGoesToSchemaParam — значение вообще не JSON
// (Windows-путь с обратными слэшами) — тоже уходит в объявленный параметр.
func TestR113_BareUnquotedStringGoesToSchemaParam(t *testing.T) {
	got := coerceToolCallsToSchema([]openAIToolCall{{
		Type:     "function",
		Function: openAIFunctionCall{Name: "editor", Arguments: `c:\Ollama\ollamalegion`},
	}}, clineToolsR113())
	if len(got) != 1 {
		t.Fatal("вызов потерян")
	}
	args := argsOf(t, got[0])
	if args["path"] != `c:\Ollama\ollamalegion` {
		t.Fatalf("arguments=%q", got[0].Function.Arguments)
	}
}

// TestR113_ObjectArgsUnchanged — корректный объект не переписывается.
func TestR113_ObjectArgsUnchanged(t *testing.T) {
	orig := `{"commands":["git status"]}`
	got := coerceToolCallsToSchema([]openAIToolCall{{
		Type:     "function",
		Function: openAIFunctionCall{Name: "run_commands", Arguments: orig},
	}}, clineToolsR113())
	if len(got) != 1 || got[0].Function.Arguments != orig {
		t.Fatalf("объект изменён: %q → %v", orig, got)
	}
}

// TestR113_DoubleEncodedStringUnwrapped — строка внутри строки.
func TestR113_DoubleEncodedStringUnwrapped(t *testing.T) {
	got := coerceToolCallsToSchema([]openAIToolCall{{
		Type:     "function",
		Function: openAIFunctionCall{Name: "run_commands", Arguments: `"{\"commands\":[\"dir\"]}"`},
	}}, clineToolsR113())
	if len(got) != 1 {
		t.Fatal("вызов потерян")
	}
	args := argsOf(t, got[0])
	if _, ok := args["commands"]; !ok {
		t.Fatalf("двойное кодирование не развёрнуто: %q", got[0].Function.Arguments)
	}
}

// TestR113_WrapperObjectUnwrapped — {"args":{…}} при объявленных внутренних ключах.
func TestR113_WrapperObjectUnwrapped(t *testing.T) {
	got := coerceToolCallsToSchema([]openAIToolCall{{
		Type:     "function",
		Function: openAIFunctionCall{Name: "editor", Arguments: `{"kwargs":{"path":"a.txt","new_text":"x"}}`},
	}}, clineToolsR113())
	if len(got) != 1 {
		t.Fatal("вызов потерян")
	}
	args := argsOf(t, got[0])
	if args["path"] != "a.txt" || args["new_text"] != "x" {
		t.Fatalf("обёртка не развёрнута: %q", got[0].Function.Arguments)
	}
}

// TestR113_UnknownToolWithScalarArgsDropped — инструмент не объявлен, значение
// не объект: схему взять неоткуда, вызов отбрасывается (клиент не выполнит его).
func TestR113_UnknownToolWithScalarArgsDropped(t *testing.T) {
	got := coerceToolCallsToSchema([]openAIToolCall{
		{Type: "function", Function: openAIFunctionCall{Name: "list_files", Arguments: `"C:\\tmp"`}},
		{Type: "function", Function: openAIFunctionCall{Name: "run_commands", Arguments: `["dir"]`}},
	}, clineToolsR113())
	if len(got) != 1 {
		t.Fatalf("после приведения вызовов: %d, ожидался 1 (list_files отброшен, run_commands приведён)", len(got))
	}
	if got[0].Function.Name != "run_commands" {
		t.Fatalf("оставлен не тот вызов: %s", got[0].Function.Name)
	}
}

// TestR113_NoSchemaKeepsBehaviour — без схемы поведение прежнее (обратная совместимость).
func TestR113_NoSchemaKeepsBehaviour(t *testing.T) {
	got := coerceToolCallsToSchema([]openAIToolCall{{
		Type:     "function",
		Function: openAIFunctionCall{Name: "run_commands", Arguments: `["dir"]`},
	}}, nil)
	if len(got) != 1 || got[0].Function.Arguments != `["dir"]` {
		t.Fatalf("без схемы значение изменено: %v", got)
	}
}

// TestR113_EndToEnd_GemmaArrayShape — от сырого вывода модели до объекта.
//
// Это и есть живой случай: gemma-4 отдала массив вместо объекта, и клиент не смог
// выполнить вызов.
func TestR113_EndToEnd_GemmaArrayShape(t *testing.T) {
	outputs := []string{
		`<|tool_call>call:run_commands{[<|"|>ls -A<|"|>]}<tool_call|>`,
		`{"tool_call": "run_commands", "args": ["ls -A"]}`,
	}
	for _, out := range outputs {
		calls := parseToolCallsFromOutputWithTools(out, clineToolsR113())
		if len(calls) != 1 {
			t.Errorf("вывод %q: распознано вызовов %d, ожидался 1", out, len(calls))
			continue
		}
		if calls[0].Function.Name != "run_commands" {
			t.Errorf("вывод %q: имя %q", out, calls[0].Function.Name)
			continue
		}
		var args map[string]interface{}
		if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
			t.Errorf("вывод %q: arguments не объект (%q)", out, calls[0].Function.Arguments)
			continue
		}
		if _, ok := args["commands"]; !ok {
			t.Errorf("вывод %q: arguments=%q, ожидался {\"commands\":[…]}", out, calls[0].Function.Arguments)
		}
	}
}

// TestR113_ToolArgShapeLogging — форма аргументов для трассировки.
func TestR113_ToolArgShapeLogging(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`: "object",
		`["a"]`:   "array",
		`"a"`:     "string",
		`5`:       "number",
		`true`:    "bool",
		`null`:    "null",
		`c:\x`:    "invalid",
		``:        "empty",
	}
	for in, want := range cases {
		if got := toolArgShape(in); got != want {
			t.Errorf("toolArgShape(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

// TestR113_StripToolCallMarkup — разметка вызова не остаётся в видимом тексте.
func TestR113_StripToolCallMarkup(t *testing.T) {
	cases := []struct{ in, want string }{
		{`<|tool_call>call:run_commands{[<|"|>dir<|"|>]}<tool_call|>`, ""},
		{`<tool_call>{"name":"run_commands"}</tool_call>`, ""},
		{`Готово. <|tool_call>call:x{}{<tool_call|>`, "Готово."},
		{`[TOOL_CALLS][{"name":"x"}]`, ""},
		{`обычный ответ без разметки`, `обычный ответ без разметки`},
		// TOOLCALL-ANGLE: angle-форма Hermes (`<function=NAME><parameter=KEY>`),
		// живой дефект — разметка приехала клиенту телом ответа.
		{toolCallAngleScreenshot, ""},
		{`Готово. <tool_call> <function=bash> <parameter=command>pwd</parameter> </function> </tool_call>`, "Готово."},
		{`<function=bash> <parameter=command>pwd</parameter> </function>`, ""},
		{`Ответ: {"note":"<parameter=x>"}`, `Ответ: {"note":"<parameter=x>"}`},
	}
	for _, tc := range cases {
		if got := stripToolCallMarkup(tc.in); got != tc.want {
			t.Errorf("stripToolCallMarkup(%q) = %q, ожидалось %q", tc.in, got, tc.want)
		}
	}
}

// toolCallAngleScreenshot — ТОЧНАЯ строка angle-вызова из живой жалобы
// (DeepSeek Harness → балансер → gemma-4): вызов уехал текстом, инструмент не
// вызвался.
const toolCallAngleScreenshot = `<tool_call> <function=bash> <parameter=command> pwd && ls -la </parameter> <parameter=description> Показать содержимое рабочего каталога </parameter> </function> </tool_call>`

// TestR113_AngleMarkupRecognized — angle-форма распознаётся как разметка вызова,
// даже если тег <tool_call> потерян (обрыв стрима).
func TestR113_AngleMarkupRecognized(t *testing.T) {
	if !looksLikeToolCallMarkup(toolCallAngleScreenshot) {
		t.Error("angle-форма вызова не распознана как разметка")
	}
	if !looksLikeToolCallMarkup(`<function=bash> <parameter=command>pwd</parameter> </function>`) {
		t.Error("<function=/<parameter= без <tool_call> не распознаны как разметка")
	}
}

// TestR113_LooksLikeToolCallMarkup — распознавание разметки в выводе.
func TestR113_LooksLikeToolCallMarkup(t *testing.T) {
	if !looksLikeToolCallMarkup(`<|tool_call>call:x{}<tool_call|>`) {
		t.Error("нативный тег gemma не распознан как разметка вызова")
	}
	if !looksLikeToolCallMarkup(`{"tool_call":"run_commands","args":["dir"]}`) {
		t.Error("JSON-форма вызова не распознана")
	}
	if looksLikeToolCallMarkup(`Просто ответ модели без вызова`) {
		t.Error("обычный текст принят за разметку вызова")
	}
	if looksLikeToolCallMarkup(`{"answer": 42}`) {
		t.Error("обычный JSON-ответ принят за вызов")
	}
}

// TestR113_StreamHoldsMidProseJSONCall — JSON вызова ПОСЛЕ прозы не уходит дельтами.
//
// Живой прогон Cline-запроса: сначала проза, затем JSON вызова — и он уехал
// клиенту дельтами (content 2456 символов с сырым JSON: «инструмент в теле ответа»).
func TestR113_StreamHoldsMidProseJSONCall(t *testing.T) {
	buf := `Сейчас посмотрю файлы. {"tool_call":"run_commands","args":["dir"]}`
	pos, ok := toolsStreamJSONCallPos(buf, 64)
	if !ok {
		t.Fatal("JSON вызова внутри прозы не распознан — дельты не удержаны, вызов уедет в content")
	}
	if want := strings.Index(buf, `{"tool_call"`); pos != want {
		t.Fatalf("позиция удержания %d, ожидалась %d (начало JSON вызова)", pos, want)
	}

	if _, ok := toolsStreamJSONCallPos(`Ответ: {"function":{"name":"run_commands"}}`, 64); !ok {
		t.Error(`JSON-форма {"function":…} не распознана`)
	}
	if _, ok := toolsStreamJSONCallPos(`Ответ: {"tool_ca`, 64); !ok {
		t.Error("частично дописанный ключ вызова не удержан")
	}
	if _, ok := toolsStreamJSONCallPos(`Ответ: {"name":"значение","count":3}`, 64); ok {
		t.Error("обычный JSON принят за вызов — стрим ответа будет зря удержан")
	}
	if _, ok := toolsStreamJSONCallPos(`Просто текст без JSON`, 64); ok {
		t.Error("текст без JSON принят за вызов")
	}
}

// TestR113_RecoverEchoedJSONCall — модель ПОВТОРЯЕТ вызов, присланный историей.
//
// Живой случай (Cline + gemma-4, второй ход): в ответе текст прозы и следом
// сериализованный вызов из промпта —
//
//	… I will execute it again.
//	[[{"id":"call_1","type":"function","function":{"name":"run_commands",
//	   "arguments":{"commands":["dir"]}}}]]
//
// Клиент получал это как content (инструмент в теле ответа), вызова не было, и
// цикл обрывался. Теперь вызов восстанавливается в tool_calls.
func TestR113_RecoverEchoedJSONCall(t *testing.T) {
	echoed := "I will execute it again.\n" +
		`[[{"id":"call_1","type":"function","function":{"name":"run_commands","arguments":{"commands":["dir"]}}}]]`

	calls := parseToolCallsForRequest(echoed, clineToolsR113())
	if len(calls) != 1 {
		t.Fatalf("восстановлено вызовов: %d, ожидался 1 (эхо вызова из истории не распознано)", len(calls))
	}
	if calls[0].Function.Name != "run_commands" {
		t.Fatalf("имя вызова = %q, ожидалось run_commands", calls[0].Function.Name)
	}
	args := argsOf(t, calls[0])
	if _, ok := args["commands"]; !ok {
		t.Fatalf("arguments = %q, ожидался {\"commands\":[…]}", calls[0].Function.Arguments)
	}

	// Плоская форма (Hermes/Qwen) — тоже восстанавливается.
	flat := `Готово. {"name":"run_commands","arguments":{"commands":["ls"]}}`
	if calls := parseToolCallsForRequest(flat, clineToolsR113()); len(calls) != 1 {
		t.Errorf("плоская форма не восстановлена: %+v", calls)
	}

	// И разметка такого вызова НЕ остаётся в видимом тексте.
	cleaned := stripToolCallMarkup(echoed)
	if strings.Contains(cleaned, `"function"`) || strings.Contains(cleaned, `"call_1"`) {
		t.Errorf("JSON вызова остался в видимом тексте: %q", cleaned)
	}
	if !strings.Contains(cleaned, "I will execute it again.") {
		t.Errorf("проза потеряна при вырезании вызова: %q", cleaned)
	}
}

// TestR113_RecoverDoesNotEatLegitJSON — обычный JSON ответа не превращается в вызов.
func TestR113_RecoverDoesNotEatLegitJSON(t *testing.T) {
	legit := `Результат: {"count": 3, "items": ["a", "b", "c"]}`
	if calls := parseToolCallsForRequest(legit, clineToolsR113()); len(calls) != 0 {
		t.Fatalf("обычный JSON принят за вызов: %+v", calls)
	}
	if got := stripToolCallMarkup(legit); got != legit {
		t.Fatalf("обычный JSON вырезан из ответа: %q", got)
	}
}

// TestR113_RetryOnUnparsedMarkup — повтор генерации, когда разметка не разобрана,
// и НЕ повтор, когда клиенту уже что-то отправлено.
func TestR113_RetryOnUnparsedMarkup(t *testing.T) {
	markup := `<|tool_call>call:run_commands{[<|"|>dir<|"|>]}<tool_call|>`
	if !shouldRetryUnparsedToolMarkup(markup, 0, nil) {
		t.Error("нераспознанная разметка при пустом выводе клиенту — повтор обязателен")
	}
	if shouldRetryUnparsedToolMarkup(markup, 10, nil) {
		t.Error("повтор недопустим: клиенту уже отправлены байты")
	}
	if shouldRetryUnparsedToolMarkup("обычный текст ответа", 0, nil) {
		t.Error("обычный текст поводом для повтора не является")
	}
}
