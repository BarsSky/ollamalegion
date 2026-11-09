// ollama_router_admin_fail_r478.go — R478 (2026-11-09): сбой проксирования
// админских ручек Ollama не должен превращаться в 200 с пустым телом.
//
// ЖИВОЙ КЛАСС ДЕФЕКТА (нашёл аудит типов бэкендов). ollama_router_admin.go
// вызывал `or.proxy.proxyRequest(w, r, backendID)` и ИГНОРИРОВАЛ ошибку. А
// proxyRequest при транспортном сбое (бэкенд недоступен, контекст отменён)
// ничего не пишет в ответ. Клиент получал HTTP 200 с пустым телом — ровно
// симптом «ответ не дошёл до клиента», при том что причина была в конкретном
// бэкенде.
//
// ЧТО ЗДЕСЬ. Обёртка, которая запоминает, начал ли обработчик писать ответ, и
// helper, который после ошибки отдаёт 502 JSON с бэкендом и причиной — но
// только если ответ ещё не начат (иначе тело уже нельзя исправить).
package balancer

import (
	"net/http"

	"ollama-loadbalancer/pkg/logger"
)

// writeProbeR478 — ResponseWriter, запоминающий факт записи ответа.
//
// Реализует Flush и Unwrap (http.ResponseController), но НЕ Hijack: админские
// ручки Ollama (_show/_pull/_copy/_delete/_push) hijack не используют, а
// «объявлять Hijack всегда» — та самая ловушка, из-за которой R478 пришлось
// чинить обёртку трассировки.
type writeProbeR478 struct {
	http.ResponseWriter
	wrote int
}

func (w *writeProbeR478) WriteHeader(code int) {
	if w.wrote == 0 {
		w.wrote = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *writeProbeR478) Write(p []byte) (int, error) {
	if w.wrote == 0 {
		w.wrote = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Flush — сохранить стриминг (например, прогресс /api/pull).
func (w *writeProbeR478) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap — для http.ResponseController.
func (w *writeProbeR478) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// proxyAdminOrFailR478 — проксировать админский запрос и честно ответить на
// ошибку, если ответ ещё не начат.
func (or *OllamaRouter) proxyAdminOrFailR478(w http.ResponseWriter, r *http.Request, backendID string) {
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "нет подходящего бэкенда для этого запроса",
		})
		return
	}
	probe := &writeProbeR478{ResponseWriter: w}
	var proxyErr error
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				proxyErr = &adminProxyPanicR478{value: rec}
			}
		}()
		proxyErr = or.proxy.proxyRequest(probe, r, backendID)
	}()
	if proxyErr != nil {
		logger.Get().Warnw("ollama admin: проксирование не удалось",
			"path", r.URL.Path, "backend", backendID, "request_id", getRequestIDFromRequest(r), "error", proxyErr)
		if probe.wrote == 0 {
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error":      proxyErr.Error(),
				"backend":    backendID,
				"request_id": getRequestIDFromRequest(r),
			})
		}
	}
}

// getRequestIDFromRequest — correlation-id запроса (для тела ошибки).
//
// Отдельная функция, а не прямой вызов: заголовок читается в нескольких местах
// и не должен зависеть от регистра/наличия при пустом контексте.
func getRequestIDFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	return RequestIDFromContext(r.Context())
}

// adminProxyPanicR478 — паника внутри обработчика как ошибка, а не как
// оборванный ответ: клиент получает 502 с текстом, лог — строку с бэкендом.
type adminProxyPanicR478 struct{ value interface{} }

func (e *adminProxyPanicR478) Error() string {
	if s, ok := e.value.(string); ok {
		return "panic: " + s
	}
	if err, ok := e.value.(error); ok {
		return "panic: " + err.Error()
	}
	return "panic: unknown value"
}
