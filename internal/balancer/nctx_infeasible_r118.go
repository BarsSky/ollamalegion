package balancer

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// nctx_infeasible_r118.go — R118.7 (2026-10-14): «окно физически недостижимо» —
// запоминаем и объясняем, а не пробуем перезагружать на каждом запросе.
//
// ЖИВОЙ ДЕФЕКТ (лог стенда, 23:04-23:06). Копия модели на узле была загружена с
// окном 8192 (4096 на слот), а клиент просил 8192 на слот — preflight планировал
// перезагрузку на суммарные 16384, cppworker отвечал отказом:
//
//	{"code":"n_ctx_infeasible","error":"requested n_ctx=16384 exceeds the physically
//	  feasible maximum ...","stage":"does_not_fit","max_vram_n_ctx":0,
//	  "suggestion":"Веса модели не помещаются ни в VRAM, ни в RAM ..."}
//
// и так на КАЖДЫЙ следующий запрос: перезагрузка планировалась снова, снова
// получала отказ, клиент ждал (в живом прогоне — до 33 с) и в итоге получал окно
// меньше запрошенного без внятного объяснения.
//
// ТЕПЕРЬ: отказ запоминается на (бэкенд, модель) вместе с причиной и потолком
// (фактически загруженное окно). Пока потолок не изменился, preflight НЕ планирует
// перезагрузку выше него, а отвечает клиенту 413 с числами и причиной от
// cppworker'а. Запись снимается сама: по TTL, при успешной перезагрузке или когда
// метрики показывают, что окно всё-таки выросло.

// nctxInfeasibleTTL — сколько помним отказ. Минуты: этого достаточно, чтобы
// клиентские ретраи не превращались в цикл перезагрузок, и мало, чтобы устаревший
// отказ не блокировал окно после освобождения памяти оператором.
const nctxInfeasibleTTL = 10 * time.Minute

// nctxInfeasible — запомненный отказ «окно не помещается».
type nctxInfeasible struct {
	// MaxNCtx — суммарное окно, выше которого перезагружать нельзя (обычно
	// фактически загруженное окно).
	MaxNCtx int
	// Requested — таргет, который cppworker отверг.
	Requested int
	// Reason — объяснение от cppworker (error + suggestion), для клиента и лога.
	Reason string
	// At — когда отказали (для TTL).
	At time.Time
}

// nctxInfeasibleKey — ключ записи: бэкенд + модель (в нижнем регистре).
func nctxInfeasibleKey(backendID, modelName string) string {
	return backendID + "|" + strings.ToLower(strings.TrimSpace(modelName))
}

// recordNCtxInfeasible — запомнить, что перезагрузка на requestedTotal отвергнута.
//
// Потолок = requestedTotal-1: выше отвергнутого таргета расти нельзя (ровно этот
// таргет cppworker и отверг), а меньший таргет может и поместиться — пусть решает
// cppworker. Кэш метрик для потолка НЕ используем: сразу после неудачной попытки
// cppworker успевает показать в /api/models ЗАПРОШЕННОЕ окно (живой прогон:
// пытались 16384 → 422 → в кэше уже 16384, и потолок «16384» не блокировал ничего,
// поэтому цикл reload→422 продолжался).
func (p *Proxy) recordNCtxInfeasible(backendID, modelName string, requestedTotal int, body []byte) {
	if p == nil || backendID == "" || modelName == "" {
		return
	}
	reason := nctxInfeasibleReason(body)
	ceiling := 0
	if requestedTotal > 0 {
		ceiling = requestedTotal - 1
	}
	loadedNow, _ := p.loadedWindowFromCache(backendID, modelName)

	p.mu.Lock()
	if p.nctxInfeasible == nil {
		p.nctxInfeasible = map[string]nctxInfeasible{}
	}
	p.nctxInfeasible[nctxInfeasibleKey(backendID, modelName)] = nctxInfeasible{
		MaxNCtx:   ceiling,
		Requested: requestedTotal,
		Reason:    reason,
		At:        time.Now(),
	}
	p.mu.Unlock()

	logger.Get().Warnw("nctxInfeasible: перезагрузка окна отвергнута cppworker'ом — запомнил потолок",
		"backend", backendID, "model", modelName,
		"requested_total_n_ctx", requestedTotal, "ceiling_n_ctx", ceiling,
		"loaded_now_n_ctx", loadedNow, "reason", reason)
}

// nctxInfeasibleReason — человекочитаемая причина из тела 422.
func nctxInfeasibleReason(body []byte) string {
	var parsed struct {
		Code       string `json:"code"`
		Error      string `json:"error"`
		Suggestion string `json:"suggestion"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return strings.TrimSpace(string(body))
	}
	parts := make([]string, 0, 2)
	if s := strings.TrimSpace(parsed.Error); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(parsed.Suggestion); s != "" {
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return strings.TrimSpace(string(body))
	}
	return strings.Join(parts, " ")
}

// isNCtxInfeasibleResponse — отказ именно про недостижимое окно (а не 404/500).
func isNCtxInfeasibleResponse(status int, body []byte) bool {
	if status != 422 {
		return false
	}
	low := strings.ToLower(string(body))
	return strings.Contains(low, "n_ctx_infeasible") ||
		strings.Contains(low, "does_not_fit") ||
		strings.Contains(low, "exceeds the physically feasible maximum")
}

// nctxInfeasibleFor — действующий потолок для (бэкенд, модель).
// ok=false, если отказа не было или он истёк по TTL.
//
// Автоочистку «окно выросло» здесь НЕ делаем: кэш метрик сразу после неудачной
// попытки показывает запрошенное окно (см. recordNCtxInfeasible), а если окно
// реально вырастет, запрос до этой ветки не дойдёт — его обслужит smart-skip
// («загруженное ≥ запрошенного»). Запись снимается по TTL и явно — при успешной
// перезагрузке (clearNCtxInfeasible).
func (p *Proxy) nctxInfeasibleFor(backendID, modelName string) (ceiling int, reason string, ok bool) {
	if p == nil || backendID == "" || modelName == "" {
		return 0, "", false
	}
	key := nctxInfeasibleKey(backendID, modelName)
	p.mu.RLock()
	rec, exists := p.nctxInfeasible[key]
	p.mu.RUnlock()
	if !exists {
		return 0, "", false
	}
	if time.Since(rec.At) > nctxInfeasibleTTL {
		p.clearNCtxInfeasible(backendID, modelName)
		return 0, "", false
	}
	return rec.MaxNCtx, rec.Reason, true
}

// clearNCtxInfeasible — снять запись (после успешной перезагрузки или роста окна).
func (p *Proxy) clearNCtxInfeasible(backendID, modelName string) {
	if p == nil || backendID == "" || modelName == "" {
		return
	}
	key := nctxInfeasibleKey(backendID, modelName)
	p.mu.Lock()
	if p.nctxInfeasible != nil {
		delete(p.nctxInfeasible, key)
	}
	p.mu.Unlock()
}

// nctxInfeasibleSnapshot — все действующие записи (для тестов и диагностики).
func (p *Proxy) nctxInfeasibleSnapshot() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.nctxInfeasible))
	for k, v := range p.nctxInfeasible {
		out = append(out, k+"="+strconv.Itoa(v.MaxNCtx))
	}
	sort.Strings(out)
	return out
}
