// preflight_stream_dialog.go — Round 34 (2026-08-12) Phase 1: stream dialog
// with heartbeats during reload.
//
// Проблема: при async preflight reload (LB_NCTX_PREFLIGHT_ASYNC_RELOAD=true)
// balancer отдаёт 503+Retry-After для non-streaming клиентов, но для
// streaming клиентов (Cline, OpenWebUI) это плохой UX — клиент должен
// прервать stream и ретраить, ломая любой ongoing conversation.
//
// Решение: при PreflightAsyncReload И streaming клиент, balancer:
//  1. Открывает chunked SSE response (200, Content-Type: text/event-stream)
//  2. Spawn'ит goroutine которая пишет keepalive каждые 5 сек:
//     `data: {"object":"chat.completion.chunk","choices":[],"created":N,"model":M}\n\n`
//     Клиент получает "alive" сигнал, connection не таймаутится.
//  3. Sync ждёт завершения reload через NCtxReloadCoordinator.WaitReloadDone
//     (есть уже, dedup по backendID+modelName — параллельные reload'ы
//     коалесцируются).
//  4. После reload success — пишет [DONE] и закрывает connection. Клиент
//     может ретраить, увидит [DONE], и retry уже попадёт в NoOp preflight
//     (state fits после reload).
//
// Env: LB_PREFLIGHT_STREAM_DIALOG (default true) — opt-out для тестов.
//      LB_PREFLIGHT_STREAM_DIALOG_KEEPALIVE_SEC (default 5) — период keepalive.
package balancer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// streamDialogConfig — настройки stream dialog mode. Читаются из env при
// каждом вызове (Round 34 не вводит config.json поле — env-only для
// быстрого rollback).
type streamDialogConfig struct {
	enabled         bool
	keepalivePeriod time.Duration
	waitTimeout     time.Duration
}

// getStreamDialogCfg — читает env каждый раз (env-only config).
func getStreamDialogCfg() streamDialogConfig {
	cfg := streamDialogConfig{
		enabled:         true, // default ON
		keepalivePeriod: 5 * time.Second,
		waitTimeout:     5 * time.Minute,
	}
	if v, ok := os.LookupEnv("LB_PREFLIGHT_STREAM_DIALOG"); ok {
		switch v {
		case "false", "0", "no":
			cfg.enabled = false
		case "true", "1", "yes":
			cfg.enabled = true
		}
	}
	if v, ok := os.LookupEnv("LB_PREFLIGHT_STREAM_DIALOG_KEEPALIVE_SEC"); ok {
		var sec int
		if _, err := fmt.Sscanf(v, "%d", &sec); err == nil && sec > 0 {
			cfg.keepalivePeriod = time.Duration(sec) * time.Second
		}
	}
	return cfg
}

// IsStreamDialogEnabled — геттер для тестов / opt-out проверки.
func IsStreamDialogEnabled() bool {
	return getStreamDialogCfg().enabled
}

