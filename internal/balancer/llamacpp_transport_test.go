package balancer

import (
	"encoding/json"
	"testing"
)

// Тесты для translateSSEChatToOllama — проверяем корректность трансляции
// OpenAI streaming чанков в Ollama NDJSON. Это ключевая часть фикса —
// без неё OpenWebUI получает обрезанные ответы.

func TestTranslateSSEChatToOllama_ContentOnly(t *testing.T) {
	chunk := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"delta": map[string]interface{}{
					"content": "Привет",
				},
			},
		},
	}
	got := translateSSEChatToOllama(chunk, "gemma-4", nil)
	if got == nil {
		t.Fatal("expected non-nil result for content chunk")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(got[:len(got)-1], &obj); err != nil { // strip trailing \n
		t.Fatalf("invalid JSON: %v: %s", err, got)
	}
	msg, ok := obj["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing message: %s", got)
	}
	if msg["content"] != "Привет" {
		t.Errorf("wrong content: %v", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("expected role=assistant, got %v", msg["role"])
	}
}

func TestTranslateSSEChatToOllama_ContentAndRole(t *testing.T) {
	chunk := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"delta": map[string]interface{}{
					"role":    "assistant",
					"content": " world",
				},
			},
		},
	}
	got := translateSSEChatToOllama(chunk, "gemma-4", nil)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(got[:len(got)-1], &obj); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, got)
	}
	msg := obj["message"].(map[string]interface{})
	if msg["content"] != " world" {
		t.Errorf("wrong content: %v", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("wrong role: %v", msg["role"])
	}
}

func TestTranslateSSEChatToOllama_RoleOnly(t *testing.T) {
	// OpenAI первый чанк: только role, без content.
	// OpenWebUI использует это как маркер начала потока.
	chunk := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"delta": map[string]interface{}{
					"role": "assistant",
				},
			},
		},
	}
	got := translateSSEChatToOllama(chunk, "gemma-4", nil)
	if got == nil {
		t.Fatal("role-only chunk must NOT return nil (fix from previous version)")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(got[:len(got)-1], &obj); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, got)
	}
	msg := obj["message"].(map[string]interface{})
	if msg["role"] != "assistant" {
		t.Errorf("wrong role: %v", msg["role"])
	}
	if msg["content"] != "" {
		t.Errorf("expected empty content, got %v", msg["content"])
	}
}

func TestTranslateSSEChatToOllama_FinishOnly(t *testing.T) {
	// Финальный чанк: пустая delta + finish_reason="stop"
	chunk := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"delta":         map[string]interface{}{},
				"finish_reason": "stop",
			},
		},
	}
	got := translateSSEChatToOllama(chunk, "gemma-4", nil)
	if got == nil {
		t.Fatal("finish-only chunk must return done marker (NOT nil)")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(got[:len(got)-1], &obj); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, got)
	}
	if obj["done"] != true {
		t.Errorf("expected done=true: %s", got)
	}
	if obj["done_reason"] != "stop" {
		t.Errorf("expected done_reason=stop: %v", obj["done_reason"])
	}
}

func TestTranslateSSEChatToOllama_Empty(t *testing.T) {
	// Пустой / невалидный chunk
	chunk := map[string]interface{}{}
	got := translateSSEChatToOllama(chunk, "gemma-4", nil)
	if got != nil {
		t.Errorf("empty chunk should return nil, got: %s", got)
	}
}

func TestTranslateSSEChatToOllama_FullReconstruction(t *testing.T) {
	// Полный сценарий: набор чанков от OpenAI должен склеиться в полный ответ.
	chunks := []map[string]interface{}{
		{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"role": "assistant"}}}},
		{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"content": "Hello"}}}},
		{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"content": " world"}}}},
		{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"content": "!"}}}},
		{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{}, "finish_reason": "stop"}}},
	}
	fullContent := ""
	for _, c := range chunks {
		b := translateSSEChatToOllama(c, "gemma-4", nil)
		if b == nil {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(b[:len(b)-1], &obj); err != nil {
			t.Fatalf("invalid JSON: %v: %s", err, b)
		}
		if msg, ok := obj["message"].(map[string]interface{}); ok {
			if c, ok := msg["content"].(string); ok {
				fullContent += c
			}
		}
	}
	if fullContent != "Hello world!" {
		t.Errorf("reconstructed content mismatch: got %q, want %q", fullContent, "Hello world!")
	}
}

func TestTranslateSSEGenerateToOllama_TextContent(t *testing.T) {
	chunk := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"text": "token1",
			},
		},
	}
	got := translateSSEGenerateToOllama(chunk, "gemma-4", nil)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(got[:len(got)-1], &obj); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, got)
	}
	if obj["response"] != "token1" {
		t.Errorf("wrong response: %v", obj["response"])
	}
}

