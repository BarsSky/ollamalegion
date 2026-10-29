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

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
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
	//
	// R267 (2026-10-29): решателю передаётся ТОТ ЖЕ эффективный потолок, что и
	// обработчику ниже (effectiveAutoReloadCeiling). Раньше он брал сырое
	// cfg.AutoReloadMaxNCtx, поэтому решение оператора из WebUI (окно 256000 в
	// карточке модели) обходилось на типовом агентском пути prompt_too_long —
	// живая жалоба «модель поддерживает больше, а на деле ограничена».
	effectiveCeiling := ReloadCeiling{}
	if cfg := p.nctxReload.Config(); cfg.AutoReloadMaxNCtx > 0 {
		effectiveCeiling = ReloadCeiling{
			Effective: p.effectiveAutoReloadCeiling(modelName, cfg.AutoReloadMaxNCtx),
			Env:       cfg.AutoReloadMaxNCtx,
		}
	}
	plan := p.nctxReload.DecideReloadBackendWithCeiling(backendID, bridgeErr, requestedOverride, effectiveCeiling)
	logger.Get().Debugw("nctx_reload: DecideReloadBackend result",
		"backend", backendID, "model", modelName,
		"decision", plan.Decision.String(),
		"new_n_ctx", plan.NewNCtx,
		"effective_ceiling", plan.EffectiveCeiling,
		"limited_by", plan.LimitedBy,
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
		// R83-политика (2026-10-01): ПРИНУДИТЕЛЬНЫЙ рост окна убран.
		//
		// Растём ТОЛЬКО когда клиент сам попросил окно больше загруженного
		// (этот случай обрабатывает preflight ДО проксирования — DecidePreflight).
		// Если клиент попросил ровно столько же (или меньше), а промпт всё равно
		// не влез — это ЕГО окно, и честный ответ: «промпт не помещается в
		// запрошенное окно N, увеличьте num_ctx или сократите промпт».
		//
		// Живой случай (лог 2026-10-01): клиент прислал num_ctx=32768 и промпт на
		// 85 544 токена; балансер вместо ответа выгрузил модель, перезагрузил её
		// С ТЕМ ЖЕ окном 65536 (стратегия срезала цель 131072 → 65536) за 121 с и
		// отчитался «reload successful, new n_ctx=131072». Клиент ждал и не
		// получил ничего.
		// Гейт срабатывает ТОЛЬКО когда клиент ЯВНО указал окно и оно не больше
		// загруженного (случай пользователя: «пришло явное указание на окно»).
		// Молчащий клиент сохраняет прежнее поведение (реактивное восстановление
		// по РЕАЛЬНОЙ ошибке «prompt too long», а не по оценке).
		if bridgeErr != nil && bridgeErr.CurrentNCtx > 0 && requestedOverride > 0 &&
			requestedOverride <= bridgeErr.CurrentNCtx {
			logger.Get().Warnw(i18n.T("balancer.nctx_reload_handlers.window_growth_not_requested_warn"),
				"backend", backendID,
				"model", modelName,
				"requested_num_ctx", requestedOverride,
				"loaded_per_client_n_ctx", bridgeErr.CurrentNCtx,
				"required_n_ctx", bridgeErr.RequiredNCtx,
				"plan_target_n_ctx", plan.NewNCtx)
			writeRequestedWindowTooSmall(w, r, modelName, backendID,
				requestedOverride, bridgeErr.CurrentNCtx, bridgeErr.RequiredNCtx)
			return
		}
		p.handleNCtxReloadActual(ctx, w, r, backendID, modelName, plan, bodyBuf, bridgeErr)
	}
}

// writeRequestedWindowTooSmall — понятный отказ вместо «молчаливой» перезагрузки.
//
// Случай: клиент САМ ограничил окно (num_ctx=N), промпт в него не влез, а
// поднять окно балансер без просьбы клиента не имеет права (R83-политика).
// Текст адресован человеку: что просили, сколько нужно, что делать.
func writeRequestedWindowTooSmall(
	w http.ResponseWriter, r *http.Request,
	modelName, backendID string, requestedPerClient, loadedPerClient, required int,
) {
	// ВАЖНО: bridgeErr.CurrentNCtx — это окно, с которым cppworker обслуживал
	// ЭТОТ запрос (то есть уже урезанное до клиентского num_ctx), а не окно
	// загруженной модели. Поэтому в тексте не утверждаем «модель загружена с
	// окном N», а называем только то, что известно точно: что просил клиент и
	// сколько нужно промпту. loadedPerClient остаётся машинным полем для
	// диагностики.
	_ = loadedPerClient
	var msg string
	if requestedPerClient > 0 {
		msg = i18n.T("balancer.nctx_reload_handlers.client_window_too_small_error",
			requestedPerClient, required, required)
	} else {
		msg = i18n.T("balancer.nctx_reload_handlers.loaded_window_too_small_error",
			required, required)
	}
	body := map[string]interface{}{
		"error":                     msg,
		"reason":                    i18n.T("balancer.nctx_reload_handlers.window_not_requested_reason"),
		"requested_num_ctx":         requestedPerClient,
		"required_n_ctx":            required,
		"cppworker_effective_n_ctx": loadedPerClient,
		"model":                     modelName,
		"backend_id":                backendID,
		"what_to_do":                i18n.T("balancer.nctx_reload_handlers.window_too_small_what_to_do", required),
	}
	// Streaming-клиенты (Cline) ждут NDJSON-чанк с done:true, а не plain JSON:
	// иначе они не понимают, что это конец стрима (см. DecisionReject выше).
	if r != nil && isNCtxReloadStreaming(r) {
		body["done"] = true
		encoded, _ := json.Marshal(body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(413)
		_, _ = w.Write(append(encoded, '\n'))
		return
	}
	writeJSON(w, 413, body)
}

// handleNCtxReloadActual — выполняет reload на бэкенде.
//
// R60.47 (2026-09-11): async mode (default) — kick off reload в goroutine,
// return 503+Retry-After immediately. Client retry-ит после reload.
//
// Pre-R60.47 (sync mode, legacy): ждал завершения reload синхронно
// (cppworker ?wait=true&waitTimeoutSec=300, до 5 мин), потом retry'ил
// original request в том же HTTP handler. Это anti-pattern: HTTP handler
// blocked → OpenWebUI/Cline timeout 30-60s → cancel → "empty response"
// / EOF / "Unexpected token '<html>'".
//
// Post-R60.47: balancer отвечает за <50ms с 503+Retry-After+digestive
// diagnostics. Client retry-ит через Retry-After → к этому моменту
// reload завершён → 200 OK.
//
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

	// R60.47: ASYNC MODE (default). Kick off reload в goroutine, return
	// 503+Retry-After immediately. Это решает "empty response" — клиент
	// получает JSON ошибку за миллисекунды вместо зависания на минуты.
	if p.lbNCtxReloadAsync {
		p.handleNCtxReloadActualAsync(backendID, modelName, plan, backendAddr, bridgeErr, w)
		return
	}

	// LEGACY SYNC MODE (LB_NCTX_RELOAD_ASYNC=0). Только для debug / long-running
	// клиентов. Single round-trip: balancer reload+retry синхронно.
	start := time.Now()
	p.nctxReload.RecordDecision(backendID, "reload-start", plan.Reason)
	// R271: в синхронном режиме тоже не выгружаем модель под чужим стримом.
	// R291: одно решение «ждать или отложить» (deferDecisionR291) и один формат
	// ответа (writeReloadDeferredR291). Раньше ждали 120 с и потом отвечали
	// текстом без чисел — «кнопка думает две минуты».
	if ok, active := p.deferDestructiveOpR291(ctx, backendID, modelName, "nctx-reload-sync"); !ok {
		logger.Get().Warnw("nctx_reload: sync reload postponed — active streams on backend (R271)",
			"backend", backendID, "model", modelName,
			"target_n_ctx", plan.NewNCtx, "active_streams", active)
		writeReloadDeferredR291(w, modelName, "nctx-reload-sync", active)
		return
	}
	p.noteDestructiveOpR271(backendID, modelName, destructiveReloadR271)
	reloadErr := p.nctxReload.DoReload(ctx, backendID, backendAddr, modelName, plan, p.newNCtxReloadHTTPClient())
	duration := time.Since(start)
	p.nctxReload.RecordReloadDuration(backendID, duration, reloadErr)
	if reloadErr != nil {
		// R296: перезагрузка не состоялась — снимаем пометку, иначе следующий
		// оборванный стрим будет объяснён нашей перезагрузкой, которой не было.
		p.clearDestructiveOpR271(backendID, modelName)
		logger.Get().Errorw("nctx_reload: DoReload failed",
			"backend", backendID, "model", modelName, "error", reloadErr)
		p.nctxReload.RecordError(backendID, reloadErr.Error())
		writeNCtxJSONError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("n_ctx auto-reload failed: %v (saved model profile with larger n_ctx and reload manually via /api/profiles)", reloadErr),
			"reload-failed")
		return
	}
	// R296: модель действительно выгружена и перезагружена — только теперь
	// обрыв чужого стрима в пределах окна атрибуции считается НАШИМ.
	p.confirmDestructiveOpUnloadedR271(backendID, modelName)

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