// runInferencePreflightStreamDialog — для streaming клиента при
// PreflightAsyncReload decision'е, держит connection open и пишет
// keepalive каждые 5 сек пока async reload в фоне завершается.
//
// Returns true если stream dialog был использован (handled=true).
//
// ОГРАНИЧЕНИЯ:
//   - Только streaming clients (isStreamingFromBody).
//   - lb_preflight_async_reload=true.
//   - Не пытается рекурсивно stream-dialog (если после reload params
//     снова не match — fallback на обычный 503+Retry-After через preflight).
func (lr *LlamaCppRouter) runInferencePreflightStreamDialog(
	args runInferencePreflightArgs,
	decision PreflightDecision,
	res *PreflightResult,
) bool {
	cfg := getStreamDialogCfg()
	if !cfg.enabled {
		return false
	}
	if decision != PreflightAsyncReload {
		return false
	}
	if !isStreamingFromBody(args.r.URL.Path, args.body) {
		logger.Get().Debugw("preflight stream dialog: client is not streaming, skipping (will return 503)",
			"backend", args.backendID, "path", args.r.URL.Path, "body_len", len(args.body))
		return false
	}
	if lr == nil || lr.proxy == nil || lr.proxy.nctxReload == nil {
		return false
	}
	if res == nil || res.TargetNCtx <= 0 {
		return false
	}

	ctx := args.r.Context()
	waitTimeout := cfg.waitTimeout
	if h, ok := ctx.Deadline(); ok {
		if remaining := time.Until(h); remaining > 0 && remaining < waitTimeout {
			waitTimeout = remaining
		}
	}

	modelName := args.model

	logger.Get().Infow("preflight stream dialog: starting keepalive during async reload",
		"backend", args.backendID, "model", modelName,
		"target_n_ctx", res.TargetNCtx, "keepalive_period", cfg.keepalivePeriod,
		"wait_timeout", waitTimeout)

	// Open stream headers via hijack (manual HTTP/1.1 chunked response).
	hijacker, ok := args.w.(http.Hijacker)
	if !ok {
		// Can't hijack (HTTP/2, etc) — fallback to plain flush.
		return lr.runInferencePreflightStreamDialogPlain(args, res, modelName, cfg)
	}
	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		logger.Get().Warnw("preflight stream dialog: hijack failed",
			"backend", args.backendID, "error", err)
		return false
	}
	defer conn.Close()

	// Write 200 OK + headers manually.
	if err := writeStreamDialogHeaders(bufrw, args.r); err != nil {
		logger.Get().Warnw("preflight stream dialog: write headers failed",
			"backend", args.backendID, "error", err)
		return false
	}

	// Spawn keepalive goroutine.
	keepaliveCtx, cancelKeepalive := context.WithCancel(ctx)
	defer cancelKeepalive()
	var keepaliveWG sync.WaitGroup
	keepaliveWG.Add(1)
	go func() {
		defer keepaliveWG.Done()
		streamDialogKeepaliveLoop(bufrw, keepaliveCtx, args.backendID, modelName, res.TargetNCtx, cfg)
	}()

	// Wait for async reload to complete (or context canceled).
	reloadErr := lr.proxy.nctxReload.WaitReloadDone(args.backendID, modelName, waitTimeout)
	if reloadErr != nil {
		logger.Get().Warnw("preflight stream dialog: reload wait failed/timed out",
			"backend", args.backendID, "model", modelName, "error", reloadErr,
			"wait_timeout", waitTimeout)
		// Send error final message and close.
		writeStreamDialogFinalError(bufrw, fmt.Sprintf(
			"reload timed out after %s, please retry", waitTimeout))
		_ = bufrw.Flush()
		return true
	}

	// Reload done — stop keepalives.
	cancelKeepalive()
	keepaliveWG.Wait()

	logger.Get().Infow("preflight stream dialog: reload done, signaling client to retry",
		"backend", args.backendID, "model", modelName, "target_n_ctx", res.TargetNCtx)

	// Write [DONE] marker — client knows to retry. On retry, state fits (NoOp preflight).
	writeStreamDialogReloadDone(bufrw, res.TargetNCtx)
	_ = bufrw.Flush()
	return true
}

// runInferencePreflightStreamDialogPlain — fallback когда hijack недоступен.
// Использует Go's standard ResponseWriter с chunked transfer encoding.
func (lr *LlamaCppRouter) runInferencePreflightStreamDialogPlain(
	args runInferencePreflightArgs,
	res *PreflightResult,
	modelName string,
	cfg streamDialogConfig,
) bool {
	w := args.w
	ctx := args.r.Context()

	// Set headers for SSE.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flusher — close connection.
		return false
	}

	// Wait for reload in goroutine; keepalive ticker.
	_, cancelKeepalive := context.WithCancel(ctx)
	defer cancelKeepalive()

	ticker := time.NewTicker(cfg.keepalivePeriod)
	defer ticker.Stop()

	// Wait for reload OR keepalive fires.
	reloadDone := make(chan struct{})
	go func() {
		if err := lr.proxy.nctxReload.WaitReloadDone(args.backendID, modelName, cfg.waitTimeout); err == nil {
			close(reloadDone)
		}
	}()

	for {
		select {
		case <-reloadDone:
			// Write final SSE comment to signal client to retry.
			fmt.Fprintf(w, ": reload_done\n\n")
			flusher.Flush()
			return true
		case <-ctx.Done():
			fmt.Fprintf(w, ": client_canceled\n\n")
			flusher.Flush()
			return true
		case <-ticker.C:
			keepalive := streamDialogKeepaliveChunk(modelName, res.TargetNCtx)
			fmt.Fprintf(w, "%s\n", keepalive)
			flusher.Flush()
		}
	}
}

