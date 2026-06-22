// llamacpp_content_filter.go — Content filtering for streaming responses from llama.cpp/cppworker.
// Удаляет служебные токены модели (Gemma <end_of_turn>, Llama3 <|eot_id|>, ChatML <|im_end|> и т.д.)
// из streaming-ответа, чтобы OpenAI-клиенты (Cline, OpenWebUI) не падали с "Invalid API Response".
package balancer

import (
	"bytes"
	"encoding/json"
	"strings"
)

// cleanContentAfterToolCallExtraction — финальная очистка content после извлечения tool_calls.
// Удаляет:
//   - дубликаты JSON tool calls (через рекурсивный вызов detectAndExtractToolCallsFromContent)
//   - role-маркеры ("system", "assistant"), оставшиеся после удаления XML-токенов
//   - лишние пробелы, символы новой строки
// Возвращает пустую строку, если контента не осталось (желаемое поведение для tool calls).
func cleanContentAfterToolCallExtraction(s string) string {
	result := strings.TrimSpace(s)
	if result == "" {
		return ""
	}

	// Пока есть дубликаты JSON tool calls — извлекаем их и продолжаем с остатком
	for {
		_, remainingTC, found := detectAndExtractToolCallsFromContent(result)
		if !found {
			break
		}
		result = strings.TrimSpace(remainingTC)
		if result == "" {
			return ""
		}
	}

	// Удаляем role-маркеры, оставшиеся после удаления XML-токенов Gemma
	// (например: <start_of_turn>system\n[JSON] → после strip → "system\n[JSON]")
	roleMarkers := []string{
		"system",
		"assistant",
		"user",
		"model",
	}
	for _, marker := range roleMarkers {
		// Удаляем маркер как целое слово (окружённое пробелами или границами строки)
		for {
			trimmed := strings.TrimSpace(result)
			if trimmed == "" {
				return ""
			}
			// Проверяем, начинается ли строка с role-маркера
			if strings.HasPrefix(trimmed, marker) {
				afterMarker := strings.TrimSpace(trimmed[len(marker):])
				// Если после маркера только пробелы — заменяем на пустоту
				result = afterMarker
				continue
			}
			// Проверяем, заканчивается ли строка на role-маркер
			if strings.HasSuffix(trimmed, marker) {
				beforeMarker := strings.TrimSpace(trimmed[:len(trimmed)-len(marker)])
				result = beforeMarker
				continue
			}
			break
		}
	}

	// Если остались только пустые скобки/JSON остатки — чистим
	result = strings.TrimSpace(result)
	if result == "[]" || result == "{}" || result == "" {
		return ""
	}

	return result
}

// stripServiceTokens — удаляет известные служебные токены из строки (Gemma <start_of_turn>,
// Llama3 <|eot_id|>, ChatML <|im_end|> и т.д.), возвращая очищенный текст.
// Используется после детекции tool calls для очистки remainingContent.
func stripServiceTokens(s string) string {
	if s == "" {
		return ""
	}
	tokens := []string{
		"<end_of_turn>",
		"</end_of_turn>",
		"<start_of_turn>",
		"</start_of_turn>",
		"<bos>",
		"<eos>",
		"<endoftext>",
		"<|endoftext|>",
		"<|eot_id|>",
		"<|eot|>",
		"<|im_start|>",
		"<|im_end|>",
		"<|start_header_id|>",
		"<|end_header_id|>",
		"<|begin_of_text|>",
		"<|end_of_text|>",
		"<sep>",
		"<pad>",
		"<unk>",
	}
	result := s
	for _, tok := range tokens {
		result = strings.ReplaceAll(result, tok, "")
	}
	return strings.TrimSpace(result)
}

