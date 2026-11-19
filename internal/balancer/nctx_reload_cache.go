// nctx_reload_cache.go — R631 (2026-11-19): Кэш наблюдённого окна: lastKnownNCtx, cacheLoadedContextLength, подтверждённое окно, правка num_ctx в теле, тексты отказов.
//
// ЗАЧЕМ ВЫНЕСЕНО. `nctx_reload_handlers.go` держал 2316 строк и четыре
// независимых слоя (обработчики, preflight, кэш наблюдённого окна, исполнение
// и метрики). Здесь — один из них; поведение не менялось, код перенесён как есть.
package balancer

import (
	"encoding/json"
	"fmt"
	"ollama-loadbalancer/pkg/types"
)

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

// cacheLoadedWindowActualR327 — R327 (2026-10-30): записать ПОДТВЕРЖДЁННОЕ окно
// во ВСЕ поля кэша, а не только в суммарное.
//
// ЖИВАЯ ЖАЛОБА («старый кэш от прошлой сессии — новый клиент, новая сессия, а
// места не хватило»). После перезагрузки модели кэш метрик получал только
// ContextLength. ContextPerSeq и MaxSlots оставались от ПРЕДЫДУЩЕЙ загрузки, а
// резолвер окна (num_ctx_resolver.go: loadedPerSeqNCtx) отдаёт приоритет именно
// ContextPerSeq — то есть до следующего тика поллера клиент видел устаревшее окно
// слота и мог получить отказ «не помещается» при фактически большем окне.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ, А НЕ ПРАВКА cacheLoadedContextLength. Та функция
// пишет ЦЕЛЕВОЕ окно в момент, когда async-reload только запущен (R68: чтобы
// второй запрос не запустил второй reload), и по контракту R83 она не должна
// трогать окно слота — иначе резолвер начнёт выдавать ещё не достигнутое окно
// (это ловят TestR83_StaleCache_ResolverUsesLiveWindow). Здесь наоборот: функция
// вызывается ТОЛЬКО после подтверждения готовности (waitForBackendNCtx вернул
// true), поэтому значения — факт, а не намерение.
//
// perSeq — окно НА КЛИЕНТА (то, что проверял waitForBackendNCtx); суммарное
// считается по числу слотов из кэша (cppworker отдаёт `max_slots`, при неизвестном
// считаем 1 — окно целиком одному клиенту, как это делает сам воркер).
func (p *Proxy) cacheLoadedWindowActualR327(backendID, modelName string, perSeq int) {
	if p == nil || p.metricsMgr == nil || perSeq <= 0 || modelName == "" {
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
			applyConfirmedWindowR327(&lm.LoadedModels[i], perSeq)
			return
		}
	}
	appended := types.LlamaCppModel{Name: modelName, State: "loaded"}
	applyConfirmedWindowR327(&appended, perSeq)
	lm.LoadedModels = append(lm.LoadedModels, appended)
}

// applyConfirmedWindowR327 — выставить три поля окна из подтверждённого значения.
//
// ContextPerSeq — окно слота (факт), MaxSlots — число слотов (из кэша воркера),
// ContextLength — суммарное окно = окно слота × слоты. Именно так их и читают
// collectPreflightState и резолвер.
func applyConfirmedWindowR327(m *types.LlamaCppModel, perSeq int) {
	if m == nil || perSeq <= 0 {
		return
	}
	slots := m.MaxSlots
	if slots <= 0 {
		slots = 1
	}
	m.MaxSlots = slots
	m.ContextPerSeq = perSeq
	m.ContextLength = perSeq * slots
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
	// R402 (2026-11-05): текст обязан объяснять ЕДИНИЦЫ и не отправлять
	// оператора «загрузить модель явно вручную»: балансер уже пробовал вырасти
	// сам, отказ означает, что расти некуда. Отдельно называем parallel — слоты
	// делят окно, и это частая причина «не даёт поднять окно» у одной сессии.
	if physicalMax > 0 && physicalMax < operatorMax {
		return fmt.Sprintf("requested num_ctx=%d exceeds the backend's physical limit max_vram_n_ctx=%d "+
			"(what fits in free VRAM with the models loaded now; operator limit auto_reload_max_n_ctx=%d is higher). "+
			"Model %q keeps its loaded window; the balancer cannot grow it automatically. "+
			"Free VRAM (unload other models, use a smaller model or fewer GPU layers) or lower the model's "+
			"parallel value: slots divide the window, so parallel=1 gives a single session the whole window.",
			requested, physicalMax, operatorMax, modelName)
	}
	return fmt.Sprintf("requested num_ctx=%d exceeds the effective window ceiling auto_reload_max_n_ctx=%d "+
		"(operator limit; the model profile's declared window may cap it too). "+
		"Model %q keeps its loaded window and the balancer cannot grow it automatically. "+
		"Raise the declared window in the model profile or LB_NCTX_RELOAD_MAX_N_CTX, or lower the model's "+
		"parallel value: slots divide the window, so parallel=1 gives a single session the whole window.",
		requested, operatorMax, modelName)
}

// formatNCtxVRAMExhaustedRefusal — отказ, когда VRAM измерена и свободного места
// НЕТ ВООБЩЕ (max_vram_n_ctx=0 при vram_known=true). R519.
//
// Отличие от formatNCtxCeilingRefusal: там «физический предел меньше операторского»,
// и совет поднять настройку бессмыслен. Здесь предела-числа нет вовсе: свободной
// VRAM не хватает даже на минимальное окно сверх загруженного. Поэтому не называем
// выдуманное число и не советуем «поднять лимит» — только освободить память.
func formatNCtxVRAMExhaustedRefusal(requested, operatorMax int, modelName string) string {
	return fmt.Sprintf("requested num_ctx=%d cannot be served: free VRAM is not enough for any window "+
		"growth right now (max_vram_n_ctx=0 with VRAM measured; operator limit auto_reload_max_n_ctx=%d is not "+
		"the binding constraint). Model %q keeps its loaded window and the balancer cannot grow it automatically. "+
		"Free VRAM: unload other models, lower gpu layers, use kv cache q8_0/q4_0, or lower the model's parallel "+
		"value (slots divide the window, so parallel=1 gives one session the whole window).",
		requested, operatorMax, modelName)
}
