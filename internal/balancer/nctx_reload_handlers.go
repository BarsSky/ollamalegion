// Package balancer — обработчик n_ctx auto-reload (Stage 3.3).
//
// handleNCtxReload — главная логика, которая вызывается из llamacpp_router.go
// когда proxyRequestLlamaCpp* / handleOpenAIChatCompletions вернул *NCtxError
// (sentinel errors.Is(err, bridge.ErrNCtxNeedsReload)).
//
// Алгоритм:
//  1. Спарсить NCtxBridgeError из *NCtxError (BridgeInfo уже заполнен на 3.1).
//  2. Достать requested_override из body (если был).
//  3. Вызвать nctxReload.DecideReloadBackend(backendID, bridgeErr, requested).
//  4. Plan=DecisionNoOp   → 502 + оригинальная ошибка клиенту (c авторинг-логированием).
//  5. Plan=DecisionReject → 413/503 + RejectMsg JSON (Stage 4).
//  6. Plan=DecisionReload → DoReload(POST /api/models/reload), затем повторить запрос.
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// handleNCtxReload — обрабатывает запрос, где upstream вернул ErrNCtxNeedsReload.
// Вызывается из proxyRequestLlamaCpp / proxyRequestLlamaCppNonStream / handleOpenAIChatCompletions.
//
// ВАЖНО: writer w и request r уже использовались в предыдущей попытке
// (которая провалилась). Поэтому handleNCtxReload НЕ пишет ответ напрямую,
// а возвращает решение, что делать, через status code / header / body.
//
// bodyBuf — оригинальный body запроса (нужен для повтора после reload).
// backendID — ID бэкенда, на котором произошла ошибка.
// modelName — для логирования / RejectMsg.
func (p *Proxy) handleNCtxReload(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	backendID, modelName string,
	origErr error,
	bodyBuf []byte,
) {
	if p.nctxReload == nil {
		// nctxReload не инициализирован (очень ранний edge-case) — пробрасываем оригинал.
		writeNCtxJSONError(w, http.StatusBadGateway, origErr.Error(), "noop")
		return
	}

	// 1. Извлекаем bridgeErr из *NCtxError.
	var bridgeErr *NCtxBridgeError
	var nctxErr *NCtxError
	if errors.As(origErr, &nctxErr) {
		bridgeErr = nctxErr.BridgeInfo
	}

	// 2. Достаём n_ctx override из body (если был задан явно).
	requestedOverride := 0
	if len(bodyBuf) > 0 {
		requestedOverride = ExtractNumCtxFromBody(bodyBuf)
	}

	// 3. Решение координатора.
	plan := p.nctxReload.DecideReloadBackend(backendID, bridgeErr, requestedOverride)
	logger.Get().Debugw("nctx_reload: DecideReloadBackend result",
		"backend", backendID, "model", modelName,
		"decision", plan.Decision.String(),
		"new_n_ctx", plan.NewNCtx,
		"reason", plan.Reason)

	switch plan.Decision {
	case DecisionNoOp:
		p.nctxReload.RecordDecision(backendID, "noop", plan.Reason)
		// Пробрасываем оригинальную ошибку клиенту.
		statusCode := http.StatusBadGateway
		if nctxErr != nil && nctxErr.HTTPStatus > 0 {
			statusCode = nctxErr.HTTPStatus
		}
		writeNCtxJSONError(w, statusCode, origErr.Error(), "noop")

	case DecisionReject:
		p.nctxReload.RecordDecision(backendID, "reject", plan.Reason)
		// Для streaming-клиентов (stream=true в body) нужен NDJSON-формат ошибки,
		// а не plain JSON. Иначе OpenWebUI/Cline получают HTTP 413 и не понимают
		// что это окончание стрима (они ждут NDJSON-чанк с done:true).
		if isNCtxReloadStreaming(r) {
			// HTTP 413 Payload Too Large (числовая константа для совместимости с go 1.24 stub-режимом)
			statusCode := 413
			if bridgeErr == nil || bridgeErr.MaxVRAMNCtx <= 0 {
				statusCode = http.StatusServiceUnavailable
			}
			writeStreamingRejectNDJSON(w, statusCode, modelName, plan, bridgeErr)
		} else {
			writeNCtxRejectResponse(w, plan, bridgeErr)
		}

	case DecisionReload:
		p.handleNCtxReloadActual(ctx, w, r, backendID, modelName, plan, bodyBuf)
	}
}

