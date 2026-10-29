// window_truth_r294.go — R294 (2026-10-29): окно контекста и кадры ошибок.
//
// Три живых дефекта, найденных на стенде r294:
//
//  1. Requested window не был НИЖНЕЙ ГРАНИЦЕЙ при расчёте окна: клиент просил
//     num_ctx=16384 при загруженных 8192, оценка промпта была маленькой, и
//     балансер перезагружал модель в МЕНЬШЕЕ окно (4165). Рост окна не может
//     быть уменьшением — см. agentRequiredWindowAtLeast и отказ от shrink в
//     preflight_nctx.go.
//
//  2. Бюджет вывода брался как сырое meta.RequestedNPredict: при num_predict=8
//     в расчёт уходило 8, а в лог — 0, из-за чего окно считалось «промпт + 0».
//     Теперь agentOutputBudget даёт осмысленный минимум.
//
//  3. Кадр ошибки от воркера внутри стрима не завершал поток: /v1 висел до
//     клиентского таймаута (curl HTTP 000), хотя ответ уже пришёл. Здесь —
//     распознавание такого кадра, чтобы hijack-путь закрыл поток терминатором.
package balancer

import "encoding/json"

// isUpstreamErrorPayload — содержит ли SSE-данные объект ошибки от воркера.
//
// Смотрим ТОЛЬКО верхнеуровневый "error" (строка или объект). Текст ответа
// модели со словом "error" внутри content сюда не попадает: разбирается JSON,
// а не подстрока — иначе обычный ответ «the error is in line 12» закрывал бы
// стрим на середине.
func isUpstreamErrorPayload(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return false
	}
	raw, ok := obj["error"]
	if !ok || len(raw) == 0 {
		return false
	}
	// null и пустая строка ошибкой не считаются.
	if string(raw) == "null" || string(raw) == `""` {
		return false
	}
	return true
}
