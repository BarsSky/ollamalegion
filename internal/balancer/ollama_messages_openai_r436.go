// ollama_messages_openai_r436.go — R436 (2026-11-05): история сообщений
// Ollama → OpenAI приводится к ФОРМЕ, которую требует OpenAI-поверхность.
//
// ЖИВОЕ УЗКОЕ МЕСТО (путь «Ollama-клиент → балансер → бэкенд llama.cpp,
// говорящий по OpenAI API»). Перевод запроса
// (`translateOllamaChatToOpenAI`) пробрасывал `messages` КАК ЕСТЬ, а формы у
// протоколов разные:
//
//	Ollama:  "tool_calls":[{"function":{"name":"read_files",
//	                                   "arguments":{"files":["a"]}}}]   ← ОБЪЕКТ
//	OpenAI:  "tool_calls":[{"function":{"name":"read_files",
//	                                   "arguments":"{\"files\":[\"a\"]}"}}] ← СТРОКА
//
// Следствие для агентского цикла: бэкенд парсит tool_calls истории строго по
// OpenAI-схеме, аргумент-объект он не принимает — и вызов из истории либо
// теряется, либо уезжает в шаблон мусором. Модель на следующем ходу видит
// «вызов без аргументов» и начинает выкручиваться: повторяет вызов, пишет
// содержимое файла текстом, придумывает формат. Ровно тот класс жалоб, который
// мы разбирали в R428–R435 (там причина была в нашем собственном рендере
// инструментов, здесь — в конвертере между протоколами).
//
// Что делаем (и чего НЕ делаем):
//   - `function.arguments` объекта → JSON-строка (OpenAI-контракт); если уже
//     строка — не трогаем (не переэкрaнируем, иначе двойное кодирование);
//   - `role:"tool"`: сохраняем `tool_call_id`, если клиент его прислал; если
//     прислал только `tool_name` — оставляем как есть (расширение Ollama,
//     llama.cpp его понимает);
//   - идентификаторы вызовов (`id`) сохраняем: по ним клиент сопоставляет
//     результат инструмента с вызовом;
//   - НИЧЕГО не придумываем: нет id — не генерируем его здесь (это делает
//     cppworker на своей стороне, где известен весь ответ модели).
package balancer

import (
	"encoding/json"
	"strings"
)

// convertMessagesToOpenAIForm — привести срез сообщений к OpenAI-форме.
//
// Возвращает новый срез: исходный не меняется (он может использоваться дальше —
// например, для логирования запроса).
func convertMessagesToOpenAIForm(messages []interface{}) []interface{} {
	if len(messages) == 0 {
		return messages
	}
	out := make([]interface{}, len(messages))
	for i, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			out[i] = raw
			continue
		}
		out[i] = convertMessageToOpenAIForm(msg)
	}
	return out
}

// convertMessageToOpenAIForm — одно сообщение.
func convertMessageToOpenAIForm(msg map[string]interface{}) map[string]interface{} {
	calls, ok := msg["tool_calls"].([]interface{})
	if !ok || len(calls) == 0 {
		return msg
	}
	converted := make([]interface{}, 0, len(calls))
	changed := false
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]interface{})
		if !ok {
			converted = append(converted, rawCall)
			continue
		}
		fn, ok := call["function"].(map[string]interface{})
		if !ok {
			converted = append(converted, call)
			continue
		}
		args := fn["arguments"]
		if s, isString := args.(string); isString {
			// Уже строка (OpenAI-форма) — не переэкрaнируем.
			_ = s
			converted = append(converted, call)
			continue
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			converted = append(converted, call)
			continue
		}
		fnCopy := make(map[string]interface{}, len(fn))
		for k, v := range fn {
			fnCopy[k] = v
		}
		fnCopy["arguments"] = string(encoded)
		callCopy := make(map[string]interface{}, len(call))
		for k, v := range call {
			callCopy[k] = v
		}
		callCopy["function"] = fnCopy
		converted = append(converted, callCopy)
		changed = true
	}
	if !changed {
		return msg
	}
	msgCopy := make(map[string]interface{}, len(msg))
	for k, v := range msg {
		msgCopy[k] = v
	}
	msgCopy["tool_calls"] = converted
	return msgCopy
}

// toolCallArgumentsAreString — вспомогательное: аргументы вызова уже строка
// (нужно тестам и диагностике).
func toolCallArgumentsAreString(messages []interface{}) bool {
	for _, raw := range messages {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		calls, ok := msg["tool_calls"].([]interface{})
		if !ok {
			continue
		}
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]interface{})
			if !ok {
				continue
			}
			fn, ok := call["function"].(map[string]interface{})
			if !ok {
				continue
			}
			if _, isString := fn["arguments"].(string); !isString && fn["arguments"] != nil {
				return false
			}
		}
	}
	return true
}

// messageHasToolNameOnly — у tool-сообщения нет tool_call_id, но есть имя
// инструмента (Ollama-расширение). Нужно, чтобы не потерять связь результата с
// вызовом при переводе.
func messageHasToolNameOnly(msg map[string]interface{}) bool {
	role, _ := msg["role"].(string)
	if !strings.EqualFold(role, "tool") {
		return false
	}
	id, _ := msg["tool_call_id"].(string)
	name, _ := msg["tool_name"].(string)
	return strings.TrimSpace(id) == "" && strings.TrimSpace(name) != ""
}
