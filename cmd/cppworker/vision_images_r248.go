// vision_images_r248.go — R248/B3: приём картинок в cppworker.
//
// ЗАЧЕМ. Балансер поднимает image-части в поле messages[].images (base64), а
// мост умеет префилл с изображениями (bridge_infer_images через libmtmd).
// Не хватало ровно середины: провалидировать payload'ы, вставить в промпт
// медиа-маркер проектора на место каждой картинки и вызвать image-инференс.
//
// ГРАНИЦЫ:
//   - поддерживается inline-base64 (png/jpg/webp — декодирует libmtmd/stb);
//     http(s)-ссылки не качаем (нет сетевой политики и кэша);
//   - только не-стриминговый путь: префилл с изображениями синхронный,
//     поэтому stream-запрос с картинками честно отбивается 400, а не
//     превращается в «пустой поток»;
//   - лимиты: 8 картинок на запрос, 8 МиБ на картинку.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"ollama-loadbalancer/c/bridge"
)

// base64DecodeTolerant принимает и стандартный padded base64, и raw-вариант
// без '=' (часть клиентов обрезает padding).
func base64DecodeTolerant(s string) ([]byte, error) {
	if data, err := base64.StdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

const (
	// visionMaxImages — предел на один запрос (совпадает с лимитом моста).
	visionMaxImages = 8
	// visionMaxImageBytes — предел на одну картинку после декодирования.
	visionMaxImageBytes = 8 << 20
)

// decodeVisionImages превращает base64-payload'ы в байты файлов.
// Принимает и `data:image/png;base64,AAA`, и голый base64 (клиенты шлют
// по-разному), допускает переносы строк внутри base64.
func decodeVisionImages(payloads []string) ([][]byte, error) {
	if len(payloads) == 0 {
		return nil, nil
	}
	if len(payloads) > visionMaxImages {
		return nil, fmt.Errorf("too_many_images: %d > %d", len(payloads), visionMaxImages)
	}
	out := make([][]byte, 0, len(payloads))
	for i, p := range payloads {
		raw := strings.TrimSpace(p)
		if raw == "" {
			return nil, fmt.Errorf("image_decode_failed: image %d is empty", i)
		}
		if strings.HasPrefix(raw, "data:") {
			comma := strings.Index(raw, ",")
			if comma < 0 || comma == len(raw)-1 {
				return nil, fmt.Errorf("image_decode_failed: image %d has malformed data URL", i)
			}
			header := raw[:comma]
			if !strings.Contains(header, "base64") {
				return nil, fmt.Errorf("image_unsupported_type: image %d: %s", i, header)
			}
			raw = raw[comma+1:]
		}
		// Переносы строк/пробелы внутри base64 — норма для «pretty» клиентов.
		raw = strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(raw)

		data, err := base64DecodeTolerant(raw)
		if err != nil {
			return nil, fmt.Errorf("image_decode_failed: image %d: %v", i, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("image_decode_failed: image %d decoded to 0 bytes", i)
		}
		if len(data) > visionMaxImageBytes {
			return nil, fmt.Errorf("image_too_large: image %d: %d bytes > %d", i, len(data), visionMaxImageBytes)
		}
		out = append(out, data)
	}
	return out, nil
}

// chatImages собирает все base64-payload'ы картинок из сообщений чата
// (Ollama-нативная форма messages[].images; балансер кладёт картинки сюда).
func chatImages(msgs []chatMessage) []string {
	var payloads []string
	for _, m := range msgs {
		payloads = append(payloads, m.Images...)
	}
	return payloads
}

// injectMediaMarkers вставляет медиа-маркер проектора в текст каждого
// сообщения — по одному на картинку, в порядке сообщений. Порядок маркеров
// обязан совпадать с порядком images в bridge_infer_images.
func injectMediaMarkers(msgs []chatMessage, marker string) []chatMessage {
	if marker == "" {
		return msgs
	}
	out := make([]chatMessage, len(msgs))
	copy(out, msgs)
	for i := range out {
		if len(out[i].Images) == 0 {
			continue
		}
		// R248/B3 (живая проба 2026-10-27): маркер ставится ПЕРЕД текстом.
		//
		// Живой результат с маркером ПОСЛЕ текста («…вопрос?\n<__media__>»):
		// SmolVLM ответил «There is no word written in the image» на картинку
		// со словом OK. Для SmolVLM (и большинства VLM-процессоров) изображение
		// идёт в промпте ДО вопроса: маркер — это место картинки, а не
		// приложение к тексту. Порядок «сначала media, затем текст» совпадает с
		// тем, как эти модели обучались.
		var b strings.Builder
		for range out[i].Images {
			b.WriteString(marker)
			b.WriteString("\n")
		}
		b.WriteString(out[i].Content)
		out[i].Content = b.String()
	}
	return out
}

// writeVisionError переводит причину отказа vision в HTTP-статус.
//
// 415 — модель без проектора (возможность не опубликована), 400 — картинка
// пришла, но непригодна (битый base64, слишком много, неподдержанный формат)
// или клиент требует stream, который для картинок не поддержан.
func writeVisionError(w http.ResponseWriter, model string, err error) {
	msg := err.Error()
	status := http.StatusBadRequest
	switch {
	case strings.HasPrefix(msg, "vision_not_supported"):
		status = http.StatusUnsupportedMediaType
	case strings.HasPrefix(msg, "vision_stream_not_supported"):
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]interface{}{
		"error":            msg,
		"type":             "invalid_request_error",
		"model":            model,
		"vision_supported": status != http.StatusUnsupportedMediaType,
		"image_error_kind": firstToken(msg),
	})
}

// firstToken — префикс ошибки до двоеточия (машинно-читаемый kind:
// image_decode_failed, too_many_images, ...).
func firstToken(s string) string {
	if i := strings.Index(s, ":"); i > 0 {
		return s[:i]
	}
	return s
}

// generateBufferedOrImages — R248/B3: одна точка выбора инференса для
// /api/chat. Без картинок — прежний буферизированный путь (R248/A, отмена по
// ctx). С картинками — синхронный image-инференс через libmtmd: он не
// стриминговый, поэтому стриминговые запросы с картинками отбиваются выше.
func generateBufferedOrImages(ctx context.Context, modelName, prompt string, params bridge.GenerationParams, hasTools bool, images [][]byte) (*bridge.InferenceResult, error) {
	if len(images) == 0 {
		return generateBufferedWithCancel(ctx, modelName, prompt, params, hasTools)
	}
	return backend.GenerateWithImages(ctx, modelName, prompt, images, params)
}

// UnmarshalJSON — R248/B3 (2026-10-27): content принимается и строкой, и
// массивом частей (стандартный OpenAI/Anthropic multi-modal формат).
//
// ЖИВОЙ ДЕФЕКТ: `content` был объявлен как string, поэтому любой запрос с
// картинкой от реального клиента падал на строгом декодере:
//
//	400 invalid JSON: json: cannot unmarshal array into Go struct field
//	    openAIChatMessage.messages.content of type string
//
// Теперь text-части собираются в Content, а inline-base64 картинки
// (image_url / Anthropic image) — в Images, то есть ровно туда, откуда их
// берёт image-инференс. Порядок картинок сохраняется.
func (m *openAIChatMessage) UnmarshalJSON(data []byte) error {
	var wire struct {
		Role             string           `json:"role"`
		Content          json.RawMessage  `json:"content"`
		ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
		ToolCallID       string           `json:"tool_call_id,omitempty"`
		Name             string           `json:"name,omitempty"`
		Images           []string         `json:"images,omitempty"`
		ReasoningContent string           `json:"reasoning_content,omitempty"`
		Thinking         string           `json:"thinking,omitempty"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	m.Role = wire.Role
	m.ToolCalls = wire.ToolCalls
	m.ToolCallID = wire.ToolCallID
	m.Name = wire.Name
	m.Images = wire.Images
	m.ReasoningContent = wire.ReasoningContent
	m.Thinking = wire.Thinking

	if len(wire.Content) == 0 {
		return nil
	}
	// Строка — самый частый случай.
	var s string
	if err := json.Unmarshal(wire.Content, &s); err == nil {
		m.Content = s
		return nil
	}
	var parts []map[string]interface{}
	if err := json.Unmarshal(wire.Content, &parts); err != nil {
		// Не строка и не массив: оставляем пустой content, а не 400 —
		// спецификация допускает null/объект, и падать на этом хуже.
		return nil
	}
	m.Content, m.Images = partitionContentParts(parts, m.Images)
	return nil
}

// partitionContentParts делит multi-modal части на текст и inline-base64
// картинки. Внешние http(s)-ссылки пропускаются: балансер их не качает, и
// притворяться, что картинка есть, нельзя.
func partitionContentParts(parts []map[string]interface{}, existing []string) (string, []string) {
	var text strings.Builder
	images := append([]string{}, existing...)
	for _, part := range parts {
		ptype, _ := part["type"].(string)
		switch ptype {
		case "", "text":
			if t, ok := part["text"].(string); ok && t != "" {
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(t)
			}
		case "image_url":
			iu, _ := part["image_url"].(map[string]interface{})
			url, _ := iu["url"].(string)
			if payload, ok := base64ImagePayloadLoose(url); ok {
				images = append(images, payload)
			}
		case "image":
			src, _ := part["source"].(map[string]interface{})
			if data, ok := src["data"].(string); ok && data != "" {
				images = append(images, data)
			}
		}
	}
	return text.String(), images
}

// base64ImagePayloadLoose снимает data:-префикс с inline-base64 (полная
// валидация — в decodeVisionImages, здесь только отделение от ссылок).
func base64ImagePayloadLoose(url string) (string, bool) {
	raw := strings.TrimSpace(url)
	if raw == "" {
		return "", false
	}
	if strings.HasPrefix(raw, "data:") {
		comma := strings.Index(raw, ",")
		if comma < 0 || comma == len(raw)-1 {
			return "", false
		}
		return raw[comma+1:], true
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return "", false
	}
	return raw, true
}

// buildNaiveChatPromptFromMsgs — R248/B3: naive-fallback (GGUF без chat
// template) по уже собранным chatMessage. Нужен, чтобы маркеры картинок,
// вставленные в сообщения, не терялись при падении на fallback-путь:
// старый buildNaiveChatPrompt работал с openAIChatMessage, то есть с ДРУГИМ
// срезом, и картинка бы пропала ровно в этом сценарии.
func buildNaiveChatPromptFromMsgs(msgs []chatMessage, modelName string) string {
	isGemma := strings.Contains(strings.ToLower(modelName), "gemma")
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>system\n%s<end_of_turn>\n", m.Content)
			} else {
				fmt.Fprintf(&sb, "<|system|>\n%s\n", m.Content)
			}
		case "assistant":
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>model\n%s<end_of_turn>\n", m.Content)
			} else {
				fmt.Fprintf(&sb, "<|assistant|>\n%s\n", m.Content)
			}
		default: // user / tool
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>user\n%s<end_of_turn>\n", m.Content)
			} else {
				fmt.Fprintf(&sb, "<|user|>\n%s\n", m.Content)
			}
		}
	}
	if isGemma {
		sb.WriteString("<start_of_turn>model\n")
	} else {
		sb.WriteString("<|assistant|>\n")
	}
	return sb.String()
}
