// upstream_error_response.go — R83-fix (2026-09-30).
//
// ПРОБЛЕМА (живой репорт). Cline при недоступном cppworker получал от балансера
// HTTP 502 с сырым текстом транспорта:
//
//	{"error":"llama.cpp request failed [backend=cppworker-gpu-bundled-agent,
//	 attempt=2, duration_ms=1120, error_type=connection_refused]:
//	 Post \"http://cppworker-gpu:18092/api/chat\": dial tcp 172.23.0.5:18092:
//	 connect: connection refused"}
//
// Cline классифицирует такую ошибку как проблему КОНТЕКСТА: показывает
// «context window exceeded — compacting and retrying», сжимает историю, повторяет
// — и на повторе получает уже настоящий 413 preflight («prompt + n_predict
// exceeds n_ctx»), после чего пишет «conversation still exceeds the model's
// context window». То есть падение бэкенда маскировалось под переполнение окна,
// и оператор искал причину не там.
//
// РЕШЕНИЕ. Классифицируем транспортные ошибки и отдаём клиенту то, что он умеет
// понимать: 503 + Retry-After для «узел недоступен» (повтори позже) и 504 для
// таймаута, с машиночитаемым телом (backend_id, error_type, retry_after).
// Сырой dial-текст остаётся в логе балансера и в поле detail — он нужен
// оператору, но не должен быть единственным, что видит клиент.
package balancer

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// upstreamUnavailableRetryAfter — сколько секунд советуем подождать клиенту,
// если backend недоступен. Cppworker поднимается за секунды (контейнер
// рестартует), поэтому 30 c — разумный первый шаг; клиенты (Cline/OpenWebUI)
// уважают Retry-After и не устраивают storm.
const upstreamUnavailableRetryAfter = 30 * time.Second

// upstreamErrorStatus — HTTP-статус по классу транспортной ошибки.
//
// ОДНО место для статуса и для тела ответа (writeUpstreamError) и для метрик
// image-плоскости: иначе «в ленте 504, а клиенту 502» — классический разъезд,
// из-за которого оператор ищет причину не там.
func upstreamErrorStatus(errType string) int {
	switch errType {
	case "connection_refused", "network_unreachable", "dns_error":
		return http.StatusServiceUnavailable
	case "timeout", "context_deadline":
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

// UpstreamErrorStatus — экспорт для других плоскостей (image-роутер обязан
// классифицировать транспортные ошибки так же, как текстовая сторона).
func UpstreamErrorStatus(errType string) int { return upstreamErrorStatus(errType) }

// UpstreamErrorType — экспорт classifyUpstreamError для image-плоскости.
func UpstreamErrorType(err error) string { return classifyUpstreamError(err) }

// writeUpstreamError — отдать клиенту понятную ошибку вместо сырого текста.
//
// isHeadersSent(w) обязателен: если стрим уже начался, заголовки менять нельзя —
// вызывающий в этом случае пишет финальный done-чанк (writeStreamErrorChunk).
func writeUpstreamError(w http.ResponseWriter, backendID, model string, err error) {
	if w == nil || err == nil {
		return
	}
	errType := classifyUpstreamError(err)
	body := map[string]interface{}{
		"error_type": errType,
		"backend_id": backendID,
		"model":      model,
		"detail":     err.Error(),
	}
	switch upstreamErrorStatus(errType) {
	case http.StatusServiceUnavailable:
		retrySec := int(upstreamUnavailableRetryAfter.Seconds())
		body["error"] = "backend " + backendID + " is unavailable (" + errType + "); " +
			"the model is not being served right now — retry in " + strconv.Itoa(retrySec) + "s"
		body["retry_after"] = retrySec
		body["hint"] = "узел не отвечает: проверьте контейнер (docker ps/logs) и статус бэкенда в WebUI"
		w.Header().Set("Retry-After", strconv.Itoa(retrySec))
		writeJSON(w, http.StatusServiceUnavailable, body)
	case http.StatusGatewayTimeout:
		retrySec := int(upstreamUnavailableRetryAfter.Seconds())
		body["error"] = "backend " + backendID + " timed out; retry in " + strconv.Itoa(retrySec) + "s"
		body["retry_after"] = retrySec
		body["hint"] = "кап времени на стороне клиента/прокси (по умолчанию капов нет — мы ждём " +
			"терминального состояния: см. docs/image-generation.md §17)"
		w.Header().Set("Retry-After", strconv.Itoa(retrySec))
		writeJSON(w, http.StatusGatewayTimeout, body)
	default:
		body["error"] = err.Error()
		body["hint"] = "бэкенд ответил ошибкой транспорта: смотрите detail и логи бэкенда"
		writeJSON(w, http.StatusBadGateway, body)
	}
}

// classifyUpstreamError — та же классификация, что уже используется в транспорте
// (determineErrorType), но без зависимости от контекста запроса: нам нужен только
// тип ошибки для выбора HTTP-статуса.
func classifyUpstreamError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case containsFold(msg, "connection refused"):
		return "connection_refused"
	case containsFold(msg, "no such host"), containsFold(msg, "server misbehaving"):
		return "dns_error"
	case containsFold(msg, "network is unreachable"), containsFold(msg, "no route to host"):
		return "network_unreachable"
	case containsFold(msg, "context deadline exceeded"), containsFold(msg, "Client.Timeout"):
		return "timeout"
	case containsFold(msg, "unexpected EOF"), containsFold(msg, "EOF"):
		return "unexpected_eof"
	}
	if t := determineErrorType(err, context.Background()); t != "" && t != "unknown" {
		return t
	}
	return "upstream_error"
}