// shouldFilterLlamaCppContent — определяет, нужно ли отфильтровать строку
// content из streaming-ответа llama.cpp/cppworker как служебный токен
// (например Gemma `<end_of_turn>`, Llama3 `<|eot_id|>`, ChatML `<|im_end|>`).
//
// Зачем: cppworker иногда отдаёт служебные токены модели как обычный content
// (из-за неполного chat template или его отсутствия). OpenAI-клиенты типа
// Cline интерпретируют такие токены как невалидный tool-call-like вывод
// и выдают "Invalid API Response: The provider returned an empty or
// unparsable response". Фильтрация на стороне балансировщика покрывает
// любые модели с подобными артефактами.
//
// Возвращает true, если строка содержит служебный токен в любом месте
// (как самостоятельная строка, как префикс, в середине или как суффикс).
// Это критично для моделей вроде Gemma, которые из-за неполного chat template
// эмитят служебные токены не только отдельным чанком, но и вкраплениями
// в обычный текст: "hello<end_of_turn>world", "<|eot_id|>ok" и т.д.
func shouldFilterLlamaCppContent(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	// Список известных служебных токенов для разных моделей.
	filteredTokens := []string{
		"<end_of_turn>",       // Gemma
		"<start_of_turn>",     // Gemma
	"</start_of_turn>",    // Gemma (closing tag)
	"</end_of_turn>",      // Gemma (closing tag)
		"<bos>",               // Llama, общий
		"<eos>",               // Llama, общий
		"<endoftext>",         // GPT-2 / некоторые GGUF
		"<|endoftext|>",       // Llama2/3 (старый формат)
		"<|eot_id|>",          // Llama3 instruct
		"<|eot|>",             // некоторые варианты
		"<|im_start|>",        // ChatML
		"<|im_end|>",          // ChatML
		"<|start_header_id|>", // Llama3 header
		"<|end_header_id|>",   // Llama3 header
		"<|begin_of_text|>",   // Llama3 begin
		"<|end_of_text|>",     // Llama3 end
		"<sep>",               // BERT, некоторые GGUF
		"<pad>",               // служебные токены
		"<unk>",               // unknown token
	}
	// Служебные токены могут появляться в любом месте стрима: как самостоятельная
	// строка ("<end_of_turn>"), как префикс ("<end_of_turn>hello"), в середине
	// ("hello<|eot_id|>world") или как суффикс. Используем Contains вместо
	// == / HasPrefix, чтобы ловить их все.
	for _, tok := range filteredTokens {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

// filterOpenAIStreamingLine — фильтрует SSE-строку с OpenAI chunk, удаляя
// content-токены, которые являются служебными (см. shouldFilterLlamaCppContent).
//
// Возвращает (filtered, wasFiltered):
//   - filtered: новая строка для записи клиенту (или nil, если нужно пропустить чанк)
//   - wasFiltered: true, если хотя бы один content-чанк был отфильтрован
//
// Работает на строке формата `data: {...}\n\n` или `data:{...}\n\n`.
func filterOpenAIStreamingLine(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return line, false
	}
	// Не data-строка — пропускаем как есть (": comment", "[DONE]", etc.)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line, false
	}
	// Извлекаем JSON-часть после "data: "
	jsonData := bytes.TrimPrefix(trimmed, []byte("data:"))
	jsonData = bytes.TrimSpace(jsonData)
	// [DONE] маркер — не трогаем
	if bytes.Equal(jsonData, []byte("[DONE]")) {
		return line, false
	}
	// Парсим JSON, чтобы достать content из choices[0].delta.content
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(jsonData, &chunk); err != nil {
		// Невалидный JSON — отдаём как есть (пусть клиент сам решает).
		return line, false
	}
	if len(chunk.Choices) == 0 {
		return line, false
	}
	content := chunk.Choices[0].Delta.Content
	if !shouldFilterLlamaCppContent(content) {
		return line, false
	}
	// content — служебный токен. Заменяем на пустую delta, чтобы чанк
	// пришёл клиенту, но с пустым content (это валидный OpenAI chunk).
	filteredChunk := map[string]interface{}{
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{},
			},
		},
	}
	// Сохраняем остальные поля (id, model, created, object) из оригинала.
	var orig map[string]interface{}
	if err := json.Unmarshal(jsonData, &orig); err == nil {
		for k, v := range orig {
			if k == "choices" {
				continue
			}
			filteredChunk[k] = v
		}
	}
	out, err := json.Marshal(filteredChunk)
	if err != nil {
		return line, false
	}
	// Возвращаем как `data: {...}\n\n` (с финальным \n для совместимости с SSE-форматом).
	return append([]byte("data: "+string(out)+"\n\n"), '\n'), true
}
