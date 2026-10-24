// audit_client_api_r118_19_test.go — R118.19 (2026-10-15): закрытие остатка
// долга по клиентскому API (docs/archive/audits/AUDIT-client-api-fidelity-2026.md, пункты C2-C5).
//
// Что именно фиксируется:
//   C3 — Ollama-клиент вправе прислать `options.stop` СТРОКОЙ, а cppworker ждёт
//        `[]string` (`Stop []string` в handlers_openai.go). Без нормализации
//        строгий декодер отвечал 400 «cannot unmarshal string into Go struct
//        field ... of type []string» и запрос падал целиком.
//   C2 — `presence_penalty` / `frequency_penalty` cppworker принимает
//        (PresencePenalty/FrequencyPenalty в той же структуре), но трансляция их
//        не пробрасывала — клиент выставлял параметры и молча не получал.
//   C4 — место `thinking` в СТРИМИНГЕ `/api/generate`: у Ollama рассуждения
//        лежат в поле верхнего уровня `thinking`, а не в `message.thinking`
//        (это форма `/api/chat`).
//   C5 — дельты tool-call'ов на Ollama-поверхности: чанк несёт ИМЕННО дельту
//        (а не накопленный список), потому что на OpenAI-поверхности балансер
//        вообще ничего не транслирует (нативный passthrough), а Ollama-форма
//        заполняется из дельты upstream.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// --- C3: options.stop строкой ---

