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
		msgMap["content"] = normalizeContentField(msgMap["content"])
		rawMsgs[i] = msgMap
	}
	req["messages"] = rawMsgs
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