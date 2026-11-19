// preflight_nctx_run.go — R635 (2026-11-19): исполнение preflight: RunPreflight и оценка времени перезагрузки.
//
// ЗАЧЕМ ВЫНЕСЕНО. `preflight_nctx.go` держал 1601 строку: разбор запроса и
// метаданных, решение о preflight, отказы и сам RunPreflight. Здесь — один из слоёв;
// поведение не менялось.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"time"
)

// RunPreflight — точка входа из роутеров. Возвращает:
//
//   - result, nil если NoOp или успешный reload
//   - result с PreflightReject если нужно отказать
//   - error если reload упал (тогда balancer вернёт 502 + retry)
//
// Использует существующий NCtxReloadCoordinator для параллельной защиты.
func (c *NCtxReloadCoordinator) RunPreflight(
	ctx context.Context,
	backendID, backendAddr string,
	meta *RequestMeta,
	state *NCtxBackendState,
	loader NCtxReloadHTTPClient,
) (*PreflightResult, error) {
	cfg := c.Config()
	decision := DecidePreflight(meta, state, cfg)
	switch decision.Decision {
	case PreflightNoOp:
		return decision, nil
	case PreflightReject:
		logger.Get().Warnw("preflight: reject (VRAM/model_max exhausted)",
			"backend_id", backendID,
			"required_n_ctx", state.CurrentNCtx,
			"reason", decision.RejectBody)
		return decision, nil
	case PreflightReload:
		if !cfg.AutoReloadNCtx {
			// Kill-switch: reload отключён в конфиге.
			logger.Get().Infow("preflight: reload disabled in config",
				"backend_id", backendID, "required_n_ctx", decision.TargetNCtx)
			reject := makePreflightReject(state, decision.TargetNCtx,
				"auto_reload_n_ctx is disabled in balancer config")
			return reject, nil
		}
		plan := &ReloadPlan{
			Decision: DecisionReload,
			NewNCtx:  reloadTargetForState(decision.TargetNCtx, state),
			Reason: fmt.Sprintf("preflight: target=%d (current=%d, context_per_seq=%d, slots=%d, max_vram=%d, model_max=%d)",
				reloadTargetForState(decision.TargetNCtx, state),
				state.CurrentNCtx, state.CurrentContextPerSeq, state.effectiveSlots(),
				state.MaxVRAMNCtx, state.ModelMaxContext),
		}
		// R294 (2026-10-29): НИКОГДА НЕ УМЕНЬШАТЬ ОКНО.
		//
		// Живой дефект: клиент просил num_ctx=16384 при загруженных 8192, а
		// выведенное окно посчиталось как «промпт + бюджет(0) = 4165», и
		// балансер перезагрузил модель в МЕНЬШЕЕ окно — под живым стримом.
		// Уменьшение окна не может быть следствием запроса «дайте больше»:
		// это либо ошибка расчёта, либо чужая политика, и в обоих случаях
		// перезагрузка бессмысленна — она только рвёт текущие ходы.
		if plan.NewNCtx > 0 && state.CurrentNCtx > 0 && plan.NewNCtx < state.CurrentNCtx {
			logger.Get().Warnw("preflight: refusing to shrink n_ctx window",
				"backend_id", backendID,
				"model", gateModelNameOr(meta),
				"target_n_ctx", plan.NewNCtx,
				"current_n_ctx", state.CurrentNCtx,
				"requested_per_client", requestedWindowOr(meta),
				"action", "no_op: a request never justifies a smaller window")
			return &PreflightResult{Decision: PreflightNoOp}, nil
		}

		// R83-fix (2026-09-30): ПРОВЕРКА ДОСТИЖИМОСТИ. Не перезагружаем модель,
		// если рост окна недостижим или достигается ценой потери GPU-слоёв —
		// подробности в growthWorthReload. Без этой проверки живой стенд уходил
		// в 2–3-минутную выгрузку/загрузку с тем же самым окном.
		gateModelName := ""
		if meta != nil {
			gateModelName = meta.ModelName
		}
		worth, achievablePerClient, why := c.growthWorthReload(backendAddr, backendID, gateModelName,
			plan.NewNCtx, state.CurrentNCtx, state.effectivePerSeqNCtx(),
			c.ReloadHintsFor(backendID, gateModelName).GPULayers, state.effectiveSlots())
		if !worth {
			reqWindow := 0
			if meta != nil {
				reqWindow = meta.RequestedNCtxOverride
			}
			// R83-политика (2026-10-01): клиент ЯВНО просит окно, которое
			// cppworker дать не может → это тот самый случай, о котором надо
			// уведомить понятным текстом (а не молча обслужить в меньшем окне).
			if reqWindow > 0 && achievablePerClient > 0 && reqWindow > achievablePerClient {
				logger.Get().Warnw(i18n.T("balancer.preflight_nctx.window_unreachable_reject_warn"),
					"backend_id", backendID,
					"model", gateModelName,
					"requested_per_client", reqWindow,
					"achievable_per_client", achievablePerClient,
					"reason", why)
				return makeWindowTooBigReject(state, meta, reqWindow, plan.NewNCtx,
					achievablePerClient*state.effectiveSlots(),
					i18n.T("balancer.preflight_nctx.achievable_window_reason",
						achievablePerClient, state.effectiveSlots())), nil
			}
			logger.Get().Warnw(i18n.T("balancer.preflight_nctx.growth_skipped_warn"),
				"backend_id", backendID,
				"model", gateModelName,
				"target_n_ctx", plan.NewNCtx,
				"current_n_ctx", state.CurrentNCtx,
				"context_per_seq", state.effectivePerSeqNCtx(),
				"slots", state.effectiveSlots(),
				"reason", why,
				"action", i18n.T("balancer.preflight_nctx.growth_skipped_action"))
			return &PreflightResult{Decision: PreflightNoOp, TargetNCtx: state.CurrentNCtx}, nil
		}

		// R493 (2026-11-17): ПОСЛЕДНЯЯ ПРОВЕРКА ПЕРЕД СТАРТОМ. Решатели уже
		// смотрели занятость, но между решением и фактическим unload+load
		// проходит время, и новый ход клиента успевает начаться. Живой факт
		// второго стенда: перезагрузки в 06:24:53 и 06:25:43 при идущих ходах.
		// Проверка стоит ПОСЛЕ гейтов «рост бесполезен» и «окно не уменьшаем»,
		// чтобы не откладывать то, что всё равно не стало бы делать.
		if allow, busyReason := c.reloadAllowed(backendID, gateModelName); !allow {
			logger.Get().Warnw("preflight: reload skipped — backend busy, not preempting a live turn (R493)",
				"backend_id", backendID,
				"model", gateModelName,
				"current_n_ctx", state.CurrentNCtx,
				"target_n_ctx", plan.NewNCtx,
				"reason", busyReason,
				"action", "no_op: перезагрузка оборвала бы чужой ответ; клиент повторит позже")
			return &PreflightResult{Decision: PreflightNoOp, TargetNCtx: state.CurrentNCtx}, nil
		}

		// Round 31 #2 (2026-08-09): async mode — не блокируем на reload.
		// Запускаем reload в goroutine и возвращаем PreflightAsyncReload.
		// Balancer отдаст клиенту 503 + Retry-After, а reload доделается в фоне.
		// Через Retry-After секунд клиент повторит, и модель уже будет готова.
		if cfg.PreflightAsyncReload {
			modelName := ""
			if meta != nil {
				modelName = meta.ModelName
			}

			// R60.6 (2026-09-07): рассчитываем estimatedRetryAfter на основе
			// (model_size + target_n_ctx) формулы, mirror cppworker's
			// estimateLoadTimeMs. Это даст клиенту адекватное время retry
			// (60-180s для 5GB+131072 n_ctx на RTX 3070 8GB) вместо hardcoded
			// 30s. Cppworker's actual estimatedLoadTimeMs появится только
			// после успешного load (200 body), а нам нужна оценка ДО запуска reload.
			//
			// Coordinator doesn't have direct access to Proxy's metrics manager.
			// Caller (preflight_helper.go) reads model size and passes via state.
			// For now we use 0 (size unknown) — caller can override EstimatedRetryAfter
			// after RunPreflight returns. Future: extend state to carry SizeBytes.
			var modelSizeBytes int64
			if state != nil && state.ModelSizeBytes > 0 {
				modelSizeBytes = state.ModelSizeBytes
			}
			estimatedMs := EstimateReloadTimeMs(modelSizeBytes, decision.TargetNCtx)
			// R549 (T-6 аудита): база → оценка → потолок в ОДНОЙ функции.
			//
			// До R549 здесь была ВТОРАЯ копия этой формулы со своим числом 120,
			// и она срезала оценку 186 с до 120: клиент возвращался, пока reload
			// ещё шёл, и запускал второй (cascading retry).
			estimatedRetryAfter := asyncRetryAfterFromEstimate(cfg, estimatedMs)

			logger.Get().Infow("preflight: triggering ASYNC reload (Round 31 #2, R60.6)",
				"backend_id", backendID,
				"current_n_ctx", state.CurrentNCtx,
				"target_n_ctx", decision.TargetNCtx,
				"max_vram_n_ctx", state.MaxVRAMNCtx,
				"model_max_context", state.ModelMaxContext,
				"model_size_bytes", modelSizeBytes,
				"estimated_load_time_ms", estimatedMs,
				"has_tools", meta != nil && meta.HasTools,
				"retry_after_sec", estimatedRetryAfter)

			// Async reload — НЕ ждём завершения. DoReload имеет внутренний
			// singleflight-like lock, так что параллельные reload'ы коалесцируются.
			//
			// R53.4 (2026-08-24) HOTFIX: defer recover() защищает balancer от
			// crash при любом nil pointer в goroutine body. Без этого — nil deref
			// (например, в c.state() или DoReload internal path) валит весь
			// balancer → in-flight Cline клиенты получают "Did not receive done".
			// Корень: cascading reload requests (Cline retries пока cppworker
			// busy) → multiple async goroutines + race conditions.
			//
			// R60.6 fix: регистрируем reload в reloadDedupRegistry ДО старта
			// goroutine, чтобы subsequent preflight calls видели IsReloadPending=true
			// и не триггерили ещё один reload (double-trigger cascade).
			if c.reloadDedup != nil {
				_, startedNew := c.reloadDedup.StartReloadIfNotPending(backendID, modelName, decision.TargetNCtx, func(entry *reloadEntry) {
					defer func() {
						if r := recover(); r != nil {
							logger.Get().Errorw("preflight: PANIC in async reload goroutine — RECOVERED",
								"backend_id", backendID, "panic", r)
						}
					}()
					reloadCtx, cancel := reloadTimeoutContext(context.Background(), cfg.effectiveTimeout())
					defer cancel()
					if err := c.DoReload(reloadCtx, backendID, backendAddr, modelName, plan, loader); err != nil {
						logger.Get().Errorw("preflight: async reload failed",
							"backend_id", backendID, "error", err)
						entry.err = err
						return
					}
					// Reload успешен — обновляем lastKnownNCtx.
					c.SetLastKnownNCtx(backendID, decision.TargetNCtx)
					logger.Get().Infow("preflight: async reload succeeded",
						"backend_id", backendID, "new_n_ctx", decision.TargetNCtx)
					c.ResetCycleCounter(backendID)
				})
				if !startedNew {
					// R60.6: reload уже в процессе для этой модели — возвращаем
					// PreflightAsyncReload БЕЗ новой goroutine (dedup hits).
					logger.Get().Infow("preflight: reload already pending, returning 503 (dedup)",
						"backend_id", backendID, "model", modelName,
						"target_n_ctx", decision.TargetNCtx)
				}
			} else {
				// fallback: no dedup registry, fire-and-forget как раньше
				go func() {
					defer func() {
						if r := recover(); r != nil {
							logger.Get().Errorw("preflight: PANIC in async reload goroutine — RECOVERED",
								"backend_id", backendID, "panic", r)
						}
					}()
					reloadCtx, cancel := reloadTimeoutContext(context.Background(), cfg.effectiveTimeout())
					defer cancel()
					if err := c.DoReload(reloadCtx, backendID, backendAddr, modelName, plan, loader); err != nil {
						logger.Get().Errorw("preflight: async reload failed",
							"backend_id", backendID, "error", err)
						return
					}
					c.SetLastKnownNCtx(backendID, decision.TargetNCtx)
					logger.Get().Infow("preflight: async reload succeeded",
						"backend_id", backendID, "new_n_ctx", decision.TargetNCtx)
					c.ResetCycleCounter(backendID)
				}()
			}

			// R68 (2026-09-23): дожидаемся reload'а и обслуживаем ПЕРВЫЙ же
			// запрос клиента (как R67a делает для авто-загрузки). Reload уже
			// зарегистрирован в dedup-реестре выше, поэтому WaitReloadDone
			// дождётся именно его (или уже завершённого — тогда nil).
			//
			// Раньше: клиенту сразу уходил 503+Retry-After (non-stream) или
			// stream-dialog с keepalive и финальным [DONE] БЕЗ ответа модели —
			// Cline показывал пустой ответ, и проходил только повторный запрос.
			// R83/v67 (2026-10-02): wait != 0 → ждём. Значение < 0 означает
			// «терминальное состояние» (переводим в 0 — так это понимает
			// WaitReloadDone: 0 = ждать до done, без таймера).
			if wait := nctxPreflightWaitTimeout(); wait != 0 {
				waitArg := wait
				if waitArg < 0 {
					waitArg = 0
				}
				waitStart := time.Now()
				if waitErr := c.WaitReloadDone(backendID, modelName, waitArg); waitErr == nil {
					c.SetLastKnownNCtx(backendID, decision.TargetNCtx)
					c.ResetCycleCounter(backendID)
					logger.Get().Infow("preflight: async reload finished while client waited, serving request",
						"backend_id", backendID, "model", modelName,
						"new_n_ctx", decision.TargetNCtx,
						"waited_ms", time.Since(waitStart).Milliseconds(),
						"wait_max_sec", int(wait.Seconds()))
					// Тот же контракт, что у sync reload: caller продолжает
					// проксирование запроса (round-trip один).
					return &PreflightResult{Decision: PreflightReload, TargetNCtx: decision.TargetNCtx}, nil
				} else {
					logger.Get().Warnw("preflight: wait for async reload did not succeed, returning 503",
						"backend_id", backendID, "model", modelName,
						"target_n_ctx", decision.TargetNCtx,
						"waited_ms", time.Since(waitStart).Milliseconds(),
						"error", waitErr)
				}
			}

			return &PreflightResult{
				Decision:            PreflightAsyncReload,
				TargetNCtx:          decision.TargetNCtx,
				EstimatedRetryAfter: estimatedRetryAfter,
			}, nil
		}

		// Sync mode (default, обратно совместимо со старым поведением).
		logger.Get().Infow("preflight: triggering reload",
			"backend_id", backendID,
			"current_n_ctx", state.CurrentNCtx,
			"target_n_ctx", decision.TargetNCtx,
			"max_vram_n_ctx", state.MaxVRAMNCtx,
			"model_max_context", state.ModelMaxContext,
			"has_tools", meta != nil && meta.HasTools)

		// R53.4 (2026-08-24) HOTFIX: sync reload path also can panic from
		// nil deref in c.state() or DoReload internal path. Defer recover
		// returns reject result instead of crashing the whole balancer.
		defer func() {
			if r := recover(); r != nil {
				logger.Get().Errorw("preflight: PANIC in sync reload — RECOVERED, returning reject",
					"backend_id", backendID, "panic", r)
			}
		}()

		reloadCtx, cancel := reloadTimeoutContext(ctx, cfg.effectiveTimeout())
		defer cancel()
		modelName := ""
		if meta != nil {
			modelName = meta.ModelName
		}
		if err := c.DoReload(reloadCtx, backendID, backendAddr, modelName, plan, loader); err != nil {
			logger.Get().Errorw("preflight: reload failed",
				"backend_id", backendID, "error", err)
			// R337 (2026-10-30, найдено стаб-матрицей): ПРОВАЛ ПЕРЕЗАГРУЗКИ —
			// ЭТО 503, А НЕ 413.
			//
			// 413 «prompt + n_predict exceeds n_ctx» обвиняет КЛИЕНТА: «твой запрос
			// не влезает». Но если перезагрузка упала технически (воркер не ответил,
			// ручка недоступна, таймаут), запрос клиента тут ни при чём — он может
			// влезать сколько угодно. Клиент должен ПОВТОРИТЬ (503 + Retry-After), а
			// не уменьшать окно и не искать «слишком большую историю».
			//
			// Живой факт из тестов контракта: при недоступной ручке reload клиент
			// получал 413 с reason "preflight reload failed: reload returned HTTP 404".
			body := map[string]interface{}{
				"error":             "preflight reload failed: the balancer could not grow the context window",
				"reason":            fmt.Sprintf("preflight reload failed: %v", err),
				"decision":          "reload_failed",
				"backend_id":        state.BackendID,
				"required_n_ctx":    decision.TargetNCtx,
				"current_n_ctx":     state.CurrentNCtx,
				"model_max_context": state.ModelMaxContext,
				"retryable":         true,
			}
			encoded, _ := json.Marshal(body)
			return &PreflightResult{
				Decision:            PreflightReject,
				TargetNCtx:          decision.TargetNCtx,
				RejectStatus:        http.StatusServiceUnavailable,
				RejectBody:          string(encoded),
				EstimatedRetryAfter: 30,
			}, nil
		}
		// Reload успешен — обновляем lastKnownNCtx.
		c.SetLastKnownNCtx(backendID, decision.TargetNCtx)
		logger.Get().Infow("preflight: reload succeeded",
			"backend_id", backendID, "new_n_ctx", decision.TargetNCtx)
		// Reset cycle counter (если был)
		c.ResetCycleCounter(backendID)
		return &PreflightResult{Decision: PreflightReload, TargetNCtx: decision.TargetNCtx}, nil
	}
	return &PreflightResult{Decision: PreflightNoOp}, nil
}

