// load_progress_r303.go — R303: ограниченное ожидание чужой загрузки и
// типизированный отказ вместо «висящего» HTTP-запроса.
//
// ЖИВОЙ ДЕФЕКТ (отчёт по двухузловому стенду, D3):
//
//	POST /api/models/load  висел 260 с без ответа; в логе нет ни строки
//	«loading model» (лог пишется по завершении запроса — см. loggingMiddleware),
//	GET /api/info → in_flight_requests {"gemma-4-E4B-it-Q4_K_M": 3},
//	POST /api/models/unload → 409 «model is currently loading», хотя загрузки нет.
//
// Механизм: заявка на загрузку (b.loading[name]) осталась от умершей горутины, а
// `respondWhileAnotherLoadInProgress` в sync-ветке (?wait=true) вызывал
// WaitForLoad БЕЗ ТАЙМАУТА. Клиент не получал ни ответа, ни ошибки — только
// тишину, пока не истекал его собственный таймаут.
//
// ПРАВИЛО R303. Ни один путь управления моделью не ждёт бесконечно:
//   - ожидание ограничено CPPWORKER_LOAD_WAIT_MAX_SEC (default 90, 0 = без лимита);
//   - по истечении отдаётся 503 с ТИПОМ (status=load_in_progress | load_stuck),
//     возрастом заявки, владельцем, retryAfterMs и подсказкой;
//   - заявка старше CPPWORKER_LOAD_STALE_SEC и без живого инстанса в состоянии
//     loading считается мёртвой и снимается (load_claims_r303.go), поэтому
//     «лечится только перезапуском контейнера» больше не воспроизводится.
package main

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

const (
	// loadWaitMaxEnvR303 — предел ожидания чужой загрузки в sync-путях.
	loadWaitMaxEnvR303 = "CPPWORKER_LOAD_WAIT_MAX_SEC"
	// loadStaleAfterEnvR303 — с какого возраста заявка без живого инстанса
	// считается мёртвой и может быть снята.
	loadStaleAfterEnvR303 = "CPPWORKER_LOAD_STALE_SEC"

	// Значения по умолчанию. 90 c — больше любой нормальной загрузки 5-8 GB
	// модели на 8 GB GPU; при этом клиент получает ответ, а не тишину.
	defaultLoadWaitMaxSecR303    = 90
	defaultLoadStaleAfterSecR303 = 300
)

// envSecondsR303 — секунды из env (значение <= 0 допустимо: 0 = «без лимита»
// для ожидания, 0 = «без порога» для staleness).
func envSecondsR303(key string, def int) (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return time.Duration(def) * time.Second, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		logger.Get().Warnw("R303: некорректное значение env, использую значение по умолчанию",
			"env", key, "value", raw, "default", def)
		return time.Duration(def) * time.Second, false
	}
	return time.Duration(n) * time.Second, true
}

// loadWaitMaxR303 — предел ожидания чужой загрузки (0 = ждать без лимита).
func loadWaitMaxR303() time.Duration {
	d, _ := envSecondsR303(loadWaitMaxEnvR303, defaultLoadWaitMaxSecR303)
	return d
}

// loadStaleAfterR303 — возраст, после которого заявка без живого инстанса
// считается мёртвой.
func loadStaleAfterR303() time.Duration {
	d, _ := envSecondsR303(loadStaleAfterEnvR303, defaultLoadStaleAfterSecR303)
	return d
}

// waitLoadBoundedR303 — ожидание завершения чужой загрузки с ограничением.
//
// Возвращает (loaded, timedOut). timedOut=true означает: загрузка всё ещё идёт
// (или заявка висит) — вызывающий ОБЯЗАН ответить типизированной ошибкой
// writeLoadInProgressR303, а не продолжать ждать.
func waitLoadBoundedR303(modelName string) (bool, bool) {
	if backend == nil {
		return false, false
	}
	return backend.WaitForLoadTimeoutR303(modelName, loadWaitMaxR303())
}

