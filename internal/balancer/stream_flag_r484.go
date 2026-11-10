// stream_flag_r484.go — R484 (2026-11-10): один разбор поля stream на все пути.
//
// ЗАЧЕМ. Предикатов «это стрим?» было три, и они расходились в ДЕФОЛТЕ для
// запроса без поля stream:
//
//	parseRequestBody (proxy.go)            — legacy-поверхность: true, OpenAI: false;
//	isStreamingRequest (proxy_request.go)  — true (если поля нет);
//	isStreamingFromBody (llamacpp_translate_req.go) — false.
//
// Из-за этого один и тот же запрос без stream на /api/chat считался стримовым
// при выборе очереди/окна, но мог уехать в НЕ-stream транспорт llama.cpp, а в
// лог при этом писалось stream=true (живой случай с BOM в теле: R480).
//
// ПРАВИЛО R484: сам разбор поля — ОДНА функция. Различия в дефолте не
// «зашиты» по файлам, а передаются явно в единственном параметре
// defaultWhenAbsent, поэтому их видно и можно покрыть тестом. Поведение не
// меняется: legacy-поверхность по-прежнему по умолчанию стримит, OpenAI —
// нет, transport llama.cpp — только по явному stream:true.
//
// Отдельно R484 добавляет в диагностику поле transport (см. transport_r484.go):
// каким транспортом запрос ОТВЕТИЛИ на самом деле.
package balancer

import "encoding/json"

// streamFlagFromBodyR484 — значение поля stream из тела запроса.
//
// Возвращает:
//   - stream   — значение, с учётом defaultWhenAbsent;
//   - explicit — поле было в теле (true/false), а не подставлен дефолт;
//   - ok       — тело разобрано (false: пусто/не JSON — вызывающий решает сам).
func streamFlagFromBodyR484(body []byte, defaultWhenAbsent bool) (stream, explicit, ok bool) {
	if len(body) == 0 {
		return false, false, false
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return false, false, false
	}
	if v, has := req["stream"].(bool); has {
		return v, true, true
	}
	return defaultWhenAbsent, false, true
}

// streamDefaultForSurfaceR484 — дефолт поверхностей, зафиксированный R484.
//
// Legacy (универсальный порт 18080 / нативные /api/*): Ollama исторически
// стримит по умолчанию. OpenAI-поверхность (18079): стрим только по явному
// stream=true.
func streamDefaultForSurfaceR484(surface requestSurface) bool {
	return surface == surfaceLegacy
}
