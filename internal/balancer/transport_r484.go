// transport_r484.go — R484 (2026-11-10): каким транспортом ответили на самом деле.
//
// ЗАЧЕМ. В прокси-логах поле stream означает ФЛАГ ЗАПРОСА, а не транспорт:
// запрос без поля stream на legacy-поверхности считается стримовым (true), но
// транспорт llama.cpp уходит в non-stream, если stream не задан явно (R484:
// isStreamingFromBody, default false). Оператор видел stream=true и искал
// «почему не стримится» в неверном месте — ровно эта ложь была в живой жалобе
// «всё одним куском» (R482/R483).
//
// ПРАВИЛО. transport — фактический путь ответа:
//
//	"stream" — потоковый транспорт (NDJSON/SSE, запись по чанкам);
//	"json"   — ответ прочитан и отдан целиком (Content-Length).
//
// Трекер в контексте запроса, как stage R477: выставляют транспортные функции
// (proxyRequestLlamaCpp / NonStream / passthrough / generic-ветка), читают строка
// request trace и запись прокси-лога.
package balancer

import (
	"context"
	"net/http"
	"sync"
)

const (
	transportStreamR484 = "stream"
	transportJSONR484   = "json"
)

// transportCtxKeyR484 — ключ контекста (свой тип, чтобы не пересечься).
type transportCtxKeyR484 struct{}

// transportTrackerR484 — выбранный транспорт одного запроса.
type transportTrackerR484 struct {
	name string
	mu   sync.Mutex
}

// withTransportTrackerR484 — положить трекер в контекст запроса.
func withTransportTrackerR484(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), transportCtxKeyR484{}, &transportTrackerR484{}))
}

func transportTrackerFromCtxR484(ctx context.Context) *transportTrackerR484 {
	if ctx == nil {
		return nil
	}
	if t, ok := ctx.Value(transportCtxKeyR484{}).(*transportTrackerR484); ok {
		return t
	}
	return nil
}

// SetTransportR484 — отметить транспорт. Безопасно без трекера.
func SetTransportR484(ctx context.Context, name string) {
	t := transportTrackerFromCtxR484(ctx)
	if t == nil || name == "" {
		return
	}
	t.mu.Lock()
	t.name = name
	t.mu.Unlock()
}

// TransportR484 — транспорт (пусто, если запрос не дошёл до транспорта).
func TransportR484(ctx context.Context) string {
	t := transportTrackerFromCtxR484(ctx)
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.name
}