// writeStreamDialogHeaders — write HTTP/1.1 200 OK + SSE headers.
func writeStreamDialogHeaders(bufrw *bufio.ReadWriter, r *http.Request) error {
	proto := "HTTP/1.1"
	if r.ProtoMajor == 2 {
		proto = "HTTP/2.0"
	}
	headers := []string{
		proto + " 200 OK",
		"Content-Type: text/event-stream",
		"Cache-Control: no-cache",
		"Connection: keep-alive",
		"X-Accel-Buffering: no",
		"X-Round-34-Stream-Dialog: keepalive-during-reload",
		"",
		"",
	}
	for _, h := range headers {
		if _, err := bufrw.WriteString(h + "\r\n"); err != nil {
			return err
		}
	}
	return bufrw.Flush()
}

// streamDialogKeepaliveChunk — format keepalive JSON.
func streamDialogKeepaliveChunk(modelName string, targetNCtx int) string {
	keepalive := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-keepalive-%d", time.Now().UnixMilli()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []interface{}{},
		"x_round_34_keepalive": map[string]interface{}{
			"target_n_ctx": targetNCtx,
			"phase":        "preload",
		},
	}
	b, _ := json.Marshal(keepalive)
	return fmt.Sprintf("data: %s\n\n", b)
}

// streamDialogKeepaliveLoop — пишет keepalive каждые keepalivePeriod до cancel.
func streamDialogKeepaliveLoop(
	bufrw *bufio.ReadWriter,
	ctx context.Context,
	backendID, modelName string,
	targetNCtx int,
	cfg streamDialogConfig,
) {
	start := time.Now()
	ticker := time.NewTicker(cfg.keepalivePeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			chunk := map[string]interface{}{
				"id":      fmt.Sprintf("chatcmpl-keepalive-%d", time.Now().UnixMilli()),
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   modelName,
				"choices": []interface{}{},
				"x_round_34_keepalive": map[string]interface{}{
					"target_n_ctx": targetNCtx,
					"elapsed_ms":   time.Since(start).Milliseconds(),
					"phase":        "preload",
				},
			}
			b, _ := json.Marshal(chunk)
			if _, err := bufrw.WriteString(fmt.Sprintf("data: %s\n\n", b)); err != nil {
				return
			}
			if err := bufrw.Flush(); err != nil {
				return
			}
			logger.Get().Debugw("preflight stream dialog: keepalive sent",
				"backend", backendID, "elapsed_ms", time.Since(start).Milliseconds())
		}
	}
}

// writeStreamDialogReloadDone — пишет SSE marker что reload завершён.
// Клиент получает [DONE] и понимает что нужно retry. На retry state fits → NoOp.
func writeStreamDialogReloadDone(bufrw *bufio.ReadWriter, targetNCtx int) {
	// Специальный event с marker'ом и [DONE] terminator.
	marker := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-reload-done-%d", time.Now().UnixMilli()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"choices": []interface{}{},
		"x_round_34_reload_done": map[string]interface{}{
			"target_n_ctx": targetNCtx,
			"phase":        "ready",
			"client_action": "retry",
		},
	}
	b, _ := json.Marshal(marker)
	bufrw.WriteString(fmt.Sprintf("data: %s\n\n", b))
	// [DONE] signals end of stream. Client should retry.
	bufrw.WriteString("data: [DONE]\n\n")
	_ = bufrw.Flush()
}

// writeStreamDialogFinalError — write SSE error event + [DONE].
func writeStreamDialogFinalError(bufrw *bufio.ReadWriter, msg string) {
	errObj, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    "preflight_stream_dialog_timeout",
		},
	})
	bufrw.WriteString(fmt.Sprintf("data: %s\n\n", errObj))
	bufrw.WriteString("data: [DONE]\n\n")
	_ = bufrw.Flush()
}

// suppress unused import warnings if conn is referenced in future refactor.
var _ net.Conn
