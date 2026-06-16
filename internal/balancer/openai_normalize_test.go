package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeOpenAIMessages_StringContent_NoChange(t *testing.T) {
	req := map[string]interface{}{
		"model": "test-model",
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "Привет, как дела?",
			},
		},
	}
	normalizeOpenAIMessages(req)
	msgs := req["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].(map[string]interface{})["content"] != "Привет, как дела?" {
		t.Fatalf("content should be unchanged, got %v", msgs[0].(map[string]interface{})["content"])
	}
}

func TestNormalizeOpenAIMessages_MultiModalTextAndImage(t *testing.T) {
	req := map[string]interface{}{
		"model": "test-model",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": "Опиши картинку",
					},
					map[string]interface{}{
						"type":     "image_url",
						"image_url": map[string]interface{}{"url": "data:image/png;base64,..."},
					},
				},
			},
		},
	}
	normalizeOpenAIMessages(req)
	msgs := req["messages"].([]interface{})
	content := msgs[0].(map[string]interface{})["content"]
	if content != "Опиши картинку" {
		t.Fatalf("expected text-only content, got %v", content)
	}
}

func TestNormalizeOpenAIMessages_MultipleTextParts(t *testing.T) {
	req := map[string]interface{}{
		"model": "m",
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Часть 1"},
					map[string]interface{}{"type": "text", "text": "Часть 2"},
					map[string]interface{}{"type": "text", "text": "Часть 3"},
				},
			},
		},
	}
	normalizeOpenAIMessages(req)
	msgs := req["messages"].([]interface{})
	got := msgs[0].(map[string]interface{})["content"]
	want := "Часть 1\nЧасть 2\nЧасть 3"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestNormalizeOpenAIMessages_EmptyArray(t *testing.T) {
	req := map[string]interface{}{
		"messages": []interface{}{},
	}
	normalizeOpenAIMessages(req)
	if msgs, ok := req["messages"].([]interface{}); !ok || len(msgs) != 0 {
		t.Fatalf("expected empty messages slice, got %v", req["messages"])
	}
}

func TestNormalizeOpenAIMessages_NoMessagesField(t *testing.T) {
	req := map[string]interface{}{"model": "x"}
	normalizeOpenAIMessages(req)
	if _, ok := req["messages"]; ok {
		t.Fatalf("messages field should not be added")
	}
}

func TestNormalizeOpenAIMessages_SystemMessage(t *testing.T) {
	req := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "system",
				"content": "Ты полезный ассистент",
			},
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Привет"},
				},
			},
		},
	}
	normalizeOpenAIMessages(req)
	msgs := req["messages"].([]interface{})
	if msgs[0].(map[string]interface{})["content"] != "Ты полезный ассистент" {
		t.Fatalf("system content should be unchanged")
	}
	if msgs[1].(map[string]interface{})["content"] != "Привет" {
		t.Fatalf("user content should be normalized to string")
	}
}

func TestNormalizeOpenAIMessages_ToolMessage(t *testing.T) {
	req := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "tool",
				"content": "результат",
			},
		},
	}
	normalizeOpenAIMessages(req)
	msgs := req["messages"].([]interface{})
	if msgs[0].(map[string]interface{})["content"] != "результат" {
		t.Fatalf("tool content should be unchanged")
	}
}

func TestNormalizeOpenAIMessages_OnlyImage_EmptyString(t *testing.T) {
	req := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{
						"type":     "image_url",
						"image_url": map[string]interface{}{"url": "data:..."},
					},
				},
			},
		},
	}
	normalizeOpenAIMessages(req)
	msgs := req["messages"].([]interface{})
	content := msgs[0].(map[string]interface{})["content"]
	if content != "" {
		t.Fatalf("expected empty string for image-only content, got %v", content)
	}
}

func TestNormalizeOpenAIMessages_InvalidContentType_Number(t *testing.T) {
	req := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": 42,
			},
		},
	}
	normalizeOpenAIMessages(req)
	msgs := req["messages"].([]interface{})
	content, ok := msgs[0].(map[string]interface{})["content"].(string)
	if !ok {
		t.Fatalf("expected content to be converted to string, got %T", msgs[0].(map[string]interface{})["content"])
	}
	if content != "42" {
		t.Fatalf("expected JSON-serialized number '42', got %q", content)
	}
}

