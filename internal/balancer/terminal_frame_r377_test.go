package balancer

// terminal_frame_r377_test.go — R377 (2026-11-01): терминальный кадр на
// транслируемом пути собирает ОДИН владелец — writeTerminalFrameR377.
//
// Тесты фиксируют три расхождения, которые были в самодельном кадре
// emitTerminalDoneOnly, и стюард, запрещающий вернуть четвёртый сборщик.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestR377_GenerateFrameUsesResponseField — на /api/generate Ollama читает поле
// response; поле message там игнорируется (до R377 самодельный кадр писал
// message, и ответ выглядел пустым).
func TestR377_GenerateFrameUsesResponseField(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &flushingResponseWriter{ResponseWriter: rec}

	if err := writeTerminalFrameR377(w, terminalFrameR377{
		path:    "/api/generate",
		model:   "gemma-4",
		content: "готовый текст",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(rec.Body.String())), &chunk); err != nil {
		t.Fatalf("invalid JSON: %v (body=%q)", err, rec.Body.String())
	}
	if chunk["response"] != "готовый текст" {
		t.Errorf("expected response with text, got %v", chunk["response"])
	}
	if _, ok := chunk["message"]; ok {
		t.Errorf("/api/generate must not carry message field, got %v", chunk["message"])
	}
	if chunk["done"] != true {
		t.Errorf("expected done=true, got %v", chunk["done"])
	}
}

// TestR377_ChatToolCallsOllamaShape — tool_calls обязаны уйти объектом
// (function.arguments — object), а не строкой: R295, живой дефект R377-предка.
func TestR377_ChatToolCallsOllamaShape(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &flushingResponseWriter{ResponseWriter: rec}

	calls := map[int]*accumulatedToolCall{
		0: {id: "call_1", function: map[string]interface{}{
			"name":      "search",
			"arguments": `{"q":"тест"}`,
		}},
	}
	if err := writeTerminalFrameR377(w, terminalFrameR377{
		path:      "/api/chat",
		model:     "gemma-4",
		toolCalls: calls,
		content:   "текст, который должен быть вытеснен tool_calls",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(rec.Body.String())), &chunk); err != nil {
		t.Fatalf("invalid JSON: %v (body=%q)", err, rec.Body.String())
	}
	msg, ok := chunk["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected message object, got %T", chunk["message"])
	}
	if msg["content"] != "" {
		t.Errorf("tool_calls must replace content, got %v", msg["content"])
	}
	if chunk["done_reason"] != "tool_calls" {
		t.Errorf("expected done_reason=tool_calls, got %v", chunk["done_reason"])
	}
	tcs, ok := msg["tool_calls"].([]interface{})
	if !ok || len(tcs) != 1 {
		t.Fatalf("expected 1 tool_call, got %v", msg["tool_calls"])
	}
	call, _ := tcs[0].(map[string]interface{})
	fn, ok := call["function"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected function object, got %v", tcs[0])
	}
	if _, ok := fn["arguments"].(map[string]interface{}); !ok {
		t.Errorf("Ollama expects arguments as OBJECT, got %T: %v", fn["arguments"], fn["arguments"])
	}
}

