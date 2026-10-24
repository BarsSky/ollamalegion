//go:build llama_stub

// writer_payload_matrix_r219_test.go — R219 (2026-10-21).
//
// Матрица по писателям SSE для буферизированной ветки /v1 (stream + tools).
// Живой дефект R216: эта ветка отдавала сырой вывод целиком в delta.content —
// размышления приезжали клиенту как ответ, а при tool_calls терялись. Тест
// прогоняет все комбинации reasoning/content и проверяет КОНТРАКТ чанков:
// reasoning только в reasoning_content, видимый текст только в content,
// role=assistant ровно один раз, finish_reason всегда есть.
package main

import (
	"testing"
)

func TestR219_StaticWriterMatrix(t *testing.T) {
	cases := []struct {
		name      string
		reasoning string
		content   string
		wantRsn   string
		wantCnt   string
	}{
		{"оба пустые", "", "", "", ""},
		{"только reasoning (обрезка)", "думаю", "", "думаю", ""},
		{"только ответ", "", "391", "", "391"},
		{"оба", "думаю", "391", "думаю", "391"},
		{"переносы и юникод", "строка1\nстрока2", "ответ ы", "строка1\nстрока2", "ответ ы"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder()
			writeStaticTextStream(rec, "m", "chatcmpl-1", 1, tc.reasoning, tc.content, "p", false)
			verifyWriterContract(t, rec.Body.String(), tc.wantRsn, tc.wantCnt, "stop")
		})
	}
}

func TestR219_ToolCallsWriterMatrix(t *testing.T) {
	calls := []openAIToolCall{newToolCall("call_1", "read_file", `{"path":"a.txt"}`)}
	cases := []struct {
		name      string
		reasoning string
	}{
		{"без reasoning", ""},
		{"с reasoning", "обдумываю вызов"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder()
			writeToolCallsStream(rec, "m", "chatcmpl-2", 1, calls, tc.reasoning, "p", false)
			// content при tool_calls пуст (клиент получает действие, а не текст).
			verifyWriterContract(t, rec.Body.String(), tc.reasoning, "", "tool_calls")
		})
	}
}
