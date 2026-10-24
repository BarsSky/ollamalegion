// toolcall_accum_r118_20.go — R118.20 (2026-10-15): накопитель фрагментов
// tool-call'ов для Ollama-поверхности (закрытие пункта C5 аудита клиентского API).
//
// ЗАЧЕМ. cppworker (как и любой OpenAI-совместимый upstream) вправе дробить
// аргументы вызова инструмента по чанкам:
//
//	delta.tool_calls[0].function.arguments = "{\"q\":\"te"
//	delta.tool_calls[0].function.arguments = "st\"}"
//
// На OpenAI-поверхности это нормально: балансер проксирует SSE как есть, и клиент
// (Cline/Roo/DSH) склеивает фрагменты сам. Но на Ollama-поверхности
// (`/api/chat`, `/api/generate`) ответ идёт в форме Ollama, где
// `message.tool_calls[].function.arguments` — ОБЪЕКТ, а не строка, и частичных
// аргументов не бывает: реальная Ollama отдаёт tool_calls один раз, целиком.
//
// До этой правки транслятор (чистая функция без состояния) отбрасывал чанк с
// tool_calls без role/content, а фрагмент с незавершённым JSON не мог
// превратиться в объект — то есть при `LB_OLLAMA_NATIVE_PATH=0` вызов инструмента
// терял аргументы. Живой тест-зонд: чанк с `arguments: "{\"q\":\"te"` не давал
// вывода вовсе (см. TestR118_19_C5_…_KnownLimitation).
//
// Состояние живёт в транспорте (на один стрим), транслятор остаётся чистым.
package balancer

import (
	"encoding/json"
	"strings"
)

// toolCallAccumulator — фрагменты tool-call'ов одного стрима.
type toolCallAccumulator struct {
	calls map[int]*toolCallFragment
	order []int
}

// toolCallFragment — один вызов, собираемый из фрагментов.
type toolCallFragment struct {
	id        string
	name      string
	arguments strings.Builder
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{calls: map[int]*toolCallFragment{}}
}

// absorb — принять delta.tool_calls одного чанка и сказать, собраны ли аргументы.
//
// Возвращает (merged, hasToolCalls, ready):
//   - hasToolCalls — в чанке вообще были tool_calls;
//   - ready — все накопленные вызовы имеют завершённый JSON аргументов;
//   - merged — полный список вызовов в форме OpenAI (id/type/function), готовый
//     к тому, чтобы транслятор превратил его в Ollama-форму.
func (a *toolCallAccumulator) absorb(deltaTC []interface{}) (merged []interface{}, hasToolCalls, ready bool) {
	if len(deltaTC) == 0 {
		return nil, false, false
	}
	hasToolCalls = true
	for i, raw := range deltaTC {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		idx := i
		if v, ok := item["index"].(float64); ok {
			idx = int(v)
		}
		frag, exists := a.calls[idx]
		if !exists {
			frag = &toolCallFragment{}
			a.calls[idx] = frag
			a.order = append(a.order, idx)
		}
		if id, ok := item["id"].(string); ok && id != "" {
			frag.id = id
		}
		if fn, ok := item["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				frag.name = name
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				// Фрагмент может ПОВТОРЯТЬ уже накопленное (некоторые upstreams
				// шлют накопительный arguments) — тогда не дублируем.
				current := frag.arguments.String()
				if !strings.HasPrefix(args, current) {
					frag.arguments.WriteString(args)
				} else {
					frag.arguments.Reset()
					frag.arguments.WriteString(args)
				}
			}
		}
	}

	merged = make([]interface{}, 0, len(a.order))
	for _, idx := range a.order {
		frag := a.calls[idx]
		args := frag.arguments.String()
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return nil, true, false
		}
		merged = append(merged, map[string]interface{}{
			"index": idx,
			"id":    frag.id,
			"type":  "function",
			"function": map[string]interface{}{
				"name":      frag.name,
				"arguments": args,
			},
		})
	}
	return merged, true, true
}

// filterToolCallDelta — R118.20: привести SSE-чанк к тому, что можно отдать
// Ollama-клиенту.
//
//   - в чанке нет tool_calls → (data, true): отдаём как есть;
//   - есть, и аргументы уже собраны → (переписанный data c ПОЛНЫМ списком, true);
//   - есть, но фрагмент неполный → ("", false): чанк отдавать нельзя (в
//     Ollama-форме частичных аргументов не существует), вызывающий его пропускает.
func (a *toolCallAccumulator) filterToolCallDelta(data string) (string, bool) {
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return data, true
	}
	choices, _ := chunk["choices"].([]interface{})
	if len(choices) == 0 {
		return data, true
	}
	choice, _ := choices[0].(map[string]interface{})
	if choice == nil {
		return data, true
	}
	delta, _ := choice["delta"].(map[string]interface{})
	if delta == nil {
		return data, true
	}
	tcRaw, has := delta["tool_calls"]
	if !has {
		return data, true
	}
	tcList, _ := tcRaw.([]interface{})
	merged, hasToolCalls, ready := a.absorb(tcList)
	if !hasToolCalls {
		return data, true
	}
	if !ready {
		return "", false
	}
	delta["tool_calls"] = merged
	out, err := json.Marshal(chunk)
	if err != nil {
		return data, true
	}
	return string(out), true
}
