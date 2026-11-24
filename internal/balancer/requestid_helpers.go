package balancer

import (
	"context"
)

// fallbackRequestID — request_id для мест, где ctx HTTP-запроса не пробрасывается
// (poll-цикл, инициализация, тесты). Значение КОНСТАНТНО, поэтому и хранить его
// негде: контекст создаётся на каждый вызов.
const fallbackRequestID = "norequest"

// lr_recentCtx — singleton fallback context с фиксированным request_id
// "norequest", используемым в LlamaCppRouter-функциях, которые не получают
// контекст HTTP-запроса напрямую (например, фоновая задача, не инициированная
// HTTP-вызовом). Логи из таких функций помечаются явно, чтобы их можно было
// отличить от логов, привязанных к конкретному Cline-запросу.
//
// ВАЖНО: эта fallback нужен ТОЛЬКО для совместимости с типизированным API
// ridLog(ctx). Реальные request-aware функции должны брать ctx из
// *http.Request.Context() и передавать его вниз.
//
// R735 (2026-11-24): РАНЬШЕ ЗДЕСЬ БЫЛА ПАКЕТНАЯ ПЕРЕМЕННАЯ И «РОТАЦИЯ».
// Код хранил `lr_recentCtxValue`/`lr_recentCtxRotatedAt` и раз в 5 минут
// перезаписывал их прямо внутри lr_recentCtx — без мьютекса. Это дало гонку,
// которую поймал CI (job «Test (self-hosted Windows)», прогон 45307667015):
//
//	WARNING: DATA RACE
//	Write at 0x… by goroutine: balancer.lr_recentCtx() requestid_helpers.go:28
//	Previous read at 0x… by goroutine: balancer.lr_recentCtx() requestid_helpers.go:31
//	--- FAIL: TestOpenWebUI_Sequential_MixedRequests/step6-llamacpp-non-streaming
//	    testing.go:1617: race detected during execution of test
//
// При этом «ротация» присваивала РОВНО ТО ЖЕ значение ("norequest"), то есть не
// меняла поведение, а только создавала запись в общую память на каждом вызове
// после пятой минуты. Поэтому вместо мьютекса убрано само состояние: значение
// константно, а контекст — новый объект на каждый вызов. Страж —
// internal/balancer/requestid_helpers_race_test.go.
func lr_recentCtx() context.Context {
	return context.WithValue(context.Background(), requestIDKey{}, fallbackRequestID)
}