func TestTranslateSSEGenerateToOllama_EmptyText(t *testing.T) {
	chunk := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"text": "",
			},
		},
	}
	got := translateSSEGenerateToOllama(chunk, "gemma-4", nil)
	if got != nil {
		t.Errorf("empty text should return nil, got: %s", got)
	}
}

func TestTranslateSSEGenerateToOllama_FinishOnly(t *testing.T) {
	chunk := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"text":          "",
				"finish_reason": "length",
			},
		},
	}
	got := translateSSEGenerateToOllama(chunk, "gemma-4", nil)
	if got == nil {
		t.Fatal("finish-only should still emit done marker")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(got[:len(got)-1], &obj); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, got)
	}
	if obj["done"] != true {
		t.Errorf("expected done=true: %s", got)
	}
}

func TestTranslateOllamaChatToOpenAI_StreamFlag(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out, err := translateOllamaChatToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Errorf("expected stream=true, got %v", obj["stream"])
	}
}

func TestTranslateOllamaChatToOpenAI_DefaultStream(t *testing.T) {
	body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
	out, _ := translateOllamaChatToOpenAI(body)
	var obj map[string]interface{}
	_ = json.Unmarshal(out, &obj)
	// По умолчанию для chat stream=true (Ollama default)
	if obj["stream"] != true {
		t.Errorf("expected default stream=true, got %v", obj["stream"])
	}
}

func TestStripStreamFlag(t *testing.T) {
	body := []byte(`{"model":"x","stream":true}`)
	out := stripStreamFlag(body)
	var obj map[string]interface{}
	_ = json.Unmarshal(out, &obj)
	if obj["stream"] != false {
		t.Errorf("expected stream=false after strip, got %v", obj["stream"])
	}
}

func TestTranslatePathForLlamaCpp(t *testing.T) {
	cases := map[string]string{
		"/api/chat":       "/v1/chat/completions",
		"/api/generate":   "/v1/completions",
		"/api/embeddings": "/v1/embeddings",
		"/api/tags":       "/api/tags", // pass-through
	}
	for in, want := range cases {
		got := translatePathForLlamaCpp(in)
		if got != want {
			t.Errorf("translatePathForLlamaCpp(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsStreamingFromBody(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"stream":true}`, true},
		{`{"stream":false}`, false},
		{`{}`, false},
		{``, false},
		{`not json`, false},
	}
	for _, c := range cases {
		got := isStreamingFromBody("/api/chat", []byte(c.body))
		if got != c.want {
			t.Errorf("isStreamingFromBody(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}

// TestTranslateSSEChatToOllama_DoneHasModel — регрессионный тест для бага
// «пустой второй ответ в OpenWebUI»: финальный done-чанк, который отдаёт
// балансер когда cppworker прислал только `data: [DONE]` без отдельного
// finish_reason-чанка, ОБЯЗАН содержать `model` и `message.role` — иначе
// OpenWebUI на втором запросе в той же сессии сбрасывает ассемблирование
// контента и рендерит пустое сообщение.
func TestTranslateSSEChatToOllama_DoneHasModel(t *testing.T) {
	// Чанк с явным finish_reason: stop и без content (как присылает OpenAI
	// при корректном завершении).
	chunk := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"delta":         map[string]interface{}{},
				"finish_reason": "stop",
			},
		},
	}
	got := translateSSEChatToOllama(chunk, "gemma-4-E4B-it-Q4_K_M", nil)
	if got == nil {
		t.Fatal("finish-only chunk must NOT return nil (done-marker required)")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(got[:len(got)-1], &obj); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, got)
	}
	if obj["done"] != true {
		t.Errorf("expected done=true, got: %s", got)
	}
	if obj["model"] != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("expected model field preserved, got: %s", got)
	}
	msg, ok := obj["message"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected message object in done-chunk, got: %s", got)
	}
	if msg["role"] != "assistant" {
		t.Errorf("expected message.role=assistant, got: %v", msg["role"])
	}
}

// TestTranslateSSEGenerateToOllama_DoneHasModel — то же для /api/generate.
func TestTranslateSSEGenerateToOllama_DoneHasModel(t *testing.T) {
	chunk := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"finish_reason": "stop",
			},
		},
	}
	got := translateSSEGenerateToOllama(chunk, "gemma-4-E4B-it-Q4_K_M", nil)
	if got == nil {
		t.Fatal("finish-only chunk must NOT return nil")
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(got[:len(got)-1], &obj); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, got)
	}
	if obj["done"] != true {
		t.Errorf("expected done=true, got: %s", got)
	}
	if obj["model"] != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("expected model field preserved, got: %s", got)
	}
	if _, ok := obj["response"]; !ok {
		t.Errorf("expected response field (even if empty) in done-chunk, got: %s", got)
	}
}

