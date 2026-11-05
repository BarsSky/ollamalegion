// tool_calls_envelope_r433.go — R433 (2026-11-05): JSON-ОБЁРТКА с массивом
// `tool_calls` разбирается как вызов (а не как текст ответа).
//
// ЖИВОЙ ПРОГОН (Cline + gemma-4 на стенде, после R432): модель ответила целым
// JSON-документом вместо вызова:
//
//	{
//	  "tool_calls": [
//	    {"name": "read_files", "arguments": {"files": ["TASK.md"]}}
//	  ],
//	  "output": ""
//	}
//
// Верхнеуровневых `name`/`arguments` тут нет — они лежат на элемент массива,
// поэтому toolCallFromJSONObject обёртку не видел, вызов считался текстом и
// уезжал клиенту как ответ (Cline: COMPLETED, файл не создан). Это буквально
// форма OpenAI-ответа (`{"tool_calls":[…]}`), модели её копируют, когда просят
// «answer with JSON».
//
// Правила безопасности те же, что у остальных восстановлений: каждый элемент
// обязан дать имя + JSON-объект аргументов, а объявленность инструмента
// проверяет filterUndeclaredToolCalls (R428) дальше по конвейеру.
package main

import (
	"encoding/json"
	"strings"
)

// toolCallsFromEnvelopeR433 — вызовы из JSON-обёртки с полем `tool_calls`.
func toolCallsFromEnvelopeR433(raw string) []openAIToolCall {
	if raw == "" || !strings.Contains(raw, `"tool_calls"`) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil
	}
	tcRaw, ok := obj["tool_calls"]
	if !ok {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(tcRaw, &items); err != nil {
		// Один объект вместо массива — принимаем и его.
		items = []json.RawMessage{tcRaw}
	}
	var out []openAIToolCall
	for _, item := range items {
		if call := toolCallFromJSONObject(string(item)); call != nil {
			out = append(out, *call)
			continue
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isToolCallsEnvelopeR433 — объект ЯВЛЯЕТСЯ обёрткой с вызовами (нужно и для
// «это разметка вызова?», чтобы такая обёртка не уезжала клиенту как текст).
func isToolCallsEnvelopeR433(raw string) bool {
	return len(toolCallsFromEnvelopeR433(raw)) > 0
}
