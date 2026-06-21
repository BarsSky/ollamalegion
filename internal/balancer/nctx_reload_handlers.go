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
		p.handleNCtxReloadActual(ctx, w, r, backendID, modelName, plan, bodyBuf, bridgeErr)
	}
}

// handleNCtxReloadActual — выполняет reload на бэкенде и повторяет запрос.
// bridgeErr — оригинальная n_ctx ошибка (нужна для writeNCtxRejectResponse при цикле).
func (p *Proxy) handleNCtxReloadActual(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	backendID, modelName string,
	plan *ReloadPlan,
	bodyBuf []byte,
	bridgeErr *NCtxBridgeError,
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

	// 3. Reload успешен — проверяем, не цикл ли это (reload успешен, но retry
	// снова возвращает n_ctx ошибку). Если cycle detected — reject вместо retry.
	logger.Get().Infow("nctx_reload: reloading done, checking for cycles before retry",
		"backend", backendID, "model", modelName,
		"new_n_ctx", plan.NewNCtx, "duration_ms", duration.Milliseconds())

	// Запоминаем reload как попытку. Если retry снова упадёт с n_ctx —
	// следующий вызов handleNCtxReloadActual снова попадёт сюда и увеличит счётчик.
	p.nctxReload.RecordCycleAttempt(backendID)

	// Детекция цикла: 5+ последовательных reload+retry неудач.
	// Лимит 5 (а не 3) даёт запас для tool calling через OpenWebUI,
	// где каждая итерация поиска может генерировать n_ctx overflow.
	if p.nctxReload.IsCycleDetected(backendID, 5) {
		logger.Get().Errorw("nctx_reload: cycle detected after reload, switching to reject",
			"backend", backendID, "model", modelName,
			"n_ctx", plan.NewNCtx, "attempts", 5)
		p.nctxReload.RecordDecision(backendID, "cycle-reject",
			"cycle detected: "+plan.Reason)
		writeNCtxRejectResponse(w, plan, bridgeErr)
		return
	}

	// Прокидываем новый n_ctx в upstream через X-Cpp-Ctx header (см. num_ctx_resolver.go).
	if r.Header == nil {
		r.Header = http.Header{}
	}
	r.Header.Set("X-Cpp-Ctx", strconv.Itoa(plan.NewNCtx))

	// ВАЖНО: удаляем num_ctx из bodyBuf перед retry, чтобы cppworker использовал
	// полный n_ctx перезагруженной модели (plan.NewNCtx). Исходный body содержал
	// старый num_ctx (например, 16384), который cppworker применит вместо нового
	// n_ctx (32768), и retry снова упадёт с code 3.
	//
	// Header X-Cpp-Ctx не может поднять n_ctx, т.к. applyCppCtxHeader — это
	// UPPER LIMIT (только зажимает, если body > header). Поэтому мы чистим
	// num_ctx из body, чтобы cppworker взял n_ctx из контекста загруженной модели.
	bodyBuf = removeNumCtxFromBody(bodyBuf)

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

// removeNumCtxFromBody — удаляет num_ctx из body перед retry после n_ctx reload.
//
// После успешного reload модели на больший n_ctx (plan.NewNCtx), retry должен
// использовать полный контекст перезагруженной модели. Если body содержит
// num_ctx (например, 16384 из OpenWebUI), cppworker будет использовать его
// вместо нового n_ctx (32768), что приведёт к повторной ошибке code 3.
//
// Удаляет:
//   - options.num_ctx (Ollama format)
//   - top-level num_ctx (OpenAI / generic format)
//
// Возвращает исходный bodyBuf, если нечего удалять или JSON невалидный.
func removeNumCtxFromBody(bodyBuf []byte) []byte {
	if len(bodyBuf) == 0 {
		return bodyBuf
	}
	var req map[string]interface{}
	if err := json.Unmarshal(bodyBuf, &req); err != nil {
		return bodyBuf
	}

	modified := false

	// 1) Ollama: options.num_ctx
	if opts, ok := req["options"].(map[string]interface{}); ok {
		if _, exists := opts["num_ctx"]; exists {
			delete(opts, "num_ctx")
			modified = true
			// Если options стал пустым — удаляем и его
			if len(opts) == 0 {
				delete(req, "options")
			}
		}
	}

	// 2) OpenAI / generic: top-level num_ctx
	if _, exists := req["num_ctx"]; exists {
		delete(req, "num_ctx")
		modified = true
	}

	if !modified {
		return bodyBuf
	}

	newBody, err := json.Marshal(req)
	if err != nil {
		logger.Get().Warnw("removeNumCtxFromBody: failed to marshal modified body", "error", err)
		return bodyBuf
	}

	logger.Get().Infow("removeNumCtxFromBody: removed num_ctx from retry body after n_ctx reload",
		"original_size", len(bodyBuf), "new_size", len(newBody))
	return newBody
}

// bytesReaderBuffer — небольшой helper для преобразования bodyBuf → io.Reader
// (используется внутри handleNCtxReload, чтобы не дублировать логику).
func bytesReaderBuffer(b []byte) io.Reader {
	return bytes.NewReader(b)
}
