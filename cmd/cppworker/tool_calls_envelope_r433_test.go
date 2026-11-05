// tool_calls_envelope_r433_test.go — R433 (2026-11-05): JSON-обёртка
// `{"tool_calls":[…]}` — это вызов, а не текст ответа.
//
// ФИКСТУРА — живой ответ gemma-4 на стенде (Cline, после R432): модель вернула
// целый JSON-документ, вызов лежал в массиве, клиент показывал его текстом.
package main

import (
	"strings"
	"testing"
)

const fixtureEnvelopeR433 = `{
  "tool_calls": [
    {
      "name": "read_files",
      "arguments": {
        "files": [
          "TASK.md"
        ]
      }
    }
  ],
  "output": ""
}`

// TestR433_EnvelopeIsParsed — вызов из обёртки распознан и выполним.
func TestR433_EnvelopeIsParsed(t *testing.T) {
	calls := parseToolCallsForRequest(fixtureEnvelopeR433, formatTestTools())
	if len(calls) != 1 || calls[0].Function.Name != "read_files" {
		t.Fatalf("вызов из обёртки не распознан: %+v", calls)
	}
	if !strings.Contains(calls[0].Function.Arguments, "TASK.md") {
		t.Fatalf("аргументы потеряны: %q", calls[0].Function.Arguments)
	}
}

// TestR433_EnvelopeIsMarkup — обёртка считается разметкой вызова, поэтому не
// уезжает клиенту как «ответ».
func TestR433_EnvelopeIsMarkup(t *testing.T) {
	if !looksLikeToolCallMarkup(fixtureEnvelopeR433) {
		t.Fatal("обёртка не опознана как разметка вызова")
	}
	if !looksLikeToolCallMarkup(`{"tool_calls":[{"name":"editor","arguments":{"path":"a","new_text":"b"}}]}`) {
		t.Fatal("однострочная обёртка не опознана")
	}
}

// TestR433_EnvelopeWithOpenAIItems — элементы могут быть в OpenAI-виде
// (`function.name` / `arguments` строкой).
func TestR433_EnvelopeWithOpenAIItems(t *testing.T) {
	raw := `{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"editor","arguments":"{\"path\":\"a.txt\",\"new_text\":\"ok\"}"}}]}`
	calls := parseToolCallsForRequest(raw, formatTestTools())
	if len(calls) != 1 || calls[0].Function.Name != "editor" {
		t.Fatalf("OpenAI-элементы в обёртке не разобраны: %+v", calls)
	}
}

// TestR433_PlainTextWithKeyWord — контроль: текст, где слово tool_calls
// встречается в прозе, не превращается в вызов.
func TestR433_PlainTextWithKeyWord(t *testing.T) {
	raw := `Поле "tool_calls" в ответе OpenAI содержит массив вызовов.`
	if calls := parseToolCallsForRequest(raw, formatTestTools()); len(calls) != 0 {
		t.Fatalf("проза принята за вызов: %+v", calls)
	}
}
