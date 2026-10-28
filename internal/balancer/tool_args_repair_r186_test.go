// tool_args_repair_r186_test.go — R186 (2026-10-20).
//
// ЖИВОЙ РАЗБОР (Cline). Клиент показывал `Error: {"error":"✖ Invalid input"}` и
// повторял один и тот же вызов, пока не останавливался по «6 identical calls».
//
// Две причины, обе наши:
//   1. вызов, собранный балансером из текста модели, уходил без `index` —
//      ключа склейки фрагментов по спецификации OpenAI (при параллельных
//      вызовах клиент склеивает всё в один и получает битый JSON);
//   2. аргументы не приводились к схеме инструмента из запроса
//      (`{"commands":"git status"}` при схеме `commands: array` → клиентский
//      валидатор: «expected array, received string»).
//
// Контракт проверяется вместе с обратным: ВАЛИДНЫЕ аргументы не переписываются.
package balancer

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// schemaForTest — схема вида Cline run_commands: {commands: [string]}.
func schemaForTest(t *testing.T, raw string) map[string]*toolParamSchema {
	t.Helper()
	schemas := toolSchemasFromBody([]byte(raw))
	if len(schemas) == 0 {
		t.Fatalf("схемы инструментов не разобраны: %s", raw)
	}
	return schemas
}

const toolsRunCommands = `{"tools":[{"type":"function","function":{"name":"run_commands",
"parameters":{"type":"object","properties":{"commands":{"type":"array","items":{"type":"string"}}},
"required":["commands"]}}}]}`

func TestR186_ScalarToArray(t *testing.T) {
	schemas := schemaForTest(t, toolsRunCommands)
	calls := []interface{}{map[string]interface{}{
		"function": map[string]interface{}{"name": "run_commands", "arguments": `{"commands":"git status"}`},
	}}
	out, repaired := repairToolCallsForSchema(calls, schemas)
	if repaired != 1 {
		t.Fatalf("ожидалась одна правка, получено %d", repaired)
	}
	args := out[0].(map[string]interface{})["function"].(map[string]interface{})["arguments"].(string)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("аргументы не JSON: %v (%s)", err, args)
	}
	arr, ok := parsed["commands"].([]interface{})
	if !ok || len(arr) != 1 || arr[0] != "git status" {
		t.Fatalf("скаляр не превращён в массив: %s", args)
	}
}

func TestR186_ObjectToArrayOfObjects(t *testing.T) {
	schemas := schemaForTest(t, `{"tools":[{"type":"function","function":{"name":"run_commands",
"parameters":{"type":"object","properties":{"commands":{"type":"array",
"items":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}},
"required":["commands"]}}}]}`)
	calls := []interface{}{map[string]interface{}{
		"function": map[string]interface{}{"name": "run_commands", "arguments": `{"commands":{"command":"git status"}}`},
	}}
	out, repaired := repairToolCallsForSchema(calls, schemas)
	if repaired != 1 {
		t.Fatalf("ожидалась одна правка, получено %d", repaired)
	}
	args := out[0].(map[string]interface{})["function"].(map[string]interface{})["arguments"].(string)
	if !strings.Contains(args, `[{"command":"git status"}]`) {
		t.Fatalf("объект не превращён в массив объектов: %s", args)
	}
}

func TestR186_SingularToPluralRequired(t *testing.T) {
	schemas := schemaForTest(t, toolsRunCommands)
	calls := []interface{}{map[string]interface{}{
		"function": map[string]interface{}{"name": "run_commands", "arguments": `{"command":"git status"}`},
	}}
	out, _ := repairToolCallsForSchema(calls, schemas)
	args := out[0].(map[string]interface{})["function"].(map[string]interface{})["arguments"].(string)
	if !strings.Contains(args, `"commands":["git status"]`) {
		t.Fatalf("singular не приведён к обязательному plural: %s", args)
	}
	if strings.Contains(args, `"command"`) {
		t.Fatalf("лишний ключ command остался: %s", args)
	}
}

func TestR186_UnwrapsToolNameWrapper(t *testing.T) {
	schemas := schemaForTest(t, toolsRunCommands)
	calls := []interface{}{map[string]interface{}{
		"function": map[string]interface{}{
			"name":      "run_commands",
			"arguments": `{"run_commands":{"commands":["git status"]}}`,
		},
	}}
	out, repaired := repairToolCallsForSchema(calls, schemas)
	if repaired != 1 {
		t.Fatalf("обёртка именем инструмента не развёрнута (repaired=%d)", repaired)
	}
	args := out[0].(map[string]interface{})["function"].(map[string]interface{})["arguments"].(string)
	if strings.Contains(args, `"run_commands"`) {
		t.Fatalf("обёртка осталась: %s", args)
	}
}