// handleNCtxReloadActualAsync — R60.47 async mode: kick off reload в
// goroutine, return 503+Retry-After+digestive immediately.
//
// Используется когда lbNCtxReloadAsync=true (default). HTTP handler
// завершается за <50ms, клиент получает actionable JSON ошибку.
//
// Reload запускается с detached context.Background() — мы не хотим
// чтобы client cancellation убивал наш reload (user cancel после 30s
// timeout не должен abort reload на 60-180s).
func (p *Proxy) handleNCtxReloadActualAsync(
	backendID, modelName string,
	plan *ReloadPlan,
	backendAddr string,
	bridgeErr *NCtxBridgeError,
	w http.ResponseWriter,
) {
	// Estimated load time (cppworker возвращает это в /api/models/load response).
	// Если неизвестно — используем default 60s (realistic для 5GB модели).
	estimatedLoadMs := 60000
	if bridgeErr != nil && bridgeErr.RequiredNCtx > 0 {
		// Эвристика: пропорционально required/current ratio + базовый load.
		if bridgeErr.CurrentNCtx > 0 && bridgeErr.RequiredNCtx > bridgeErr.CurrentNCtx {
			ratio := float64(bridgeErr.RequiredNCtx) / float64(bridgeErr.CurrentNCtx)
			// reload 2048→8192 (4x) занимает ~30s на RTX 3070 8GB
			// reload 2048→32768 (16x) занимает ~90s
			estimatedLoadMs = int(float64(estimatedLoadMs) * (ratio / 4.0))
			if estimatedLoadMs < 30000 {
				estimatedLoadMs = 30000
			}
			if estimatedLoadMs > 180000 {
				estimatedLoadMs = 180000
			}
		}
	}
	retryAfter := clampEstimatedMsToRetryAfter(estimatedLoadMs)

	// Формируем 503 response с diagnostics (R60.40 pattern).
	diag := ServiceUnavailableDiagnostic{
		Error:           fmt.Sprintf("model '%s' n_ctx auto-reload in progress, retry in %ds (target n_ctx=%d)", modelName, retryAfter, plan.NewNCtx),
		Model:           modelName,
		BackendID:       backendID,
		TargetNCtx:      plan.NewNCtx,
		RetryAfterSec:   retryAfter,
		EstimatedLoadMs: estimatedLoadMs,
	}
	if bridgeErr != nil {
		diag.FeasibleMaxContext = bridgeErr.MaxVRAMNCtx
		diag.CurrentNCtx = bridgeErr.CurrentNCtx
		diag.RequiredNCtx = bridgeErr.RequiredNCtx
		diag.Suggestion = fmt.Sprintf("n_ctx auto-reload from %d to %d is in progress; reduce num_predict OR wait %ds and retry", bridgeErr.CurrentNCtx, plan.NewNCtx, retryAfter)
	}

	logger.Get().Infow("nctx_reload: R60.47 async reload kicked off, returning 503+Retry-After",
		"backend", backendID, "model", modelName,
		"current_n_ctx", diag.CurrentNCtx,
		"target_n_ctx", plan.NewNCtx,
		"retry_after_sec", retryAfter,
		"estimated_load_ms", estimatedLoadMs)

	// R69 (2026-09-23): не планируем reload МЕНЬШЕ уже известного n_ctx.
	//
	// План строится из bridge_info текущего запроса (cppworker сообщает
	// current_n_ctx из effective n_ctx ЭТОГО запроса) — после ручного/профильного
	// reload'а это значение устаревает, и балансер запускал «reload вниз»
	// (наблюдалось на живом стенде: модель на 65536, а план — 16384 → качели
	// reload'ов и 503 у клиента). Координатор знает фактический n_ctx
	// (LastKnownNCtx) — если он уже покрывает цель, загрузка бессмысленна.
	if known := p.lastKnownNCtxFor(backendID); known > 0 && known >= plan.NewNCtx {
		logger.Get().Warnw("nctx_reload: skipping pointless reload — known n_ctx already covers target (R69)",
			"backend", backendID, "model", modelName,
			"known_n_ctx", known,
			"target_n_ctx", plan.NewNCtx,
			"bridge_current_n_ctx", diag.CurrentNCtx)
		diag.TargetNCtx = known
		diag.Error = fmt.Sprintf("model '%s' rejected the request with its effective n_ctx=%d (model is loaded with n_ctx=%d); retry the request or reduce num_predict",
			modelName, diag.CurrentNCtx, known)
		diag.Suggestion = fmt.Sprintf("balancer already has n_ctx=%d loaded — reload skipped; retry the request", known)
		writeServiceUnavailableWithDiagnostics(w, diag)
		return
	}

	// Kick off reload in detached goroutine. Используем context.Background()
	// чтобы client cancellation не abort'ил наш reload (user cancel после
	// timeout не должен отменять многоминутный reload).
	p.nctxReload.RecordDecision(backendID, "reload-async-start", plan.Reason)
	doReload := func(start time.Time) {
		// R271: не выгружать модель под чужим активным стримом (живой дефект:
		// второй агент оборвал стрим первого). Ждём до LB_RELOAD_DEFER_MAX_SEC.
		// R291: отложенное решение принимается сразу — в фоне ждать 120 с
		// незачем, следующий запрос сам повторит попытку.
		if ok, active := p.deferDestructiveOpR291(context.Background(), backendID, modelName, "nctx-reload-async"); !ok {
			logger.Get().Warnw("nctx_reload: async reload postponed — active streams on backend (R271)",
				"backend", backendID, "model", modelName,
				"target_n_ctx", plan.NewNCtx, "active_streams", active)
			return
		}
		p.noteDestructiveOpR271(backendID, modelName, destructiveReloadR271)
		// detached context — НЕ cancel-ится при client disconnect
		reloadErr := p.nctxReload.DoReload(context.Background(), backendID, backendAddr, modelName, plan, p.newNCtxReloadHTTPClient())
		duration := time.Since(start)
		p.nctxReload.RecordReloadDuration(backendID, duration, reloadErr)
		if reloadErr != nil {
			// R296: перезагрузка не состоялась — снимаем пометку, чтобы чужой
			// обрыв не был объяснён нашей перезагрузкой, которой не было.
			p.clearDestructiveOpR271(backendID, modelName)
			logger.Get().Errorw("nctx_reload: async reload failed",
				"backend", backendID, "model", modelName,
				"target_n_ctx", plan.NewNCtx,
				"duration_ms", duration.Milliseconds(),
				"error", reloadErr)
			p.nctxReload.RecordError(backendID, reloadErr.Error())
		} else {
			// R296: модель действительно выгружена/перезагружена — теперь обрыв
			// чужого стрима в пределах окна атрибуции честно считается нашим.
			p.confirmDestructiveOpUnloadedR271(backendID, modelName)
			logger.Get().Infow("nctx_reload: async reload complete",
				"backend", backendID, "model", modelName,
				"new_n_ctx", plan.NewNCtx,
				"duration_ms", duration.Milliseconds())
		}
	}
	// R69 (2026-09-23): запускаем reload через ТОТ ЖЕ dedup-реестр, что и
	// preflight-путь. Раньше R60.47-путь стартовал загрузку «как есть»: если
	// параллельно шёл preflight-reload (или второй 413 от другого запроса),
	// cppworker получал две команды загрузки, отменял первую
	// («load cancelled: model load aborted by user»), rollback падал
	// («reload rollback failed: model is no longer loaded») и модель оставалась
	// выгруженной. Теперь вторая загрузка к той же модели не стартует.
	if !p.nctxReload.StartModelReloadIfNotPending(backendID, modelName, func(_ *reloadEntry) {
		doReload(time.Now())
	}) {
		logger.Get().Infow("nctx_reload: reload already pending for this model, reusing it (R69 dedup)",
			"backend", backendID, "model", modelName, "target_n_ctx", plan.NewNCtx)
	}

	// Write 503+Retry-After immediately. Это главное отличие R60.47:
	// handler завершается за <50ms с actionable JSON.
	//
	// R296 (2026-10-29): формат ОДИН на все отложенные случаи (R291), и в нём
	// ЧИСЛА: сколько токенов нужно (max из запроса клиента и оценки промпта),
	// сколько загружено и СКОЛЬКО СТРИМОВ сейчас идёт по живому счётчику R271.
	// Живой факт до правки: клиент видел `required=4098` (это оценка ПРОМПТА,
	// а не его запрос 32768) и `activeStreams=0` при живом стриме — по таким
	// числам причину не понять.
	neededTokens := diag.RequiredNCtx
	if plan.NewNCtx > neededTokens {
		neededTokens = plan.NewNCtx
	}
	activeNow := p.ActiveStreamsOnBackend(backendID)
	logger.Get().Infow("nctx_reload: reload deferred, answering immediately (R291/R296)",
		"backend", backendID, "model", modelName,
		"required_tokens", neededTokens, "loaded_window_tokens", diag.CurrentNCtx,
		"active_streams", activeNow, "retry_after_sec", retryAfter)
	writeDeferredJSON(w, deferredBodyR291{
		DeferredReason: "context_growth_pending",
		Model:          modelName,
		Operation:      "nctx-reload-async",
		RetryAfterSec:  retryAfter,
		ActiveStreams:  activeNow,
		Extra: map[string]interface{}{
			"error":              diag.Error,
			"requiredTokens":     neededTokens,
			"loadedWindowTokens": diag.CurrentNCtx,
			"targetNCtx":         plan.NewNCtx,
			"activeStreams":      activeNow,
			"hint":               "raise the model context window (WebUI → model card) or shorten the history; retry after the reload finishes",
		},
	})
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
	// R83/v62 (2026-10-02): по умолчанию — БЕЗ таймаута HTTP-клиента (0 = нет
	// таймаута в Go). Клиентский таймаут означает, что балансер сам обрывает
	// перезагрузку большой модели (A10, qwen3.8 15.7 ГБ) на 300-й секунде.
	// Числовой таймаут — только при явном opt-in LB_ALLOW_NCTX_RELOAD_TIMEOUT=1.
	timeout := time.Duration(0)
	if p.nctxReload != nil {
		if d := p.nctxReload.Config().effectiveTimeout(); d > 0 {
			timeout = d + 30*time.Second
			if timeout > 600*time.Second {
				timeout = 600 * time.Second
			}
			logger.Get().Warnw("armed nctx reload http client timeout (operator opt-in)",
				"timeout", timeout.String(), "source", "LB_ALLOW_NCTX_RELOAD_TIMEOUT")
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
	// R301 (2026-10-29): streaming-вердикт считается ДО проверки координатора.
	//
	// Почему здесь, а не ниже (где раньше стоял безусловный пропуск стриминга):
	//   - координатор (p.nctxReload) может быть выключен конфигом, и тогда
	//     прежний выход `p.nctxReload == nil` возвращал запрос как есть — то есть
	//     «окно меньше нужного» снова уезжало в воркер;
	//   - вердикт не перезагружает модель (это делает не-стриминговый путь), он
	//     только приводит num_ctx к загруженному окну либо отказывает с числами.
	// См. growth_stream_guard_r301.go — там же живой дефект и обоснование.
	if len(bodyBuf) == 0 {
		return bodyBuf, true, "", http.StatusOK
	}
	if modelName != "" && isStreamingFromBody("", bodyBuf) {
		v := p.growthVerdictR301(backendID, modelName, bodyBuf, requestPath, true)
		logGrowthVerdictR301("transport-preflight", backendID, modelName, requestPath, v)
		if v.Served {
			return v.Body, true, "", http.StatusOK
		}
		if v.Defer {
			return bodyBuf, false, growthDeferMessageR301(modelName, v), http.StatusServiceUnavailable
		}
		return bodyBuf, true, "", http.StatusOK
	}
	if p.nctxReload == nil {
		return bodyBuf, true, "", http.StatusOK
	}

	// R60.23 (2026-09-09): empty-model guard. Без этой проверки OLD preflight
	// отправлял POST /api/models/load с `"name": ""` → cppworker возвращал
	// 400 "name is required" → state-машина coordinator зависала в
	// "reloading" → ВСЕ последующие запросы получали 503 "retry in 30s"
	// даже после того как реальные /api/chat запросы с правильным model
	// приходили (те попадали на IsReloadPending check с пустым model и
	// пропускались, но profile=65536 mismatch detection в новом
	// runInferencePreflight триггерил ещё одну попытку reload — и тоже
	// падал с 400 "name is required").
	//
	// Корень бага: кто-то отправляет POST /api/load на balancer с пустым
	// body (например, OpenWebUI preload probe или webui ping). parseRequestBody
	// возвращает пустой model, контекст пустой, preflight срабатывает,
	// executeAsyncReload отправляет cppworker'у "name": "" → 400.
	//
	// Защита: при пустом modelName — пропускаем preflight, проксируем
	// запрос как есть. cppworker сам ответит 400/404 на /api/load если
	// endpoint не поддерживается, и это правильное поведение.
	if modelName == "" {
		// R60.23: расширенный лог для диагностики — оператор должен видеть
		// что именно прислал клиент (truncated до 200 символов чтобы не
		// раздувать логи при больших body). Это помогает понять источник
		// (OpenWebUI preload probe, custom integration, webui ping).
		bodyPreview := string(bodyBuf)
		if len(bodyPreview) > 200 {
			bodyPreview = bodyPreview[:200] + "...(truncated)"
		}
		logger.Get().Debugw("preflightNCtxReloadIfNeeded: empty modelName, skipping preflight (R60.23)",
			"backend", backendID, "request_path", requestPath, "body_len", len(bodyBuf),
			"body_preview", bodyPreview)
		return bodyBuf, true, "", http.StatusOK
	}

	// R119 (2026-10-15): модели вообще НЕТ на этом бэкенде — перезагрузкой её не
	// создать. Проверяем ЭТО ПЕРВЫМ, до primary/IsReloadPending/streaming-веток:
	// живой цикл выглядел так — запрос → async-загрузка → cppworker ответил 404
	// со списком available_models → балансер проигнорировал, на следующий запрос
	// снова планировал загрузку, а на запрос после этого срабатывал
	// IsReloadPending-guard и клиент получал 503 «reload already in progress» /
	// «is being reloaded to n_ctx=… retry in 30s». Клиент ждал сотни секунд и
	// падал по своему таймауту («Internal Server Error» у OpenWebUI); замер:
	// POST /api/generate через балансер не отвечал более 4 минут.
	//
	// 404 (а не 503) принципиален: 503 означает «повтори, станет лучше», и клиенты
	// уходят в retry-цикл, а модели от повторения не появится. Ollama для
	// неизвестной модели тоже отдаёт 404.
	if reason, available, known := p.modelMissingFor(backendID, modelName); known {
		logger.Get().Warnw(i18n.T("balancer.nctx_reload_handlers.preflight_model_missing_404_warn"),
			"backend", backendID, "model", modelName,
			"request_path", requestPath, "available_models", len(available))
		return bodyBuf, false,
			formatModelMissingMessage(modelName, reason, available),
			http.StatusNotFound
	}

	// R83 (2026-09-29): PRIMARY — настройки модели зафиксированы администратором.
	//
	// Клиентский запрос НЕ меняет параметры загруженной модели:
	//   * хватает зафиксированного контекста — обслуживаем, подменив num_ctx в теле
	//     на загруженный (cppworker не должен видеть чужой num_ctx);
	//   * не хватает — отказ с указанием зафиксированных значений и подсказкой
	//     «править через администратора». Раньше в этом случае балансер молча
	//     перезагружал модель на бОльший контекст, и администратор узнавал об этом
	//     только по расходу VRAM.
	if p.isPrimaryModel(modelName) {
		return p.applyPrimaryModelPolicy(backendID, modelName, bodyBuf, requestPath)
	}

	// R60.6 (2026-09-07): IsReloadPending guard. Если reload для этой модели
	// уже в процессе (через новый coordinator dedup path в RunPreflight),
	// НЕ запускаем второй reload — просто возвращаем 503. Без этого
	// stream-запросы (которые идут через этот OLD path) триггерят cascading
	// reload goroutines, каждая из которых отменяется http.Client.Timeout
	// на 90s. Client видит 503 → retry → 503 → retry → 5 reloads в логе.
	if p.nctxReload.IsReloadPending(backendID, modelName) {
		// R291: в тексте отказа обязаны быть ЧИСЛА. Живой факт: клиент получал
		// «reload already in progress, retry later» без указания, сколько нужно
		// и сколько есть, — оператор не мог понять, вырастет ли окно вообще.
		estimated, loaded, needed := p.contextGrowthNumbers(backendID, modelName, bodyBuf, requestPath)
		// R299: числа берём из общего источника (growth_guard_r299.go), поэтому
		// activeStreams здесь — максимум из учёта по бэкенду и по модели, а не
		// одно наблюдение, которое могло не увидеть стрим под расходящимся именем.
		active, pending := growthBlockersR288(p, backendID, modelName)
		logger.Get().Infow("preflightNCtxReloadIfNeeded: reload already pending (R60.6 dedup)",
			"backend", backendID, "model", modelName, "request_path", requestPath,
			"estimated_prompt_tokens", estimated, "loaded_window_tokens", loaded,
			"required_tokens", needed,
			"active_streams", active, "reload_pending", pending)
		if estimated > 0 && loaded > 0 {
			// R299: «active streams=0» при идущей перезагрузке — не ложь счётчика,
			// а разные факты: запрос, ждущий в preflight, стримом ещё не учтён
			// (beginClientStreamR271 — после admission). Пишем оба факта явно.
			return bodyBuf, false,
				fmt.Sprintf("model %q reload already in progress (required=%d tokens, loaded window=%d tokens, active streams=%d, reload pending=%t), retry later",
					modelName, needed, loaded, active, pending),
				http.StatusServiceUnavailable
		}
		return bodyBuf, false,
			fmt.Sprintf("model %q reload already in progress, retry later", modelName),
			http.StatusServiceUnavailable
	}

	// R299 (2026-10-29): ТРАНСПОРТНЫЙ путь проверяет то же самое ДО reload'а.
	//
	// Живой факт: это ВТОРОЙ вход в preflight (после раунтера), и на нём запрос
	// точно так же проводил десятки секунд в RunPreflight, хотя окно вырасти не
	// могло. Проверка одна на оба входа (growthDeferR299): если бы правило жило
	// только в раунтере, транспортный путь остался бы прежним — ровно так в этой
	// серии уже случалось с другими механизмами (второй путь обходит общий).
	if deferNow, estimated, loaded, needed, active, pending := p.growthDeferR299(
		backendID, modelName, bodyBuf, requestPath); deferNow {
		logger.Get().Infow("preflightNCtxReloadIfNeeded: context growth deferred before reload (R299)",
			"backend", backendID, "model", modelName, "request_path", requestPath,
			"estimated_prompt_tokens", estimated, "required_tokens", needed,
			"loaded_window_tokens", loaded, "active_streams", active,
			"reload_pending", pending)
		return bodyBuf, false,
			fmt.Sprintf("model %q needs %d tokens of context, the loaded window is %d and cannot grow right now (active streams=%d, reload pending=%t), retry later",
				modelName, needed, loaded, active, pending),
			http.StatusServiceUnavailable
	}

	// R301 (2026-10-29): streaming-запросы сюда уже не доходят — вердикт по ним
	// считается в самом начале функции (см. блок сразу после проверки пустого
	// тела), потому что он нужен и при выключенном координаторе. Здесь остаётся
	// только НЕ-стриминговый путь, где рост окна возможен и выполняется ниже.

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
	// R83-fix (2026-09-30): клиент вообще не задал окно? Это отдельный случай,
	// см. правило ниже (не перезагружать уже загруженную модель).
	clientSilentAboutNCtx := requestedNCtx <= 0
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

	// R83 (2026-09-29): ПОТОЛОК auto-reload применяется и на ЭТОМ пути.
	//
	// ДЕФЕКТ (пойман на живом). В балансере два независимых пути n_ctx-reload:
	// координатор (nctx_reload.go, проверяет cfg.AutoReloadMaxNCtx) и этот —
	// preflightNCtxReloadIfNeeded. Второй потолок НЕ проверял, поэтому клиент
	// мог поднять контекст выше операторского лимита: при LB_NCTX_RELOAD_MAX_N_CTX=16384
	// и загруженных 16384 запрос с num_ctx=32768 вызвал async reload
	//   "preflightNCtxReload: detected n_ctx mismatch, scheduling async reload,
	//    loaded_n_ctx":16384,"requested_n_ctx":32768
	// и модель загрузилась с 32768 — настройка оператора была перебита
	// клиентским запросом, причём молча (в ответе 200).
	//
	// Теперь запрос, требующий контекст выше потолка, отклоняется с понятным
	// объяснением вместо тихой перезагрузки. Сам потолок НЕ ограничивает
	// явную загрузку модели (WebUI/load-with-params) — он про авто-reload.
	// R255 (2026-10-28): потолок авто-роста учитывает ЗАЯВЛЕННОЕ оператором окно.
	//
	// Живая жалоба: оператор выставил в карточке модели 256000 (и модель обучена
	// на 262144, VRAM хватает), а запрос агента на окно больше 131072 упирался в
	// env-потолок LB_NCTX_RELOAD_MAX_N_CTX. Потолок — защита от АВТО-роста по
	// клиентскому запросу; если политику задал сам оператор (профиль модели), его
	// окно и есть потолок. Физический предел (VRAM/GGUF) проверяется отдельно
	// ниже, поэтому «в никуда» окно не вырастет.
	// R267: та же ЕДИНАЯ формула потолка, что у решателя (второй не существует).
	effCeil := 0
	if cfg := p.nctxReload.Config(); cfg.AutoReloadMaxNCtx > 0 {
		effCeil = p.effectiveAutoReloadCeiling(modelName, cfg.AutoReloadMaxNCtx)
	}
	if effCeil > 0 && requestedNCtx > effCeil {
		// R140 (2026-10-16): называем СВЯЗЫВАЮЩИЙ предел, а не только
		// операторский.
		//
		// max_vram_n_ctx воркер считает по ФАКТИЧЕСКИ свободной VRAM, поэтому
		// он учитывает и другие загруженные модели. Если он ниже операторского
		// лимита, совет «поднимите LB_NCTX_RELOAD_MAX_N_CTX» невыполним: окно
		// всё равно не влезет, и клиент уйдёт в бесплодный цикл поднятия
		// настройки. Когда физический потолок неизвестен (0 = poller ещё не
		// опросил бэкенд) — не выдумываем число и говорим только про
		// операторский лимит, как раньше.
		physicalMax := p.getMaxVRAMNCtxFromMetrics(backendID)
		binding := effCeil
		if physicalMax > 0 && physicalMax < binding {
			binding = physicalMax
		}
		logger.Get().Warnw(i18n.T("balancer.nctx_reload_handlers.requested_above_limit_no_reload_warn"),
			"backend", backendID, "model", modelName,
			"requested_n_ctx", requestedNCtx,
			"auto_reload_max_n_ctx", effCeil,
			"max_vram_n_ctx", physicalMax,
			"binding_limit", binding,
			"hint", i18n.T("balancer.nctx_reload_handlers.requested_above_limit_hint"))
		return bodyBuf, false,
			formatNCtxCeilingRefusal(requestedNCtx, effCeil, physicalMax, modelName),
			http.StatusRequestEntityTooLarge
	}

	// 2. R118 (2026-10-14): ЧТО ИМЕННО известно о модели — «загружена»,
	//    «грузится прямо сейчас», «модели нет» или «состояние неизвестно».
	//
	// ДО R118 все четыре случая приходили сюда как loaded_n_ctx=0, и preflight в
	// каждом из них планировал загрузку/перезагрузку. Живое воспроизведение на
	// стенде (см. шапку loadedWindowState в num_ctx_resolver.go): пока оператор
	// грузил модель с ctx=32768, клиентский запрос с num_ctx=8192 запустил
	// ВТОРУЮ загрузку, cppworker выгрузил первую («reloading model because
	// parameters changed»), она упала с «model load aborted by user» (71 c
	// впустую), а клиент получил 503 «being reloaded ... retry in 30s».
	//
	// ТЕПЕРЬ: «грузится» — ждём свою же загрузку, второй не запускаем;
	// «неизвестно» (опрос /api/models не прошёл) — не принимаем разрушительных
	// решений, запрос идёт как есть; «загружена» — обычная логика ниже;
	// «модели нет» — загрузка с окном из настроек (тоже ниже).
	loadedNCtx, loadedState := p.resolveLoadedWindowNCtx(backendID, modelName, true)
	switch loadedState {
	case loadedWindowLoading:
		logger.Get().Infow(i18n.T("balancer.nctx_reload_handlers.model_loading_skip_second_load_info"),
			"backend", backendID, "model", modelName,
			"requested_n_ctx", requestedNCtx,
			"hint", i18n.T("balancer.nctx_reload_handlers.model_loading_hint"))
		return bodyBuf, false,
			fmt.Sprintf("model %q is loading on backend %s; waiting for the load already in progress "+
				"(no second load started, prepared n_ctx is preserved)", modelName, backendID),
			http.StatusServiceUnavailable
	case loadedWindowUnknown:
		logger.Get().Warnw(i18n.T("balancer.nctx_reload_handlers.model_state_unknown_no_reload_warn"),
			"backend", backendID, "model", modelName,
			"requested_n_ctx", requestedNCtx,
			"hint", i18n.T("balancer.nctx_reload_handlers.model_state_unknown_hint"))
		return bodyBuf, true, "", http.StatusOK
	}

	// R83-fix (2026-09-30): сравниваем с ОКНОМ СЛОТА, а не с суммарным.
	//
	// `loadedNCtx` — СУММАРНОЕ окно модели (его же кладём в кэш метрик и
	// сравниваем с памятью). Но клиент просит окно ДЛЯ СЕБЯ: при двух слотах
	// суммарные 65536 дают клиенту 32768, и запрос с num_ctx=65536 формально
	// «покрыт» (65536 ≥ 65536), хотя фактически урезан вдвое — длинный промпт
	// Cline не влезает, клиент получает
	//   "preflight: prompt + n_predict exceeds n_ctx for this backend".
	// Живое воспроизведение: gemma-4-E4B загружена ctx=65536, max_slots=2,
	// клиент просит 65536 → на клиента 32768.
	//
	// Поэтому все сравнения/патчи тела ниже идут по perSeq, а суммарное значение
	// остаётся для reload-таргета (окно клиента × слоты) и для кэша метрик.
	if perSeq := p.loadedPerSeqNCtx(backendID, modelName); perSeq > 0 {
		if perSeq != loadedNCtx {
			logger.Get().Infow(i18n.T("balancer.nctx_reload_handlers.window_total_vs_per_seq_info"),
				"backend", backendID, "model", modelName,
				"total_n_ctx", loadedNCtx, "context_per_seq", perSeq)
		}
		loadedNCtx = perSeq
	}

	// R83-fix (2026-09-30): КЛИЕНТ НЕ ЗАДАЛ ОКНО + МОДЕЛЬ ЗАГРУЖЕНА → НЕ ТРОГАЕМ.
	//
	// ТРЕБОВАНИЕ ЭКСПЛУАТАЦИИ: «если клиент не прислал, с каким окном загружать,
	// то если модель загружена — перезагружать не надо; если не загружена — грузить
	// дефолтными значениями из настроек».
	//
	// ЧТО БЫЛО. При body_num_ctx=0 preflight подставлял contextLength профиля
	// (он задуман как HINT) и, если загруженный n_ctx ему не равнялся, запускал
	// async reload. Живой пример (проверено на стенде): оператор загрузил модель
	// с ctx=8192, первый же клиентский запрос БЕЗ num_ctx перезагрузил её в 16384
	// (в логе: chosen_source=profile, «detected n_ctx mismatch, scheduling async
	// reload»). Для оператора это выглядит как «настройки сбиваются сами».
	//
	// ЧТО СТАЛО. Если модель уже загружена и клиент окно не запрашивал — работаем
	// с тем, что загружено: это и есть настройки администратора. Клиент, который
	// не помещается в текущий контекст, получит от cppworker понятную ошибку, и
	// только тогда сработает реактивный путь (handleNCtxReload /
	// DecideReloadBackend) — то есть перезагрузка происходит по фактической
	// необходимости, а не спекулятивно.
	//
	// Если модель НЕ загружена (loadedNCtx == 0), правило не применяется: ниже
	// работает обычный приоритет «профиль → дефолт бэкенда», то есть загрузка
	// идёт значениями из настроек.
	if clientSilentAboutNCtx && loadedNCtx > 0 {
		logger.Get().Infow(i18n.T("balancer.nctx_reload_handlers.client_silent_loaded_no_reload_info"),
			"backend", backendID, "model", modelName,
			"loaded_n_ctx", loadedNCtx,
			"profile_hint_n_ctx", profileNCtx,
			"note", i18n.T("balancer.nctx_reload_handlers.client_silent_loaded_note"))
		return bodyBuf, true, "", http.StatusOK
	}

	// 3. R60.35: stickiness check с учётом `required` (prompt + n_predict).
	//
	// Симптом (R60.35): OpenWebUI шлёт num_ctx=2048 в body, модель loaded
	// с n_ctx=2048 (default), prompt=33 + n_predict=2048 (cppworker
	// default) = 2082 > 2048 → 400 "prompt too long". Старый preflight
	// видел `loaded (2048) >= requested (2048)` → NoOp. Не учитывал что
	// `required (prompt + n_predict + slack) > loaded` → reload бы
	// помог. Дубликат R60.31 bug, но в OLD preflight path.
	//
	// R60.31 fix был только в DecidePreflight. Этот path — старый
	// "smart reload skip" (lines 586+), он работает для n_predict=0
	// (Ollama streaming с default = unlimited), но НЕ для n_predict>0.
	// Исправляем здесь тоже: если required > loaded, всё равно reload.
	//
	// R60.46 (2026-09-11) FIX: НЕ reload если user не указал n_predict.
	// Pre-R60.46: n_predict=0 → assumed cppworker default 2048 → required=2050
	// > loaded=2048 → trigger reload to 4096. Это FALSE POSITIVE:
	// 1) User не просил 2048 токенов output, может ему хватит 100
	// 2) Reload 2048→4096 занимает 90-180s (cascade timeout)
	// 3) User застрял в reload loop
	// Post-R60.46: n_predict=0 → не assume, skip reload check.
	// Если действительно overflow — cppworker вернёт 400, user retry с proper n_predict.
	if loadedNCtx >= requestedNCtx {
		// R60.35: дополнительно проверяем required (с учётом n_predict).
		// OpenAI /v1/chat/completions: max_tokens=0 → cppworker default 2048.
		// Ollama /api/chat: num_predict=0 → cppworker default 2048.
		// Реальный required = prompt + 2048 + 1 = может превысить loaded.
		//
		// R60.46: только check required если user ЯВНО указал n_predict > 0.
		// Иначе (n_predict=0 = default) — НЕ reload, пусть cppworker сам
		// отдаст 400 если overflow.
		meta := p.extractRequestMeta(bodyBuf, requestPath)
		if meta == nil {
			return bodyBuf, true, "", http.StatusOK
		}
		// R60.46: skip reload check если user не указал n_predict.
		// (Иначе false positive: user не просил 2048 токенов, но R60.35
		// assumes cppworker default и triggers reload 2048→4096.)
		if meta.RequestedNPredict <= 0 {
			return bodyBuf, true, "", http.StatusOK
		}
		// R83-fix (2026-09-30): запас ограничен сверху (см. preflight_nctx.go) —
		// иначе на длинных сессиях 10% превращались в десятки тысяч токенов и
		// запрос, который помещается, отклонялся как «prompt + n_predict exceeds».
		reserveSlack := meta.EstimatedPromptTokens / 10
		if reserveSlack > maxReserveSlackTokens {
			reserveSlack = maxReserveSlackTokens
		}
		nPredict := meta.RequestedNPredict
		required := meta.EstimatedPromptTokens + nPredict + 1 + reserveSlack
		if required > loadedNCtx {
			// R60.35: prompt не влезает → trigger reload на larger n_ctx.
			// Сначала patch'нем body чтобы убрать body num_ctx (чтобы
			// после reload cppworker не получал n_ctx=2048 опять).
			// Новый target n_ctx: max(required, loadedNCtx*2) rounded up.
			newTarget := required
			if newTarget < loadedNCtx*2 {
				newTarget = loadedNCtx * 2
			}
			// Round up to power of 2
			roundedTarget := roundUpPow2(newTarget)
			patchedBody := patchNumCtxInBody(bodyBuf, roundedTarget)
			if patchedBody == nil {
				patchedBody = bodyBuf
			}
			logger.Get().Infow("preflightNCtxReload: R60.35 — required > loaded, triggering reload",
				"backend", backendID, "model", modelName,
				"loaded_n_ctx", loadedNCtx,
				"requested_n_ctx", requestedNCtx,
				"required_n_ctx", required,
				"new_target_n_ctx", roundedTarget,
				"estimated_prompt_tokens", meta.EstimatedPromptTokens,
				"n_predict", nPredict)
			// Fall through to reload path (don't return early)
			bodyBuf = patchedBody
			requestedNCtx = roundedTarget
			// Continue to the reload logic below
		} else {
			// Smart skip: patch body to use loaded_n_ctx, no reload needed.
			patchedBody := patchNumCtxInBody(bodyBuf, loadedNCtx)
			if patchedBody != nil {
				logger.Get().Debugw("preflightNCtxReload: R60.35 — required ≤ loaded, no reload",
					"backend", backendID, "model", modelName,
					"loaded_n_ctx", loadedNCtx,
					"required_n_ctx", required)
				return patchedBody, true, "", http.StatusOK
			}
			return bodyBuf, true, "", http.StatusOK
		}
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
	// R60.44 (2026-09-11) FIX: smart-skip ТОЛЬКО если requested_n_ctx <= loadedNCtx.
	// До фикса: smart-skip firing когда prompt помещается в loaded, даже если
	// user явно просил num_ctx > loaded (например body=16384, loaded=8192).
	// Patch body до loadedNCtx (8192), но X-Cpp-Ctx header ставился от
	// ОРИГИНАЛЬНОГО body (16384). cppworker получал body с num_ctx=8192 +
	// header n_ctx=16384, отвергал 400 "exceeds model's effective n_ctx".
	// Затем balancer auto-reload триггерился → unload → reload → 3+ минуты.
	// User видел "empty response / timeout" (exactly user's complaint).
	//
	// Правильная логика: smart-skip ТОЛЬКО когда requested_n_ctx <= loadedNCtx.
	// В этом случае patch body (downgrade к loadedNCtx — same value anyway,
	// noop) И не триггерим reload.
	//
	// Когда requested_n_ctx > loadedNCtx: skip the smart-skip и fallthrough
	// к reload path (line 692+). Preflight запустит async reload, user
	// получит 503 + Retry-After, через retry получит успех с новым n_ctx.
	if requestedNCtx > 0 && loadedNCtx > 0 && requestedNCtx <= loadedNCtx {
		meta := p.extractRequestMeta(bodyBuf, requestPath)
		if meta != nil {
			required := requiredNCtxForMeta(meta)
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
	// R83-fix (2026-09-30): клиентское окно — НА КЛИЕНТА, а не суммарное.
	//
	// Семантика клиента (Cline/OpenWebUI/Ollama): `num_ctx=N` — окно, которое
	// нужно ЭТОМУ запросу. llama.cpp при kv_unified=false делит суммарное окно
	// между слотами (n_ctx_seq = PAD(n_ctx / slots, 256)), поэтому при двух
	// слотах запрос с num_ctx=65536 получал бы только 32768 — и длинный промпт
	// Cline (system + tools + история) не влезал: клиент видел
	//   "preflight: prompt + n_predict exceeds n_ctx for this backend".
	// Именно это воспроизведено на стенде: gemma-4 загружена с ctx=65536 и
	// max_slots=2 → на клиента 32768.
	//
	// Теперь суммарное окно, до которого делаем reload, считается как
	// «окно клиента × слоты» (PAD256), то есть каждый клиент получает ровно
	// запрошенное, а memfit проверяет суммарный объём.
	reloadTarget := requestedNCtx
	if slots := p.loadedSlotsNCtx(backendID, modelName); slots > 1 {
		if total, per := cppbackend.EffectiveContextSize(0, requestedNCtx, slots); total > 0 {
			reloadTarget = total
			logger.Get().Infow(i18n.T("balancer.nctx_reload_handlers.client_window_total_recalculated_info"),
				"backend", backendID, "model", modelName,
				"requested_per_seq", per, "slots", slots, "total_n_ctx", total)
		}
	}

	// R118.7 (2026-10-14): cppworker уже отвечал 422 n_ctx_infeasible на такое
	// окно — не планируем эту перезагрузку снова, а объясняем клиенту.
	//
	// ЖИВОЙ ЦИКЛ (лог стенда): запрос → «detected n_ctx mismatch» → async reload
	// target 16384 → 422 n_ctx_infeasible («stage: does_not_fit», «веса не
	// помещаются ни в VRAM, ни в RAM») → запрос снова → снова reload… Клиент при
	// этом ждал до 33 с и получал окно меньше запрошенного без объяснения.
	if ceiling, infeasibleReason, known := p.nctxInfeasibleFor(backendID, modelName); known && reloadTarget > ceiling {
		logger.Get().Warnw(i18n.T("balancer.nctx_reload_handlers.window_infeasible_no_reload_warn"),
			"backend", backendID, "model", modelName,
			"loaded_n_ctx", loadedNCtx, "requested_n_ctx", requestedNCtx,
			"reload_target_total", reloadTarget, "ceiling_n_ctx", ceiling,
			"reason", infeasibleReason)
		return bodyBuf, false,
			fmt.Sprintf("backend %s cannot grow the context to %d tokens (client asked %d per slot): %s",
				backendID, reloadTarget, requestedNCtx, infeasibleReason),
			http.StatusRequestEntityTooLarge
	}

	// R119 (2026-10-15): модели вообще НЕТ на этом бэкенде — перезагрузкой её не
	// создать. Проверка стоит В НАЧАЛЕ функции (см. выше), сюда доходит только
	// случай, когда запись появилась уже во время этого запроса.
	if reason, available, known := p.modelMissingFor(backendID, modelName); known {
		return bodyBuf, false,
			formatModelMissingMessage(modelName, reason, available),
			http.StatusNotFound
	}

	logger.Get().Infow("preflightNCtxReload: detected n_ctx mismatch, scheduling async reload",
		"backend", backendID, "model", modelName,
		"loaded_n_ctx", loadedNCtx,
		"requested_n_ctx", requestedNCtx,
		"reload_target_total", reloadTarget,
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
	//
	// R69 (2026-09-23): через единый gate — если для этой модели reload уже идёт
	// (preflight-путь или обработчик 413), вторую загрузку не стартуем: cppworker
	// отменяет предыдущую («model load aborted by user»), rollback падает и модель
	// остаётся выгруженной.
	if !p.nctxReload.StartModelReloadIfNotPending(backendID, modelName, func(_ *reloadEntry) {
		p.executeAsyncReload(backendID, modelName, reloadTarget, backendState.Backend)
	}) {
		logger.Get().Infow("preflightNCtxReload: reload already pending for this model, reusing it (R69 dedup)",
			"backend", backendID, "model", modelName, "requested_n_ctx", requestedNCtx)
	}

	// Обновляем кэш метрик: ставим ContextLength = requestedNCtx,
	// чтобы следующие preflight-чеки не запускали reload повторно.
	// Реальное значение обновит metrics_poller через ~5 сек после успешного reload.
	p.cacheLoadedContextLength(backendID, modelName, reloadTarget)

	// R68 (2026-09-23): ждём завершения reload'а и обслуживаем ПЕРВЫЙ же запрос.
	// Раньше клиент получал 503 «being reloaded to n_ctx=…; retry in 30s» и
	// (Cline/OpenWebUI) показывал ошибку — жалоба «проходит только повторный
	// запрос». Ожидание ограничено LB_NCTX_PREFLIGHT_WAIT_SEC (default 240 c,
	// 0 = прежнее поведение).
	if wait := nctxPreflightWaitTimeout(); wait > 0 {
		waitStart := time.Now()
		if p.waitForBackendNCtx(context.Background(), backendID, modelName, reloadTarget, wait) {
			p.cacheLoadedContextLength(backendID, modelName, reloadTarget)
			logger.Get().Infow("preflightNCtxReload: reload finished while client waited, serving request",
				"backend", backendID, "model", modelName,
				"new_n_ctx", reloadTarget,
				"waited_ms", time.Since(waitStart).Milliseconds())
			return bodyBuf, true, "", http.StatusOK
		}
		logger.Get().Warnw("preflightNCtxReload: wait for reload timed out, returning 503",
			"backend", backendID, "model", modelName,
			"requested_n_ctx", requestedNCtx,
			"waited_ms", time.Since(waitStart).Milliseconds())
	}

	// R119 (2026-10-15): пока клиент ждал, фоновая загрузка могла получить от
	// cppworker терминальный 404 «модели нет» (executeAsyncReload записал это в
	// реестр). Тогда отдаём 404 сразу, а не 503 «повтори через 30 с»: модели от
	// повторения не появится, а клиент (OpenWebUI) на 503 уходит в retry-цикл и в
	// итоге показывает «Internal Server Error».
	//
	// Гонку «запись появилась на миллисекунды позже решения» здесь НЕ закрываем
	// ожиданием: preflight обязан возвращать управление сразу после планирования
	// перезагрузки (на это опираются тесты роста окна — им важно, что reload ещё
	// в полёте). Короткое ожидание делает вызывающий транспорт перед тем, как
	// отдать клиенту 503 (см. llamacpp_transport*.go).
	if reason, available, known := p.modelMissingFor(backendID, modelName); known {
		logger.Get().Warnw(i18n.T("balancer.nctx_reload_handlers.model_missing_404_warn"),
			"backend", backendID, "model", modelName,
			"available_models", len(available))
		return bodyBuf, false,
			formatModelMissingMessage(modelName, reason, available),
			http.StatusNotFound
	}

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

// lastKnownNCtxFor — R68: фактический n_ctx из координатора reload'а
// (0 = неизвестен). Нужен как более свежий источник, чем кэш metrics poller.
func (p *Proxy) lastKnownNCtxFor(backendID string) int {
	if p == nil || p.nctxReload == nil || backendID == "" {
		return 0
	}
	return p.nctxReload.LastKnownNCtx(backendID)
}

// cacheLoadedContextLength — R68 (2026-09-23): записать n_ctx загруженной модели
// в кэш метрик СРАЗУ после reload'а.
//
// Зачем: transport-level preflight (preflightNCtxReloadIfNeeded) и
// collectPreflightState читают loaded n_ctx из metricsMgr.llamaMetrics, который
// обновляет poller раз в ~30 c. Без немедленного обновления кэша следующий
// запрос видел бы старый n_ctx и запускал ВТОРОЙ reload (клиент получал 503
// сразу после того, как дождался первого).
func (p *Proxy) cacheLoadedContextLength(backendID, modelName string, nCtx int) {
	if p == nil || p.metricsMgr == nil || nCtx <= 0 || modelName == "" {
		return
	}
	p.metricsMgr.mu.Lock()
	defer p.metricsMgr.mu.Unlock()
	lm, ok := p.metricsMgr.llamaMetrics[backendID]
	if !ok || lm == nil {
		lm = &types.LlamaCppMetrics{}
		p.metricsMgr.llamaMetrics[backendID] = lm
	}
	for i := range lm.LoadedModels {
		if lm.LoadedModels[i].Name == modelName || containsFold(lm.LoadedModels[i].Name, modelName) {
			lm.LoadedModels[i].ContextLength = nCtx
			return
		}
	}
	lm.LoadedModels = append(lm.LoadedModels, types.LlamaCppModel{
		Name:          modelName,
		State:         "loaded",
		ContextLength: nCtx,
	})
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
//
// formatNCtxCeilingRefusal — отказ на окно выше предела авто-перезагрузки.
//
// R140 (2026-10-16): предел зависит от ДВУХ чисел, и клиенту нужно то, которое
// реально связывает:
//
//   - operatorMax — потолок оператора (LB_NCTX_RELOAD_MAX_N_CTX);
//   - physicalMax — max_vram_n_ctx от воркера: сколько токенов контекста влезает
//     в ФАКТИЧЕСКИ свободную VRAM, то есть с учётом других загруженных моделей.
//
// Когда связывает физический предел, совет «поднимите LB_NCTX_RELOAD_MAX_N_CTX»
// невыполним — окно не влезет. Тогда говорим именно это и подсказываем то, что
// действительно освобождает окно (выгрузить другие модели, меньшая модель,
// меньше слоёв на GPU). Когда физический предел неизвестен (0) — не выдумываем
// число, текст остаётся прежним.
func formatNCtxCeilingRefusal(requested, operatorMax, physicalMax int, modelName string) string {
	if physicalMax > 0 && physicalMax < operatorMax {
		return fmt.Sprintf("requested num_ctx=%d exceeds the backend's physical limit max_vram_n_ctx=%d "+
			"(what fits in free VRAM with the models loaded now; operator limit auto_reload_max_n_ctx=%d is higher). "+
			"Model %q keeps its loaded n_ctx. Free VRAM (unload other models, use a smaller model or fewer GPU layers) "+
			"or load the model explicitly with the needed contextSize.",
			requested, physicalMax, operatorMax, modelName)
	}
	return fmt.Sprintf("requested num_ctx=%d exceeds operator limit auto_reload_max_n_ctx=%d "+
		"(model %q keeps its loaded n_ctx). Raise LB_NCTX_RELOAD_MAX_N_CTX "+
		"or load the model explicitly with the needed contextSize.",
		requested, operatorMax, modelName)
}

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

	// R60.23 (2026-09-09): defensive guard — двойная защита на случай если
	// какой-то caller забудет проверить modelName. Без этого cppworker
	// получает POST /api/models/load с `"name": ""` и возвращает 400
	// "name is required" — reload фактически не происходит, но state-машина
	// coordinator'а может зависнуть в "reloading" если логика обработки
	// статуса опирается только на success/failure без проверки body.
	if modelName == "" {
		logger.Get().Warnw("executeAsyncReload: empty modelName, skipping (R60.23 defensive guard)",
			"backend", backendID, "target_n_ctx", requestedNCtx)
		return
	}

	// R271 (2026-10-29): НЕ выгружать модель под чужим активным стримом.
	//
	// ЖИВОЙ ДЕФЕКТ (второй стенд): пока Cline стримил ответ, второй агент (DSH)
	// запросил другое окно n_ctx → preflight решил перезагрузить модель →
	// cppworker для смены n_ctx делает unload+load → у первого клиента поток
	// закрылся без терминального маркера («stream ended by upstream before
	// completion»). Теперь ждём завершения стримов на этом бэкенде (до
	// LB_RELOAD_DEFER_MAX_SEC, дефолт 120 с); не дождались — перезагрузку
	// пропускаем: следующий запрос снова упрётся в n_ctx и повторит попытку,
	// это дешевле, чем оборвать чужую генерацию.
	// R291: этот вызов идёт СИНХРОННО из preflight, то есть держит HTTP-ответ
	// клиента. Живой факт: /api/chat ждал 122 с и только потом получал 503 без
	// чисел — ровно это ожидание и называлось «балансер молчит». Теперь при
	// активных стримах отвечаем/пропускаем сразу, а клиенту числа объяснит
	// context-growth-отказ (R288/R291).
	if ok, active := p.deferDestructiveOpR291(context.Background(), backendID, modelName, "nctx-preflight-reload"); !ok {
		logger.Get().Warnw("executeAsyncReload: postponed — active streams on backend (R271)",
			"backend", backendID, "model", modelName,
			"target_n_ctx", requestedNCtx, "active_streams", active,
			"defer_budget_sec", reloadDeferMaxWaitR271().Seconds())
		return
	}
	p.noteDestructiveOpR271(backendID, modelName, destructiveReloadR271)

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

	reloadPayload, _ := json.Marshal(asyncReloadLoadBody(modelName, requestedNCtx,
		p.reloadHintsFor(backendID, modelName)))

	reloadCtx, cancel := reloadTimeoutContext(context.Background(), 0)
	defer cancel()

	req, reqErr := http.NewRequestWithContext(reloadCtx, "POST", targetURL, bytes.NewReader(reloadPayload))
	if reqErr != nil {
		// R296: запрос даже не собран — перезагрузки не было, пометка не должна
		// объяснять чужие обрывы.
		p.clearDestructiveOpR271(backendID, modelName)
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
		// R296: запрос не дошёл (воркер убит / сеть) — модель НЕ выгружали по
		// нашей команде, значит причина обрыва не наша.
		p.clearDestructiveOpR271(backendID, modelName)
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
		// R296: перезагрузка не состоялась — снимаем пометку, иначе следующий
		// оборванный стрим будет объяснён нашей перезагрузкой, которой не было.
		p.clearDestructiveOpR271(backendID, modelName)
		logger.Get().Warnw("preflightNCtxReload(async): reload returned non-OK status",
			"backend", backendID, "model", modelName,
			"target_n_ctx", requestedNCtx,
			"status", resp.StatusCode,
			"duration_ms", duration.Milliseconds(),
			"body", string(body))
		// R118.7 (2026-10-14): 422 n_ctx_infeasible — окно физически не помещается.
		// Запоминаем потолок, чтобы НЕ планировать эту перезагрузку на каждом
		// следующем запросе (живой цикл: запрос → reload → 422 → запрос → reload…),
		// и чтобы клиент получил внятный отказ с числами и причиной.
		if isNCtxInfeasibleResponse(resp.StatusCode, body) {
			p.recordNCtxInfeasible(backendID, modelName, requestedNCtx, body)
		}
		// R119 (2026-10-15): 404 «модели нет» — ТЕРМИНАЛЬНЫЙ отказ, а не повод
		// повторить. Раньше он игнорировался, и балансер на каждый следующий
		// запрос снова планировал загрузку, а клиенту отдавал 503 «is being
		// reloaded … retry in 30s» → retry-цикл до таймаута клиента (живой замер:
		// POST /api/generate через балансер не отвечал более 4 минут).
		if isModelMissingResponse(resp.StatusCode, body) {
			p.recordModelMissing(backendID, modelName, body)
		}
		return
	}

	logger.Get().Infow("preflightNCtxReload(async): reload SUCCESS",
		"backend", backendID, "model", modelName,
		"target_n_ctx", requestedNCtx,
		"duration_ms", duration.Milliseconds())
	// R296: перезагрузка ПОДТВЕРЖДЕНА воркером — только теперь обрыв чужого
	// стрима в пределах окна атрибуции честно называется нашим.
	//
	// Исключение — dedup-ответ `status=already_loaded`: cppworker ничего не
	// выгружал (модель уже стояла с тем же n_ctx), снимать её было не за что.
	// Подтверждать unload в этом случае значило бы снова врать о причине.
	if reloadDedupNoUnloadR296(body) {
		p.clearDestructiveOpR271(backendID, modelName)
		logger.Get().Infow("preflightNCtxReload(async): worker reported already_loaded — model was not unloaded, marker cleared (R296)",
			"backend", backendID, "model", modelName, "target_n_ctx", requestedNCtx)
	} else {
		p.confirmDestructiveOpUnloadedR271(backendID, modelName)
	}
	// Перезагрузка удалась — прежний отказ больше не актуален.
	p.clearNCtxInfeasible(backendID, modelName)
	// R119: модель загрузилась — значит она ЕСТЬ, и запись «модели нет» устарела.
	p.clearModelMissing(backendID, modelName)
}

// reloadDedupNoUnloadR296 — воркер ответил dedup'ом, то есть НЕ выгружал модель.
//
// ЗАЧЕМ (R296). Причина обрыва стрима обязана определяться фактом («модель
// реально снимали»), а не фактом «мы попросили её снять». `/api/models/load`
// отвечает `{"status":"already_loaded"}`, когда параметры совпали: cppworker
// пропускает unload+load, ни один стрим от этой команды оборваться не мог.
// Засчитывать такой ответ за подтверждённую выгрузку — значит сохранить живой
// дефект: `docker kill` воркера подписывался как «балансер перезагрузил модель».
func reloadDedupNoUnloadR296(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var probe struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(probe.Status), "already_loaded")
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
//
// R265 (2026-10-28): ПОЧЕМУ ЭТО НЕ СВЕДЕНО С resolvePhysicalMaxContext.
// Вопрос здесь другой: «какой предел окна отдать решению о reload», а не
// «что физически возможно». Различия содержательные, а не косметические:
//   - в hard-режиме contextLength (профиль) — CAP, то есть значение МОЖЕТ быть
//     НИЖЕ физики (оператор сознательно ограничил модель), а физический потолок
//     по определению ниже не опускается;
//   - в auto-режиме наоборот: профиль — HINT, а cap = min(ggufMax, contextLengthMax);
//   - здесь же живут диагностические предупреждения оператору (профиль
//     консервативнее физики, потолок >4x текущей достижимости), которых в
//     физическом потолке нет по смыслу.
//
// Сведение изменило бы поведение preflight, поэтому в R265 оставлено обёрткой-
// комментарием и закреплено характеризационными тестами.
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
					"recommendation", i18n.T("balancer.nctx_reload_handlers.profile_conservative_recommendation"),
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
