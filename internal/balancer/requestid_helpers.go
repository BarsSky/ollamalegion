package balancer

import (
	"context"
	"time"
)

// lr_recentCtx — singleton fallback context с фиксированным request_id
// "norequest", используемым в LlamaCppRouter-функциях, которые не получают
// контекст HTTP-запроса напрямую (например, фоновая задача, не инициированная
// HTTP-вызовом). Логи из таких функций помечаются явно, чтобы их можно было
// отличить от логов, привязанных к конкретному Cline-запросу.
//
// ВАЖНО: эта fallback нужен ТОЛЬКО для совместимости с типизированным API
// ridLog(ctx). Контекст периодически ротируется, чтобы не накапливать
// нерелевантный request_id в длительных background-циклах.
var lr_recentCtxValue = "norequest"
var lr_recentCtxRotatedAt = time.Now()

// lr_recentCtx возвращает context с placeholder request_id.
// Реальные request-aware функции должны брать ctx из *http.Request.Context()
// и передавать его вниз. Эта helper нужна только в местах, где ctx пока
// не пробрасывается (poll-цикл, инициализация, тесты).
func lr_recentCtx() context.Context {
	// Ротация каждые 5 минут — чтобы лог-trail старых background-операций
	// было легко отличить от свежих.
	if time.Since(lr_recentCtxRotatedAt) > 5*time.Minute {
		lr_recentCtxValue = "norequest"
		lr_recentCtxRotatedAt = time.Now()
	}
	return context.WithValue(context.Background(), requestIDKey{}, lr_recentCtxValue)
}