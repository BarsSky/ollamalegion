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
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
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
// Автоматически подхватывает API-токен и имя header'а из конфига балансировщика для
// аутентификации на cppworker (по умолчанию X-API-Token).
//
// ВАЖНО: берём первый токен из Auth.Tokens даже если auth выключен (Auth.Enabled=false).
// Токен нужен для internal-коммуникации с cppworker (reload модели), и это не связано
// с тем, требует ли балансер аутентификации от внешних клиентов.
//
// R60.6 (2026-09-07): http.Client.Timeout теперь derived from
// cfg.effectiveTimeout() вместо hardcoded 90s. Иначе http.Client убьёт
// reload на 90s, даже если cppworker получил ?waitTimeoutSec=300.
// Симптом: на RTX 3070 8GB reload 5GB модели + 131072 n_ctx занимает
// 60-180s, http.Client.Timeout=90s отменяет request с "Client.Timeout
// exceeded while awaiting headers", reload fails, LastKnownNCtx не
// обновляется, следующий preflight снова триггерит reload (cascading).
func (p *Proxy) newNCtxReloadHTTPClient() NCtxReloadHTTPClient {
	token := ""
	headerName := "X-API-Token" // default
	if p.config != nil {
		if len(p.config.Auth.Tokens) > 0 {
			token = p.config.Auth.Tokens[0]
		}
		if p.config.Auth.HeaderName != "" {
			headerName = p.config.Auth.HeaderName
		}
	}
	// Override with LB_API_TOKEN env var if set (config.json has placeholder token,
	// actual token comes from compose env). The cppworker uses the same token from
	// API_TOKEN env var, so they must match for /api/models/reload auth.
	if envToken := os.Getenv("LB_API_TOKEN"); envToken != "" {
		token = envToken
	}
	// R60.6: derive http.Client.Timeout from nctxReload config. Default 300s,
	// max 600s (clamped). +30s buffer для HTTP overhead (cppworker шлёт headers
	// после завершения reload, balancer должен успеть их прочитать).
	timeout := 300 * time.Second // R60.6: was 90s
	if p.nctxReload != nil {
		cfg := p.nctxReload.Config()
		timeout = cfg.effectiveTimeout() + 30*time.Second
		if timeout > 600*time.Second {
			timeout = 600 * time.Second
		}
	}
	return &DefaultNCtxReloadHTTPClient{
		HTTPClient: &http.Client{Timeout: timeout},
		APIToken:   token,
		HeaderName: headerName,
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

// preflightNCtxReloadIfNeeded — проверяет ПЕРЕД проксированием, что loaded n_ctx
// на бэкенде покрывает запрашиваемый клиентом num_ctx. Если нет —
// АСИНХРОННО запускает POST /api/models/reload с требуемым n_ctx в фоне
// и возвращает клиенту HTTP 503 + Retry-After.
//
// ЗАЧЕМ: при первом запросе от OpenWebUI с tools (system + tool definitions
// ~3000-5000 токенов) клиент посылает options.num_ctx=16384, а модель
// загружена с n_ctx=4096 (default cppworker). Без preflight — cppworker
// сразу возвращает code 2 (ErrNCtxNeedsReload), балансер делает reload
// по факту ошибки, модель выгружается и загружается заново (10-30 сек).
//
// ВАЖНО (2026-06-22): preflight БОЛЬШЕ НЕ ДЕЛАЕТ синхронный reload.
// Раньше здесь был блокирующий POST /api/models/reload, который:
//   - выгружал текущую модель из VRAM на 10-30 сек,
//   - параллельные запросы получали 500 с "handle is nil",
//   - streaming-клиент получал обрыв без полного ответа.
//
// Теперь reload запускается в горутине, а клиент сразу получает
// 503 Service Unavailable с Retry-After: 5. Клиент (OpenWebUI, Cline, curl)
// делает повторный запрос через 5 секунд — к этому моменту reload либо
// уже завершён, либо ещё идёт (тогда клиент получит 503 ещё раз).
// Стрим НЕ обрывается посередине.
//
// Возвращает:
//   - modifiedBody = bodyBuf (если reload не нужен) —
//     рекомендуется проксировать с этим body
//   - needsProxy = true если можно проксировать (reload не нужен)
//   - needsProxy = false если требуется reload (клиенту возвращён 503)
//   - errorMessage = "" если ОК, иначе сообщение об ошибки
//   - statusCode = http.StatusOK если можно проксировать,
//     http.StatusServiceUnavailable если запущен reload
func (p *Proxy) preflightNCtxReloadIfNeeded(
	ctx context.Context,
	backendID, modelName string,
	bodyBuf []byte,
	requestPath string,
) (modifiedBody []byte, needsProxy bool, errMsg string, statusCode int) {
	if p.nctxReload == nil || len(bodyBuf) == 0 {
		return bodyBuf, true, "", http.StatusOK
	}

	// R60.6 (2026-09-07): IsReloadPending guard. Если reload для этой модели
	// уже в процессе (через новый coordinator dedup path в RunPreflight),
	// НЕ запускаем второй reload — просто возвращаем 503. Без этого
	// stream-запросы (которые идут через этот OLD path) триггерят cascading
	// reload goroutines, каждая из которых отменяется http.Client.Timeout
	// на 90s. Client видит 503 → retry → 503 → retry → 5 reloads в логе.
	if p.nctxReload.IsReloadPending(backendID, modelName) {
		logger.Get().Infow("preflightNCtxReloadIfNeeded: reload already pending (R60.6 dedup)",
			"backend", backendID, "model", modelName, "request_path", requestPath)
		return bodyBuf, false,
			fmt.Sprintf("model %q reload already in progress, retry later", modelName),
			http.StatusServiceUnavailable
	}

	// Не делаем preflight-reload для streaming-запросов.
	// Причина: если запустить reload посреди стрима (или прямо перед ним),
	// текущий стрим оборвётся с "handle is nil", а следующий вызов preflight
	// зависнет в ожидании reload. Клиент получит обрыв ответа.
	//
	// Для streaming-клиентов n_ctx reload обрабатывается в handleNCtxReload
	// ПОСЛЕ получения ошибки ErrNCtxNeedsReload от cppworker (там уже есть
	// streaming-aware retry-логика).
	if isStreamingFromBody("", bodyBuf) {
		return bodyBuf, true, "", http.StatusOK
	}

	// 1. Извлекаем requested n_ctx: body > profile > backend.
	//
	// Round 35c+ (2026-08-13): раньше использовался только ExtractNumCtxFromBody
	// который читал num_ctx ТОЛЬКО из body. Это приводило к reload на 8192 если
	// клиент (Open WebUI) слал num_ctx=8192 по дефолту, даже если profile говорил
	// 32768 — потому что first-load preflight не видел fallback'а на profile.
	//
	// НОВАЯ ЛОГИКА (Round 35c+, исправленная): мы НЕ используем ResolveNumCtx
	// напрямую, потому что он clamping'ит body num_ctx к maxNumCtxForModel.
	// Это ломает smart-skip (см. баг cdae3e4 → 7b50 commit): если body=16384
	// и loaded=4096, resolver возвращает 4096 (clamped), preflight видит
	// loaded >= requested, НЕ patching body — и cppworker получает body с
	// num_ctx=16384 при загруженной модели с 4096 → error code 2 → reload loop.
	//
	// Вместо этого: body имеет АБСОЛЮТНЫЙ приоритет (клиент знает что хочет).
	// Fallback на profile/backend только если body НЕ задал num_ctx — это
	// гарантирует first-load с правильным ctx (32768 из profile, не 8192 из
	// body default клиента).
	requestedNCtx := ExtractNumCtxFromBody(bodyBuf)
	profileNCtx := p.GetModelProfileNumCtx(modelName)
	backendDefaultNCtx := 0
	if backendID != "" {
		backendDefaultNCtx = p.GetBackendDefaultNumCtx(backendID)
	}
	// Round 35c+ (2026-08-13): diagnostic logging для отладки "num_ctx downgrade
	// 32768 → 8192". Логируем ВСЕГДА на preflight (Info уровень), чтобы можно
	// было увидеть в balancer-логах что реально приходит от клиента:
	//   - body_num_ctx: что прислал клиент (0 если молчит)
	//   - profile_num_ctx: что в профиле модели
	//   - backend_default_num_ctx: что в default конфиге бэкенда
	//   - chosen_source: откуда взяли requestedNCtx (body/profile/backend/none)
	var chosenSource string
	switch {
	case requestedNCtx > 0:
		chosenSource = "body"
	case profileNCtx > 0:
		chosenSource = "profile"
	case backendDefaultNCtx > 0:
		chosenSource = "backend_default"
	default:
		chosenSource = "none"
	}
	logger.Get().Infow("preflightNCtxReload: num_ctx decision",
		"backend", backendID, "model", modelName,
		"body_num_ctx", requestedNCtx,
		"profile_num_ctx", profileNCtx,
		"backend_default_num_ctx", backendDefaultNCtx,
		"chosen_source", chosenSource)
	if requestedNCtx <= 0 {
		// Body не задал num_ctx → fallback на profile → backend default.
		// Это Round 35c+ fix для "Open WebUI шлёт num_ctx=8192 по дефолту,
		// даже если profile говорит 32768". First-load preflight теперь reload
		// в profile.ContextLength (32768) вместо body num_ctx (8192).
		requestedNCtx = profileNCtx
		if requestedNCtx <= 0 {
			requestedNCtx = backendDefaultNCtx
		}
		if requestedNCtx <= 0 {
			return bodyBuf, true, "", http.StatusOK
		}
		logger.Get().Debugw("preflightNCtxReload: using profile/backend fallback (body had no num_ctx)",
			"backend", backendID, "model", modelName, "requested_n_ctx", requestedNCtx)
	}

	// 2. Получаем loaded n_ctx из метрик.
	p.metricsMgr.mu.RLock()
	lm, hasLm := p.metricsMgr.llamaMetrics[backendID]
	var loadedNCtx int
	if hasLm && lm != nil {
		for _, m := range lm.LoadedModels {
			if m.Name == modelName || containsFold(m.Name, modelName) || containsFold(modelName, m.Name) {
				if m.ContextLength > 0 {
					loadedNCtx = m.ContextLength
				}
				break
			}
		}
	}
	p.metricsMgr.mu.RUnlock()

	// 3. Если loaded >= requested — перезагрузка не нужна.
	if loadedNCtx >= requestedNCtx {
		return bodyBuf, true, "", http.StatusOK
	}

	// 3.1 Round 23 (2026-08-04): SMART RELOAD SKIP.
	//
	// Проблема (issue #6+#8): клиент (Cline/OpenWebUI) посылает num_ctx=32000
	// "на всякий случай" (default), но реальный prompt — "2+2?" (1 токен).
	// Текущая логика ТУПО триггерит async reload на 50с, клиент получает
	// 503 + Retry-After: 5, retry 10 раз за 50с, потом (когда reload закончен)
	// получает успех. Субъективно для пользователя: "бесконечная перезагрузка".
	//
	// Решение: оценить реальный prompt size. Если estimated_prompt_tokens +
	// n_predict + slack <= loaded_n_ctx, то reload НЕ НУЖЕН — просто
	// patch'нем body (options.num_ctx = loaded_n_ctx) и проксируем дальше.
	// Модель работает с loaded_n_ctx, prompt помещается, всё OK.
	//
	// Стоимость: +1 JSON parse на preflight (≈50µs). Эвристика 1 token ≈ 4 chars
	// даёт ~25% точности; для коротких промптов это перестраховка, для длинных —
	// clamp'нем n_predict в cppworker (clampNPredictToFitContext).
	if requestedNCtx > 0 && loadedNCtx > 0 {
		meta := ExtractRequestMeta(bodyBuf, requestPath)
		if meta != nil {
			required := meta.EstimatedPromptTokens + meta.RequestedNPredict + 1
			// +10% slack под округление tokenizer'а и накопление KV.
			slack := meta.EstimatedPromptTokens / 10
			required += slack
			if required <= loadedNCtx {
				// Промпт влезает в текущий n_ctx. Patch body и проксируем.
				patchedBody := patchNumCtxInBody(bodyBuf, loadedNCtx)
				if patchedBody != nil {
					logger.Get().Infow("preflightNCtxReload: smart-skip reload (prompt fits in current n_ctx)",
						"backend", backendID, "model", modelName,
						"loaded_n_ctx", loadedNCtx,
						"requested_n_ctx", requestedNCtx,
						"estimated_prompt_tokens", meta.EstimatedPromptTokens,
						"required_n_ctx", required,
						"n_predict", meta.RequestedNPredict)
					return patchedBody, true, "", http.StatusOK
				}
				// patchNumCtxInBody вернул nil (не смог распарсить) — fallback на reload.
			}
		}
	}

	// 4. Loaded < requested — нужен reload. Запускаем его АСИНХРОННО в горутине.
	//
	// Почему не синхронно (как было раньше):
	//   - reload выгружает текущую модель из VRAM на 10-30 сек,
	//   - параллельные запросы получают HTTP 500 с "handle is nil",
	//   - streaming-клиент теряет середину ответа (обрыв стрима),
	//   - клиент получает context deadline exceeded при медленном reload.
	//
	// Новая логика: запускаем reload в горутине, сразу возвращаем клиенту
	// HTTP 503 Service Unavailable + Retry-After: 5. Клиент повторит
	// запрос через 5 секунд — к этому моменту reload обычно завершён.
	logger.Get().Infow("preflightNCtxReload: detected n_ctx mismatch, scheduling async reload",
		"backend", backendID, "model", modelName,
		"loaded_n_ctx", loadedNCtx,
		"requested_n_ctx", requestedNCtx,
		"loaded_n_ctx_is_zero", loadedNCtx == 0)

	// Получаем backend state для URL.
	p.mu.RLock()
	backendState, exists := p.backends[backendID]
	p.mu.RUnlock()
	if !exists {
		// Backend не найден — проксируем как есть, cppworker вернёт свою ошибку.
		return bodyBuf, true, "", http.StatusOK
	}

	// Запускаем reload в горутине. Неблокирующий режим: клиент сразу
	// получит 503 + Retry-After, а reload идёт параллельно.
	go p.executeAsyncReload(backendID, modelName, requestedNCtx, backendState.Backend)

	// Обновляем кэш метрик: ставим ContextLength = requestedNCtx,
	// чтобы следующие preflight-чеки не запускали reload повторно.
	// Реальное значение обновит metrics_poller через ~5 сек после успешного reload.
	p.metricsMgr.mu.Lock()
	if lm == nil {
		lm = &types.LlamaCppMetrics{}
		p.metricsMgr.llamaMetrics[backendID] = lm
	}
	found := false
	for i := range lm.LoadedModels {
		if lm.LoadedModels[i].Name == modelName ||
			containsFold(lm.LoadedModels[i].Name, modelName) {
			lm.LoadedModels[i].ContextLength = requestedNCtx
			found = true
			break
		}
	}
	if !found {
		lm.LoadedModels = append(lm.LoadedModels, types.LlamaCppModel{
			Name:          modelName,
			State:         "loaded",
			ContextLength: requestedNCtx,
		})
	}
	p.metricsMgr.mu.Unlock()

	// Возвращаем клиенту 503 + Retry-After: 30. needsProxy=false — вызывающий код
	// должен сам сформировать ответ (status, headers, body).
	//
	// Round 23 (2026-08-04): Retry-After увеличен 5 → 30 секунд.
	// Причина: реальный reload (unload + load с большим n_ctx) занимает 30-60с
	// на типичной GPU. Старое Retry-After: 5 заставляло клиента (Cline/OpenWebUI)
	// повторять 10-12 раз за время reload'а, накапливая 503 в логах и путая
	// retry-логику. С 30с клиент делает 1-2 retry и попадает в окно
	// завершения reload'а.
	return bodyBuf, false,
		fmt.Sprintf("model %q is being reloaded to n_ctx=%d (loaded=%d); retry in 30s",
			modelName, requestedNCtx, loadedNCtx),
		http.StatusServiceUnavailable
}

// patchNumCtxInBody — патчит options.num_ctx (Ollama) или top-level num_ctx
// (OpenAI/generic) в JSON body к заданному значению.
//
// Round 23 (2026-08-04): для smart-skip reload (см. preflightNCtxReloadIfNeeded).
// Используется чтобы "понизить" запрос клиента с num_ctx=32000 до loaded_n_ctx=16384,
// когда prompt помещается в текущий контекст.
//
// Возвращает nil если body не JSON или не содержит options/num_ctx (тогда caller
// fallback'нет на reload — безопасно).
func patchNumCtxInBody(body []byte, newNCtx int) []byte {
	if len(body) == 0 || newNCtx <= 0 {
		return nil
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	patched := false
	// Ollama: options.num_ctx
	if opts, ok := req["options"].(map[string]interface{}); ok {
		if _, exists := opts["num_ctx"]; exists {
			opts["num_ctx"] = float64(newNCtx)
			patched = true
		}
	}
	// OpenAI / generic: top-level num_ctx
	if !patched {
		if _, exists := req["num_ctx"]; exists {
			req["num_ctx"] = float64(newNCtx)
			patched = true
		}
	}
	if !patched {
		return nil
	}
	out, err := json.Marshal(req)
	if err != nil {
		return nil
	}
	return out
}

// preflightNCtxReloadIfNeededSync — синхронная версия preflight.
//
// В отличие от preflightNCtxReloadIfNeeded (которая сразу возвращает 503),
// эта версия БЛОКИРУЕТСЯ и ждёт завершения reload в фоне (горутина,
// запущенная executeAsyncReload), а затем проксирует запрос. Round-trip
// получается ОДИН — клиент не получает EOF/503.
//
// Алгоритм:
//  1. Запустить preflightNCtxReloadIfNeeded (запустит executeAsyncReload).
//  2. Poll /api/info heartbeat (`reload_pending` поле) каждые 500ms.
//  3. Когда `reload_pending` исчез И loaded_n_ctx >= requestedNCtx → return success.
//  4. Если timeout (PreflightSyncTimeoutMs, default 60s, max 180s) — fallback
//     на async 503 + Retry-After: 15.
//
// Преимущества: клиент не получает EOF и не видит reload-процесса вообще.
// OpenWebUI получает ответ за один round-trip (а не два: 503 + retry).
//
// Когда стоит выключить (PreflightSyncEnabled=false):
//   - Streaming-клиенты (curl без retry, скрипты) — async 503 + Retry-After лучше.
//   - Если reload ОЧЕНЬ долгий (cold start большой модели > 60s) — async
//     даст клиенту возможность повторить через 15s.
func (p *Proxy) preflightNCtxReloadIfNeededSync(
	ctx context.Context,
	backendID, modelName string,
	bodyBuf []byte,
	requestPath string,
) (modifiedBody []byte, needsProxy bool, errMsg string, statusCode int, retryAfter int) {
	// Сначала делаем все проверки (как в async версии).
	body, ok, msg, status := p.preflightNCtxReloadIfNeeded(ctx, backendID, modelName, bodyBuf, requestPath)
	if ok || status == http.StatusOK {
		// Можно проксировать сразу (reload не нужен или ошибка до запуска reload).
		return body, true, "", http.StatusOK, 0
	}
	// Reload запущен в фоне — ждём его завершения.
	logger.Get().Infow("preflightNCtxReloadIfNeededSync: waiting for async reload to complete",
		"backend", backendID, "model", modelName,
		"reason", msg)

	// Дефолт 60s, max 180s.
	timeoutMs := 60000
	if p.config != nil && p.config.Balancing.PreflightSyncTimeoutMs > 0 {
		timeoutMs = p.config.Balancing.PreflightSyncTimeoutMs
		if timeoutMs > 180000 {
			timeoutMs = 180000
		}
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return bodyBuf, false, "client cancelled while waiting for reload", http.StatusServiceUnavailable, 15
		case <-ticker.C:
		}

		if time.Now().After(deadline) {
			logger.Get().Warnw("preflightNCtxReloadIfNeededSync: timeout waiting for reload",
				"backend", backendID, "model", modelName, "timeout_ms", timeoutMs)
			return bodyBuf, false,
				fmt.Sprintf("reload still in progress after %dms; retry in 15s", timeoutMs),
				http.StatusServiceUnavailable, 15
		}

		// Проверяем reload_pending через heartbeat /api/info.
		pendingReload := p.queryBackendReloadPending(backendID)
		if pendingReload == "" {
			// Reload завершён (или не начинался). Проверяем loaded_n_ctx.
			p.metricsMgr.mu.RLock()
			lm, hasLm := p.metricsMgr.llamaMetrics[backendID]
			loadedNCtx := 0
			if hasLm && lm != nil {
				for _, m := range lm.LoadedModels {
					if m.Name == modelName || containsFold(m.Name, modelName) || containsFold(modelName, m.Name) {
						if m.ContextLength > 0 {
							loadedNCtx = m.ContextLength
						}
						break
					}
				}
			}
			p.metricsMgr.mu.RUnlock()

			requestedNCtx := ExtractNumCtxFromBody(bodyBuf)
			if loadedNCtx >= requestedNCtx {
				logger.Get().Infow("preflightNCtxReloadIfNeededSync: reload completed successfully",
					"backend", backendID, "model", modelName,
					"loaded_n_ctx", loadedNCtx, "requested_n_ctx", requestedNCtx)
				return bodyBuf, true, "", http.StatusOK, 0
			}
			logger.Get().Warnw("preflightNCtxReloadIfNeededSync: reload completed but loaded_n_ctx still insufficient",
				"backend", backendID, "model", modelName,
				"loaded_n_ctx", loadedNCtx, "requested_n_ctx", requestedNCtx)
			return bodyBuf, false,
				fmt.Sprintf("reload completed but loaded_n_ctx=%d < requested=%d; retry in 15s", loadedNCtx, requestedNCtx),
				http.StatusServiceUnavailable, 15
		}
	}
}

// queryBackendReloadPending возвращает имя модели, для которой сейчас идёт reload
// на бэкенде, или пустую строку. Используется в preflight-sync для ожидания.
//
// Реализация: HTTP GET на /api/info cppworker-бэкенда, парсим JSON,
// ищем поле reload_pending.model. При ошибке (EOF, connection reset) —
// возвращаем "" (best-effort — reload_pending истечёт по таймауту).
//
// HTTP-клиент берётся из p.metricsHTTPDoer (если задан) — это позволяет
// тестам подменить реальный *http.Client на in-memory stub без сети
// (см. nctx_reload_sync_test.go). По умолчанию используется реальный
// http.Client с таймаутом 2s.
func (p *Proxy) queryBackendReloadPending(backendID string) string {
	backend := p.GetBackend(backendID)
	if backend == nil {
		return ""
	}
	port := p.getBackendPort(backend)
	if port <= 0 {
		return ""
	}
	url := fmt.Sprintf("http://%s:%d/api/info", backend.Host, port)
	// HTTP-клиент берётся из p.metricsHTTPDoer (если задан) для тестируемости.
	// По умолчанию — реальный *http.Client с 2s таймаутом.
	doer := p.metricsHTTPDoer
	if doer == nil {
		doer = &http.Client{Timeout: 2 * time.Second}
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	// Phase 1 FIX: hardcoded token from CPPWORKER_API_TOKEN env (config.json loading is broken in this build)
	tok := os.Getenv("CPPWORKER_API_TOKEN")
	if tok == "" {
		tok = os.Getenv("LB_API_TOKEN")
	}
	if tok == "" && p.config != nil && len(p.config.Auth.Tokens) > 0 {
		tok = p.config.Auth.Tokens[0]
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := doer.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var info map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return ""
	}
	rp, ok := info["reload_pending"].(map[string]interface{})
	if !ok {
		return ""
	}
	model, _ := rp["model"].(string)
	return model
}

// executeAsyncReload — запускает POST /api/models/reload в фоне.
// Используется из preflightNCtxReloadIfNeeded для неблокирующего reload.
//
// При успехе: модель перезагружена, metrics_poller подхватит новые метрики.
// При ошибке: логируем, модель остаётся со старым n_ctx (cppworker вернёт
// ErrNCtxNeedsReload при следующем запросе — тогда сработает handleNCtxReload).
func (p *Proxy) executeAsyncReload(backendID, modelName string, requestedNCtx int, backend *types.Backend) {
	start := time.Now()
	// Round 35 (2026-08-12) bugfix: prefer /api/models/load over /api/models/reload.
	//
	// PROBLEM: cppworker /api/models/reload returns 404 "model not currently
	// loaded" if the model is not yet in VRAM (e.g. just after cppworker restart,
	// before lazy-load triggered by first request). In that case the cached
	// "loaded" state in balancer's llamaCppMetricsPoller is stale (from previous
	// cppworker instance), preflight triggers reload based on stale cache, reload
	// fails with 404, model stays unloaded, Cline gets 503+Retry-After loop until
	// its 120s timeout → ECONNREFUSED (after balancer restart from earlier crash).
	//
	// FIX: /api/models/load already handles both cases:
	//   - Model not loaded: starts load (202 + progressUrl)
	//   - Model loaded with different params: unload + reload
	//   - Model loaded with same params: returns 200 "already_loaded" (dedup)
	// So we can safely use /api/models/load for both preflight (loaded<requested)
	// and recovery (model gone after restart) without a separate fallback path.
	//
	// Trade-off: /api/models/load writes a "dedup by path" log when model is
	// already loaded with same options. That's fine — it confirms the model
	// is ready and metrics_poller will pick up the state on next 30s poll.
	targetURL := p.getBackendBaseURL(backend) + "/api/models/load"

	reloadPayload, _ := json.Marshal(map[string]interface{}{
		"name":        modelName,
		"contextSize": requestedNCtx,
		// "force" removed: /api/models/load doesn't have this field.
		// The "reason" field is also load-specific (it triggers profile re-apply).
		"reason": "balancer preflight async auto-load-or-reload (loaded<requested or cppworker restart)",
		// Phase 1 (2026-07-06): RAM fallback to maximize n_ctx in 8GB VRAM.
		// Without q4_0 KV-cache + GPU-offload=-2, gemma-4 maxes at ~17K n_ctx.
		"kvCacheType": "q4_0",
		"gpuLayers":   -2,
		"useMmap":     true,
	})

	reloadCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	req, reqErr := http.NewRequestWithContext(reloadCtx, "POST", targetURL, bytes.NewReader(reloadPayload))
	if reqErr != nil {
		logger.Get().Errorw("preflightNCtxReload(async): failed to create reload request",
			"backend", backendID, "model", modelName, "error", reqErr)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	logger.Get().Warnw("executeAsyncReload DEBUG", "p_config_nil", p.config == nil, "tokens_len", func() int {
		if p.config == nil {
			return -1
		}
		return len(p.config.Auth.Tokens)
	}(), "first_token_len", func() int {
		if p.config == nil || len(p.config.Auth.Tokens) == 0 {
			return -1
		}
		return len(p.config.Auth.Tokens[0])
	}())
	// Phase 1 FIX: hardcoded token from CPPWORKER_API_TOKEN env (config.json loading is broken in this build)
	tok := os.Getenv("CPPWORKER_API_TOKEN")
	if tok == "" {
		tok = os.Getenv("LB_API_TOKEN")
	}
	if tok == "" && p.config != nil && len(p.config.Auth.Tokens) > 0 {
		tok = p.config.Auth.Tokens[0]
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	logger.Get().Infow("preflightNCtxReload(async): starting reload",
		"backend", backendID, "model", modelName,
		"target_n_ctx", requestedNCtx, "url", targetURL)

	resp, err := p.client.Do(req)
	if err != nil {
		logger.Get().Errorw("preflightNCtxReload(async): reload HTTP request failed",
			"backend", backendID, "model", modelName,
			"target_n_ctx", requestedNCtx, "duration_ms", time.Since(start).Milliseconds(),
			"error", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	duration := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		logger.Get().Warnw("preflightNCtxReload(async): reload returned non-OK status",
			"backend", backendID, "model", modelName,
			"target_n_ctx", requestedNCtx,
			"status", resp.StatusCode,
			"duration_ms", duration.Milliseconds(),
			"body", string(body))
		return
	}

	logger.Get().Infow("preflightNCtxReload(async): reload SUCCESS",
		"backend", backendID, "model", modelName,
		"target_n_ctx", requestedNCtx,
		"duration_ms", duration.Milliseconds())
}

// getLoadedNCtxFromMetrics — возвращает загруженный n_ctx модели из метрик.
// Используется в handleChat для предотвращения понижения n_ctx:
// если модель загружена с n_ctx=131072, а клиент шлёт num_ctx=8192,
// мы сохраняем загруженное значение.
func (p *Proxy) getLoadedNCtxFromMetrics(backendID, modelName string) int {
	if p.metricsMgr == nil {
		return 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	lm, hasLm := p.metricsMgr.llamaMetrics[backendID]
	if !hasLm || lm == nil {
		return 0
	}
	for _, m := range lm.LoadedModels {
		if m.Name == modelName || containsFold(m.Name, modelName) || containsFold(modelName, m.Name) {
			return m.ContextLength
		}
	}
	return 0
}

// getMaxVRAMNCtxFromMetrics возвращает max_vram_n_ctx из llama.cpp метрик бэкенда.
// Позволяет preflight узнать реальный VRAM-потолок без round-trip к cppworker.
func (p *Proxy) getMaxVRAMNCtxFromMetrics(backendID string) int {
	if p == nil || p.metricsMgr == nil {
		return 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	if lm, ok := p.metricsMgr.llamaMetrics[backendID]; ok && lm != nil {
		return lm.MaxVRAMNCtx
	}
	return 0
}

// resolveModelMaxContext выбирает modelMaxContext для preflight v3 (Round 43).
//
// 3-tier resolution с conceptual fix (R43, 2026-08-19):
//
//  1. profile.contextLengthAuto == false (default для backward compat):
//     profile.contextLength — жёсткий cap, но min(profile, contextLengthMax) —
//     operator cap всегда уважается. Pre-R43: contextLengthMax в schema
//     был задекларирован, но resolver его ИГНОРИРОВАЛ. R43 fix: применён.
//  2. profile.contextLengthAuto == true (auto-adapt, opt-in per profile):
//     profile — HINT (не cap). Реальный cap = MIN(ggufMax, contextLengthMax).
//     perModelFeasible — HINT (current-state), не cap. Причина: feasible
//     отражает "что влезает с ТЕКУЩИМИ gpu_layers". После auto-tune (partial
//     offload в RAM) feasible вырастет. Если использовать feasible как cap,
//     preflight будет reject'ить запросы которые cppworker фактически может
//     обработать после reload с auto-offload. R43 fix: feasible — HINT, не cap.
//     Round 37 PRODUCTION BUG: pre-bug balancer reject'ил Cline num_ctx=65536
//     потому что feasible=32719 (max_vram_n_ctx при gpu_layers=-1) < 65536.
//     Post-R43: feasible игнорируется в auto mode, cap = min(ggufMax=262144,
//     contextLengthMax=131072) = 131072 → 65536 проходит.
//  3. Fallback (нет profile): ggufMax (hard upper bound from training).
//     Pre-R43: fallback на default profile (32768) — это conservative HINT
//     которая блокировала 65536 даже для моделей без явного profile. R43 fix:
//     fall through to ggufMax; если и его нет — to metrics.
//
// КЛЮЧЕВОЙ ПРИНЦИП R43: "балансер должен автоматом все определят и выделять
// под нужды модели". Никаких hardcoded n_ctx в resolver chain. Все значения
// либо auto-derived (ggufMax from training), либо явный operator policy
// (contextLengthMax). profile.contextLength — HINT для initial load, не cap.
//
// Параметр profileMaxContext — contextLength из per-model profile (0 = no profile).
// Параметр contextLengthAuto — true если profile разрешает auto-adapt.
// Параметр contextLengthMax — operator soft cap (0 = unlimited up to ggufMax).
// Параметр perModelFeasible — per-model feasible (из LoadedModels), HINT в auto mode.
func (p *Proxy) resolveModelMaxContext(backendID, model string, profileMaxContext int, contextLengthAuto bool, contextLengthMax int, perModelFeasible int) int {
	// Tier 1+2: profile present, decide based on auto flag
	if profileMaxContext > 0 {
		if !contextLengthAuto {
			// Tier 1: hard cap (backward compat). R43 fix: respect contextLengthMax
			// as operator cap EVEN в hard mode (schema field was previously ignored).
			cap := profileMaxContext
			if contextLengthMax > 0 && cap > contextLengthMax {
				logger.Get().Warnw("resolveModelMaxContext: profile exceeds contextLengthMax operator cap, clamping",
					"backend", backendID, "model", model,
					"profile_n_ctx", profileMaxContext,
					"context_length_max", contextLengthMax,
				)
				cap = contextLengthMax
			}
			// Diagnostic: warn если feasible >> profile (явно conservative profile).
			feasible := perModelFeasible
			if feasible == 0 {
				feasible = p.getFeasibleMaxContext(backendID)
			}
			if feasible > 0 && feasible > cap*3/2 {
				logger.Get().Warnw("resolveModelMaxContext: profile conservative (no auto-adaptation enabled)",
					"backend", backendID, "model", model,
					"profile_n_ctx", cap,
					"feasible_n_ctx", feasible,
					"headroom_ratio", float64(feasible)/float64(cap),
					"recommendation", "Add contextLengthAuto:true к profile для auto-relax",
				)
			}
			return cap
		}
		// Tier 2: auto-adapt. profile — HINT. Real cap = MIN(ggufMax, contextLengthMax).
		// CRITICAL R43 CHANGE: perModelFeasible is HINT, not cap. After cppworker
		// auto-tunes gpu_layers, feasible grows. Using it as cap blocks legit requests.
		cap := contextLengthMax // operator policy (soft cap, default 0 = unlimited)
		ggufMax := p.getGGUFMaxContext(backendID)
		if ggufMax > 0 && (cap == 0 || ggufMax < cap) {
			cap = ggufMax
		}
		// If both contextLengthMax and ggufMax are 0, we have NO data.
		// Return 0 (no cap) and let cppworker decide. This is the
		// "trust the model" path: if operator set auto=true, they trust
		// the system to figure it out. cppworker will validate against
		// its own context_size (computed from GGUF header).
		if cap == 0 {
			return 0
		}
		// If perModelFeasible is reported AND is BIGGER than profileMaxContext,
		// that's a confirmation that auto-relax is right. We don't use it as
		// a cap, but we can validate against it (warn if operator's cap is
		// more permissive than current achievable).
		if perModelFeasible > 0 && cap > perModelFeasible*4 {
			// Operator cap (e.g. 131072) is way more than current feasible
			// (e.g. 32719). After auto-tune, feasible will grow. Don't reject.
			// Just info-log so operator knows.
			logger.Get().Infow("resolveModelMaxContext: operator cap > 4x current feasible, will rely on cppworker auto-tune",
				"backend", backendID, "model", model,
				"resolved_cap", cap,
				"current_feasible", perModelFeasible,
				"note", "cppworker will reduce gpu_layers on reload to grow feasible via RAM offload")
		}
		return cap
	}
	// Tier 3: no profile. R43 fix: fall through to ggufMax (model's hard upper
	// bound from training) instead of default profile (conservative 32768).
	// Pre-R43: Tier 3 returned getMetricsModelMaxContext() which is also
	// conservative (cppworker reports max_vram_n_ctx, not GGUFMax).
	ggufMax := p.getGGUFMaxContext(backendID)
	if ggufMax > 0 {
		return ggufMax
	}
	// Final fallback: metrics ModelMaxContext (preserved for backward compat).
	return p.getMetricsModelMaxContext(backendID)
}

// getFeasibleMaxContext — возвращает MaxFeasibleContext из llamaMetrics
// (заполняется из cppworker /api/models response, поле feasible_max_context).
// 0 = unknown (cppworker не сообщил).
func (p *Proxy) getFeasibleMaxContext(backendID string) int {
	if p == nil || p.metricsMgr == nil {
		return 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	if lm, ok := p.metricsMgr.llamaMetrics[backendID]; ok && lm != nil {
		return lm.MaxFeasibleContext // Round 37: 0 если не заполнено
	}
	return 0
}

// getGGUFMaxContext — возвращает GGUFMaxContext из llamaMetrics (top-level).
// Per-model override передаётся явно через resolveModelMaxContext parameter.
func (p *Proxy) getGGUFMaxContext(backendID string) int {
	if p == nil || p.metricsMgr == nil {
		return 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	if lm, ok := p.metricsMgr.llamaMetrics[backendID]; ok && lm != nil {
		return lm.GGUFMaxContext
	}
	return 0
}

// getMetricsModelMaxContext — старое fallback (ModelMaxContext из poller'а).
func (p *Proxy) getMetricsModelMaxContext(backendID string) int {
	if p == nil || p.metricsMgr == nil {
		return 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	if lm, ok := p.metricsMgr.llamaMetrics[backendID]; ok && lm != nil {
		return lm.ModelMaxContext
	}
	return 0
}
