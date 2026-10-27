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
		var b strings.Builder
		b.WriteString(out[i].Content)
		for range out[i].Images {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(marker)
		}
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
