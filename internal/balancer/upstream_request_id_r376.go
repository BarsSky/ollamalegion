// upstream_request_id_r376.go — R376 (2026-11-01): id, который балансер выдал
// клиенту, обязан доехать до воркера, иначе отмена по нему не работает.
//
// ЖИВОЙ ДЕФЕКТ (последняя живая проверка аудита отмены: Stop при двух
// одноимённых копиях). Сценарий:
//
//	клиент → балансер /api/generate (stream)   получает X-Request-ID: req8fab2156
//	параллельно на втором узле идёт своя генерация той же модели
//	клиент → балансер POST /api/cancel {"request_id":"req8fab2156"}
//	ответ: status=unreachable,
//	       мастер и донор: "request_id not found: req8fab2156"
//	       обе генерации продолжились до конца (600 токенов, done_reason=stop)
//
// ПРИЧИНА. Балансер генерирует correlation-id (requestid.go: `req<hex>`), кладёт
// его в контекст и отдаёт клиенту заголовком X-Request-ID. Но в ЗАПРОС к воркеру
// он его не передавал: воркер видел пустой X-Request-Id и заводил в трекере
// отмены собственный id (`gen-<наносекунды>`, cancel_tracking.go:26). То есть
// клиент, оператор и воркер говорили о трёх разных идентификаторах одной
// генерации — и отмена по «своему» id не находила адресата.
//
// ЧТО ДЕЛАЕМ. Сразу после сборки запроса к воркеру проставляем в него ровно тот
// id, который получил клиент (из контекста; если клиент прислал свой — это и есть
// он же, getOrGenerateRequestID использует клиентское значение). Тогда:
//
//	worker cancel_tracking: requestID = X-Request-Id = req8fab2156
//	POST /api/cancel {"request_id":"req8fab2156"} → попадает в нужную генерацию
//
// Почему пост-копирование, а не «до»: билдеры запросов сначала переносят
// заголовки клиента и только потом мы ставим итоговый id — это гарантирует, что
// уйдёт именно выданный клиенту, даже если клиент прислал свой в другом регистре
// или дублировал его.
package balancer

import (
	"net/http"
	"strings"
)

// stampUpstreamRequestIDR376 — проставить в запрос к воркеру correlation-id
// клиента. Пустой id — no-op (запрос без контекста, служебные вызовы).
func stampUpstreamRequestIDR376(req *http.Request, rid string) {
	if req == nil {
		return
	}
	rid = strings.TrimSpace(rid)
	if rid == "" {
		return
	}
	// Header.Set канонизирует имя в X-Request-Id — ровно то, что читает воркер
	// (cancel_tracking.go:26).
	req.Header.Set(requestIDHeader, rid)
}

// stampUpstreamRequestIDFromRequestR376 — удобная форма для мест, где есть
// исходный клиентский запрос: берём id из контекста (его положил proxy.go).
func stampUpstreamRequestIDFromRequestR376(dst, src *http.Request) {
	if dst == nil || src == nil {
		return
	}
	stampUpstreamRequestIDR376(dst, RequestIDFromContext(src.Context()))
}
