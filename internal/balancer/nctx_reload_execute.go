// nctx_reload_execute.go — R631 (2026-11-19): Исполнение перезагрузки: синхронный preflight, ожидание pending, асинхронный executeAsyncReload.
//
// ЗАЧЕМ ВЫНЕСЕНО. `nctx_reload_handlers.go` держал 2316 строк и четыре
// независимых слоя (обработчики, preflight, кэш наблюдённого окна, исполнение
// и метрики). Здесь — один из них; поведение не менялось, код перенесён как есть.
package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"os"
	"time"
)

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

	// R472 (2026-11-06): 202 Accepted — это НАЧАТАЯ фоновая загрузка, а не отказ.
	//
	// Живая проверка: воркер отвечает 202 на POST /api/models/load («Model load
	// started in background. Poll progressUrl…»), а балансер считал это
	// «reload returned non-OK status», снимал пометку destructive-op и писал WARN
	// на КАЖДОМ успешном росте окна. Оператор видел предупреждение при полностью
	// здоровой перезагрузке, а координатор терял след «идёт перезагрузка».
	if resp.StatusCode == http.StatusAccepted {
		logger.Get().Infow("preflightNCtxReload(async): reload accepted in background (202)",
			"backend", backendID, "model", modelName,
			"target_n_ctx", requestedNCtx,
			"duration_ms", duration.Milliseconds())
		return
	}
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
