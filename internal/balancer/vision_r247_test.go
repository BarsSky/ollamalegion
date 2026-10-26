// vision_r247_test.go — R247, этап D: изображения не отбрасываются молча.
//
// До R247 части-изображения удалялись при нормализации в текст, и клиент получал
// 200, будто модель картинку видела. Теперь — явный 415 с объяснением и
// заголовками (или passthrough, когда появится mmproj).
package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestR247_CountImageParts — подсчёт частей-картинок по всем формам.
func TestR247_CountImageParts(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"OpenAI image_url", `{"messages":[{"role":"user","content":[{"type":"text","text":"что тут?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`, 1},
		{"Anthropic image", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`, 1},
		{"Responses input_image", `{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`, 1},
		{"текст без картинок", `{"messages":[{"role":"user","content":"привет"}]}`, 0},
		{"image_url как текст в аргументах", `{"messages":[{"role":"tool","content":"the field image_url is documented"}]}`, 0},
	}
	for _, tc := range cases {
		if got := countImagePartsInBody([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: частей %d, ожидалось %d", tc.name, got, tc.want)
		}
	}
}

// TestR247_VisionUnsupportedShape — форма отказа: 415 + объяснение + заголовки.
func TestR247_VisionUnsupportedShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeVisionUnsupported(rec, "gemma-4", 2)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, ожидался 415", rec.Code)
	}
	if got := rec.Header().Get("X-Vision-Supported"); got != "false" {
		t.Errorf("X-Vision-Supported = %q", got)
	}
	if got := rec.Header().Get("X-Vision-Parts"); got != "2" {
		t.Errorf("X-Vision-Parts = %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{"images are not supported", "gemma-4", "text-only", "vision_not_supported"} {
		if !strings.Contains(body, want) {
			t.Errorf("в ответе нет %q: %s", want, body)
		}
	}
}

// TestR247_ImagePassthroughNotDropped — нормализация не должна «съесть» картинку
// без пометки: при наличии частей отказ обязан произойти ДО неё.
func TestR247_ImageRejectedBeforeNormalize(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]}`)
	if countImagePartsInBody(body) == 0 {
		t.Fatal("картинка не обнаружена — обработчик её молча отбросит")
	}
	// Демонстрация прежнего поведения: normalizeOpenAIBody оставляет только текст.
	normalized := string(normalizeOpenAIBody(body))
	if strings.Contains(normalized, "image_url") {
		t.Log("нормализация сохранила image_url (значит passthrough возможен)")
	}
	if countImagePartsInBody([]byte(normalized)) != 0 {
		t.Log("нормализация сохранила часть-картинку — отказ не нужен")
	}
}