func TestR186_NonJSONArgumentsWithSingleRequiredField(t *testing.T) {
	schemas := schemaForTest(t, toolsRunCommands)
	calls := []interface{}{map[string]interface{}{
		"function": map[string]interface{}{"name": "run_commands", "arguments": "git status"},
	}}
	out, repaired := repairToolCallsForSchema(calls, schemas)
	if repaired != 1 {
		t.Fatalf("неJSON-аргументы не вложены в обязательное поле (repaired=%d)", repaired)
	}
	args := out[0].(map[string]interface{})["function"].(map[string]interface{})["arguments"].(string)
	// Объявленный тип — массив строк, поэтому голая команда вкладывается массивом:
	// именно это примет валидатор клиента.
	if args != `{"commands":["git status"]}` {
		t.Fatalf("неожиданный ремонт: %s", args)
	}
}

// TestR186_ValidArgumentsUntouched — обратная сторона: корректный вызов не
// переписывается (иначе мы «чинили» бы то, что клиент и так принимает).
func TestR186_ValidArgumentsUntouched(t *testing.T) {
	schemas := schemaForTest(t, toolsRunCommands)
	calls := []interface{}{map[string]interface{}{
		"function": map[string]interface{}{"name": "run_commands", "arguments": `{"commands":["git status","ls"]}`},
	}}
	out, repaired := repairToolCallsForSchema(calls, schemas)
	if repaired != 0 {
		t.Fatalf("валидные аргументы переписаны (%d правок)", repaired)
	}
	args := out[0].(map[string]interface{})["function"].(map[string]interface{})["arguments"].(string)
	if args != `{"commands":["git status","ls"]}` {
		t.Fatalf("аргументы изменились: %s", args)
	}
}

// TestR186_UnknownToolUntouched — про инструмент, которого нет в запросе, ничего
// не выдумываем.
func TestR186_UnknownToolUntouched(t *testing.T) {
	schemas := schemaForTest(t, toolsRunCommands)
	calls := []interface{}{map[string]interface{}{
		"function": map[string]interface{}{"name": "other_tool", "arguments": `{"x":1}`},
	}}
	out, repaired := repairToolCallsForSchema(calls, schemas)
	if repaired != 0 {
		t.Fatalf("правился неизвестный инструмент (%d)", repaired)
	}
	args := out[0].(map[string]interface{})["function"].(map[string]interface{})["arguments"].(string)
	if args != `{"x":1}` {
		t.Fatalf("аргументы неизвестного инструмента изменились: %s", args)
	}
}

// TestR186_SSEChunkHasIndexAndRepairedArgs — главный живой дефект: собранный из
// текста вызов уходил БЕЗ index, а аргументы — без приведения к схеме.
//
// До правки тест падал: `index` отсутствовал, `commands` оставался строкой.
func TestR186_SSEChunkHasIndexAndRepairedArgs(t *testing.T) {
	schemas := schemaForTest(t, toolsRunCommands)
	// Так выглядит чанк cppworker, когда модель написала вызов ТЕКСТОМ.
	chunk := []byte(`{"choices":[{"delta":{"content":"{\"name\":\"run_commands\",\"arguments\":{\"commands\":\"git status\"}}"},"index":0}],"model":"m"}`)
	out := extractToolCallsFromSSEContentWithSchema(chunk, schemas, nil)
	if out == nil {
		t.Fatal("tool call из content не извлечён")
	}
	var parsed struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("выходной чанк не JSON: %v", err)
	}
	tc := parsed.Choices[0].Delta.ToolCalls
	if len(tc) != 1 {
		t.Fatalf("ожидался один вызов, получено %d", len(tc))
	}
	if tc[0].Index == nil {
		t.Fatal("в delta.tool_calls НЕТ index: клиент не сможет склеить фрагменты")
	}
	if *tc[0].Index != 0 {
		t.Errorf("index=%d, ожидался 0", *tc[0].Index)
	}
	if tc[0].ID == "" || tc[0].Type != "function" {
		t.Errorf("вызов не в форме OpenAI: id=%q type=%q", tc[0].ID, tc[0].Type)
	}
	if !strings.Contains(tc[0].Function.Arguments, `["git status"]`) {
		t.Errorf("аргументы не приведены к объявленной схеме: %s", tc[0].Function.Arguments)
	}
}