// handleNCtxReloadActual — выполняет reload на бэкенде и повторяет запрос.
func (p *Proxy) handleNCtxReloadActual(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	backendID, modelName string,
	plan *ReloadPlan,
	bodyBuf []byte,
) {
	// 1. Получаем backend (для формирования backendAddr).
	p.mu.RLock()
	backendState, exists := p.backends[backendID]
	p.mu.RUnlock()
	if !exists || backendState == nil || backendState.Backend == nil {
		p.nctxReload.RecordError(backendID, "backend not found")
		writeNCtxJSONError(w, http.StatusInternalServerError, "backend not found", "reload-failed")
		return
	}
	backend := backendState.Backend
	backendAddr := fmt.Sprintf("http://%s:%d", backend.Host, p.getBackendPort(backend))

	// 2. Выполняем reload синхронно.
	start := time.Now()
	p.nctxReload.RecordDecision(backendID, "reload-start", plan.Reason)
	reloadErr := p.nctxReload.DoReload(ctx, backendID, backendAddr, modelName, plan, p.newNCtxReloadHTTPClient())
	duration := time.Since(start)
	p.nctxReload.RecordReloadDuration(backendID, duration, reloadErr)
	if reloadErr != nil {
		logger.Get().Errorw("nctx_reload: DoReload failed",
			"backend", backendID, "model", modelName, "error", reloadErr)
		p.nctxReload.RecordError(backendID, reloadErr.Error())
		writeNCtxJSONError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("n_ctx auto-reload failed: %v (saved model profile with larger n_ctx and reload manually via /api/profiles)", reloadErr),
			"reload-failed")
		return
	}

	// 3. Reload успешен — повторяем исходный запрос.
	logger.Get().Infow("nctx_reload: reloading done, retrying request",
		"backend", backendID, "model", modelName,
		"new_n_ctx", plan.NewNCtx, "duration_ms", duration.Milliseconds())

	// Прокидываем новый n_ctx в upstream через X-Cpp-Ctx header (см. num_ctx_resolver.go).
	if r.Header == nil {
		r.Header = http.Header{}
	}
	r.Header.Set("X-Cpp-Ctx", strconv.Itoa(plan.NewNCtx))

	// 4. Повторно проксируем. Используем существующие proxyRequestLlamaCpp / proxyRequestLlamaCppNonStream,
	// но с восстановленным body.
	w.Header().Set("X-NCtx-Reload-Decision", "reloaded")
	w.Header().Set("X-NCtx-New-Context", strconv.Itoa(plan.NewNCtx))
	w.Header().Set("X-NCtx-Reload-Duration-Ms", strconv.FormatInt(duration.Milliseconds(), 10))

	// Определяем streaming vs non-streaming из bodyBuf.
	if isStreamingFromBody(r.URL.Path, bodyBuf) {
		_ = p.proxyRequestLlamaCpp(w, r, backendID, bodyBuf)
	} else {
		_ = p.proxyRequestLlamaCppNonStream(w, r, backendID, bodyBuf)
	}
}

// newNCtxReloadHTTPClient — factory для HTTP-клиента reload-а (используется в DoReload).
// Автоматически подхватывает API-токен из конфига балансировщика для аутентификации
// на cppworker (Authorization: Bearer <token>).
//
// ВАЖНО: берём первый токен из Auth.Tokens даже если auth выключен (Auth.Enabled=false).
// Токен нужен для internal-коммуникации с cppworker (reload модели), и это не связано
// с тем, требует ли балансер аутентификации от внешних клиентов.
func (p *Proxy) newNCtxReloadHTTPClient() NCtxReloadHTTPClient {
	token := ""
	if p.config != nil && len(p.config.Auth.Tokens) > 0 {
		token = p.config.Auth.Tokens[0]
	}
	return &DefaultNCtxReloadHTTPClient{
		HTTPClient: &http.Client{Timeout: 90 * time.Second},
		APIToken:   token,
	}
}