// claimForModelR303 — снимок заявки (ok=false — заявки нет).
func claimForModelR303(modelName string) (cppbackend.LoadClaimR303, bool) {
	if backend == nil {
		return cppbackend.LoadClaimR303{}, false
	}
	return backend.LoadClaimR303(modelName)
}

// reclaimStaleClaimR303 — снять мёртвую заявку, если она мёртвая.
// Возвращает true, если заявка снята (после этого можно грузить модель).
func reclaimStaleClaimR303(modelName, why string) bool {
	if backend == nil {
		return false
	}
	if !backend.IsLoading(modelName) {
		return false
	}
	claim, reclaimed := backend.ReclaimStaleLoadClaimR303(modelName, loadStaleAfterR303())
	if !reclaimed {
		return false
	}
	logger.Get().Warnw("R303: мёртвая заявка на загрузку снята — операция продолжается",
		"name", modelName, "why", why,
		"claim_age_sec", claim.AgeSec,
		"claim_requester", claim.Requester,
		"loading_instance", claim.LoadingInstance,
		"stale_after_sec", loadStaleAfterR303().Seconds())
	return true
}

// writeLoadInProgressR303 — ответ «модель занята загрузкой, но это не тишина».
//
// status=load_in_progress — загрузка живая (есть инстанс в состоянии loading
//
//	либо заявка свежая): клиенту честно сказано подождать и сколько уже ждём.
//
// status=load_stuck — заявка висит давно и живого инстанса нет: это и есть
//
//	состояние «лечится перезапуском», о котором писал оператор. В таком ответе
//	есть возраст заявки, владелец и явная подсказка про ?force=true.
func writeLoadInProgressR303(w http.ResponseWriter, modelName string, waited time.Duration) {
	claim, ok := claimForModelR303(modelName)
	staleAfter := loadStaleAfterR303()
	stale := false
	if ok {
		// «Мёртвая» = висит долго И нет живого инстанса в состоянии loading.
		stale = !claim.LoadingInstance && claim.AgeSec >= staleAfter.Seconds()
	} else {
		// Заявки уже нет (сняли параллельно) — отвечаем как на «не готово».
		claim = cppbackend.LoadClaimR303{Model: modelName}
	}

	status := "load_in_progress"
	errMsg := "model " + modelName + " is still loading; the request was not started"
	hint := "wait for the load to finish (poll /api/models or /api/models/load/progress) and retry"
	if stale {
		status = "load_stuck"
		errMsg = "model " + modelName + " is marked as loading, but no live load exists " +
			"(stale load claim)"
		hint = "retry with ?force=true (POST /api/models/load?force=true) or unsubscribe the claim " +
			"via POST /api/models/unload?force=true — the claim is safely reclaimed when no load instance exists"
	}

	retryAfterMs := 3000
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterMs/1000))
	w.Header().Set("X-Load-Claim-State", status)
	body := map[string]interface{}{
		"error":            errMsg,
		"status":           status,
		"model":            modelName,
		"loading":          true,
		"waitedMs":         waited.Milliseconds(),
		"loadWaitLimitSec": loadWaitMaxR303().Seconds(),
		"retryAfterMs":     retryAfterMs,
		"stale":            stale,
		"claimAgeSec":      claim.AgeSec,
		"claimRequester":   claim.Requester,
		"loadingInstance":  claim.LoadingInstance,
		"hint":             hint,
	}
	if staleAfter > 0 {
		body["staleAfterSec"] = staleAfter.Seconds()
	}
	logger.Get().Warnw("R303: загрузка не завершилась за отведённое время — типизированный отказ",
		"name", modelName,
		"status", status,
		"waited_ms", waited.Milliseconds(),
		"claim_age_sec", claim.AgeSec,
		"claim_requester", claim.Requester,
		"loading_instance", claim.LoadingInstance,
		"stale", stale)
	writeJSON(w, http.StatusServiceUnavailable, body)
}
