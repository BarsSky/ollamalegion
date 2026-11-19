// autotune_reload.go — R636 (2026-11-19): исполнение перезагрузки: triggerAutoTuneReload и executeAutoTuneReload.
//
// ЗАЧЕМ ВЫНЕСЕНО. `autotune.go` держал 1666 строк: типы профиля, правила
// выбора KV-типа и числа слоёв, отчёт, предохранитель, трекер, две перезагрузки
// и планирование применения. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"fmt"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"time"
)

// triggerAutoTuneReload — Round 54.4 (2026-08-24): main entry point для autonomous reload.
//
// Вызывается из proxyRequestLlamaCpp ПОСЛЕ preflightNCtxReloadIfNeeded.
// Логика:
//  1. Check global + per-model AutoTune switch (IsAutoTuneEnabled).
//  2. Get loaded model from metrics.
//  3. Build ModelProfileInfo + run analyzeLoadedModel.
//  4. If sub-optimal → PlanApplyAutoTune → check circuit → async reload.
//
// Возвращает AutoTuneReloadResult. Не блокирует (reload асинхронный).
func (p *Proxy) triggerAutoTuneReload(backendID, modelName string, freeVRAM, freeRAM, totalVRAM uint64) *AutoTuneReloadResult {
	res := &AutoTuneReloadResult{}

	// 1) AutoTune switch
	if !IsAutoTuneEnabled(p, modelName) {
		res.SkippedReason = "AutoTune disabled (global or per-model)"
		return res
	}

	// 1b) R283: пауза после «занято».
	//
	// Проверяем ДО анализа и ДО circuit breaker: пока модель занята, повторный
	// анализ всё равно вернёт тот же план, а попытка apply снова упрётся в
	// «busy» — именно так на стенде r282 получилась петля «каждые ~70 секунд
	// reload failed». Пауза растёт (2→5→10→30→60 мин) и сбрасывается успехом.
	if p.autoTuneTracker != nil {
		if wait, attempts, deferred := p.autoTuneTracker.AutoTuneDeferredUntil(backendID, modelName); deferred {
			res.SkippedReason = fmt.Sprintf(
				"autotune deferred (busy): retry in %s (attempt %d)", wait.Round(time.Second), attempts)
			return res
		}
	}

	// 2) Get loaded model from metrics
	if p.metricsMgr == nil {
		res.SkippedReason = "no metrics manager"
		return res
	}
	// R245: снапшот под RLock — живой указатель поллер переписывает под Lock.
	lm, ok := p.metricsMgr.SnapshotLlamaCppMetrics(backendID)
	if !ok || lm == nil {
		res.SkippedReason = "no llamaCpp metrics for backend"
		return res
	}
	var loadedModel types.LlamaCppModel
	for _, m := range lm.LoadedModels {
		// R66d: нормализация .gguf/пути/регистра (cppworker отдаёт имя без расширения).
		if modelNameMatches(m.Name, modelName) && m.State == "loaded" {
			loadedModel = m
			break
		}
	}
	if loadedModel.Name == "" {
		res.SkippedReason = "model not in loaded state"
		return res
	}

	// 3) Build profile + analysis
	// R230: reload выгружает текущую копию модели → бюджет = free + usage этой
	// модели (VRAMUsage/RAMUsage в МБ), иначе анализ видит только остаток.
	prof := &ModelProfileInfo{
		Name:                 loadedModel.Name,
		Architecture:         loadedModel.Architecture,
		NLayers:              loadedModel.NLayers,
		NEmbd:                loadedModel.NEmbd,
		NKvHeads:             loadedModel.NKvHeads,
		HeadDimK:             loadedModel.HeadDimK,
		SizeBytes:            loadedModel.Size,
		GgufMaxContext:       loadedModel.MaxContext,
		CurrentContextLength: loadedModel.ContextLength,
		CurrentKVCacheType:   loadedModel.KvCacheType,
		CurrentNumGPULayers:  loadedModel.NumGPULayers,
		FreeVRAMBytes:        reloadBudgetBytes(freeVRAM, totalVRAM, loadedModel.VRAMUsage),
		FreeRAMBytes:         reloadBudgetBytes(freeRAM, 0, loadedModel.RAMUsage),
		TotalVRAMBytes:       totalVRAM,
		FeasibleMaxContext:   loadedModel.FeasibleMaxContext,
		AutoTuneEnabled:      true,
	}
	// R54.9: workload-aware analysis — если есть WorkloadTracker, используем
	// реальную статистику num_ctx чтобы выбрать KV cache type оптимально.
	var analysis *AutoTuneAnalysis
	if p.workloadTracker != nil {
		workload := p.workloadTracker.Stats(backendID, modelName)
		cfg := DefaultWorkloadTrackerConfig()
		analysis = analyzeLoadedModelWithWorkload(prof, workload, cfg.MinSamplesForRecommendation)
	}
	if analysis == nil {
		analysis = analyzeLoadedModel(prof) // fallback
	}
	if analysis == nil || !analysis.IsSubOptimal {
		res.SkippedReason = "model is optimal"
		return res
	}

	// 4) Plan
	// R69: не «оптимизируем» n_ctx ниже того, что просил клиент — иначе
	// ping-pong reload (65536 → 38385 → 65536 …) и клиент получает 503.
	minContext := 0
	if p.nctxReload != nil {
		minContext = p.nctxReload.DesiredNCtx(backendID, modelName)
	}
	// R425 (2026-11-03): окно, ЗАЯВЛЕННОЕ ОПЕРАТОРОМ в профиле модели, — это
	// политика, а не «over-allocation». AutoTune не имеет права опускать n_ctx
	// ниже profile.contextLength.
	//
	// ЖИВОЙ ДЕФЕКТ (стенд, балансер r261103-2245). Сразу после первого запроса
	// без num_ctx AutoTune выгрузил модель, чтобы поставить «оптимум»
	// 32768 → 20654 («over-allocation fix»), хотя в профиле заявлено окно 32768
	// и потолок 65536. Клиент, пришедший с окном 32768, получил бы 20654, а
	// сессия агента — обрыв по контексту; профиль оператора при этом никто не
	// менял. Потолок профиля (contextLengthMax) уже проверяется в гардах R416b/c/d,
	// а НИЖНЯЯ граница (contextLength) — нет.
	minContext = autoTuneMinContextR425(minContext, p.GetModelProfileNumCtx(modelName))
	plan := PlanApplyAutoTuneWithMinContext(analysis, loadedModel, minContext)
	if plan == nil {
		if minContext > 0 {
			res.SkippedReason = fmt.Sprintf(
				"client requested n_ctx=%d (within %s): over-allocation downgrade skipped",
				minContext, desiredNCtxTTL)
		} else {
			res.SkippedReason = "no actionable recommendations (sub-optimal detected but no plan)"
		}
		return res
	}
	res.Plan = plan

	// R416 (2026-11-03): KV-cache не переключается туда-обратно и не спорит с
	// профилем модели. Без этого q8_0 → q4_0 → q8_0 … перезагружали модель на
	// каждом запросе (~80 c unload+load), и клиент видел «медленную модель».
	// См. autotune_kv_guard.go.
	recommendedKV := plan.KVCacheType
	if dropped := guardAutoTuneKVPlanR416(p, backendID, modelName, plan); dropped != "" {
		logger.Get().Warnw("autotune: kv-cache change suppressed (R416)",
			"backend", backendID, "model", modelName,
			"recommended_kv", recommendedKV, "reason", dropped)
	}
	if !plan.NeedsReload() {
		res.SkippedReason = "R416: only the kv-cache change was recommended, and it is pinned by profile or on flip cooldown"
		return res
	}
	// R416d (2026-11-03): после гардов R416/R416b в плане мог остаться только
	// «сохранённый» контекст (PlanApplyAutoTune кладёт в ContextSize фактически
	// загруженное окно, чтобы reload не сбросил его env-дефолтом). NeedsReload
	// считает это изменением, и балансер выгружает/загружает модель ПОСЛЕ КАЖДОГО
	// ответа. Живой случай: Qwen3.8-9B — 33/32 слоя, ctx 16384, kv q8_0; KV и
	// слои гарды отклонили, остался ContextSize=16384 (== текущему), и reload
	// шёл по кругу.
	if !autoTunePlanHasRealChangeR416d(plan, loadedModel.ContextLength) {
		res.SkippedReason = "R416d: plan has no real change after guards (context is preserved, not changed)"
		return res
	} // R444 (2026-11-05): по умолчанию AutoTune ТОЛЬКО рекомендует.
	//
	// Живой дефект: агентский цикл Cline платил 43–67 с за unload+load перед
	// каждым ходом, потому что план строился по сиюминутной свободной VRAM
	// («освободилось → поднять слои»). Рекомендация при этом остаётся: её видно в
	// панели и можно применить вручную. См. autotune_apply_mode.go.
	if !autoTuneApplyAutomaticR444() {
		res.SkippedReason = "R444: AutoTune is recommend-only (LB_AUTOTUNE_APPLY=recommend); " +
			"apply the recommendation manually (model card / POST /api/v1/admin/autotune/{id}/apply)"
		logger.Get().Infow("autotune: plan built but not applied (recommend-only, R444)",
			"backend", backendID, "model", modelName,
			"reason", plan.Reason,
			"context_size", plan.ContextSize,
			"kv_cache_type", plan.KVCacheType,
			"gpu_layers", plan.NumGPULayers)
		return res
	}

	// 5) Circuit breaker
	if p.autoTuneTracker == nil {
		p.autoTuneTracker = NewAutoTuneTracker(DefaultAutoTuneCircuitConfig())
	}
	circuit := p.autoTuneTracker.GetCircuit(backendID, modelName)
	res.CircuitState = p.autoTuneTracker.GetCircuitSnapshot(backendID, modelName)
	canReload, reason := circuit.CanReload(p.autoTuneTracker.GetConfig())
	if !canReload {
		res.SkippedReason = reason
		// R54.8 (2026-08-24): publish circuit_cool_down event — оператор видит
		// в WebUI что AutoTune временно приостановлен после серии неудач.
		// Это НЕ новый circuit-open логика (cool-down уже работает), а visibility
		// в UI, чтобы не приходилось лезть в логи чтобы понять почему
		// AutoTune не срабатывает.
		// R55.2: используем publishAutoTuneEvent helper — пишет в EventBus + history log.
		p.publishAutoTuneEvent(types.Event{
			Type:      types.EventAutoTuneCircuitOpen,
			Timestamp: time.Now(),
			BackendID: backendID,
			Model:     modelName,
			Severity:  types.SeverityWarning,
			Source:    "autotune",
			Message:   "AutoTune circuit cooling down: " + reason,
			Data: map[string]interface{}{
				"reason":      reason,
				"lastError":   circuit.LastError,
				"lastAttempt": circuit.LastAttempt,
			},
		})
		return res
	}

	// 6) Trigger async reload via existing nctx_reload infrastructure
	circuit.RecordAttempt()

	// R230: берём маркер «reload идёт» СИНХРОННО, до запуска горутины и до
	// форварда текущего запроса. Иначе запрос успевал уйти в воркер, тот видел
	// выгруженную модель и делал ленивую загрузку по профилю (q4_0/40), а наш
	// load с планом (f16/-1) потом только дожидался чужой загрузки и рапортовал
	// success при неизменной конфигурации. Неудача (reload уже идёт) — план не
	// запускаем вовсе.
	if p.nctxReload != nil {
		finish, started := p.nctxReload.BeginExternalReload(backendID, modelName)
		if !started {
			res.SkippedReason = "reload already in progress for model"
			return res
		}
		plan.externalFinish = finish
	}

	logger.Get().Infow("autotune: triggering async reload",
		"backend", backendID, "model", modelName,
		"reason", plan.Reason,
		"new_context_size", plan.ContextSize,
		"new_kv_cache_type", plan.KVCacheType,
		"new_num_gpu_layers", plan.NumGPULayers,
	)

	// Запускаем async reload в goroutine
	go func() {
		// R54.8 (2026-08-24): publish autotune_reload_triggered event
		// R55.2: publishAutoTuneEvent пишет в EventBus + history log.
		p.publishAutoTuneEvent(types.Event{
			Type:      types.EventAutoTuneReloadTriggered,
			Timestamp: time.Now(),
			BackendID: backendID,
			Model:     modelName,
			Severity:  types.SeverityInfo,
			Source:    "autotune",
			Message:   "AutoTune async reload triggered: " + plan.Reason,
			Data: map[string]interface{}{
				"contextSize":  plan.ContextSize,
				"kvCacheType":  plan.KVCacheType,
				"numGpuLayers": plan.NumGPULayers,
				"reason":       plan.Reason,
			},
		})

		// R83: проверка «reload уже идёт» перенесена ВЫШЕ, в синхронную часть:
		// маркер (BeginExternalReload) берётся до запуска горутины, иначе проверка
		// видела бы собственный маркер и план не запускался бы никогда (R230).

		err := p.executeAutoTuneReload(backendID, modelName, plan, circuit)
		// R283: три исхода вместо двух — applied / deferred (занято) / failed.
		// Решения (breaker, событие, backoff, лог) принимает один обработчик:
		// раньше «воркер занят» попадал в ветку «failed» и писал ERROR с
		// растущим cool-down'ом, хотя перезагрузку просто нельзя было делать
		// сейчас. См. autotune_defer.go.
		p.finalizeAutoTuneReload(backendID, modelName, plan, circuit, err)
	}()

	res.Triggered = true
	res.CircuitState = p.autoTuneTracker.GetCircuitSnapshot(backendID, modelName)
	return res
}

