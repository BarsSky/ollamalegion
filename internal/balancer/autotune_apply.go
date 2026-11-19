// autotune_apply.go — R636 (2026-11-19): применение плана: ApplyAutoTunePlan.
//
// ЗАЧЕМ ВЫНЕСЕНО. `autotune.go` держал 1666 строк: типы профиля, правила
// выбора KV-типа и числа слоёв, отчёт, предохранитель, трекер, две перезагрузки
// и планирование применения. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"fmt"
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"strings"
	"time"
)

// ApplyAutoTunePlan — R54.6 (2026-08-24): применяет AutoTuneReloadPlan к бэкенду.
//
// Алгоритм:
//  1. Unload текущую модель (через cppworker /api/models/unload).
//  2. Load с обновленными параметрами (через ModelManager.executeLlamaCppLoad).
//  3. Возвращает результат (success/error + message).
//
// Используется из /api/v1/admin/autotune/{backendID}/apply endpoint.
//
// NB: circuit breaker не применяется (manual apply — operator override).
// Но circuit state обновляется (RecordSuccess/RecordError) для telemetry.
func (p *Proxy) ApplyAutoTunePlan(backendID string, plan *AutoTuneReloadPlan) *AutoTuneApplyResult {
	result := &AutoTuneApplyResult{
		BackendID: backendID,
		ModelName: plan.ModelName,
		StartedAt: time.Now(),
	}

	if p == nil {
		result.Error = "nil proxy"
		return result
	}
	if plan == nil {
		result.Error = "nil plan"
		return result
	}

	// 1) Find backend
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
		result.Error = fmt.Sprintf("backend %s not found", backendID)
		return result
	}
	if backend.CppWorkerPort == 0 {
		result.Error = fmt.Sprintf("backend %s has no CppWorkerPort", backendID)
		return result
	}

	// R230: пока идёт apply, помечаем reload как идущий. Иначе клиентский
	// запрос, пришедший между unload и load, успевает вызвать ленивую загрузку
	// по профилю — и она выигрывает гонку, оставляя конфигурацию неизменной
	// (см. reloadDedupRegistry.BeginExternalReload).
	//
	// Async trigger (executeAutoTuneReload) уже взял маркер СИНХРОННО — тогда
	// используем его, а не пытаемся взять второй (иначе получили бы «reload уже
	// идёт» на собственном маркере).
	var finishReload func()
	if plan.externalFinish != nil {
		finishReload = plan.externalFinish
	} else if p.nctxReload != nil {
		var started bool
		finishReload, started = p.nctxReload.BeginExternalReload(backendID, plan.ModelName)
		if !started {
			result.Error = "reload already in progress for model"
			result.FinishedAt = time.Now()
			return result
		}
	}
	if finishReload != nil {
		defer finishReload()
	}

	// 2) Unload current model
	unloadResult := p.executeLlamaCppUnload(backendID, plan.ModelName)
	if !unloadResult.Success {
		result.Error = fmt.Sprintf("unload failed: %s", unloadResult.Error)
		result.FinishedAt = time.Now()
		return result
	}
	logger.Get().Infow("autotune: unloaded model for AutoTune apply",
		"backend", backendID, "model", plan.ModelName)
	result.Unloaded = true

	// 3) Build load request
	req := ModelOpRequest{
		Operation: "load",
		ModelName: plan.ModelName,
	}
	if plan.ContextSize > 0 {
		cs := plan.ContextSize
		req.ContextSize = &cs
	}
	if plan.NumGPULayers != 0 {
		gl := plan.NumGPULayers
		req.GPULayers = &gl
	}
	if plan.KVCacheType != "" {
		kt := plan.KVCacheType
		req.KVCacheType = &kt
	}
	if plan.UseMmap != nil {
		req.UseMmap = plan.UseMmap
	}
	if plan.FlashAttn != 0 {
		fa := plan.FlashAttn
		req.FlashAttn = &fa
	}

	// 4) Load with new params (wait for completion)
	mm := p.autotuneModelManager()
	if mm == nil {
		result.Error = "ModelManager not available"
		result.FinishedAt = time.Now()
		return result
	}
	loadResult := mm.executeLlamaCppLoad(backend.Host, backend.CppWorkerPort, backendID, req)
	if !loadResult.Success {
		result.Error = fmt.Sprintf("load failed: %s", loadResult.Error)
		result.FinishedAt = time.Now()
		return result
	}

	// R230: проверяем, что план ДЕЙСТВИТЕЛЬНО применился. Раньше apply отвечал
	// success, даже когда параллельная ленивая загрузка успевала раньше и грузила
	// модель с профильными params (q4_0/40 вместо f16/-1) — AutoTune «применял»
	// неизменную конфигурацию снова и снова, а оператор видел reload без эффекта.
	if actual, ok := p.freshLoadedModel(backendID, backend.Host, backend.CppWorkerPort, plan.ModelName); ok {
		if mismatch := autoTuneAppliedMismatch(plan, actual); mismatch != "" {
			result.Error = fmt.Sprintf("load reported success, но конфигурация не применилась: %s", mismatch)
			result.Message = "plan not applied"
			result.FinishedAt = time.Now()
			return result
		}
	}

	result.Success = true
	result.Message = fmt.Sprintf("reloaded with new params: %s", plan.Reason)
	result.FinishedAt = time.Now()
	return result
}

