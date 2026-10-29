// ollama_toolcall_shape_r295.go — R295 (2026-10-29): форма tool_calls на выходе
// Ollama API и терминатор стрима.
//
// ЖИВАЯ ЖАЛОБА (Cline через Ollama API, порт 18080, бэкенд llama.cpp):
//  1. «клиент получает флаг о завершении» — то есть в NDJSON приходит done:true
//     РАНЬШЕ, чем кончился ответ; после него идут ещё кадры, и клиент их уже не
//     принимает;
//  2. «непонятно, может ли модель правильно применять инструменты» — вызовы
//     приходят, но клиент их не применяет.
//
// ПОЧЕМУ ВТОРОЕ. У двух протоколов РАЗНАЯ форма `function.arguments`:
//   - OpenAI (/v1): строка с JSON — `"arguments": "{\"path\":\"a\"}"`;
//   - Ollama (/api/chat): ОБЪЕКТ — `"arguments": {"path": "a"}`.
//
// Внутри балансера всё живёт в OpenAI-форме (контракт cppworker→балансер,
// см. docs/architecture/balancer-cppworker-contract.md), поэтому на выходе
// Ollama-поверхности аргументы обязаны быть РАЗОБРАНЫ обратно в объект.
// До этой правки они уходили строкой: `/v1`-клиенты (OpenAI SDK) это любят,
// а Ollama-клиенты (Cline с ollama-провайдером) получают вызов с аргументами
// неверного типа и не могут его выполнить — снаружи выглядит как «модель не
// умеет инструменты».
//
// ПОЧЕМУ ПЕРВОЕ. В переводе SSE→NDJSON был «safety net»: чанк, в котором
// finish_reason пришёл ВМЕСТЕ с content, помечался done:true сразу. cppworker
// после такого чанка ещё присылает usage-чанк (со статистикой), и клиент
// получал «конец ответа», после которого шли кадры. Правильная семантика
// Ollama — ровно ОДИН done:true и он ПОСЛЕДНИЙ; терминатор пишет транспорт
// (writeStreamingSSEDone / translateUsageChunkToOllama), когда поток реально
// закончился. Поэтому safety-net больше не ставит done:true, а только отдаёт
// content (done:false).
package balancer

import (
	"encoding/json"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// argumentsToOllamaObject — привести `function.arguments` к форме Ollama.
//
// На входе: JSON-строка (формат OpenAI) ИЛИ уже объект/массив (некоторые
// сборки воркера отдают объект). На выходе: объект/массив, если строку удалось
// разобрать; иначе строка как есть — потерять аргументы хуже, чем отдать их
// не в том типе: клиент хотя бы увидит вызов и сможет сообщить об ошибке.
func argumentsToOllamaObject(args interface{}) interface{} {
	switch v := args.(type) {
	case nil:
		return map[string]interface{}{}
	case map[string]interface{}, []interface{}:
		return v
	case json.RawMessage:
		return rawArgumentsToObject(v)
	case string:
		return rawArgumentsToObject(json.RawMessage(v))
	default:
		// Число/булево — не аргументы функции; отдаём как есть.
		return v
	}
}

// rawArgumentsToObject — разобрать JSON-строку аргументов в объект.
func rawArgumentsToObject(raw json.RawMessage) interface{} {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return map[string]interface{}{}
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
		switch parsed.(type) {
		case map[string]interface{}, []interface{}:
			return parsed
		}
		// Строка/число внутри — не валидные аргументы, но это не наша ошибка:
		// отдаём как распарсили, клиент сам решит.
		return parsed
	}
	logger.Get().Warnw("ollama egress: tool_call arguments are not valid JSON, passing as string",
		"args_len", len(trimmed), "preview", previewForLog(trimmed, 120))
	return trimmed
}

// previewForLog — короткий безопасный превью для лога.
func previewForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// toOllamaToolCallsObject — нормализовать МАССИВ вызовов под Ollama.
//
// Что делаем с каждым вызовом:
//   - `function.name` — оставляем;
//   - `function.arguments` — разбираем в объект (см. argumentsToOllamaObject);
//   - `type` — "function" по умолчанию (Ollama-клиенты читают это поле);
//   - `id` — сохраняем, если был (Cline использует его для сопоставления
//     результата вызова с ходом модели);
//   - `index` — сохраняем, если был (нужен клиентам, склеивающим вызовы).
//
// Вызовы, пришедшие в «плоской» форме (`{"name":…, "arguments":…}` — так их
// отдаёт наш текстовый детектор), тоже приводятся к полной форме с function.
func toOllamaToolCallsObject(list []interface{}) []interface{} {
	if len(list) == 0 {
		return list
	}
	out := make([]interface{}, 0, len(list))
	for _, raw := range list {
		tc, ok := raw.(map[string]interface{})
		if !ok {
			out = append(out, raw)
			continue
		}
		normalized := normalizeOneOllamaToolCall(tc)
		out = append(out, normalized)
	}
	// R295: одна debug-строка на ответ с вызовами. Помогает на живом стенде
	// отличить «модель не вызвала инструмент» от «вызов ушёл, но в неверной
	// форме» — раньше это выяснялось только погружением в поток.
	logger.Get().Debugw("ollama egress: tool_calls normalized for Ollama",
		"count", len(out), "arguments_as_object", true)
	return out
}

// normalizeOneOllamaToolCall — один вызов → Ollama-форма.
func normalizeOneOllamaToolCall(tc map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}

	// function: берём вложенную, иначе собираем из плоской формы.
	var fn map[string]interface{}
	if existing, ok := tc["function"].(map[string]interface{}); ok {
		fn = existing
	} else {
		fn = map[string]interface{}{}
		if name, ok := tc["name"].(string); ok && name != "" {
			fn["name"] = name
		}
		if args, ok := tc["arguments"]; ok {
			fn["arguments"] = args
		}
	}

	// Копия function, чтобы не мутировать исходную карту (она может быть
	// снапшотом из аккумулятора, который ещё используется транспорт-циклом).
	fnCopy := make(map[string]interface{}, len(fn)+1)
	for k, v := range fn {
		fnCopy[k] = v
	}
	if args, ok := fnCopy["arguments"]; ok {
		fnCopy["arguments"] = argumentsToOllamaObject(args)
	} else {
		fnCopy["arguments"] = map[string]interface{}{}
	}

	out["function"] = fnCopy

	// type по умолчанию "function": Ollama-клиенты (и Cline) читают это поле.
	if t, ok := tc["type"].(string); ok && t != "" {
		out["type"] = t
	} else {
		out["type"] = "function"
	}
	if id, ok := tc["id"].(string); ok && id != "" {
		out["id"] = id
	}
	if idx, ok := tc["index"]; ok {
		out["index"] = idx
	}
	return out
}

// toInterfaceSlice — []map[string]interface{} → []interface{} без потери данных.
//
// Нужен там, где вызовы собираются типизированным срезом (финальный done-чанк),
// а нормализатор работает с []interface{} — как приходят из JSON.
func toInterfaceSlice(in []map[string]interface{}) []interface{} {
	if len(in) == 0 {
		return nil
	}
	out := make([]interface{}, 0, len(in))
	for _, m := range in {
		out = append(out, m)
	}
	return out
}

// ollamaToolCallsFromRaw — удобная обёртка для мест, где tool_calls пришли
// как json.RawMessage (нативный путь/дельта OpenAI).
func ollamaToolCallsFromRaw(raw json.RawMessage) []interface{} {
	if len(raw) == 0 {
		return nil
	}
	var arr []interface{}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	return toOllamaToolCallsObject(arr)
}
