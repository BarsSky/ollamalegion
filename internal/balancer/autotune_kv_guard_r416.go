// autotune_kv_guard_r416.go — R416 (2026-11-03): AutoTune не переключает
// KV-cache туда-обратно и не спорит с профилем модели.
//
// ЖИВОЙ ДЕФЕКТ (стенд, Qwen3.8-9B, первый же запрос после загрузки). Лог
// балансера подряд:
//
//	autotune: triggering async reload … reason: kv_cache q8_0 → q4_0 (quality fix)
//	autotune: triggering async reload … reason: kv_cache q4_0 → q8_0 (quality fix)
//	autotune: triggering async reload … reason: kv_cache q8_0 → q4_0 (quality fix)
//
// Каждый reload — это unload + load (~80 c). Причина пинг-понга — обратная
// связь: анализ строит «оптимальный» KV по СВОБОДНОЙ VRAM в момент замера.
// После загрузки q8_0 свободной VRAM меньше → «оптимально q4_0»; после q4_0
// свободной больше → «оптимально q8_0». Оба состояния «неоптимальны» с точки
// зрения друг друга, и AutoTune перезагружает модель на каждом запросе.
//
// Два правила, которые это заканчивают:
//  1. ЯВНЫЙ выбор оператора в профиле модели (kvCacheType) не оспаривается:
//     если профиль говорит q8_0, рекомендация q4_0 не применяется вовсе.
//  2. Гистерезис: тип KV для пары (backend, model) меняется не чаще раза в
//     autoTuneKVFlipCooldownR416. Первое применение проходит, обратное —
//     отклоняется, пока окно не истекло. Это не «запрет оптимизации»: n_ctx и
//     слои по-прежнему можно менять, ограничен только пинг-понг KV.
package balancer

import (
	"strings"
	"sync"
	"time"
)

// autoTuneKVFlipCooldownR416 — минимальный интервал между сменами типа KV-cache
// для одной пары (backend, model). Переменная, а не константа: тесты подменяют
// её миллисекундами, иначе проверка гистерезиса занимала бы полчаса.
var autoTuneKVFlipCooldownR416 = 30 * time.Minute

type autoTuneKVApplyR416 struct {
	kv string
	at time.Time
}

var (
	autoTuneKVGuardMuR416 sync.Mutex
	autoTuneKVGuardR416   = map[string]autoTuneKVApplyR416{}
)

func autoTuneKVKeyR416(backendID, model string) string {
	return backendID + "\x00" + strings.ToLower(strings.TrimSpace(model))
}

// autoTuneKVPinR416 — тип KV, зафиксированный профилем модели ("" = не задан).
func autoTuneKVPinR416(p *Proxy, model string) string {
	if p == nil || model == "" {
		return ""
	}
	if prof, ok := p.GetModelProfile(model); ok {
		return strings.TrimSpace(prof.KVCacheType)
	}
	return ""
}

// guardAutoTuneKVPlanR416 — убрать смену KV-cache из плана, если она спорит с
// профилем или нарушает гистерезис. Возвращает причину отказа ("" = план не
// тронут).
func guardAutoTuneKVPlanR416(p *Proxy, backendID, model string, plan *AutoTuneReloadPlan) (dropped string) {
	if plan == nil || plan.KVCacheType == "" {
		return ""
	}
	if pin := autoTuneKVPinR416(p, model); pin != "" && !strings.EqualFold(pin, plan.KVCacheType) {
		plan.KVCacheType = ""
		return "kv-cache type is pinned by the model profile (" + pin + "), recommendation ignored"
	}
	autoTuneKVGuardMuR416.Lock()
	last, ok := autoTuneKVGuardR416[autoTuneKVKeyR416(backendID, model)]
	autoTuneKVGuardMuR416.Unlock()
	if ok && !strings.EqualFold(last.kv, plan.KVCacheType) && time.Since(last.at) < autoTuneKVFlipCooldownR416 {
		plan.KVCacheType = ""
		return "kv-cache flip cooldown: last applied " + last.kv + " at " + last.at.Format(time.RFC3339)
	}
	return ""
}

// recordAutoTuneKVAppliedR416 — запомнить успешно применённый тип KV.
func recordAutoTuneKVAppliedR416(backendID, model, kv string) {
	kv = strings.TrimSpace(kv)
	if kv == "" {
		return
	}
	autoTuneKVGuardMuR416.Lock()
	autoTuneKVGuardR416[autoTuneKVKeyR416(backendID, model)] = autoTuneKVApplyR416{kv: kv, at: time.Now()}
	autoTuneKVGuardMuR416.Unlock()
}

// autoTunePlanHasRealChangeR416d — есть ли в плане изменение, ради которого
// стоит перезагружать модель.
//
// ContextSize в плане — не только «рекомендация», но и «сохранить фактическое
// окно»: PlanApplyAutoTune кладёт туда currentLoaded.ContextLength, чтобы reload
// не сбросил окно env-дефолтом (R83). После гардов R416/R416b это поле может
// остаться единственным непустым — и NeedsReload() = true даёт перезагрузку на
// каждом ответе, хотя менять нечего. Сравниваем с ФАКТИЧЕСКИМ окном: изменение
// контекста реально, только если оно отличается от загруженного.
func autoTunePlanHasRealChangeR416d(plan *AutoTuneReloadPlan, currentContext int) bool {
	if plan == nil {
		return false
	}
	if plan.KVCacheType != "" || plan.NumGPULayers != 0 || plan.FlashAttn != 0 || plan.UseMmap != nil {
		return true
	}
	if plan.ContextSize > 0 && plan.ContextSize != currentContext {
		return true
	}
	return false
}

// resetAutoTuneKVGuardR416 — сбросить состояние (тесты и Shutdown).
func resetAutoTuneKVGuardR416() {
	autoTuneKVGuardMuR416.Lock()
	autoTuneKVGuardR416 = map[string]autoTuneKVApplyR416{}
	autoTuneKVGuardMuR416.Unlock()
}