func TestNormalizeOpenAIMessages_NoContentField(t *testing.T) {
	req := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":        "assistant",
				"other_field": "value",
			},
		},
	}
	// Не должно паниковать при отсутствии поля content.
	// normalizeContentField(nil) возвращает "", и это безопасное поведение
	// (валидный OpenAI assistant-tool-вызов часто приходит без content).
	normalizeOpenAIMessages(req)
	msgs := req["messages"].([]interface{})
	if _, ok := msgs[0].(map[string]interface{})["other_field"]; !ok {
		t.Fatalf("existing other_field should be preserved")
	}
}

func TestNormalizeContentField_DirectCases(t *testing.T) {
	tests := []struct {
		name string
		in   interface{}
		want string
	}{
		{"nil", nil, ""},
		{"empty string", "", ""},
		{"plain string", "hello", "hello"},
		{"number", 42, "42"},
		{"bool", true, "true"},
		{"array of text", []interface{}{
			map[string]interface{}{"type": "text", "text": "a"},
			map[string]interface{}{"type": "text", "text": "b"},
		}, "a\nb"},
		{"empty array", []interface{}{}, ""},
		{"mixed array", []interface{}{
			map[string]interface{}{"type": "text", "text": "x"},
			map[string]interface{}{"type": "image_url", "image_url": nil},
		}, "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeContentField(tt.in)
			if got != tt.want {
				t.Fatalf("normalizeContentField(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeOpenAIBody_RoundTripJSON(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	out := normalizeOpenAIBody(body)
	// Проверяем, что JSON валидный и содержит нормализованный content
	var parsed map[string]interface{}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	msgs := parsed["messages"].([]interface{})
	content := msgs[0].(map[string]interface{})["content"]
	if content != "hi" {
		t.Fatalf("expected content to be 'hi', got %v", content)
	}
	// Также проверяем, что JSON-ключи сохранены (не потеряли "model")
	if parsed["model"] != "m" {
		t.Fatalf("model field should be preserved")
	}
}

func TestNormalizeOpenAIBody_NoMessages_AsIs(t *testing.T) {
	body := []byte(`{"model":"m","prompt":"raw"}`)
	out := normalizeOpenAIBody(body)
	if string(out) != string(body) {
		t.Fatalf("body without messages should be returned as-is, got %s", out)
	}
}

func TestNormalizeOpenAIBody_NotJSON_AsIs(t *testing.T) {
	body := []byte(`not a json at all`)
	out := normalizeOpenAIBody(body)
	if string(out) != string(body) {
		t.Fatalf("non-JSON body should be returned as-is, got %s", out)
	}
}

func TestNormalizeOpenAIBody_Empty_AsIs(t *testing.T) {
	out := normalizeOpenAIBody(nil)
	if out != nil {
		t.Fatalf("empty body should be returned as-is, got %v", out)
	}
	out = normalizeOpenAIBody([]byte{})
	if len(out) != 0 {
		t.Fatalf("empty body should be returned as-is, got %v", out)
	}
}

func TestNormalizeOpenAIBody_ClineRealistic(t *testing.T) {
	// Реалистичный payload, который шлёт Cline / Roo Code / OpenWebUI
	// при вызове с image+text.
	body := []byte(`{
		"model": "qwen",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."},
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Что на картинке?"},
					{"type": "image_url", "image_url": {"url": "https://example.com/a.png"}}
				]
			}
		],
		"stream": true,
		"max_tokens": 2048
	}`)
	out := normalizeOpenAIBody(body)
	if !strings.Contains(string(out), `"content":"Что на картинке?"`) {
		t.Fatalf("normalized body should contain text content as string. Body: %s", out)
	}
	if strings.Contains(string(out), `"image_url"`) {
		t.Fatalf("image_url should be stripped from body. Body: %s", out)
	}
	// Проверяем, что остальные поля сохранены
	if !strings.Contains(string(out), `"stream":true`) {
		t.Fatalf("stream field should be preserved. Body: %s", out)
	}
	if !strings.Contains(string(out), `"max_tokens":2048`) {
		t.Fatalf("max_tokens field should be preserved. Body: %s", out)
	}
	if !strings.Contains(string(out), `"You are a helpful assistant."`) {
		t.Fatalf("system message should be preserved. Body: %s", out)
	}
}