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
//
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

// llamaCppServiceTokens — канонический список служебных токенов моделей в
// порядке «длинные раньше коротких».
//
// Почему порядок важен: без него `ReplaceAll("<eos>", "")` съедает префикс
// более длинного токена и оставляет мусор от «хвоста». Например, при
// обработке `<|eos|>` сначала должен сработать длинный вариант.
//
// Раньше список был продублирован в двух функциях этого файла
// (shouldFilterLlamaCppContent и stripServiceTokens) с разным составом —
// это и приводило к расхождению поведения фильтра и очистки.
var llamaCppServiceTokens = []string{
	// Llama 3 / ChatML — самые длинные
	"<|start_header_id|>",
	"<|end_header_id|>",
	"<|begin_of_text|>",
	"<|end_of_text|>",
	"<|start_of_turn|>",
	"<|end_of_turn|>",
	"</start_of_turn>",
	"</end_of_turn>",
	"<start_of_turn>",
	"<end_of_turn>",
	"<|endoftext|>",
	"<|im_start|>",
	"<|im_end|>",
	"<endoftext>",
	"<|eot_id|>",
	"<|eot|>",
	"<|end|>",
	"<|eos|>",
	// Короткие — строго после длинных
	"<bos>",
	"<eos>",
	"<sep>",
	"<pad>",
	"<unk>",
}

// llamaCppReasoningMarkers — маркеры размышлений, которые не должны попадать
// в ВИДИМЫЙ текст ответа. Порядок — от длинных к коротким (иначе
// `<|channel>thought` оставит после себя `thought`).
//
// R231: единый список для защиты на границе балансера (см.
// stripReasoningMarkersFromVisible). cppworker чистит их сам, но клиент не
// должен зависеть от версии воркера.
var llamaCppReasoningMarkers = []string{
	// R243: варианты "thought"-маркеров (DeepSeek/производные шаблоны). Раньше
	// список знал только think/reasoning/analysis/channel, и `</thought>` уезжал
	// в видимый ответ как обычный текст.
	"<|begin_of_thought|>", "<|end_of_thought|>",
	"<|begin_of_solution|>", "<|end_of_solution|>",
	"<|/thought|>", "<|thought|>",
	"</thought>", "<thought>",
	"<|channel>thought\n", "<|channel>thought",
	"<|channel>analysis\n", "<|channel>analysis",
	"<|channel>", "<channel|>",
	"<|think>", "<think|>",
	"<thinking>", "</thinking>",
	"<reasoning>", "</reasoning>",
	"<analysis>", "</analysis>",
	"<think>", "</think>",
}

// stripReasoningMarkersFromVisible вырезает маркеры размышлений из видимого
// текста, сохраняя сам текст и пробелы вокруг (клиент склеивает дельты, срезать
// пробелы нельзя).
//
// Возвращает (очищенный текст, сколько маркеров вырезано).
func stripReasoningMarkersFromVisible(s string) (string, int) {
	if s == "" {
		return s, 0
	}
	out := s
	removed := 0
	for _, tag := range llamaCppReasoningMarkers {
		if tag == "" || !strings.Contains(out, tag) {
			continue
		}
		removed += strings.Count(out, tag)
		out = strings.ReplaceAll(out, tag, "")
	}
	return out, removed
}

// stripLlamaCppServiceTokens вырезает ВСЕ служебные токены из content,
// оставляя окружающий полезный текст.
//
// Возвращает (очищенный текст, сколько токенов вырезано).
//
// R65d (2026-09-20): до этого фикса filterOpenAIStreamingLine при обнаружении
// служебного токена заменял ВЕСЬ delta на {} — то есть выбрасывал не только
// токен, но и весь легитимный текст чанка. Для ответов, где эти строки
// упомянуты как текст (документация по chat-шаблонам, код парсеров, разбор
// спец-токенов), пользователь терял слова и целые предложения.
func stripLlamaCppServiceTokens(content string) (string, int) {
	if content == "" {
		return "", 0
	}
	result := content
	removed := 0
	for _, tok := range llamaCppServiceTokens {
		if !strings.Contains(result, tok) {
			continue
		}
		n := strings.Count(result, tok)
		result = strings.ReplaceAll(result, tok, "")
		removed += n
	}
	return result, removed
}

// shouldFilterLlamaCppContent — определяет, является ли content настолько
// «служебным», что его не нужно показывать клиенту: либо это ровно служебный
// токен, либо после вырезания токенов не осталось значимого текста.
//
// Возвращает true, если чанк не несёт полезной для пользователя информации.
// ВНИМАНИЕ: семантика изменена в R65d. Раньше функция возвращала true при
// ЛЮБОМ вхождении токена (включая "hello<|eot_id|>world"), и вызывающий код
// выбрасывал текст целиком. Теперь true означает только «показывать нечего».
func shouldFilterLlamaCppContent(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}
	// Служебный токен может прийти в любом месте потока: отдельным чанком,
	// префиксом, суффиксом или вкраплением в текст. Вырезаем все вхождения и
	// смотрим, остался ли содержательный текст.
	stripped, removed := stripLlamaCppServiceTokens(trimmed)
	if removed == 0 {
		return false
	}
	return strings.TrimSpace(stripped) == ""
}