// TestR377_DoneReasonAlwaysPresentAndRegistered — завершающий кадр обязан нести
// done_reason из закрытого реестра R328, даже если текста в нём нет.
func TestR377_DoneReasonAlwaysPresentAndRegistered(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		content string
		tools   map[int]*accumulatedToolCall
		want    string
	}{
		{"пустой текст без tools", "/api/chat", "", nil, "stop"},
		{"пустой текст /api/generate", "/api/generate", "", nil, "stop"},
		{"есть текст", "/api/chat", "ответ", nil, "stop"},
		{"есть tool_calls", "/api/chat", "", map[int]*accumulatedToolCall{
			0: {id: "c1", function: map[string]interface{}{"name": "f", "arguments": "{}"}},
		}, "tool_calls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			w := &flushingResponseWriter{ResponseWriter: rec}
			if err := writeTerminalFrameR377(w, terminalFrameR377{
				path: tc.path, model: "gemma-4", content: tc.content, toolCalls: tc.tools,
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(strings.TrimSpace(rec.Body.String())), &chunk); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			got, _ := chunk["done_reason"].(string)
			if got != tc.want {
				t.Errorf("done_reason = %q, want %q", got, tc.want)
			}
			// Причина обязана быть ИЗ реестра: normalizeDoneReasonForSurface
			// вернул бы converted=true, если бы значение пришлось переводить.
			if !doneReasonsOllama[DoneReason(got)] {
				t.Errorf("done_reason %q вне закрытого реестра R328", got)
			}
			if _, converted := normalizeDoneReasonForSurface("ollama", got); converted {
				t.Errorf("done_reason %q требует перевода в реестре — кадр обязан писать каноническое значение", got)
			}
		})
	}
}

// TestR377_StreamingDoneGenerateRestoresContent — путь «апстрим не прислал
// терминальный кадр» по-прежнему восстанавливает накопленный текст (это
// решение вызывающего, а не сборщика кадра).
func TestR377_StreamingDoneGenerateRestoresContent(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &flushingResponseWriter{ResponseWriter: rec}

	if err := writeStreamingSSEDone(w, "/api/generate", "gemma-4", nil,
		"накопленный текст", "", false, false /* usageChunkSeen */); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(rec.Body.String())), &chunk); err != nil {
		t.Fatalf("invalid JSON: %v (body=%q)", err, rec.Body.String())
	}
	if chunk["response"] != "накопленный текст" {
		t.Errorf("expected restored content in response, got %v", chunk["response"])
	}
	if chunk["done_reason"] != "stop" {
		t.Errorf("expected done_reason=stop, got %v", chunk["done_reason"])
	}
}

// TestR377_Steward_OneTerminalFrameBuilder — стюард: на транслируемом пути кадр
// завершения собирает один файл/одна функция. Если кто-то снова напишет
// `"done": true` руками в транспорте или хелперах — тест падает и требует либо
// вызова владельца, либо записи в журнал причин ниже.
func TestR377_Steward_OneTerminalFrameBuilder(t *testing.T) {
	// Разрешённые места помимо владельца: у каждого своя причина кадра,
	// которую владелец не знает (пустой поток, усечение).
	allowed := map[string]map[string]string{
		"llamacpp_transport_helpers.go": {
			"buildDoneResponse": "маркер пустого потока: текста нет вовсе",
		},
		"llamacpp_transport.go": {
			"emitTruncatedChunk": "R328: причина truncated + error, это не успешное завершение",
		},
	}

	files := []string{
		"llamacpp_transport.go",
		"llamacpp_transport_helpers.go",
		"terminal_frame_r377.go",
	}
	ownerHits := 0
	for _, name := range files {
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("не разобрать %s: %v", name, err)
		}
		for _, decl := range af.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if ok && hasDoneTrueKey(lit) {
					found = true
				}
				return true
			})
			if !found {
				continue
			}
			if name == "terminal_frame_r377.go" && fn.Name.Name == "writeTerminalFrameR377" {
				ownerHits++
				continue
			}
			if _, ok := allowed[name][fn.Name.Name]; ok {
				continue
			}
			t.Errorf("%s: %s сам собирает терминальный кадр «done» — используйте writeTerminalFrameR377 "+
				"или внесите функцию в журнал причин стюарда", name, fn.Name.Name)
		}
	}
	if ownerHits == 0 {
		t.Fatal("владелец writeTerminalFrameR377 не найден — стюард проверяет не то")
	}
}

// hasDoneTrueKey — есть ли в литерале ключ "done" со значением true.
func hasDoneTrueKey(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Value != `"done"` {
			continue
		}
		if id, ok := kv.Value.(*ast.Ident); ok && id.Name == "true" {
			return true
		}
	}
	return false
}
