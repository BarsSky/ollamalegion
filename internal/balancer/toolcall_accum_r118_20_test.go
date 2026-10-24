// toolcall_accum_r118_20_test.go — R118.20 (2026-10-15): закрытие пункта C5
// аудита клиентского API — фрагменты tool-call'ов собираются в транспорте и
// отдаются Ollama-клиенту ОДНИМ полным вызовом.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func sseChunkWithToolCall(id, name, args string, index int) string {
	chunk := map[string]interface{}{
		"id": "cmpl-1", "object": "chat.completion.chunk", "created": 1, "model": "m",
		"choices": []interface{}{
			map[string]interface{}{
				"index": 0,
				"delta": map[string]interface{}{
					"role": "assistant", "content": nil,
					"tool_calls": []interface{}{
						map[string]interface{}{
							"index": index, "id": id, "type": "function",
							"function": map[string]interface{}{"name": name, "arguments": args},
						},
					},
				},
				"finish_reason": nil,
			},
		},
	}
	raw, _ := json.Marshal(chunk)
	return string(raw)
}

func TestR118_20_FragmentsMergedAndEmittedOnce(t *testing.T) {
	acc := newToolCallAccumulator()

	// Первый фрагмент: аргументы ещё не завершены → чанк отдавать нельзя.
	if out, ok := acc.filterToolCallDelta(sseChunkWithToolCall("call_1", "search", `{"q":"te`, 0)); ok {
		t.Fatalf("неполный фрагмент не должен уходить клиенту, получено: %s", out)
	}

	// Второй фрагмент: аргументы собраны → уходит ПОЛНЫЙ вызов.
	out, ok := acc.filterToolCallDelta(sseChunkWithToolCall("call_1", "search", `st"}`, 0))
	if !ok {
		t.Fatal("после второго фрагмента вызов должен быть готов")
	}
	if !strings.Contains(out, `{\"q\":\"test\"}`) && !strings.Contains(out, `{"q":"test"}`) {
		t.Errorf("собранные аргументы неполные: %s", out)
	}
	if !strings.Contains(out, "call_1") || !strings.Contains(out, "search") {
		t.Errorf("id/name вызова потеряны: %s", out)
	}
}

func TestR118_20_CumulativeArgumentsNotDoubled(t *testing.T) {
	acc := newToolCallAccumulator()
	if _, ok := acc.filterToolCallDelta(sseChunkWithToolCall("call_1", "search", `{"q":"te`, 0)); ok {
		t.Fatal("первый фрагмент не должен быть готов")
	}
	// Некоторые upstreams шлют НАКОПИТЕЛЬНЫЙ arguments — дублировать нельзя.
	out, ok := acc.filterToolCallDelta(sseChunkWithToolCall("call_1", "search", `{"q":"test"}`, 0))
	if !ok {
		t.Fatal("накопительный фрагмент должен быть готов")
	}
	// Разбираем и сравниваем САМИ аргументы (строковое сравнение JSON хрупко:
	// кавычки внутри arguments экранированы).
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choices := parsed["choices"].([]interface{})
	delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})
	tcs := delta["tool_calls"].([]interface{})
	args := tcs[0].(map[string]interface{})["function"].(map[string]interface{})["arguments"].(string)
	if args != `{"q":"test"}` {
		t.Errorf("arguments = %q, want {\"q\":\"test\"} (дублирование накопительного фрагмента)", args)
	}
}

func TestR118_20_ChunkWithoutToolCallsPassesThrough(t *testing.T) {
	acc := newToolCallAccumulator()
	plain := `{"choices":[{"index":0,"delta":{"content":"привет"},"finish_reason":null}]}`
	out, ok := acc.filterToolCallDelta(plain)
	if !ok || out != plain {
		t.Errorf("обычный чанк обязан проходить как есть: ok=%v out=%s", ok, out)
	}
	// Не-JSON тоже не ломаем.
	if out, ok := acc.filterToolCallDelta("data: [DONE]"); !ok || out != "data: [DONE]" {
		t.Errorf("[DONE] должен проходить как есть: %v %q", ok, out)
	}
}

// TestR118_20_TranslatorEmitsCompleteToolCall — сквозная проверка: собранный
// накопителем чанк превращается транслятором в Ollama-форму с полным вызовом
// (именно этого не хватало до правки: чанк с фрагментом отбрасывался).
func TestR118_20_TranslatorEmitsCompleteToolCall(t *testing.T) {
	acc := newToolCallAccumulator()
	if _, ok := acc.filterToolCallDelta(sseChunkWithToolCall("call_1", "search", `{"q":"te`, 0)); ok {
		t.Fatal("фрагмент не должен быть готов")
	}
	ready, ok := acc.filterToolCallDelta(sseChunkWithToolCall("call_1", "search", `st"}`, 0))
	if !ok {
		t.Fatal("вызов должен быть собран")
	}
	out := translateOpenAISSEDataToOllama("/api/chat", []byte(ready), "m", nil, time.Time{}, time.Time{}, "")
	if out == nil {
		t.Fatal("собранный tool-call обязан транслироваться в Ollama-форму")
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg, _ := got["message"].(map[string]interface{})
	tcs, _ := msg["tool_calls"].([]interface{})
	if len(tcs) != 1 {
		t.Fatalf("ожидался один tool_call, получено %d: %s", len(tcs), strings.TrimSpace(string(out)))
	}
	if !strings.Contains(strings.TrimSpace(string(out)), "search") {
		t.Errorf("в Ollama-чанке нет имени функции: %s", strings.TrimSpace(string(out)))
	}
}