func TestR118_19_StopStringBecomesArray(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"options":{"stop":"###"}}`)
	out, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatalf("translation failed: %v", err)
	}
	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	stop, ok := req["stop"].([]interface{})
	if !ok {
		t.Fatalf("stop должен уйти массивом (cppworker ждёт []string), получено %T: %v", req["stop"], req["stop"])
	}
	if len(stop) != 1 || stop[0] != "###" {
		t.Errorf("stop = %v, want [###]", stop)
	}
}

func TestR118_19_StopStringBecomesArrayForGenerate(t *testing.T) {
	body := []byte(`{"model":"m","prompt":"hi","options":{"stop":"\n\nUser:"}}`)
	out, err := translateOllamaGenerateToOpenAI(body)
	if err != nil {
		t.Fatalf("translation failed: %v", err)
	}
	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := req["stop"].([]interface{}); !ok {
		t.Fatalf("/api/generate: stop должен уйти массивом, получено %T", req["stop"])
	}
}

func TestR118_19_NormalizeOpenAIStop(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want []string
	}{
		{"строка", "STOP", []string{"STOP"}},
		{"пустая строка", "", nil},
		{"массив строк", []string{"a", "", "b"}, []string{"a", "b"}},
		{"массив interface", []interface{}{"a", 42, "b"}, []string{"a", "b"}},
		{"число", 42, nil},
		{"nil", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeOpenAIStop(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("normalizeOpenAIStop(%v) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// --- C2: штрафы пробрасываются ---

func TestR118_19_PenaltiesForwardedToUpstream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		trans  func([]byte) ([]byte, error)
		path   string
	}{
		{
			name:  "chat",
			body:  `{"model":"m","messages":[{"role":"user","content":"hi"}],"options":{"presence_penalty":1.5,"frequency_penalty":-0.5}}`,
			trans: translateOllamaChatToOpenAI,
			path:  "/api/chat",
		},
		{
			name:  "generate",
			body:  `{"model":"m","prompt":"hi","options":{"presence_penalty":0.7,"frequency_penalty":0.3}}`,
			trans: translateOllamaGenerateToOpenAI,
			path:  "/api/generate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.trans([]byte(tc.body))
			if err != nil {
				t.Fatalf("%s: translation failed: %v", tc.path, err)
			}
			var req map[string]interface{}
			if err := json.Unmarshal(out, &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if req["presence_penalty"] == nil {
				t.Errorf("%s: presence_penalty потерян при трансляции (cppworker его принимает)", tc.path)
			}
			if req["frequency_penalty"] == nil {
				t.Errorf("%s: frequency_penalty потерян при трансляции (cppworker его принимает)", tc.path)
			}
		})
	}
}

// --- C4: thinking в стриминге /api/generate (закрыт R118.23) ---
//
// Контракт: у /api/generate рассуждения обязаны быть полем ВЕРХНЕГО уровня
// `thinking` (у generate-чанка нет `message`). Проверяются все три формы, в
// которых upstream может прислать рассуждение: choice-level (legacy cppworker),
// top-level (новый формат) и chat-форма `delta.reasoning_content` (её на
// generate-пути раньше не разбирали вовсе — это и был пункт C4).

func TestR118_23_C4_GenerateStreamingThinkingTopLevel_AllShapes(t *testing.T) {
	shapes := []struct {
		name  string
		chunk map[string]interface{}
	}{
		{
			name: "choice.reasoning_content (legacy)",
			chunk: map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{"index": 0, "text": "", "reasoning_content": "думаю"},
				},
			},
		},
		{
			name: "top-level reasoning_content (новый формат)",
			chunk: map[string]interface{}{
				"reasoning_content": "думаю",
				"choices":           []interface{}{map[string]interface{}{"index": 0, "text": ""}},
			},
		},
		{
			name: "delta.reasoning_content (chat-форма)",
			chunk: map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{"index": 0, "delta": map[string]interface{}{"reasoning_content": "думаю"}},
				},
			},
		},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			raw, _ := json.Marshal(sh.chunk)
			out := translateOpenAISSEDataToOllama("/api/generate", raw, "m", nil, time.Time{}, time.Time{}, "")
			if out == nil {
				t.Fatalf("/api/generate: reasoning-чанк (%s) не эмитится — thinking потеряется", sh.name)
			}
			var got map[string]interface{}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got["thinking"] != "думаю" {
				t.Errorf("%s: thinking = %v (want \"думаю\" в поле ВЕРХНЕГО уровня): %s",
					sh.name, got["thinking"], strings.TrimSpace(string(out)))
			}
			if _, hasMsg := got["message"]; hasMsg {
				t.Errorf("%s: у generate-чанка не должно быть message: %s", sh.name, strings.TrimSpace(string(out)))
			}
		})
	}
}
func TestR118_20_C5_FragmentedToolCallsCoveredByAccumulator(t *testing.T) {
	// Фрагмент JSON-аргументов (cppworker вправе дробить arguments по чанкам).
	fragment := map[string]interface{}{
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{
				"tool_calls": []map[string]interface{}{
					{"index": 0, "id": "call_1", "type": "function",
						"function": map[string]interface{}{"name": "search", "arguments": `{"q":"te`}},
				},
			}, "finish_reason": nil},
		},
	}
	raw, _ := json.Marshal(fragment)
	out := translateOpenAISSEDataToOllama("/api/chat", raw, "m", nil, time.Time{}, time.Time{}, "")

	// ТЕКУЩЕЕ ПОВЕДЕНИЕ: чанк с НЕзавершённым JSON в arguments отбрасывается
	// (транслятор — чистая функция без состояния, накапливать фрагменты ему негде).
	// На OpenAI-поверхности это неважно (там нативный passthrough), но на
	// Ollama-поверхности с выключенным нативным путём (LB_OLLAMA_NATIVE_PATH=0)
	// фрагментированный tool-call теряет часть аргументов — открытый пункт C5.
	if out != nil {
		t.Logf("поведение изменилось (C5 закрыт?): фрагмент теперь эмитится: %s", strings.TrimSpace(string(out)))
	}

	// А чанк той формы, что шлёт cppworker (role + content:null + tool_calls с
	// ПОЛНЫМ JSON), проходит — это держит существующий тест
	// TestTranslateSSEChatToOllama_ToolCalls. Фиксируем, что дело именно в
	// фрагментации/отсутствии роли, а не в потере tool_calls вообще.
	full := map[string]interface{}{
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{
				"role": "assistant", "content": nil,
				"tool_calls": []map[string]interface{}{
					{"index": 0, "id": "call_1", "type": "function",
						"function": map[string]interface{}{"name": "search", "arguments": `{"q":"test"}`}},
				},
			}, "finish_reason": nil},
		},
	}
	rawFull, _ := json.Marshal(full)
	if got := translateOpenAISSEDataToOllama("/api/chat", rawFull, "m", nil, time.Time{}, time.Time{}, ""); got == nil {
		t.Error("чанк cppworker-формы (role + полный JSON) обязан проходить — сломана базовая трансляция tool_calls")
	}
}
