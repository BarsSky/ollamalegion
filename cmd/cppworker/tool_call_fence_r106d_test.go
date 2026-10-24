// tool_call_fence_r106d_test.go — R106d (2026-10-12).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (Cline → балансер → cppworker): «ответы смешаны с
// маркерами think и использованием tools в ответе клиенту».
//
// ВОСПРОИЗВЕДЕНО на стенде (gemma-4-E4B-it-Q4_K_M, запрос Cline с 18 tools,
// stream=true): модель положила вызов инструмента в ```json-блок, и клиент получил
// его ДЕЛЬТАМИ как обычный текст:
//
//	{"message":{"content":"```j"}}, {"content":"so"}, {"content":"n"},
//	{"content":"\n[\n "}, {"content":"{\n    \"i"}, …
//
// Финальный чанк — done_reason:"stop" и БЕЗ message.tool_calls. То есть:
//   - сырой JSON вызова ушёл в content (клиент печатает его как ответ);
//   - инструмент не вызвался вовсе (tool_calls не пришёл).
//
// Причины (обе — про правила протокола, а не про конкретную модель):
//  1. предохранитель потока отдавал дельты, пока первый непробельный символ не
//     '{'/'['; при фенсе первым идёт '`', поэтому удержание не включалось;
//  2. parseToolCallsFromOutput искал только СТРОГИЙ ```json\n…\n``` и не
//     распознавал ни фенс без перевода строки, ни незакрытый фенс.
//
// Тесты фиксируют: фенс на старте ответа = удержание дельт; вызов из фенса
// (закрытого, незакрытого, однострочного) распознаётся.
package main

import (
	"strings"
	"testing"
)

func TestR106d_ToolsStreamHoldback_FenceAtStart(t *testing.T) {
	cases := []struct {
		name string
		buf  string
		want bool
	}{
		{"закрытый фенс json", "```json\n[", true},
		{"фенс без языка", "```\n[{\"name\":\"x\"}]", true},
		{"фенс с ведущими пробелами", "  \n ```json\n[", true},
		{"незакрытый фенс", "```json [{\"name\":\"read_files\"", true},
		{"обычный текст", "Сейчас прочитаю файл.", false},
		{"JSON без фенса", "{\"name\":\"x\"}", false},
		{"массив без фенса", "[{\"name\":\"x\"}]", false},
		{"фенс посреди ответа", "Вот результат: ```json", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolsStreamStartsWithFence(tc.buf); got != tc.want {
				t.Errorf("toolsStreamStartsWithFence(%q) = %v, want %v", tc.buf, got, tc.want)
			}
		})
	}
}

func TestR106d_ParseToolCalls_FencedJSONArray(t *testing.T) {
	// Реальная форма из живого прогона (id + type + function).
	output := "```json\n[\n  {\n    \"id\": \"call_1\",\n    \"type\": \"function\",\n" +
		"    \"function\": {\n      \"name\": \"read_files\",\n" +
		"      \"arguments\": {\"path\": \"c:\\\\Ollama\\\\ollamalegion\"}\n    }\n  }\n]\n```"
	calls := parseToolCallsFromOutput(output)
	if len(calls) != 1 {
		t.Fatalf("вызовов %d, want 1 (фенс ```json не распознан — клиент получит JSON как текст)", len(calls))
	}
	if calls[0].Function.Name != "read_files" {
		t.Errorf("name = %q, want read_files", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "ollamalegion") {
		t.Errorf("arguments = %q, want путь внутри", calls[0].Function.Arguments)
	}
}

func TestR106d_ParseToolCalls_UnclosedFence(t *testing.T) {
	// Модель часто не дописывает закрывающий фенс (упёрлась в кап токенов).
	output := "```json\n[\n  {\"id\":\"call_2\",\"type\":\"function\"," +
		"\"function\":{\"name\":\"run_commands\",\"arguments\":{\"command\":\"ls -F\"}}}]"
	calls := parseToolCallsFromOutput(output)
	if len(calls) != 1 {
		t.Fatalf("вызовов %d, want 1 (незакрытый фенс должен распознаваться)", len(calls))
	}
	if calls[0].Function.Name != "run_commands" {
		t.Errorf("name = %q, want run_commands", calls[0].Function.Name)
	}
}

func TestR106d_ParseToolCalls_EmptyFenceIsNotACall(t *testing.T) {
	// Пустой/бессмысленный фенс не должен превращаться в вызов.
	for _, output := range []string{"```", "```json", "```json\n```", "обычный текст ```json```"} {
		if calls := parseToolCallsFromOutput(output); len(calls) > 0 {
			t.Errorf("output %q дал %d вызовов, want 0", output, len(calls))
		}
	}
}
