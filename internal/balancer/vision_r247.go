// vision_r247.go — R247, этап D плана
// (docs/plans/2026-10-26-clients-agents-contract-r247.md).
//
// ЗАЧЕМ. До R247 изображения из `content[]` (OpenAI `image_url`, Anthropic `image`,
// Responses `input_image`) МОЛЧА отбрасывались при нормализации в текст: клиент
// получал 200 и ответ, будто модель картинку видела. Это самый дорогой класс
// дефекта — тихая ложь: пользователь думает, что модель посмотрела на скриншот.
//
// РЕШЕНИЕ. Слой больше не притворяется: если в запросе есть изображения, а
// backend их не умеет (C-мост сейчас текстовый, mmproj не подключён), клиент
// получает HTTP 415 с объяснением и заголовком `X-Vision-Supported: false`.
// Когда vision появится (mmproj), здесь же будет ветка passthrough — контракт
// ответа уже зафиксирован тестами.
package balancer

import (
	"encoding/json"
	"net/http"
	"strings"
)

// visionPartTypes — типы частей контента, означающие «картинка».
var visionPartTypes = map[string]bool{
	"image_url":    true, // OpenAI chat/completions
	"image":        true, // Anthropic content blocks
	"input_image":  true, // OpenAI Responses items
	"image_base64": true,
}

// countImageParts — сколько частей-изображений в теле запроса (рекурсивно).
// Считаем по JSON, а не по подстрокам: тело может содержать `image_url` в
// аргументах инструмента как текст.
func countImageParts(v interface{}) int {
	switch node := v.(type) {
	case map[string]interface{}:
		count := 0
		if t, ok := node["type"].(string); ok && visionPartTypes[t] {
			count++
		}
		for _, child := range node {
			count += countImageParts(child)
		}
		return count
	case []interface{}:
		count := 0
		for _, child := range node {
			count += countImageParts(child)
		}
		return count
	}
	return 0
}

// countImagePartsInBody — countImageParts для тела запроса.
func countImagePartsInBody(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	var doc interface{}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0
	}
	return countImageParts(doc)
}

// writeVisionUnsupported — честный отказ вместо молчаливого отбрасывания.
func writeVisionUnsupported(w http.ResponseWriter, model string, images int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Vision-Supported", "false")
	w.Header().Set("X-Vision-Parts", itoaR247(images))
	w.WriteHeader(http.StatusUnsupportedMediaType)
	message := "images are not supported by this backend: model \"" + model + "\" is served text-only " +
		"(vision/mmproj is not wired in cppworker). Remove image parts or use a vision-capable route."
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message":     message,
			"type":        "invalid_request_error",
			"code":        "vision_not_supported",
			"model":       model,
			"image_parts": images,
		},
	})
}

// itoaR247 — маленький помощник (без strconv, чтобы не плодить импорты).
func itoaR247(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// anthropicVisionMessage — текст ошибки для Anthropic-поверхности.
func anthropicVisionMessage(model string) string {
	return strings.TrimSpace("images are not supported by this backend: model \"" + model + "\" is served text-only")
}
