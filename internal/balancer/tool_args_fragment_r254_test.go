// tool_args_fragment_r254_test.go — R254 (2026-10-28): незакрытый фрагмент
// аргументов инструмента НЕ ремонтируется.
//
// ЖИВОЙ ДЕФЕКТ. На /v1 каждый SSE-чанк с tool_call нормализовался отдельно, и на
// неполном фрагменте arguments ремонт уходил в ветку «не JSON»: при схеме с ОДНИМ
// обязательным полем обрывок оборачивался в {"field":"<обрывок>"} и уезжал
// клиенту. Клиент склеивает дельты по index, поэтому обёртка склеивалась с
// последующими кусками настоящего JSON → «✖ Invalid input» → повтор и остановка.
//
// Тесты проверяют поток ТАК, КАК ЕГО ВИДИТ КЛИЕНТ: склеивают arguments из
// последовательных чанков и разбирают результат.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// r254Schema — инструмент с ОДНИМ обязательным полем (именно эта форма
// провоцировала обёртку обрывка).
func r254Schema(t *testing.T) map[string]*toolParamSchema {
	t.Helper()
	s := parseToolParamSchema(json.RawMessage(`{"type":"object","required":["content"],` +
		`"properties":{"content":{"type":"string"}}}`))
	if s == nil {
		t.Fatal("схема не разобрана")
	}
	return map[string]*toolParamSchema{"write_file": s}
}

// r254ShellChunk — чанк tool_call с куском arguments.
func r254ShellChunk(args string) []byte {
	chunk := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"delta": map[string]interface{}{
					"tool_calls": []interface{}{
						map[string]interface{}{
							"index": 0,
							"id":    "call-1",
							"type":  "function",
							"function": map[string]interface{}{
								"name":      "write_file",
								"arguments": args,
							},
						},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// r254ConcatArgs — склеить arguments из всех чанков так, как это делает клиент.
func r254ConcatArgs(t *testing.T, chunks [][]byte) string {
	t.Helper()
	var sb strings.Builder
	for _, chunk := range chunks {
		var parsed map[string]interface{}
		if err := json.Unmarshal(chunk, &parsed); err != nil {
			t.Fatalf("чанк не JSON: %v (%s)", err, chunk)
		}
		choices, _ := parsed["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]interface{})
		delta, _ := choice["delta"].(map[string]interface{})
		calls, _ := delta["tool_calls"].([]interface{})
		if len(calls) == 0 {
			continue
		}
		call, _ := calls[0].(map[string]interface{})
		fn, _ := call["function"].(map[string]interface{})
		args, _ := fn["arguments"].(string)
		sb.WriteString(args)
	}
	return sb.String()
}

// TestR254_UnclosedFragmentNotWrapped — (а)+(в): обрывок аргументов при схеме с
// одним обязательным полем НЕ оборачивается; склеенный клиентом поток — валидный
// JSON.
func TestR254_UnclosedFragmentNotWrapped(t *testing.T) {
	schemas := r254Schema(t)
	state := &toolRepairState{}

	// Поток: "{"cont" → "ent":"hel" → "lo"}" (как дельты настоящего стрима).
	fragments := []string{`{"cont`, `ent":"hel`, `lo"}`, ""}
	var normalized [][]byte
	for _, frag := range fragments {
		out := normalizeOpenAISSEPayload(r254ShellChunk(frag), schemas, state)
		if out == nil {
			// Ничего менять не потребовалось — клиент получил бы исходный чанк.
			normalized = append(normalized, r254ShellChunk(frag))
			continue
		}
		normalized = append(normalized, out)
	}

	joined := r254ConcatArgs(t, normalized)
	if strings.Contains(joined, `"content":"{\"cont`) {
		t.Fatalf("обрывок обёрнут в единственное обязательное поле — клиент получит битый JSON: %q", joined)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(joined), &args); err != nil {
		t.Fatalf("склеенные аргументы не валидный JSON (%q): %v", joined, err)
	}
	if args["content"] != "hello" {
		t.Errorf("content = %v, want hello (аргументы не должны искажаться)", args["content"])
	}
}

// TestR254_CompleteStringArgStillRepaired — (б): ПОЛНОЕ значение, пришедшее
// строкой, по-прежнему ремонтируется (поведение R186 не потеряно).
func TestR254_CompleteStringArgStillRepaired(t *testing.T) {
	schemas := r254Schema(t)

	// JSON-строка в аргументах стрима — значение законченное (json.Valid=true).
	chunk := r254ShellChunk(`"hello"`)
	out := normalizeOpenAISSEPayload(chunk, schemas, &toolRepairState{})
	if out == nil {
		t.Fatal("полное строковое значение не отремонтировано")
	}
	if got := r254ConcatArgs(t, [][]byte{out}); got != `{"content":"hello"}` {
		t.Fatalf("аргументы = %q, want {\"content\":\"hello\"}", got)
	}

	// Non-stream/собранный ответ: «голая команда» без JSON тоже ремонтируется.
	calls := []interface{}{map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":      "write_file",
			"arguments": "hello",
		},
	}}
	fixed, repaired := repairToolCallsForSchema(calls, schemas)
	if repaired == 0 {
		t.Fatal("полное не-JSON значение в non-stream ответе не отремонтировано (регресс R186)")
	}
	fn, _ := fixed[0].(map[string]interface{})["function"].(map[string]interface{})
	if got, _ := fn["arguments"].(string); got != `{"content":"hello"}` {
		t.Fatalf("аргументы = %q, want {\"content\":\"hello\"}", got)
	}
}

// TestR254_RepairableFragmentPolicy — единая точка решения: законченный JSON
// ремонтируется всегда, обрывок — никогда, не-JSON «голая команда» — только когда
// значение пришло целиком.
func TestR254_RepairableFragmentPolicy(t *testing.T) {
	cases := []struct {
		args     string
		complete bool
		want     bool
	}{
		{`{"content":"hi"}`, false, true}, // законченный объект
		{`"hello"`, false, true},          // законченная JSON-строка
		{`{"cont`, false, false},          // обрывок в стриме
		{`hel`, false, false},             // обрывок/голая команда в стриме
		{`{"cont`, true, false},           // обрывок даже в финале — не ремонтируем
		{`hel`, true, true},               // голая команда в целом значении
		{"   ", true, false},              // пусто
	}
	for _, tc := range cases {
		if got := repairableToolArgsFragment(tc.args, tc.complete); got != tc.want {
			t.Errorf("repairableToolArgsFragment(%q, complete=%v) = %v, want %v",
				tc.args, tc.complete, got, tc.want)
		}
	}
}

// TestR254_DiagnosticOncePerStream — (г): пропуск ремонта пишется в лог не более
// одного раза на стрим (на длинном tool-call'е иначе сотни строк).
func TestR254_DiagnosticOncePerStream(t *testing.T) {
	schemas := r254Schema(t)
	state := &toolRepairState{}
	for i := 0; i < 8; i++ {
		_ = normalizeOpenAISSEPayload(r254ShellChunk(`{"cont`), schemas, state)
		_ = normalizeOpenAISSEPayload(r254ShellChunk(`inu`), schemas, state)
	}
	if state.logged != 1 {
		t.Fatalf("диагностика записана %d раз, want 1 (спам в журнале)", state.logged)
	}
}
