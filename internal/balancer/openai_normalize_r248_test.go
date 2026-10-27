package balancer

import (
	"encoding/json"
	"testing"
)

// R248/B3: картинка из OpenAI content-массива обязана ДОЕХАТЬ до cppworker в
// поле `images` (base64), а текст — остаться в content. До правки image_url
// молча выбрасывался, и vision-модель отвечала по одному тексту.
func TestR248_NormalizeLiftsInlineImage(t *testing.T) {
	body := []byte(`{"model":"smolvlm","messages":[{"role":"user","content":[
		{"type":"text","text":"What is in the image?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}
	]}]}`)
	out := normalizeOpenAIBody(body)

	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("нормализованное тело не JSON: %v", err)
	}
	msgs := req["messages"].([]interface{})
	msg := msgs[0].(map[string]interface{})

	if got := msg["content"]; got != "What is in the image?" {
		t.Fatalf("content = %v, ожидался только текст", got)
	}
	images, _ := msg["images"].([]interface{})
	if len(images) != 1 || images[0] != "iVBORw0KGgo=" {
		t.Fatalf("images = %v, ожидался base64 без data:-префикса", msg["images"])
	}
}

// R248/B3: Anthropic-форма (image/ source.data) тоже поднимается в images.
func TestR248_NormalizeLiftsAnthropicImage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}},
		{"type":"text","text":"опиши"}
	]}]}`)
	out := normalizeOpenAIBody(body)

	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("не JSON: %v", err)
	}
	msg := req["messages"].([]interface{})[0].(map[string]interface{})
	images, _ := msg["images"].([]interface{})
	if len(images) != 1 || images[0] != "QUJD" {
		t.Fatalf("images = %v, ожидался QUJD", msg["images"])
	}
}

// R248/B3: http(s)-ссылка не качается балансером — картинка не поднимается
// (запрос должен получить честный отказ, а не «пустую картинку»).
func TestR248_NormalizeDoesNotFetchRemoteImage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[
		{"type":"text","text":"hi"},
		{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}
	]}]}`)
	out := normalizeOpenAIBody(body)

	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("не JSON: %v", err)
	}
	msg := req["messages"].([]interface{})[0].(map[string]interface{})
	if _, ok := msg["images"]; ok {
		t.Fatalf("для http(s)-ссылки images не должно появляться: %v", msg["images"])
	}
	if msg["content"] != "hi" {
		t.Fatalf("content = %v, ожидался текст", msg["content"])
	}
}

// R248/B3: текстовые запросы не меняются (нет поля images, content как был).
func TestR248_NormalizePlainTextUnchanged(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"просто текст"}]}`)
	out := normalizeOpenAIBody(body)

	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("не JSON: %v", err)
	}
	msg := req["messages"].([]interface{})[0].(map[string]interface{})
	if msg["content"] != "просто текст" {
		t.Fatalf("content = %v", msg["content"])
	}
	if _, ok := msg["images"]; ok {
		t.Fatalf("для текстового запроса images не должно появляться")
	}
}

// R248/B3: data-URL без base64 (например svg-текст) не считается картинкой.
func TestR248_Base64ImagePayloadRejectsNonBase64DataURL(t *testing.T) {
	if _, ok := base64ImagePayload("data:image/svg+xml,<svg/>"); ok {
		t.Fatal("data-URL без base64 не должен приниматься как картинка")
	}
	if _, ok := base64ImagePayload("data:image/png;base64,"); ok {
		t.Fatal("data-URL без полезной нагрузки не должен приниматься")
	}
	if p, ok := base64ImagePayload("data:image/png;base64,AAA"); !ok || p != "AAA" {
		t.Fatalf("payload = %q ok=%v, ожидалось AAA", p, ok)
	}
}
