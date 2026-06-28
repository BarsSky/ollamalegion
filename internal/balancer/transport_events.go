package balancer

import (
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// F.0.6 (2026-06-28): session F — транспортные события для EOF diagnostics.
//
// Контекст: при EOF от upstream cppworker клиент (Cline/OpenWebUI/Roo Code)
// получает "пустой ответ" без какой-либо диагностики. Чтобы оператор мог
// увидеть эти события в WebUI notifications (после реализации F.α), публикуем
// их через единый helper.
//
// Текущая реализация — no-op с structured-логированием. После реализации
// F.α (EventPublisher) добавим вызов publish() в publishTransportEOF ниже.

// publishTransportEOF публикует событие "EOF from upstream" в EventPublisher.
//
// Параметры:
//   - backendID: ID бэкенда, от которого пришёл EOF (например, "cppworker-gpu-bundled")
//   - model: имя модели из запроса (может быть пустым)
//   - path: путь запроса (например, "/v1/chat/completions")
//   - err: оригинальная ошибка от http.Client.Do()
//   - durationMs: сколько миллисекунд прошло до EOF
//
// Событие имеет severity=warning (не error, потому что EOF часто транзиентный —
// cppworker перезагружал модель), source="transport" (отличает от других источников).
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
	// F.α: после реализации EventPublisher раскомментировать:
	// if p.eventBus != nil {
	//     p.eventBus.Publish(TransportEvent{
	//         Type:      EventTransportEOF,
	//         BackendID: backendID,
	//         Model:     model,
	//         Path:      path,
	//         Error:     err.Error(),
	//         DurationMs: durationMs.Milliseconds(),
	//         Ts:        time.Now(),
	//     })
	// }
}