// getBackendPort определён в backend_state.go (учитывает engine + CppWorkerPort/OllamaPort).

// writeNCtxJSONError — формирует JSON-ошибку с дополнительным заголовком.
// ЯВНО устанавливает Content-Length, чтобы клиент (OpenWebUI/aiohttp) НЕ получал
// chunked transfer encoding для plain JSON-ошибки. Без Content-Length Go может
// автоматически включить chunked encoding, и aiohttp выдаст TransferEncodingError:
// "Not enough data to satisfy transfer length header".
func writeNCtxJSONError(w http.ResponseWriter, status int, msg, decision string) {
	body, _ := json.Marshal(map[string]interface{}{
		"error":    msg,
		"decision": decision,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-NCtx-Reload-Decision", decision)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeNCtxRejectResponse — формирует HTTP 413/503 ответ с RejectMsg JSON
// (Stage 4). Выбирает 413 если есть max_vram_n_ctx (можно посчитать safe_max),
// иначе 503 (CPU-only, no VRAM info).
// ЯВНО устанавливает Content-Length, чтобы клиент НЕ получал chunked transfer
// encoding (предотвращает TransferEncodingError в aiohttp/OpenWebUI).
func writeNCtxRejectResponse(w http.ResponseWriter, plan *ReloadPlan, bridgeErr *NCtxBridgeError) {
	// HTTP 413 Payload Too Large (числовая константа для совместимости с go 1.24+)
	statusCode := 413
	if bridgeErr == nil || bridgeErr.MaxVRAMNCtx <= 0 {
		// CPU-only или не сообщил max_vram_n_ctx — 503 (Service Unavailable)
		// потому что мы не можем даже посчитать safe_max.
		statusCode = http.StatusServiceUnavailable
	}

	var body []byte
	if plan != nil && plan.RejectMsg != "" {
		body = []byte(plan.RejectMsg)
	} else {
		// Fallback (не должно случаться, но safety net).
		body, _ = json.Marshal(map[string]interface{}{
			"error":    "n_ctx_too_large_for_backend",
			"decision": "reject",
			"reason":   ifEmptyStr(plan, "n_ctx exceeds backend capability"),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-NCtx-Reload-Decision", "reject")
	if plan != nil && plan.NewNCtx > 0 {
		w.Header().Set("X-NCtx-Required", strconv.Itoa(plan.NewNCtx))
	}
	if bridgeErr != nil && bridgeErr.CurrentNCtx > 0 {
		w.Header().Set("X-NCtx-Current", strconv.Itoa(bridgeErr.CurrentNCtx))
	}
	w.WriteHeader(statusCode)
	_, _ = w.Write(body)
}

func ifEmptyStr(p *ReloadPlan, fallback string) string {
	if p == nil || p.Reason == "" {
		return fallback
	}
	return p.Reason
}

// isNCtxReloadStreaming — определяет, что запрос от клиента был streaming
// (т.е. ожидает NDJSON-стрим от OpenAI/Ollama). Используется в handleNCtxReload
// для выбора формата reject-ответа (plain JSON vs NDJSON).
//
// Эвристика (такая же как в isStreamingFromBody):
//   - body содержит "stream": true
//   - ИЛИ Accept: text/event-stream
//   - ИЛИ path = /api/chat (Ollama по умолчанию stream=true)
func isNCtxReloadStreaming(r *http.Request) bool {
	if r == nil {
		return false
	}
	if isStreamingFromBody(r.URL.Path, nil) {
		return true
	}
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "text/event-stream") ||
		strings.Contains(accept, "application/x-ndjson") {
		return true
	}
	return false
}

// bytesReaderBuffer — небольшой helper для преобразования bodyBuf → io.Reader
// (используется внутри handleNCtxReload, чтобы не дублировать логику).
func bytesReaderBuffer(b []byte) io.Reader {
	return bytes.NewReader(b)
}
