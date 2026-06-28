package balancer

import (
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// F.0.6 + F.α (2026-06-28): session F — транспортные события для EOF diagnostics.
//
// Контекст: при EOF от upstream cppworker клиент (Cline/OpenWebUI/Roo Code)
// получает "пустой ответ" без какой-либо диагностики. Чтобы оператор мог
// увидеть эти события в WebUI notifications (F.α — SSE endpoint + bell icon),
// публикуем их через существующий EventBus (типы: types.Event).

// publishTransportEOF публикует событие "EOF from upstream" в EventBus.
//
// Параметры:
//   - backendID: ID бэкенда, от которого пришёл EOF (например, "cppworker-gpu-bundled")
//   - model: имя модели из запроса (может быть пустым)
//   - path: путь запроса (например, "/v1/chat/completions")
//   - err: оригинальная ошибка от http.Client.Do()
//   - durationMs: сколько миллисекунд прошло до EOF
//
// Событие имеет severity=warning (EOF часто транзиентный — cppworker
// перезагружал модель), source="transport".
//
// Используется из:
//   - proxyRequestLlamaCpp (streaming SSE/NDJSON)
//   - proxyRequestLlamaCppNonStream (non-streaming JSON)
//   - proxyRequest (legacy Ollama-стиль)
func (p *Proxy) publishTransportEOF(backendID, model, path string, err error, durationMs time.Duration) {
	if err == nil {
		return
	}
	logger.Get().Warnw("Proxy.publishTransportEOF: EOF event",
		"backend", backendID,
		"model", model,
		"path", path,
		"duration_ms", durationMs.Milliseconds(),
		"error", err,
	)
	// Публикуем в EventBus для F.α SSE endpoint + WebUI notifications.
	// EventBus.Publish неблокирующий, поэтому не влияет на latency proxy.
	if p.eventBus != nil {
		p.eventBus.Publish(types.Event{
			Type:      types.EventNotification,
			Timestamp: time.Now(),
			BackendID: backendID,
			Model:     model,
			Severity:  types.SeverityWarning,
			Source:    "transport",
			Message:   "EOF from upstream: " + err.Error(),
			Data: map[string]interface{}{
				"event_kind":  "transport_eof",
				"path":        path,
				"duration_ms": durationMs.Milliseconds(),
				"error":       err.Error(),
			},
		})
	}
}