// filterOpenAIStreamingLine — фильтрует SSE-строку с OpenAI chunk, удаляя
// content-токены, которые являются служебными (см. shouldFilterLlamaCppContent).
//
// Возвращает (filtered, wasFiltered):
//   - filtered: новая строка для записи клиенту (или nil, если нужно пропустить чанк)
//   - wasFiltered: true, если хотя бы один content-чанк был отфильтрован
//
// Работает на строке формата `data: {...}\n\n` или `data:{...}\n\n`.
// reasoningStreamState — состояние построчной фильтрации SSE.
//
// ЗАЧЕМ (R243). Маркер размышлений может прийти РАЗРЕЗАННЫМ между чанками
// (`</thi` + `nk>`). Построчный ReplaceAll такой маркер не видит, и он утекает
// в видимый content (жалоба Cline: «в ответе остаётся </think>»). Состояние
// удерживает хвост, который может оказаться началом маркера, и отдаёт его в
// следующем чанке (или выбрасывает, если поток закончился: недособранный
// служебный маркер не является текстом ответа).
type reasoningStreamState struct {
	pending string
}

// filterContent — вырезать маркеры и удержать хвост-возможный-префикс.
func (st *reasoningStreamState) filterContent(s string) string {
	if st == nil {
		out, _ := stripReasoningMarkersFromVisible(s)
		return out
	}
	s = st.pending + s
	st.pending = ""
	s, _ = stripReasoningMarkersFromVisible(s)
	if n := trailingReasoningMarkerPrefix(s); n > 0 {
		st.pending = s[len(s)-n:]
		s = s[:len(s)-n]
	}
	return s
}

// trailingReasoningMarkerPrefix — длина хвоста s, который является началом
// какого-либо маркера размышлений, но короче его.
func trailingReasoningMarkerPrefix(s string) int {
	if s == "" {
		return 0
	}
	maxLen := 0
	for _, m := range llamaCppReasoningMarkers {
		if len(m) > maxLen {
			maxLen = len(m)
		}
	}
	if maxLen > len(s) {
		maxLen = len(s)
	}
	lower := strings.ToLower(s)
	for n := maxLen - 1; n >= 1; n-- {
		suffix := lower[len(lower)-n:]
		for _, m := range llamaCppReasoningMarkers {
			lm := strings.ToLower(m)
			if len(suffix) < len(lm) && strings.HasPrefix(lm, suffix) {
				return n
			}
		}
	}
	return 0
}

func filterOpenAIStreamingLine(line []byte) ([]byte, bool) {
	return filterOpenAIStreamingLineWithState(line, nil)
}

// filterOpenAIStreamingLineWithState — то же, но с состоянием между чанками
// (см. reasoningStreamState). st == nil — прежнее построчное поведение.
func filterOpenAIStreamingLineWithState(line []byte, st *reasoningStreamState) ([]byte, bool) {
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
	// R65d (2026-09-20): вырезаем ТОЛЬКО служебные токены, сохраняя остальной
	// текст чанка. Раньше при любом вхождении токена весь delta заменялся на {}
	// (см. stripLlamaCppServiceTokens) — это удаляло из ответа легитимные
	// слова и предложения, если модель упоминала спец-токен как текст.
	stripped, removed := stripLlamaCppServiceTokens(content)
	// R231 (2026-10-24): маркеры размышлений не должны доезжать до КЛИЕНТА в
	// видимом канале. cppworker чистит их сам, но старая версия воркера (или
	// путь без reasoning-парсера) может прислать `</think>`/`<|channel>thought`
	// в delta.content — живая жалоба Cline: «в основном теле ответа </think>».
	// Здесь вторая линия обороны на границе балансера (Cline/OpenWebUI).
	//
	// R243: со состоянием (st != nil) маркер вырезается даже если он разрезан
	// между чанками: хвост-возможный-префикс удерживается до следующего чанка.
	if st != nil {
		prevPending := st.pending
		stripped = st.filterContent(stripped)
		if stripped != content || st.pending != prevPending {
			// Пересборка нужна и когда текст только удержан: клиент не должен
			// получить начало маркера.
			removed++
		}
	} else if s, n := stripReasoningMarkersFromVisible(stripped); n > 0 {
		stripped = s
		removed += n
	}
	if removed == 0 {
		// Служебных токенов нет — строка не изменяется.
		return line, false
	}

	// Пересобираем чанк, сохранив все поля (id, model, created, object,
	// finish_reason, role, tool_calls...), меняя ровно одно: content внутри
	// choices[0].delta. Полезная нагрузка остальных полей не должна теряться.
	var orig map[string]interface{}
	if err := json.Unmarshal(jsonData, &orig); err != nil {
		// Не смогли разобрать как объект — отдаём как есть, чтобы не потерять
		// чанк целиком (лучше лишний токен, чем пропавший текст).
		return line, false
	}
	choices, ok := orig["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return line, false
	}
	first, ok := choices[0].(map[string]interface{})
	if !ok {
		return line, false
	}
	delta, ok := first["delta"].(map[string]interface{})
	if !ok {
		return line, false
	}
	// Если после вырезания не осталось текста — оставляем пустую строку, а не
	// удаляем ключ: клиенты (Cline/OpenWebUI) ожидают форму чанка, и пустой
	// content безопаснее отсутствующего.
	delta["content"] = stripped
	first["delta"] = delta
	choices[0] = first
	orig["choices"] = choices

	out, err := json.Marshal(orig)
	if err != nil {
		return line, false
	}
	// SSE-кадр: ровно один перевод строки после data-строки плюс пустая строка
	// — разделитель событий. Раньше здесь был лишний третий '\n'
	// (append(..., '\n')), который раздувал поток и ломал строгие парсеры.
	return []byte("data: " + string(out) + "\n\n"), true
}
