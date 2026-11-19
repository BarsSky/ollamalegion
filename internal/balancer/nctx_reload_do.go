// nctx_reload_do.go — R634 (2026-11-19): исполнение перезагрузки: DoReload (один длинный сценарий: POST, ожидание, вердикт).
//
// ЗАЧЕМ ВЫНЕСЕНО. `nctx_reload.go` держал 1865 строк: типы и конфигурацию,
// состояние координатора, решения о перезагрузке, HTTP-клиент, длинный DoReload,
// метрики и опрос прогресса. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"strings"
	"time"
)

func (c *NCtxReloadCoordinator) DoReload(
	ctx context.Context,
	backendID string,
	backendAddr string,
	modelName string,
	plan *ReloadPlan,
	loader NCtxReloadHTTPClient,
) error {
	if plan.Decision != DecisionReload {
		return fmt.Errorf("DoReload called with non-reload plan: %s", plan.Decision)
	}
	cfg := c.Config()
	timeout := cfg.effectiveTimeout()
	// R83/v67 (2026-10-02): reloadTimeoutContext, а НЕ context.WithTimeout.
	// effectiveTimeout() == 0 означает «без капа» (штатный режим), а
	// context.WithTimeout(ctx, 0) — это НЕМЕДЛЕННО истёкший контекст: DoReload
	// падал бы с "context deadline exceeded" ещё до обращения к бэкенду.
	rctx, cancel := reloadTimeoutContext(ctx, timeout)
	defer cancel()

	state := c.state(backendID)
	state.inflightMu.Lock()
	if state.inflight != nil {

		waitCh := state.inflight.done
		state.inflightMu.Unlock()
		logger.Get().Debugf("[nctx_reload] backend %s: another reload in flight, waiting", backendID)
		select {
		case <-waitCh:
			return state.inflight.err
		case <-rctx.Done():
			return fmt.Errorf("timeout waiting for in-flight reload: %w", rctx.Err())
		}
	}

	state.inflight = &reloadInflight{done: make(chan struct{})}
	cur := state.inflight
	state.inflightMu.Unlock()

	defer func() {
		state.inflightMu.Lock()
		state.inflight = nil
		state.inflightMu.Unlock()
		close(cur.done)
	}()

	logger.Get().Infof("[nctx_reload] backend %s: reloading with n_ctx=%d (model=%s, reason=%s)",
		backendID, plan.NewNCtx, modelName, plan.Reason)

	// Query adaptive strategy from cppworker (kvCacheType, gpuLayers, n_ctx).
	// R70: передаём известный рабочий тип KV-cache (профиль/загруженная модель),
	// иначе стратегия считает по f16 и может выбрать cpu_only там, где с q4_0
	// модель влезает на GPU.
	hints := c.ReloadHintsFor(backendID, modelName)
	if hints.KVCacheType != "" {
		logger.Get().Infow("nctx_reload: passing kv_cache_type hint to adaptive strategy",
			"backend", backendID, "model", modelName, "kv_cache_type", hints.KVCacheType)
	}
	strategy := queryAdaptiveStrategy(backendAddr, modelName, plan.NewNCtx, hints.KVCacheType, nil)

	// R83-fix (2026-09-30): reload НЕ имеет права понижать GPU-оффлоад.
	//
	// Адаптивная стратегия считает бюджет по СВОБОДНОЙ VRAM. Для перезагрузки
	// УЖЕ загруженной модели это неверный бюджет: свободная память не включает
	// ту, которую занимает сама перезагружаемая модель. На живом стенде
	// (gemma-4, RTX 3070 8 GB, q4_0) это давало stage=cpu_only с gpuLayers=0 для
	// модели, которая прямо сейчас работает с 19 слоями на GPU, — и reload
	// выкидывал модель из VRAM. Здесь оставляем не меньше слоёв, чем работает
	// сейчас: хуже текущей раскладки перезагрузка быть не должна.
	if clampStrategyToCurrentLayout(strategy, hints.GPULayers) {
		logger.Get().Warnw(i18n.T("balancer.nctx_reload.strategy_lowers_gpu_offload_warn"),
			"backend", backendID,
			"model", modelName,
			"strategy_gpu_layers", strategy.GPULayers,
			"current_gpu_layers", hints.GPULayers,
			"stage", strategy.Stage,
			"reason", i18n.T("balancer.nctx_reload.strategy_lowers_gpu_offload_reason"))
	}

	// Cap target n_ctx to MaxViableNCtx from adaptive strategy.
	// This prevents auto-reload from escalating to unusable context sizes
	// on small GPUs (e.g. 131K on 8GB where even q4_0 KV-cache doesn't fit).
	targetNCtx := plan.NewNCtx
	if strategy != nil && strategy.MaxViableNCtx > 0 && targetNCtx > strategy.MaxViableNCtx {
		targetNCtx = strategy.MaxViableNCtx
		logger.Get().Infow("nctx_reload: capping target n_ctx to MaxViableNCtx from adaptive strategy",
			"model", modelName,
			"original_target", plan.NewNCtx,
			"capped_target", targetNCtx,
			"max_viable", strategy.MaxViableNCtx,
			"reason", "VRAM insufficient for requested n_ctx even with best kvCacheType")
	}

	// R83-политика (2026-10-01): ЧЕСТНАЯ цель и защита от «пустого» reload.
	//
	// Живой случай (лог 2026-10-01 04:02): plan.NewNCtx=131072, адаптивная
	// стратегия вернула nCtx=65536, enrichReloadPayload подменил contextSize на
	// 65536 — то есть модель перезагрузилась с ТЕМ ЖЕ окном, что уже было.
	// Балансер при этом отчитался «reload successful, new n_ctx=131072»
	// (логировал цель, а не факт), модель была выгружена на 121 с, и клиент не
	// получил ничего. Здесь считаем ЭФФЕКТИВНУЮ цель (то, что реально уйдёт в
	// cppworker) и отказываемся от перезагрузки, если она не больше текущего окна.
	effectiveTarget := targetNCtx
	if strategy != nil && strategy.NCtx > 0 {
		effectiveTarget = strategy.NCtx
	}
	if hints.WindowNCtx > 0 && effectiveTarget <= hints.WindowNCtx {
		cur.err = fmt.Errorf(
			"reload skipped: effective target n_ctx=%d does not exceed currently loaded n_ctx=%d "+
				"(adaptive strategy clamped the target); window would not change",
			effectiveTarget, hints.WindowNCtx)
		logger.Get().Warnw(i18n.T("balancer.nctx_reload.reload_skipped_window_unchanged_warn"),
			"backend", backendID,
			"model", modelName,
			"plan_target_n_ctx", plan.NewNCtx,
			"strategy_n_ctx", func() int {
				if strategy != nil {
					return strategy.NCtx
				}
				return 0
			}(),
			"effective_target_n_ctx", effectiveTarget,
			"loaded_n_ctx", hints.WindowNCtx,
			"reason", i18n.T("balancer.nctx_reload.reload_skipped_reason"))
		return cur.err
	}

	payloadMap := map[string]interface{}{
		"name":        modelName,
		"contextSize": targetNCtx,
		"force":       true,
		"gpuLayers":   -2,
		// Round 34 (2026-08-12) Phase 4: default flashAttn=-1 (auto) для optimal
		// performance. User's expectation: "балансер всегда использует настройки
		// заточенные под оптимальную скорость и производительность - это mmap,
		// flash_attention и настраивает gpu_layers -2". AdaptiveStrategy может
		// переопределить (см. enrichReloadPayload).
		"flashAttn": -1,
		"useMmap":   true,
		// Round 51.4 (2026-08-20): REMOVED `reason` field. cppworker's
		// reloadModelRequest struct (cmd/cppworker/types.go:134) does NOT have
		// a "reason" field. Go's json.Decoder rejects unknown fields with HTTP
		// 400 "invalid JSON: unknown field 'reason'" → reload always failed.
		// The reason is for balancer logs only — moved to the Infow below.
	}
	// R119 (2026-10-15): переносим в тело reload намерения оператора из профиля
	// (kvCacheType / parallel / enableReasoning) ДО адаптивной стратегии, чтобы
	// стратегия могла их переопределить, а не наоборот.
	applyReloadHintsToPayload(payloadMap, hints)
	if strategy != nil {
		payloadMap = enrichReloadPayload(payloadMap, strategy)
		logger.Get().Infow("nctx_reload: using adaptive strategy",
			"model", modelName,
			"stage", strategy.Stage,
			"kvCacheType", strategy.KVCacheType,
			"gpuLayers", strategy.GPULayers)
	}
	payload, _ := json.Marshal(payloadMap)
	// Round 33: добавлен ?wait=true&waitTimeoutSec=300 — cppworker БЛОКИРУЕТ
	// ответ до завершения reload (max 5 мин). Без этого cppworker возвращает
	// HTTP 202 (async load), а балансер не умел его обрабатывать и валил
	// preflight с HTTP 413. Теперь:
	//  - cppworker блокирует до 5 мин
	//  - если 5 мин не хватило (маловероятно для 5GB) — получаем 202,
	//    парсим progressUrl и поллим до state=loaded
	endpoint := backendAddr + "/api/models/reload?wait=true&waitTimeoutSec=300"
	resp, err := loader.PostReload(rctx, endpoint, payload)
	if err != nil {
		cur.err = fmt.Errorf("reload HTTP request failed: %w", err)
		logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
		return cur.err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	// Round 33: HTTP 202 = async load. cppworker не успел в waitTimeoutSec.
	// Парсим progressUrl и поллим пока не state=loaded / state=error.
	// На текущем железе (5GB модель на RTX 3070 8GB) это маловероятно
	// (reload занимает 60-90 сек при waitTimeoutSec=300), но защищаемся.
	if resp.StatusCode == http.StatusAccepted {
		progressURL, perr := extractProgressURL(body, backendAddr)
		if perr != nil {
			cur.err = fmt.Errorf("reload returned 202 but progressUrl parse failed: %w (body: %s)", perr, string(body))
			logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
			return cur.err
		}
		logger.Get().Infow("[nctx_reload] backend: 202 received, polling progressUrl",
			"backend_id", backendID, "progress_url", progressURL)
		// Poll до state=loaded или state=error. maxWait = оставшееся
		// время от rctx + 60s buffer (но не больше 10 мин).
		deadline, hasDeadline := rctx.Deadline()
		maxWait := 10 * time.Minute
		if hasDeadline {
			remaining := time.Until(deadline) + 60*time.Second
			if remaining < maxWait {
				maxWait = remaining
			}
		}
		pollBody, pollErr := pollLoadProgress(rctx, loader, backendAddr, progressURL, maxWait, backendID)
		if pollErr != nil {
			cur.err = pollErr
			logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
			return cur.err
		}
		// pollBody содержит синтетический ответ (model state в формате 200).
		// Подменяем body и status — дальнейший код ожидает StatusOK + JSON.
		body = pollBody
		resp.StatusCode = http.StatusOK
	}

	// Round 35 (2026-08-12) bugfix: fallback to /api/models/load if reload returns
	// 404 "model not currently loaded". This happens when cppworker was just
	// restarted but balancer's metrics cache still says the model is loaded
	// (stale data from previous cppworker instance). Without this fallback,
	// preflight triggers reload → 404 → client gets 503+Retry-After loop until
	// its 120s timeout → ECONNREFUSED.
	if resp.StatusCode == http.StatusNotFound && strings.Contains(string(body), "not currently loaded") {
		loadEndpoint := backendAddr + "/api/models/load?wait=true&waitTimeoutSec=300"
		logger.Get().Warnw("[nctx_reload] backend: reload returned 404 (model not currently loaded), falling back to /api/models/load",
			"backend_id", backendID, "model", modelName, "target_n_ctx", plan.NewNCtx)
		// Reuse the same payload, but /api/models/load doesn't support
		// "force" field — it handles unload-then-load internally.
		//
		// R53.4 (2026-08-24) HOTFIX: REMOVED "reason" field. R51.4 (2026-08-20)
		// removed it from the primary reload payload (cppworker rejects unknown
		// fields with HTTP 400), but the fallback below kept it. Without this
		// fix, fallback path always returns "invalid JSON: unknown field 'reason'"
		// → reload effectively never succeeds via fallback → cascading retries
		// → "Did not receive done" errors in Cline clients.
		//
		// R119 (2026-10-15): тело больше НЕ собирается заново — копируем
		// payloadMap (см. loadBodyFromReloadPayload). Раньше здесь перечислялись
		// четыре ключа, и всё, что добавили hint'ы/стратегия (parallel,
		// enableReasoning, overrideTensors), на fallback-пути терялось.
		loadBody := loadBodyFromReloadPayload(payloadMap)
		loadBody["contextSize"] = plan.NewNCtx
		loadPayload, _ := json.Marshal(loadBody)
		resp.Body.Close()
		resp, err = loader.PostReload(rctx, loadEndpoint, loadPayload)
		if err != nil {
			cur.err = fmt.Errorf("load fallback HTTP request failed: %w", err)
			logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
			return cur.err
		}
		body, _ = io.ReadAll(resp.Body)
		// Same 202 → poll handling as above
		if resp.StatusCode == http.StatusAccepted {
			progressURL, perr := extractProgressURL(body, backendAddr)
			if perr != nil {
				cur.err = fmt.Errorf("load fallback returned 202 but progressUrl parse failed: %w (body: %s)", perr, string(body))
				logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
				return cur.err
			}
			deadline, hasDeadline := rctx.Deadline()
			maxWait := 10 * time.Minute
			if hasDeadline {
				remaining := time.Until(deadline) + 60*time.Second
				if remaining < maxWait {
					maxWait = remaining
				}
			}
			pollBody, pollErr := pollLoadProgress(rctx, loader, backendAddr, progressURL, maxWait, backendID)
			if pollErr != nil {
				cur.err = pollErr
				logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
				return cur.err
			}
			body = pollBody
			resp.StatusCode = http.StatusOK
		}
	}

	const maxRetries = 5
	retryInterval := 5 * time.Second
	for retry := 0; retry < maxRetries && resp.StatusCode == http.StatusServiceUnavailable &&
		strings.Contains(string(body), "model is loading"); retry++ {
		logger.Get().Infof("[nctx_reload] backend %s: model still loading, retrying in %v (attempt %d/%d)",
			backendID, retryInterval, retry+1, maxRetries)
		resp.Body.Close()
		select {
		case <-time.After(retryInterval):
		case <-rctx.Done():
			cur.err = fmt.Errorf("reload context cancelled while waiting for model load: %w", rctx.Err())
			return cur.err
		}
		resp, err = loader.PostReload(rctx, endpoint, payload)
		if err != nil {
			cur.err = fmt.Errorf("reload HTTP request failed (retry %d): %w", retry+1, err)
			logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
			return cur.err
		}
		body, _ = io.ReadAll(resp.Body)
	}

	if resp.StatusCode != http.StatusOK {
		cur.err = fmt.Errorf("reload returned HTTP %d: %s", resp.StatusCode, string(body))
		logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
		return cur.err
	}

	var result struct {
		ContextSize int    `json:"context_size"`
		NCtx        int    `json:"n_ctx"`
		Status      string `json:"status"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		cur.err = fmt.Errorf("reload response parse failed: %w (raw: %s)", err, string(body))
		logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
		return cur.err
	}
	if result.Error != "" {
		cur.err = fmt.Errorf("reload backend error: %s", result.Error)
		logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
		return cur.err
	}
	actual := result.ContextSize
	if actual == 0 {
		actual = result.NCtx
	}
	if actual == 0 {
		actual = plan.NewNCtx
	}
	// R505 / аудит N-6: LastKnownNCtx — окно НА СЛОТ (так его читают preflight и
	// резолвер), а ответ reload'а (ContextSize) — СУММАРНЫЙ. Раньше сюда писали
	// суммарное: при parallel=2 preflight видел вдвое большее окно, чем есть у
	// слота, и «промпт влезает» оказывалось ложью.
	perSeq := perSeqFromTotal(actual, normalizedSlotsR504(hints.Parallel))
	c.SetLastKnownNCtx(backendID, perSeq)
	cur.err = nil
	logger.Get().Infof("[nctx_reload] backend %s: reload successful, n_ctx=%d суммарно, %d на слот, слотов %d",
		backendID, actual, perSeq, normalizedSlotsR504(hints.Parallel))

	// R83-fix (2026-09-30): честно сообщаем, если окно НЕ доросло до запрошенного.
	//
	// Живой случай (стенд, gemma-4 8 ГБ): preflight просил 131072 (окно клиента
	// 65536 × 2 слота), адаптивная стратегия вернула stage=cpu_only с nCtx=65536,
	// цель была срезана до 65536 — а в логе печаталось «reload successful,
	// new n_ctx=131072» (бралось из плана). Оператор видел успех и не понимал,
	// почему клиент по-прежнему получает половину окна.
	if actual < plan.NewNCtx {
		logger.Get().Warnw(i18n.T("balancer.nctx_reload.window_grew_less_than_requested_warn"),
			"backend", backendID, "model", modelName,
			"requested_n_ctx", plan.NewNCtx,
			"achieved_n_ctx", actual,
			"shortfall", plan.NewNCtx-actual,
			"reason", i18n.T("balancer.nctx_reload.window_grew_less_reason"),
			"hint", i18n.T("balancer.nctx_reload.window_grew_less_hint"))
	}
	return nil
}

// ============================================================

// ============================================================
