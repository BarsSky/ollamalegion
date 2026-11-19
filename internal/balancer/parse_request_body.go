// parse_request_body.go — R629 (2026-11-19): однократный разбор тела запроса.
//
// ЗАЧЕМ ВЫНЕСЕНО. `parsedRequest` — общий тип, который читают и `ServeHTTP`,
// и `recordRecentClient`, и слои очереди. Пока он лежал в `proxy.go` среди
// 2400 строк, найти «где именно читается тело и что из него извлекается» было
// нельзя без прокрутки.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть, включая дефолт `stream=true`
// на legacy-поверхности и лимит тела (`TooLarge`).
package balancer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// parsedRequest — результат однократного разбора тела запроса.
// Используется чтобы избежать повторного чтения тела (recordRecentClient + proxyRequest).
type parsedRequest struct {
	Model   string
	Stream  bool
	RawBody []byte
	// TooLarge — тело превысило LB_MAX_REQUEST_BODY_MB: ServeHTTP отвечает 413
	// и НЕ отправляет запрос бэкенду (R479).
	TooLarge bool
	// Plan — единое решение о запросе (R493, этап 1). Собирается здесь же, где
	// тело разобрано один раз; потребители берут его через requestPlanOfR493.
	Plan *requestPlanR493
	// NumCtx — окно, которое запросил клиент (0 = не задавал).
	//
	// R118.5 (2026-10-14): нужно ДО выбора бэкенда: при нескольких загруженных
	// копиях одной модели выбирать надо ту, чьё окно на слот покрывает запрос
	// (иначе подготовленная копия мастера подменялась свободной копией с меньшим
	// окном — живой случай: num_ctx=8192 ушёл на копию с 4096 на слот, +33 с и
	// reload). Тело уже разобрано здесь, поэтому дополнительного чтения нет.
	NumCtx int
}

// parseRequestBody — читает и разбирает тело запроса ровно один раз.
// Возвращает parsedRequest с извлечёнными model/stream и сырым телом для последующего проксирования.
// Восстанавливает r.Body через NopCloser для повторного чтения в proxyRequest.
func (p *Proxy) parseRequestBody(r *http.Request) *parsedRequest {
	// default stream=true (Ollama behaviour) на legacy-поверхности (18080).
	//
	// R-Image (2026-09-27): на OpenAI-поверхности (18079) такого дефолта НЕТ —
	// стриминг включается только явным полем stream в теле запроса. Иначе
	// запрос без поля stream (обычный случай для image/OpenAI-клиентов) уходил
	// бы в streaming/NDJSON-ветку без таймаута и с чужой обработкой ответа.
	// R484: дефолт surface вынесен в streamDefaultForSurfaceR484; сам разбор
	// поля — один на все пути (streamFlagFromBodyR484).
	defaultStreamR484 := streamDefaultForSurfaceR484(requestSurfaceOf(r))
	result := &parsedRequest{Stream: defaultStreamR484}

	if r.Method != http.MethodPost || r.Body == nil {
		return result
	}

	// R479: тот же лимит тела, что на handler-путях (LB_MAX_REQUEST_BODY_MB).
	// Раньше generic-путь читал тело без предела: длинная история с картинками
	// уходила в память целиком.
	body, err := readRequestBodyLimited(r)
	if errors.Is(err, errBodyTooLarge) {
		result.TooLarge = true
		return result
	}
	if err != nil {
		return result
	}
	r.Body = io.NopCloser(bytes.NewBuffer(body))
	result.RawBody = body

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return result
	}

	if m, ok := req["model"].(string); ok {
		result.Model = m
	}
	// Round 21: also extract "name" field (used by /api/show, /api/pull,
	// /api/delete, /api/copy, /api/create — Ollama-style endpoints).
	// Without this, parsed.Model="" for these endpoints, balancer thinks
	// there's no model to route to, and times out.
	if result.Model == "" {
		if n, ok := req["name"].(string); ok {
			result.Model = n
		}
	}
	if stream, _, ok := streamFlagFromBodyR484(body, defaultStreamR484); ok {
		result.Stream = stream
	}
	// R118.5: окно клиента (options.num_ctx у Ollama, num_ctx/context_length у
	// OpenAI-совместимых тел) — тем же извлекателем, что и в preflight.
	result.NumCtx = ExtractNumCtxFromBody(body)

	// R493 (этап 1): единое решение о запросе — в одной точке, где тело уже
	// разобрано. Пока НИ ОДИН потребитель не переключён на план (поведение не
	// меняется), поэтому сборка безопасна и проверяется тестами.
	// Источник решения о стриме различаем по ФАКТУ поля в теле: streamFlagFromBodyR484
	// сообщает «решение получено», а не «поле было» (поле может отсутствовать, и
	// тогда действует дефолт поверхности).
	streamSource := streamSourceDefaultR493
	if _, isBool := req["stream"].(bool); isBool {
		streamSource = streamSourceClientR493
	}
	outputLimit, outputLimitSource := outputLimitFromBodyR493(body)
	result.Plan = buildRequestPlanR493(
		requestSurfaceOf(r), result.Model, result.Stream, streamSource,
		result.NumCtx, outputLimit, outputLimitSource,
	)
	logRequestPlanR493(result.Plan)

	return result
}
