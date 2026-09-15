package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// setupCancelTracking — Round 18 P0.2 cancel tracking для inference handlers.
//
// Возвращает:
//   - newR: *http.Request с r.Context() заменённым на child context.
//     Caller ДОЛЖЕН передать newR в downstream-функции (writeChatStreamResponse и т.п.)
//     вместо оригинального r — иначе callback будет смотреть на parent и при отмене
//     child через /api/cancel не увидит ctx.Done().
//   - requestID: либо из X-Request-Id, либо сгенерированный ("chat-<nanos>", "gen-<nanos>" и т.п.).
//   - cleanup: defer cleanup() чтобы снять регистрацию при выходе из handler'а.
//
// КРИТИЧНО (фикс первого P0.2-бага): раньше cancelFunc был от child context, а стрим
// смотрел на parent (r.Context() внутри writeChatStreamResponse). При отмене child
// parent не отменялся — callback в Go не видел ctx.Done() — стрим жил до n_predict.
// С этим хелпером r.WithContext(ctx) гарантирует, что и cancelFunc, и ctx в стриме
// работают с одним и тем же контекстом.
func setupCancelTracking(r *http.Request, modelName, backendID, prefix string) (*http.Request, string, func()) {
	requestID := r.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	userID := r.Header.Get("X-User-Id")
	ctx, cancel := context.WithCancel(r.Context())
	// R63 (2026-09-15): REMOVED `defer cancel()`. Round 60.30 fix ставил defer
	// cancel здесь, что ГАСИЛО контекст сразу после возврата — стрим-фильтр
	// (safeStreamWriter) сразу видел ctx.Done() и ни одного токена не уходило.
	// См. regression: "ctx_done_on_write", tokens_sent=0.
	//
	// Теперь cancel вызывается в cleanup: один источник правды, ровно один
	// раз при выходе handler'а. Если tracker.Add ниже panic'ал — cleanup всё
	// равно вызовет cancel. Контекст-leak fix перенесён с defer-в-setup на
	// defer-via-cleanup (если cleanup забыли — fixed в deferred cancel в
	// handler через cancelCleanup()).
	if tracker := backend.ActiveGenerations(); tracker != nil {
		tracker.Add(requestID, userID, modelName, backendID, cancel)
	}
	cleanup := func() {
		// Гарантированно отменяем child ctx при выходе handler'а —
		// это и бывший defer cancel, и новый leak-fix.
		cancel()
		// Всегда вызываем Remove.
		if tracker := backend.ActiveGenerations(); tracker != nil {
			tracker.Remove(requestID)
		}
	}
	return r.WithContext(ctx), requestID, cleanup
}