// executeAutoTuneReload — Round 54.4 (2026-08-24): actual reload execution.
// Использует существующую инфраструктуру cppworker (executeAsyncReload).
func (p *Proxy) executeAutoTuneReload(backendID, modelName string, plan *AutoTuneReloadPlan, circuit *AutoTuneCircuit) error {
	if plan == nil {
		return nil
	}
	// Получаем текущий backend
	backends := p.GetAllBackends()
	var backend types.Backend
	var found bool
	for _, b := range backends {
		if b.ID == backendID {
			backend = b
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("backend %s not found", backendID)
	}
	if backend.CppWorkerPort == 0 {
		return fmt.Errorf("backend %s has no CppWorkerPort", backendID)
	}

	// R66c (2026-09-22): применяем план РЕАЛЬНО реализованным путём —
	// ApplyAutoTunePlan (unload + load с новыми параметрами), тем же, что
	// использует ручной endpoint /api/v1/admin/autotune/{id}/apply.
	//
	// Было: собирался env-map и вызывался applyCppWorkerEnvReload, который
	// ВСЕГДА возвращал ошибку «env-based reload not yet implemented; apply plan
	// manually». Последствия: авто-перезагрузка AutoTune не работала никогда,
	// а ошибка записывалась в circuit breaker (RecordError) и в историю событий
	// — оператор в UI видел «AutoTune reload failed» и растущий cool-down вместо
	// применения плана, хотя ручной apply в это же время работал.
	//
	// Параметры плана не теряются: ApplyAutoTunePlan прокидывает ContextSize,
	// NumGPULayers, KVCacheType, UseMmap и FlashAttn (см. ниже) — ровно те же
	// поля, что раньше складывались в env.
	// R230: маркер, взятый синхронно в triggerAutoTuneReload, обязан быть снят
	// на ЛЮБОМ пути (в т.ч. ошибка «backend not found» до ApplyAutoTunePlan),
	// иначе модель осталась бы «reload идёт» до истечения TTL. Повторный вызов
	// идемпотентен (sync.Once внутри).
	if plan.externalFinish != nil {
		defer plan.externalFinish()
	}
	// R280 (2026-10-29): AutoTune-перезагрузка — ТАКАЯ ЖЕ разрушительная
	// операция, как reload по чужому окну n_ctx, и подчиняется тому же правилу
	// R271: не выгружать модель под живым стримом.
	//
	// ЖИВОЙ ФАКТ (стенд r279): AutoTune сработал дважды за ~10 минут
	// («kv_cache q4_0 → f16») и каждый раз делал unload + load. Путь
	// triggerAutoTuneReload → executeAutoTuneReload → ApplyAutoTunePlan не имел
	// guard'а, который уже стоял в трёх местах nctx_reload_handlers.go, поэтому
	// авто-перезагрузка могла оборвать чужую генерацию — и клиент получал
	// «stream ended by upstream before completion», хотя upstream был жив.
	// R291: решение «ждать или отложить» принимает ОДНА функция. Дольше
	// LB_RELOAD_DEFER_FAST_FAIL_SEC в ответе не ждём: оператор/клиент видит
	// зависание там, где ответ уже известен.
	if ok, active := p.deferDestructiveOpR291(context.Background(), backendID, modelName, "autotune-reload"); !ok {
		logger.Get().Infow("autotune reload deferred: active streams on backend",
			"backend", backendID, "model", modelName,
			"active_streams", active,
			"reason", plan.Reason, "context_size", plan.ContextSize,
			"kv_cache_type", plan.KVCacheType)
		return errAutoTuneReloadDeferred
	}
	p.noteDestructiveOpR271(backendID, modelName, destructiveAutotuneR271)

	applyRes := p.ApplyAutoTunePlan(backendID, plan)
	// R283: «воркер занят» — это НЕ сбой. Разница живёт в одной функции
	// (autoTuneErrorFromApply), чтобы третье место не стало четвёртым, где
	// busy снова превращается в «failed».
	if err := autoTuneErrorFromApply(applyRes); err != nil {
		// Пометка разрушительной операции ставится ПЕРЕД apply, а apply мог не
		// состояться (модель не выгружали). Снимаем её, иначе следующий
		// оборванный стрим получит причину `balancer_autotune` без всякой
		// автоперезагрузки — клиенту сообщат неправду о причине (класс R271).
		// Если выгрузка УЖЕ произошла, пометка остаётся: обрыв в этом окне
		// действительно наш.
		if applyRes == nil || !applyRes.Unloaded {
			p.clearDestructiveOpR271(backendID, modelName)
		}
		return err
	}
	logger.Get().Infow("autotune: plan applied (unload+load)",
		"backend", backendID, "model", modelName, "reason", plan.Reason,
		"context_size", plan.ContextSize, "kv_cache_type", plan.KVCacheType,
		"gpu_layers", plan.NumGPULayers,
		"duration_ms", applyRes.FinishedAt.Sub(applyRes.StartedAt).Milliseconds())
	return nil
}

// applyCppWorkerEnvReload удалён в R66c (2026-09-22).
//
// Он был заглушкой: всегда логировал «env-based reload not fully implemented»
// и возвращал ошибку «env-based reload not yet implemented; apply plan
// manually». Вызывался ТОЛЬКО из executeAutoTuneReload, поэтому авто-применение
// плана AutoTune гарантированно не работало и вдобавок засоряло circuit
// breaker и историю событий ошибками. Замена — ApplyAutoTunePlan (ниже):
// unload + load с новыми параметрами, тот же путь, что у ручного apply.
