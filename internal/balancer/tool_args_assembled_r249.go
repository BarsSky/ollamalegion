// tool_args_assembled_r249.go — R249 (2026-10-28): ремонт аргументов на
// СОБРАННОМ (auto-stream → non-stream) ответе балансера.
//
// ЗАЧЕМ. Стриминговый путь прогоняет аргументы через repairToolArguments, а
// auto-stream workaround (Round 31 #1) собирает ответ из SSE здесь и исторически
// отдавал tool_calls как пришли. Живой прогон на gemma-4 (write):
//
//	стриминг:          {"content":"...","path":"C:/work/hello.cpp"}   ← чинится
//	собранный non-stream: {"content":"{\"content\":...}"}            ← path потерян
//
// Из-за этого non-stream клиенты не могли применить инструмент.
package balancer

import "encoding/json"

// repairAssembledToolCalls прогоняет tool_calls собранного ответа через тот же
// ремонт, что и стриминговый путь. Схемы инструментов берутся из ИСХОДНОГО тела
// клиента (там tools[].function.name/parameters). Если схем нет или тело не
// разбирается — ответ возвращается без изменений: ремонт не должен ломать
// нормальные ответы.
func repairAssembledToolCalls(respBody []byte, clientReqBody []byte) []byte {
	if len(respBody) == 0 || len(clientReqBody) == 0 {
		return respBody
	}
	schemas := toolSchemasFromBody(clientReqBody)
	if len(schemas) == 0 {
		return respBody
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return respBody
	}
	changed := false
	for _, rawChoice := range asInterfaceSlice(resp["choices"]) {
		choice, ok := rawChoice.(map[string]interface{})
		if !ok {
			continue
		}
		msg, ok := choice["message"].(map[string]interface{})
		if !ok {
			continue
		}
		calls := asInterfaceSlice(msg["tool_calls"])
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]interface{})
			if !ok {
				continue
			}
			fn, ok := call["function"].(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := fn["name"].(string)
			schema := schemas[name]
			if schema == nil {
				continue
			}
			args, ok := fn["arguments"].(string)
			if !ok || args == "" {
				continue
			}
			if fixed, did := repairToolArguments(args, schema); did {
				fn["arguments"] = fixed
				changed = true
			}
		}
		if changed {
			msg["tool_calls"] = calls
		}
	}
	if !changed {
		return respBody
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return respBody
	}
	return out
}

// asInterfaceSlice — безопасное приведение к []interface{}.
func asInterfaceSlice(v interface{}) []interface{} {
	if s, ok := v.([]interface{}); ok {
		return s
	}
	return nil
}
