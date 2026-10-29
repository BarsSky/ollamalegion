// marker_hold_ndjson_r266_1.go — R266.1 (2026-10-28): удержание хвоста
// служебных меток на пути SSE→NDJSON (`/api/chat`).
//
// ЖИВОЙ ДЕФЕКТ (Cline, /api/chat, qwen3.8). В видимом content утекали
// `<|assistant|>` и `</think>`. Разбор живого потока: `<|assistant|>` пришёл
// РАЗРЕЗАННЫМ на четыре чанка (`\u003c|as` + `sista` + `nt` + `|>`), а `</think>`
// вообще отсутствовал в списке меток.
//
// ПОЧЕМУ ПОЧАНКОВОЕ ВЫЧИЩЕНИЕ НЕ ПОМОГАЛО. `stripQwenServiceMarkers` знает
// только ЦЕЛЫЕ метки. Если метка разрезана, каждый чанк по отдельности её не
// содержит — значит ничего не вырезается, и половины уезжают клиенту текстом.
// Удержание хвоста (`splitMarkerSuffixLen`) существовало, но применялось лишь
// в ветке НАКОПЛЕНИЯ tool-вызова. А после того, как в стриме уже был распознан
// tool call (`contentToolCallsProcessed == true`), весь последующий текст шёл
// мимо и вычищения, и удержания: ровно там оператор и видел «перемешку»
// (`… <|tool|> tool_call_result(…) <|assistant|> The file has ~348 lines …`).
//
// РЕШЕНИЕ. Одно удержание на весь стрим NDJSON-пути: перед отдачей чанка клиенту
// хвост, который может оказаться началом метки, придерживается и склеивается со
// следующим чанком; на конце стрима придержанное отдаётся (после вычищения),
// чтобы текст не терялся. Тот же принцип, что в R256 (`reasoningStreamState.
// pending`), но для SSE→NDJSON, где клиент видит обычный `message.content`.
package balancer

import (
	"encoding/json"
	"strings"
)

// markerHold — придержанный хвост текста, который может быть началом
// служебной метки (метка разрезана между чанками).
//
// Нулевое значение рабочее (pending пуст). Не потокобезопасен: живёт в пределах
// одного стрима, как и остальные per-stream аккумуляторы транспорта.
type markerHold struct {
	pending string
}

// Apply добавляет придержанное к новому фрагменту, вычищает целые метки и
// придерживает новый подозрительный хвост. Возвращает то, что можно отдавать
// клиенту прямо сейчас.
func (h *markerHold) Apply(content string) string {
	s := content
	if h != nil {
		if h.pending != "" {
			s = h.pending + s
			h.pending = ""
		}
	}
	if s == "" {
		return ""
	}
	n := splitMarkerSuffixLen(s)
	if n > 0 && h != nil {
		h.pending = s[len(s)-n:]
		s = s[:len(s)-n]
	}
	return stripQwenServiceMarkers(s)
}

// Flush отдаёт придержанный хвост (после вычищения). Вызывается на конце стрима:
// без него ответ, закончившийся на `<` или `[`, терял бы последние байты —
// ровно тот дефект, что чинили в R261 для другого пути.
func (h *markerHold) Flush() string {
	if h == nil || h.pending == "" {
		return ""
	}
	out := stripQwenServiceMarkers(h.pending)
	h.pending = ""
	return out
}

// filterSSEDeltaContentMarkers переписывает SSE-чанк так, чтобы его видимый
// `delta.content` не содержал служебных меток шаблона.
//
// ЗАЧЕМ НА УРОВНЕ ДАННЫХ, А НЕ ПЕРЕМЕННОЙ. Клиенту на этом пути уходит результат
// `translateOpenAISSEDataToOllamaWithFlags`, который берёт content из сырого
// SSE-чанка, а не из нашей локальной переменной. Поэтому чистить надо именно
// данные — тогда единственная точка вычищения остаётся одна на всех ветках
// (обычный текст, чанк с finish_reason, чанк после распознанного tool call).
//
// Если content в чанке нет или он не изменился — возвращаются ИСХОДНЫЕ байты,
// чтобы не гонять JSON туда-обратно на каждом кадре горячего пути.
func filterSSEDeltaContentMarkers(data string, hold *markerHold) string {
	if data == "" || !strings.Contains(data, "content") {
		return data
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return data
	}
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return data
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return data
	}
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return data
	}
	content, ok := delta["content"].(string)
	if !ok || content == "" {
		return data
	}

	cleaned := hold.Apply(content)
	if cleaned == content {
		return data
	}
	delta["content"] = cleaned
	out, err := json.Marshal(chunk)
	if err != nil {
		return data
	}
	return string(out)
}

// filterNDJSONLineMarkers — то же вычищение для НАТИВНОГО Ollama-пути, где
// cppworker уже отдаёт NDJSON (`message.content` / `response`) и балансер
// проксирует строки как есть.
//
// ПОЧЕМУ ОТДЕЛЬНАЯ ФУНКЦИЯ, А НЕ ОБЩАЯ С SSE. Живой факт (R266.1): Cline на
// `/api/chat` получал метки именно по этому пути — cppworker отвечает NDJSON, и
// балансер писал строку клиенту без разбора. Здесь правится та же мысль, но на
// другом носителе: у NDJSON есть готовое поле content, а не delta.
//
// Поведение на финальном чанке: придержанный хвост отдаётся В НЁМ (после
// вычищения) — иначе ответ, закончившийся на `<`, потерял бы последние байты.
func filterNDJSONLineMarkers(line string, hold *markerHold) string {
	if line == "" {
		return line
	}
	// ВАЖНО: придержанный хвост обязан участвовать в следующей строке, даже
	// если в ней самой нет «<». Иначе получалось ровно то, что поймал тест:
	// `"<|as"` придержан, а следующие куски (`sista`, `nt`, `|>`) шли мимо
	// фильтра (в них нет символа метки) — и клиент видел `sistant|>`.
	if (hold == nil || hold.pending == "") && !containsAnyMarkerHint(line) {
		return line
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return line
	}
	done, _ := obj["done"].(bool)

	// message.content — форма /api/chat; response — форма /api/generate.
	if msg, ok := obj["message"].(map[string]interface{}); ok {
		if c, ok := msg["content"].(string); ok && c != "" {
			if done {
				msg["content"] = hold.Flush() + hold.Apply(c)
			} else {
				msg["content"] = hold.Apply(c)
			}
		} else if done {
			if tail := hold.Flush(); tail != "" {
				msg["content"] = tail
			}
		}
	}
	if r, ok := obj["response"].(string); ok && r != "" {
		if done {
			obj["response"] = hold.Flush() + hold.Apply(r)
		} else {
			obj["response"] = hold.Apply(r)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return line
	}
	return string(out)
}

// containsAnyMarkerHint — дешёвая проверка «в строке вообще может быть метка»
// (в NDJSON метки приходят как есть либо как \u003c-экранирование).
func containsAnyMarkerHint(s string) bool {
	return strings.Contains(s, "<") || strings.Contains(s, `\u003c`) || strings.Contains(s, `\u003C`)
}
