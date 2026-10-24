// json_decode_client_r106c.go — R106c (2026-10-12).
//
// КЛИЕНТСКОЕ декодирование тела: терпимое к незнакомым полям.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ХЕЛПЕР. Ошибка «invalid JSON: json: unknown field "…"» на
// клиентском API повторялась годами: options.top_k (OpenWebUI), options.use_mmap
// и num_gpu (advanced params OpenWebUI), tool_choice и tool_name (Cline),
// max_output_tokens (Cline), thinking/reasoning_content (история сессии, которую
// клиент вернул нам же). Каждая такая ошибка лечилась добавлением ОДНОГО поля в
// структуру — и повторялась со следующим.
//
// Причина не в полях, а в политике: настоящая Ollama (и llama.cpp-сервер)
// декодируют запрос обычным encoding/json, то есть НЕИЗВЕСТНЫЕ ПОЛЯ
// ИГНОРИРУЮТСЯ. Запрос «ollama api должно работать одинаково для всех клиентов»
// выполним только при такой же терпимости.
//
// РАЗДЕЛЕНИЕ ОТВЕТСТВЕННОСТИ:
//   - клиентские API (/api/chat, /api/generate, /api/cancel, /v1/*) — терпимы:
//     неизвестные поля игнорируются, их имена уходят в лог (Debugw);
//   - управляющие API (/api/models/load*, /reload, /config/update, /api/hf/*) —
//     остаются строгими (types.DecodeJSONRequest): там клиент — наш собственный
//     балансер, и 400 действительно ловит наш баг, а не прихоть чужого клиента.
package main

import (
	"net/http"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// decodeClientJSON декодирует тело клиентского запроса, игнорируя незнакомые
// поля. Возвращает ошибку только на реальные проблемы: пустое тело, слишком
// большое тело, битый JSON, несовпадение типа поля.
func decodeClientJSON(r *http.Request, maxBytes int64, v interface{}, where string) error {
	if r == nil || r.Body == nil {
		return types.ErrBodyEmpty
	}
	unknown, err := types.DecodeJSONRequestLenient(r.Body, maxBytes, v)
	if err != nil {
		return err
	}
	if len(unknown) > 0 && logger.Get() != nil {
		// Debug, а не Warn: клиенты шлют свои расширения постоянно (OpenWebUI
		// advanced params, Cline tool_choice), и Warn на каждый запрос превратил
		// бы лог в шум. Диагностика при этом не теряется.
		logger.Get().Debugw("клиент прислал поля, которых нет в структуре — игнорируем (Ollama-совместимость)",
			"handler", where, "fields", unknown)
	}
	return nil
}
