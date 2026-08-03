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
	if tracker := backend.ActiveGenerations(); tracker != nil {
		tracker.Add(requestID, userID, modelName, backendID, cancel)
	}
	cleanup := func() {
		// Всегда вызываем Remove, даже если tracker nil-safe.
		backend.ActiveGenerations().Remove(requestID)
	}
	return r.WithContext(ctx), requestID, cleanup
}