// RoundUpPow2 — публичный алиас для roundUpPow2 (нужен в других пакетах).
// Вынесен из nctx_reload.go:roundUpPow2 для использования в preflight.
func RoundUpPow2(v int) int {
	return roundUpPow2(v)
}

// R60.6 (2026-09-07): EstimateReloadTimeMs — оценочное время reload в мс.
// Mirrors cppworker cmd/cppworker/handlers_model_async.go:estimateLoadTimeMs
// (тот же formula, тот же fallback). Используется как fallback когда
// cppworker 200 body с estimatedLoadTimeMs недоступен (e.g. модель ещё
// не загружена и metricsMgr не имеет size info).
//
// Формула: max(1000, size_MB / 100 + ctx/4K*500 + 2000) мс
// - base 1000ms minimum (модель не может грузиться мгновенно)
// - size/speed 100MB/s fallback для cold load
// - ctxInit 500ms per 4K tokens (KV-cache init, q4_0/q8_0 typical)
// - overhead 2000ms (mmap, metadata parse, locks)
func EstimateReloadTimeMs(modelSizeBytes int64, targetNCtx int) int64 {
	const baseSpeed = 100 * 1024 * 1024 // 100 MB/s fallback
	const baseMinMs = 1000
	const overheadMs = 2000
	const ctxInitMsPer4K = 500
	if modelSizeBytes <= 0 && targetNCtx <= 0 {
		return 0 // unknown, caller должен fall back на cfg default
	}
	var baseMs int64
	if modelSizeBytes > 0 {
		baseMs = modelSizeBytes * 1000 / baseSpeed
		if baseMs < baseMinMs {
			baseMs = baseMinMs
		}
	} else {
		baseMs = baseMinMs
	}
	var ctxMs int64
	if targetNCtx > 0 {
		blocksOf4K := int64((targetNCtx + 4095) / 4096)
		ctxMs = blocksOf4K * ctxInitMsPer4K
	}
	return baseMs + ctxMs + overheadMs
}