// TestTranslateSSEChatToOllama_FullSequenceDoneHasModel —
// полный сценарий: chat со множеством токенов и финальным done.
// Проверяем, что ВСЕ чанки содержат `model` — раньше первый done-чанк
// иногда терял `model`, и это ломало OpenWebUI на 2-м запросе.
func TestTranslateSSEChatToOllama_FullSequenceDoneHasModel(t *testing.T) {
	chunks := []map[string]interface{}{
		{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"role": "assistant"}}}},
		{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"content": "Привет"}}}},
		{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"content": " мир"}}}},
		{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{}, "finish_reason": "stop"}}},
	}
	for i, c := range chunks {
		b := translateSSEChatToOllama(c, "test-model", nil)
		if b == nil {
			t.Fatalf("chunk %d: returned nil (non-content chunks must be skipped, but role-only and done MUST be emitted)", i)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(b[:len(b)-1], &obj); err != nil {
			t.Fatalf("chunk %d: invalid JSON: %v: %s", i, err, b)
		}
		if obj["model"] != "test-model" {
			t.Errorf("chunk %d: missing model field, got: %s", i, b)
		}
	}
}

// TestTranslateSSEChatToOllama_FilterServiceTokens — регрессионный тест
// для фильтрации служебных токенов модели (Gemma <end_of_turn>, Llama3
// <|eot_id|>, ChatML <|im_end|> и т.д.) в Ollama NDJSON-пути.
//
// Без фильтрации ollama-js (используется в Cline) получает «мусорный»
// content в message.content и падает с ошибкой "Invalid API Response".
// Тест проверяет, что чанки со служебными токенами заменяются на
// валидный Ollama NDJSON-чанк с пустым content.
func TestTranslateSSEChatToOllama_FilterServiceTokens(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"gemma_end_of_turn", "<end_of_turn>"},
		{"gemma_end_of_turn_whitespace", "  <end_of_turn>  "},
		{"gemma_start_of_turn", "<start_of_turn>"},
		{"llama_eot_id", "<|eot_id|>"},
		{"llama_im_end", "<|im_end|>"},
		{"mixed_with_text", "hello<end_of_turn>world"},
		{"gemma_prefix", "<end_of_turn>hello"},
		{"gemma_suffix", "hello<end_of_turn>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunk := map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"delta": map[string]interface{}{
							"content": tc.content,
						},
					},
				},
			}
			got := translateSSEChatToOllama(chunk, "gemma-3-4b-it", nil)
			if got == nil {
				t.Fatalf("expected non-nil result for filtered service token")
			}
			var obj map[string]interface{}
			if err := json.Unmarshal(got[:len(got)-1], &obj); err != nil {
				t.Fatalf("invalid JSON: %v: %s", err, got)
			}
			msg, ok := obj["message"].(map[string]interface{})
			if !ok {
				t.Fatalf("missing or invalid message field: %s", got)
			}
			content, _ := msg["content"].(string)
			if content != "" {
				t.Errorf("expected empty content for service token, got %q", content)
			}
			if obj["done"] != false {
				t.Errorf("expected done=false for filtered content chunk, got %v", obj["done"])
			}
		})
	}
}

// TestTranslateSSEChatToOllama_PreservesValidContent — контр-тест:
// нормальный (не служебный) content должен проходить БЕЗ фильтрации.
func TestTranslateSSEChatToOllama_PreservesValidContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"plain_text", "Hello, world!"},
		{"german", "Hallo Welt"},
		{"russian", "Привет, мир"},
		{"code_block", "```go\nfunc main() {}\n```"},
		{"with_angle_bracket", "use <stdio.h> for C"},
		{"eot_in_word", "footnote"}, // содержит "eot", но не "<|eot_id|>"
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunk := map[string]interface{}{
				"choices": []interface{}{
					map[string]interface{}{
						"delta": map[string]interface{}{
							"content": tc.content,
						},
					},
				},
			}
			got := translateSSEChatToOllama(chunk, "gemma-3-4b-it", nil)
			if got == nil {
				t.Fatalf("expected non-nil result for valid content")
			}
			var obj map[string]interface{}
			if err := json.Unmarshal(got[:len(got)-1], &obj); err != nil {
				t.Fatalf("invalid JSON: %v: %s", err, got)
			}
			msg, ok := obj["message"].(map[string]interface{})
			if !ok {
				t.Fatalf("missing or invalid message field")
			}
			content, _ := msg["content"].(string)
			if content != tc.content {
				t.Errorf("content was filtered/changed: want %q, got %q", tc.content, content)
			}
		})
	}
}