// freshLoadedModel — R230: принудительно опрашивает /api/models бэкенда и
// возвращает загруженную модель (нужно, чтобы проверить факт применения плана,
// а не ждать следующего тика поллера).
func (p *Proxy) freshLoadedModel(backendID, host string, port int, modelName string) (types.LlamaCppModel, bool) {
	if p == nil || p.metricsMgr == nil {
		return types.LlamaCppModel{}, false
	}
	if poller := p.llamaCppMetricsPoller; poller != nil {
		poller.pollBackend(backendInfo{id: backendID, host: host, port: port})
	}
	lm, ok := p.metricsMgr.SnapshotLlamaCppMetrics(backendID)
	if !ok || lm == nil {
		return types.LlamaCppModel{}, false
	}
	for _, m := range lm.LoadedModels {
		if modelNameMatches(m.Name, modelName) && m.State == "loaded" {
			return m, true
		}
	}
	return types.LlamaCppModel{}, false
}

// autoTuneAppliedMismatch — R230: расхождение между планом и фактически
// загруженной моделью. Пустая строка = план применён.
func autoTuneAppliedMismatch(plan *AutoTuneReloadPlan, actual types.LlamaCppModel) string {
	if plan == nil {
		return ""
	}
	var diffs []string
	// Сверяем только то, что воркер реально сообщил: старые сборки/моки могут
	// не отдавать параметры вообще (0/""), и это не повод объявлять провал.
	//
	// R493-12h: окно сравнивается ВЫРОВНЕННЫМ. Воркер всегда выравнивает n_ctx
	// вверх до 256 (GGML_PAD) и отдаёт в /api/models выровненное значение.
	// Прямое сравнение объявляло провал там, где план применился: план 40000 →
	// фактически 40192 → «load reported success, но конфигурация не применилась:
	// n_ctx=40192, ожидалось 40000», и AutoTune повторял применение снова и снова.
	if want := cppbackend.PadContext(plan.ContextSize); plan.ContextSize > 0 && actual.ContextLength > 0 &&
		actual.ContextLength != want {
		diffs = append(diffs, fmt.Sprintf("n_ctx=%d, ожидалось %d (план %d + выравнивание по 256)",
			actual.ContextLength, want, plan.ContextSize))
	}
	if want := normalizeKVType(plan.KVCacheType); want != "" && actual.KvCacheType != "" {
		if got := normalizeKVType(actual.KvCacheType); got != want {
			diffs = append(diffs, fmt.Sprintf("kv_cache=%s, ожидалось %s", actual.KvCacheType, plan.KVCacheType))
		}
	}
	if plan.NumGPULayers > 0 && actual.NumGPULayers > 0 {
		// -1 (all) в плане не сравниваем: cppworker сообщает фактическое число.
		allLayers := actual.NumGPULayers == actual.NLayers && actual.NLayers > 0
		wantAll := plan.NumGPULayers >= actual.NLayers && actual.NLayers > 0
		if !(allLayers && wantAll) && actual.NumGPULayers != plan.NumGPULayers {
			diffs = append(diffs, fmt.Sprintf("num_gpu_layers=%d, ожидалось %d", actual.NumGPULayers, plan.NumGPULayers))
		}
	}
	return strings.Join(diffs, "; ")
}
