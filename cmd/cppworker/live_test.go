// live_test.go — Reproduction tests for tool_calls parser using LIVE model output.
// Run with: go test -v -run TestLive ./cmd/cppworker/
package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestLiveMultiToolTwoGetWeather reproduces the EXACT model output that
// failed in production (Qwen3-4B-Instruct-2507, /v1/chat/completions).
// Model output (228 bytes, no trailing newline, no trailing period):
//
//	[{"id":"call_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"London\"}"}}]
func TestLiveMultiToolTwoGetWeather(t *testing.T) {
	output := `[{"id":"call_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"London\"}"}}]`
	t.Logf("input bytes: %d, content: %q", len(output), output)
	calls := parseToolCallsFromOutput(output)
	t.Logf("→ %d tool call(s) extracted", len(calls))
	for i, c := range calls {
		b, _ := json.Marshal(c)
		t.Logf("  [%d] %s", i, string(b))
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}
}

// TestLiveMultiToolCalculatorListFiles reproduces a second live failure
// (Qwen3-4B-Instruct, calculator + list_files):
//
//	[{"id":"call_calc","type":"function","function":{"name":"calculator","arguments":"{\"a\":2,\"b\":3,\"op\":\"add\"}"}},{"id":"call_list","type":"function","function":{"name":"list_files","arguments":"{\"path\":\"/tmp\"}"}}]
func TestLiveMultiToolCalculatorListFiles(t *testing.T) {
	output := `[{"id":"call_calc","type":"function","function":{"name":"calculator","arguments":"{\"a\":2,\"b\":3,\"op\":\"add\"}"}},{"id":"call_list","type":"function","function":{"name":"list_files","arguments":"{\"path\":\"/tmp\"}"}}]`
	t.Logf("input bytes: %d, content: %q", len(output), output)
	calls := parseToolCallsFromOutput(output)
	t.Logf("→ %d tool call(s) extracted", len(calls))
	for i, c := range calls {
		b, _ := json.Marshal(c)
		t.Logf("  [%d] %s", i, string(b))
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}
}

// TestLiveSingleToolListFiles — the case that WORKS in production.
func TestLiveSingleToolListFiles(t *testing.T) {
	output := `[{"id":"call_list_files","type":"function","function":{"name":"list_files","arguments":"{\"path\":\"/var/log\"}"}}]`
	t.Logf("input bytes: %d", len(output))
	calls := parseToolCallsFromOutput(output)
	t.Logf("→ %d tool call(s) extracted", len(calls))
	for i, c := range calls {
		b, _ := json.Marshal(c)
		t.Logf("  [%d] %s", i, string(b))
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
}

// Sanity check: with garbage input the parser should return nil.
func TestLiveParser_NilOnGarbage(t *testing.T) {
	calls := parseToolCallsFromOutput("Hello! How can I help you today?")
	if len(calls) != 0 {
		t.Fatalf("expected 0 tool calls, got %d", len(calls))
	}
}

// Sanity dump helper: print parser strategies invocation order.
func TestLiveParser_DumpStrategies(t *testing.T) {
	output := `[{"id":"call_x","type":"function","function":{"name":"foo","arguments":"{}"}},{"id":"call_y","type":"function","function":{"name":"bar","arguments":"{}"}}]`
	fmt.Println("--- dump strategies ---")
	calls := parseToolCallsFromOutput(output)
	fmt.Printf("got %d calls\n", len(calls))
	for i, c := range calls {
		b, _ := json.Marshal(c)
		fmt.Printf("  [%d] %s\n", i, string(b))
	}
}

// TestLiveParser_LeadingText — model может префикснуть JSON текстом вроде
// "Sure, I'll check the weather in both cities:" — Strategy 4 (full JSON unmarshal)
// упадёт, Strategy 6 (last "\n[") должна спасти.
func TestLiveParser_LeadingText(t *testing.T) {
	output := `Sure, I'll check the weather in both cities:
[{"id":"call_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"London\"}"}}]`
	calls := parseToolCallsFromOutput(output)
	t.Logf("LeadingText → %d calls", len(calls))
	if len(calls) != 2 {
		t.Fatalf("expected 2, got %d", len(calls))
	}
}

// TestLiveParser_LeadingTextNoNewline — модель может вывести JSON без
// перевода строки после префикса: "I'll help:[{...}]" — Strategy 6 должна
// найти первый "[" и распарсить оттуда.
func TestLiveParser_LeadingTextNoNewline(t *testing.T) {
	output := `I'll help you with that:[{"id":"call_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"London\"}"}}]`
	calls := parseToolCallsFromOutput(output)
	t.Logf("LeadingTextNoNewline → %d calls", len(calls))
	if len(calls) != 2 {
		t.Fatalf("expected 2, got %d", len(calls))
	}
}

// TestLiveParser_TrailingNewline — модель может добавить \n в конце.
func TestLiveParser_TrailingNewline(t *testing.T) {
	output := "[{\"id\":\"call_get_weather\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"Paris\\\"}\"}},{\"id\":\"call_get_weather\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"London\\\"}\"}}]\n"
	calls := parseToolCallsFromOutput(output)
	t.Logf("TrailingNewline → %d calls", len(calls))
	if len(calls) != 2 {
		t.Fatalf("expected 2, got %d", len(calls))
	}
}

// TestLiveParser_CodeFence — модель может обернуть в ```json ... ```.
func TestLiveParser_CodeFence(t *testing.T) {
	output := "```json\n" + `[{"id":"call_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"London\"}"}}]` + "\n```"
	calls := parseToolCallsFromOutput(output)
	t.Logf("CodeFence → %d calls", len(calls))
	if len(calls) != 2 {
		t.Fatalf("expected 2, got %d", len(calls))
	}
}

// TestLiveParser_ThinkingPrefix — модель с reasoning (Qwen3 thinking) может
// вывести <think>... блок + JSON. Reasoning content отделён до парсера, но
// проверим что парсер не падает.
func TestLiveParser_ThinkingPrefix(t *testing.T) {
	output := "I'll check both cities.\n[{\"id\":\"call_get_weather\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"Paris\\\"}\"}},{\"id\":\"call_get_weather\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"London\\\"}\"}}]"
	calls := parseToolCallsFromOutput(output)
	t.Logf("ThinkingPrefix → %d calls", len(calls))
	if len(calls) != 2 {
		t.Fatalf("expected 2, got %d", len(calls))
	}
}

// TestLiveParser_TrailingBraceBug — Round 16 multi-tool recovery.
//
// EXACT live failure (Qwen3-4B-Instruct-2507, 2026-07-30):
//   Output: [{...function1...},{...function2...}]}
//   228 bytes, last byte = 0x7d ("}")
//   Standard json.Unmarshal fails at char 226 (Expecting ',' delimiter).
//   Strategy 4b (recoverToolCallsByPrefixTrimming) должна найти префикс
//   [...function2...]} (без последней "}) который парсится валидно.
func TestLiveParser_TrailingBraceBug(t *testing.T) {
	output := `[{"id":"call_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call_get_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"London\"}"}]}`
	if len(output) != 228 {
		t.Logf("WARNING: expected 228 bytes, got %d", len(output))
	}
	calls := parseToolCallsFromOutput(output)
	t.Logf("TrailingBraceBug → %d calls", len(calls))
	for i, c := range calls {
		b, _ := json.Marshal(c)
		t.Logf("  [%d] %s", i, string(b))
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 (after recovery), got %d", len(calls))
	}
}