// TestR186_ParallelCallsGetOwnIndexes — параллельные вызовы обязаны получить
// РАЗНЫЕ index, иначе клиент склеит их в один.
func TestR186_ParallelCallsGetOwnIndexes(t *testing.T) {
	calls := []interface{}{
		map[string]interface{}{"function": map[string]interface{}{"name": "a", "arguments": `{}`}},
		map[string]interface{}{"function": map[string]interface{}{"name": "b", "arguments": `{}`}},
	}
	out, _ := repairToolCallsForSchema(calls, nil)
	for i, raw := range out {
		call := raw.(map[string]interface{})
		idx, ok := call["index"].(int)
		if !ok || idx != i {
			t.Fatalf("вызов %d: index=%v, ожидался %d", i, call["index"], i)
		}
	}
}

// TestR186_NativeChunkWithoutIndexGetsIndex — ЖИВОЙ дефект, снятый с потока:
// cppworker отдал нативный чанк tool_calls БЕЗ index, а клиент (Cline) через
// балансер получил его без изменений. Именно отсутствие index ломает склейку
// фрагментов и параллельных вызовов → «✖ Invalid input».
func TestR186_NativeChunkWithoutIndexGetsIndex(t *testing.T) {
	schemas := schemaForTest(t, toolsRunCommands)
	chunk := []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{\"commands\":\"git status\"}","name":"run_commands"},"id":"call_run_commands","type":"function"}],"finish_reason":null}}]}`)
	out := normalizeOpenAISSEPayload(chunk, schemas, nil)
	if out == nil {
		t.Fatal("нативный чанк без index не нормализован")
	}
	var parsed struct {
		Choices []struct {
			Delta struct {
				ToolCalls []map[string]interface{} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("выход не JSON: %v", err)
	}
	tc := parsed.Choices[0].Delta.ToolCalls[0]
	if _, ok := tc["index"]; !ok {
		t.Fatalf("index не добавлен: %s", string(out))
	}
	fn := tc["function"].(map[string]interface{})
	if !strings.Contains(fn["arguments"].(string), `["git status"]`) {
		t.Errorf("аргументы не приведены к схеме: %v", fn["arguments"])
	}
}

// TestR186_PlainContentChunkUntouched — обычный текстовый чанк не переписываем.
func TestR186_PlainContentChunkUntouched(t *testing.T) {
	chunk := []byte(`{"choices":[{"index":0,"delta":{"content":"привет"}}]}`)
	if out := normalizeOpenAISSEPayload(chunk, nil, nil); out != nil {
		t.Fatalf("изменён чанк без tool_calls: %s", string(out))
	}
}

// TestR186_SynthesizedSSECarriesIndexAndFinishReason — ЖИВОЙ путь Cline: балансер
// объявил свой инструмент картинок, поэтому ответ переизлучается из
// буферизованного сообщения. Раньше вызовы уходили без `index` и с
// finish_reason="stop", а аргументы — без приведения к схеме запроса.
func TestR186_SynthesizedSSECarriesIndexAndFinishReason(t *testing.T) {
	schemas := schemaForTest(t, toolsRunCommands)
	upstream := []byte(`{"id":"chatcmpl-1","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_run_commands","type":"function","function":{"name":"run_commands","arguments":"{\"commands\":\"git status\"}"}}]},"finish_reason":"stop"}]}`)
	rec := httptest.NewRecorder()
	synthesizeChatSSEWithSchemas(rec, upstream, "m", 200, schemas)
	body := rec.Body.String()
	if !strings.Contains(body, "data: ") {
		t.Fatalf("SSE не сформирован: %s", body)
	}
	var sawCall, sawToolFinish bool
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []map[string]interface{} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason interface{} `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("чанк не JSON: %v (%s)", err, line)
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				sawCall = true
				if _, ok := tc["index"]; !ok {
					t.Errorf("в переизлучённом вызове нет index: %v", tc)
				}
				fn, _ := tc["function"].(map[string]interface{})
				if args, _ := fn["arguments"].(string); !strings.Contains(args, `["git status"]`) {
					t.Errorf("аргументы не приведены к схеме: %v", fn["arguments"])
				}
			}
			if fr, _ := c.FinishReason.(string); fr == "tool_calls" {
				sawToolFinish = true
			}
		}
	}
	if !sawCall {
		t.Fatal("tool_calls не переизлучены")
	}
	if !sawToolFinish {
		t.Fatal("finish_reason=tool_calls не выставлен: клиент примет ответ за текст и не исполнит инструмент")
	}
}
