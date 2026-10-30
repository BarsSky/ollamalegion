// openai_normalize_r338_test.go — R338 (2026-10-30): покрытие «непокрытого»
// пункта аудита маршрутов — normalizeOpenAIBody.
//
// Аудит (docs/plans/2026-10-30-route-map.md, §12 п.5) отметил: «normalizeOpenAIBody —
// что именно нормализует, не проверялось». Здесь это зафиксировано тестами, чтобы
// поведение не менялось молча.
//
// ЧТО ДЕЛАЕТ НОРМАЛИЗАЦИЯ (по коду openai_normalize.go):
//   - content-массив OpenAI (текстовые части) превращается в строку — cppworker
//     ждёт строку, а клиенты (Cline/OpenWebUI) шлют массив частей;
//   - части-картинки (image_url/data:) НЕ выбрасываются: они поднимаются в
//     нативное поле `images` (base64 без префикса data:), иначе vision-модель
//     получала запрос без картинки и отвечала «успешно», но не по изображению;
//   - тела без messages и не-JSON отдаются как есть (чужая ответственность).
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR338_NormalizeTextPartsToContent — массив текстовых частей → строка content.
func TestR338_NormalizeTextPartsToContent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"первая"},{"type":"text","text":"вторая"}]}]}`)

	out := normalizeOpenAIBody(body)
	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("результат не JSON: %v (%q)", err, string(out))
	}
	msgs := req["messages"].([]interface{})
	msg := msgs[0].(map[string]interface{})
	content, ok := msg["content"].(string)
	if !ok {
		t.Fatalf("content не строка: %T (%v)", msg["content"], msg["content"])
	}
	if !strings.Contains(content, "первая") || !strings.Contains(content, "вторая") {
		t.Errorf("текстовые части потеряны: %q", content)
	}
}

// TestR338_ImagesLiftedNotDropped — картинка поднимается в images, content остаётся текстом.
func TestR338_ImagesLiftedNotDropped(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"что на картинке?"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)

	out := normalizeOpenAIBody(body)
	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	msg := req["messages"].([]interface{})[0].(map[string]interface{})

	images, ok := msg["images"].([]interface{})
	if !ok || len(images) != 1 {
		t.Fatalf("images = %v, want массив из одного элемента (картинка не должна выбрасываться)", msg["images"])
	}
	img, _ := images[0].(string)
	if strings.HasPrefix(img, "data:") {
		t.Errorf("images[0] начинается с data:-префикса: %q (cppworker ждёт чистый base64)", img)
	}
	if !strings.Contains(img, "AAAA") {
		t.Errorf("base64-полезная нагрузка потеряна: %q", img)
	}
	// Текст вопроса обязан остаться в content.
	content, _ := msg["content"].(string)
	if !strings.Contains(content, "что на картинке") {
		t.Errorf("текст вопроса потерян: %q", content)
	}
}

// TestR338_NonJSONAndNonChatBodiesUntouched — чужие тела не трогаем.
func TestR338_NonJSONAndNonChatBodiesUntouched(t *testing.T) {
	cases := [][]byte{
		[]byte(`not json at all`),
		[]byte(`{"model":"m","prompt":"без messages"}`), // completions, не chat
		{},
	}
	for _, body := range cases {
		out := normalizeOpenAIBody(body)
		if string(out) != string(body) {
			t.Errorf("тело изменено без причины:\n got %q\nwant %q", string(out), string(body))
		}
	}
}

// TestR338_StringContentStaysString — обычная строка content не превращается в массив.
func TestR338_StringContentStaysString(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"просто текст"}]}`)
	out := normalizeOpenAIBody(body)
	var req map[string]interface{}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	msg := req["messages"].([]interface{})[0].(map[string]interface{})
	if content, ok := msg["content"].(string); !ok || content != "просто текст" {
		t.Errorf("content = %v (%T), want строка «просто текст»", msg["content"], msg["content"])
	}
}
