package balancer

import (
	"encoding/json"
	"strings"
)

// normalizeOpenAIMessages преобразует OpenAI Chat Completions запрос в форму,
// которую принимает llama.cpp cppworker (строго типизированная структура с
// `messages[].content` типа string).
//
// Зачем: современные OpenAI-клиенты (Cline, Roo Code, Continue.dev, OpenWebUI)
// посылают multi-modal content в виде массива объектов:
//
//	"content": [
//	  {"type": "text", "text": "Привет"},
//	  {"type": "image_url", "image_url": {"url": "data:..."}}
//	]
//
// cppworker парсит это в C-структуру `openai_chat_message` с `char *content`
// (строка) и падает с ошибкой:
//
//	json: cannot unmarshal array into Go struct field
//	openAIChatMessage.messages.content of type string
//
// Эта функция собирает все text-сегменты в одну строку (через "\n") и
// отбрасывает неподдерживаемые части (image_url и т.п.) — llama.cpp в
// большинстве моделей всё равно не умеет vision.
//
// Нормализация идемпотентна: если content уже строка, ничего не меняется.
// Невалидные структуры не приводят к ошибке (best-effort).
//
// Поддерживаются все типы сообщений: system, user, assistant, tool, function,
// developer — везде, где есть поле content.
func normalizeOpenAIMessages(req map[string]interface{}) {
	rawMsgs, ok := req["messages"].([]interface{})
	if !ok {
		return
	}
	for i, rawM := range rawMsgs {
		msgMap, ok := rawM.(map[string]interface{})
		if !ok {
			continue
		}
		// R248/B3: картинки НЕ выбрасываем. Раньше image_url молча удалялся
		// (llama.cpp text-only), и vision-модель получала запрос без картинки —
		// внешне успешный, но не основанный на изображении ответ. Теперь
		// image-части поднимаются в нативное для cppworker поле `images`
		// (base64 без data:-префикса), а text-части остаются в content.
		// Отбивать картинки по-прежнему решает visionShouldReject ДО этого
		// шага: для модели без mmproj запрос уходит клиенту как 415.
		if images := collectImageParts(msgMap["content"]); len(images) > 0 {
			msgMap["images"] = images
		}
		msgMap["content"] = normalizeContentField(msgMap["content"])
		rawMsgs[i] = msgMap
	}
	req["messages"] = rawMsgs
}

// collectImageParts — R248/B3: base64-полезные нагрузки картинок из
// multi-modal content-массива. Поддерживаются формы, которые реально шлют
// клиенты:
//
//	{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}
//	{"type":"image_url","image_url":{"url":"https://..."}}   → http(s) НЕ качаем
//	{"type":"image","source":{"data":"AAA","media_type":"image/png"}} (Anthropic)
//
// Возвращает только inline-base64: ссылки требуют сетевого доступа из
// балансера и политики кэша, поэтому остаются неподдержанными — такой запрос
// должен получить честный отказ, а не «пустую картинку».
func collectImageParts(content interface{}) []string {
	parts, ok := content.([]interface{})
	if !ok {
		return nil
	}
	var images []string
	for _, raw := range parts {
		part, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		ptype, _ := part["type"].(string)
		switch ptype {
		case "image_url":
			if iu, ok := part["image_url"].(map[string]interface{}); ok {
				if url, ok := iu["url"].(string); ok {
					if payload, ok := base64ImagePayload(url); ok {
						images = append(images, payload)
					}
				}
			}
		case "image":
			if src, ok := part["source"].(map[string]interface{}); ok {
				if data, ok := src["data"].(string); ok && data != "" {
					images = append(images, data)
				}
			}
		}
	}
	return images
}

// base64ImagePayload — вытаскивает base64 из data-URL:
// `data:image/png;base64,AAA` → `AAA`. Строка base64 без префикса тоже
// принимается (часть клиентов шлёт её так). http(s)-ссылки не поддержаны.
func base64ImagePayload(url string) (string, bool) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", false
	}
	if strings.HasPrefix(url, "data:") {
		comma := strings.Index(url, ",")
		if comma < 0 || comma == len(url)-1 {
			return "", false
		}
		header := url[len("data:"):comma]
		if !strings.Contains(header, "base64") {
			return "", false
		}
		return url[comma+1:], true
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return "", false
	}
	return url, true
}

// normalizeContentField возвращает строковое представление content:
//   - если уже string — возвращает as-is
//   - если []interface{} (multi-modal) — собирает text-сегменты через "\n"
//   - иначе — сериализует в JSON-строку как fallback
//
// Не возвращает ошибку: при любых неожиданных структурах возвращает
// безопасное строковое представление (пустую строку, если совсем ничего
// не удалось извлечь), чтобы upstream получил валидный запрос.
func normalizeContentField(content interface{}) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []interface{}:
		return collectTextParts(v)
	default:
		// Неизвестный формат (число, bool, map). Сериализуем в JSON-строку,
		// чтобы upstream получил что-то осмысленное и не упал на типе.
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// collectTextParts собирает все text-сегменты multi-modal content-массива в
// одну строку, разделяя их переносом строки. Неподдерживаемые типы частей
// (image_url, image, audio, file и т.п.) игнорируются.
func collectTextParts(parts []interface{}) string {
	var b strings.Builder
	for _, raw := range parts {
		part, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		// Стандартный OpenAI формат: {"type": "text", "text": "..."}
		ptype, _ := part["type"].(string)
		if ptype == "" || ptype == "text" {
			if text, ok := part["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(text)
			}
			continue
		}
		// image_url / image / audio / file — игнорируем (llama.cpp text-only).
		// Можно расширить логикой суммаризации, но текущий scope — не падать.
	}
	return b.String()
}

// normalizeOpenAIBody читает JSON-тело, нормализует messages[].content (если
// они есть) и возвращает обновлённый JSON-байтовый буфер.
//
// Если body пуст или не JSON, возвращает его as-is. Если в теле нет поля
// `messages` или оно не массив — возвращает исходный body без изменений.
//
// Используется в handleOpenAIChatCompletions перед проксированием в cppworker.
func normalizeOpenAIBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		// Не JSON — отдаём upstream как есть, он сам разберётся.
		return body
	}
	if _, ok := req["messages"]; !ok {
		return body
	}
	normalizeOpenAIMessages(req)
	normalized, err := json.Marshal(req)
	if err != nil {
		// Не удалось сериализовать обратно — лучше отдать оригинал, чем 500.
		return body
	}
	return normalized
}
