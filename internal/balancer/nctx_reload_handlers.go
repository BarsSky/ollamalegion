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

	"ollama-loadbalancer/internal/i18n"
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
	// R504 / аудит N-2: решателю передаётся МОДЕЛЬ, чтобы он знал число слотов:
	// воркер сообщает требование на слот, а contextSize в перезагрузке суммарный.
	// Без этого при parallel=2 модель поднималась ровно на окно слота, и клиент
	// получал тот же 413 по кругу.
	plan := p.nctxReload.DecideReloadBackendForModel(backendID, modelName, bridgeErr, requestedOverride, effectiveCeiling)
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
		// R402: отказ перезагрузки — это техническая причина, а не повод
		// отправлять клиента «сохранить профиль и перезагрузить вручную».
		// Перезагрузку уже пытался сделать сам балансер; сообщаем, что именно
		// упало, и оставляем числа/код для клиента и оператора.
		writeNCtxJSONError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("n_ctx auto-reload failed: %v (the balancer attempted the reload itself; see balancer log for the backend error and memory verdict)", reloadErr),
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
