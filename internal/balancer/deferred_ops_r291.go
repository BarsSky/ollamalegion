// deferred_ops_r291.go — R291 (2026-10-29): «кнопка думает две минуты» и
// разные форматы ответа при отложенных операциях.
//
// ЖИВЫЕ ФАКТЫ (стенд r290):
//  1. Ручной reload при АКТИВНОМ стриме отвечал верно (503 + deferred), но
//     только через 120 с — то есть ровно по истечении LB_RELOAD_DEFER_MAX_SEC.
//     Для оператора это «кнопка думает две минуты», хотя решение известно
//     сразу: стримы идут, ждать их дольше разумного бюджета бессмысленно.
//  2. `/api/chat` с num_ctx больше загруженного окна при активном стриме ждал
//     122 с и только потом отдавал 503 без чисел о требуемом окне; `/v1` в том
//     же сценарии ждал 73 с и обслуживался через admission. Один и тот же
//     сценарий — три разных поведения.
//
// ЧТО ЗДЕСЬ. Одно решение «ждать или отложить» (deferDecisionR291) и один
// формат «отложено» (writeDeferredJSON). Ожидание в 120 с остаётся только там,
// где до освобождения стримов действительно близко (бюджет не больше
// LB_RELOAD_DEFER_FAST_FAIL_SEC, дефолт 5 с); иначе отвечаем сразу.
//
// ПОЧЕМУ НЕ «ВСЕГДА ОТВЕЧАТЬ СРАЗУ». Короткое ожидание — это шанс, что стрим
// завершится и операция выполнится в том же запросе (не мигая клиенту 503).
// Дольше ждать в HTTP-ответе нельзя: клиент воспринимает это как зависание, а
// не как честный отказ.
package balancer

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
)

// Значения по умолчанию (совпадают с встроенным описанием в settings: они
// читаются и без Init, поэтому «лёгкие» тесты видят те же числа).
const (
	defaultReloadDeferFastFailSec = 5
	defaultDeferredRetryAfterSec  = 30
)

// reloadDeferFastFailR291 — сколько ещё согласны ждать в HTTP-ответе.
//
// Смысл: если активные стримы есть и бюджет ожидания БОЛЬШЕ этого значения,
// ждать в ответе бессмысленно — отвечаем deferred сразу. 0 = отвечать сразу
// всегда (ожидание в ответе запрещено).
func reloadDeferFastFailR291() int64 {
	v, ok := settings.HotInt("LB_RELOAD_DEFER_FAST_FAIL_SEC")
	if !ok || v < 0 {
		return defaultReloadDeferFastFailSec
	}
	return v
}

// deferredRetryAfterSecR291 — что писать в Retry-After/retryAfterSec.
//
// Берём окно быстрого отказа: именно через него имеет смысл повторить (после
// него либо стрим закончится, либо операция всё равно будет отложена тем же
// решением). Не ноль — Retry-After: 0 клиенты читают как «сразу» и получают
// молотьбу запросами.
func deferredRetryAfterSecR291() int {
	sec := int(reloadDeferFastFailR291())
	if sec <= 0 {
		sec = defaultDeferredRetryAfterSec
	}
	return sec
}

// deferDecisionR291 — единое решение для всех разрушительных операций.
//
// Возвращает:
//
//	activeStreams — сколько стримов сейчас на бэкенде;
//	wait          — сколько ждать (0, если ждать не нужно или нельзя);
//	failFast      — отвечать «отложено» сразу, не ожидая.
//
// Правило (одно на все пути, чтобы /api/chat и /v1 не расходились):
//
//	стримов нет                     → (0, 0, false)       выполняем
//	бюджет ожидания <= 0            → (n, 0, true)        не ждём (явный запрет)
//	бюджет > fast-fail (дефолт 5 с) → (n, 0, true)        «кнопка не думает»
//	иначе                           → (n, бюджет, false)  ждём короткий остаток
func (p *Proxy) deferDecisionR291(backendID string) (activeStreams int, wait time.Duration, failFast bool) {
	if p == nil || backendID == "" {
		return 0, 0, false
	}
	activeStreams = p.activeStreamsOnBackend(backendID)
	if activeStreams == 0 {
		return 0, 0, false
	}
	budget := reloadDeferMaxWaitR271()
	fastFail := reloadDeferFastFailR291()
	if budget <= 0 {
		return activeStreams, 0, true
	}
	if int64(budget/time.Second) > fastFail {
		return activeStreams, 0, true
	}
	return activeStreams, budget, false
}

// deferDestructiveOpR291 — единый вход перед разрушительной операцией.
//
// Возвращает (можно выполнять, сколько стримов было). Второе значение нужно
// вызывающему для честного текста отказа («мешает N стримов»), иначе оператор
// видит безликое «deferred».
//
// Заменяет прямые вызовы waitStreamsIdleR271 на горячих путях: там, где ответ
// ждёт клиент (синхронный reload, операторский reload из API), 120-секундное
// ожидание недопустимо — решение принимается сразу.
func (p *Proxy) deferDestructiveOpR291(ctx context.Context, backendID, model, operation string) (bool, int) {
	active, wait, failFast := p.deferDecisionR291(backendID)
	if active == 0 {
		return true, 0
	}
	if failFast {
		logger.Get().Infow("R291: destructive op deferred immediately — no point waiting in response",
			"operation", operation, "backend", backendID, "model", model,
			"active_streams", active,
			"defer_budget_sec", reloadDeferMaxWaitR271().Seconds(),
			"fast_fail_sec", reloadDeferFastFailR291(),
			"retry_after_sec", deferredRetryAfterSecR291())
		return false, active
	}
	ok := p.waitStreamsIdleR271(ctx, backendID, model, wait)
	return ok, p.activeStreamsOnBackend(backendID)
}

// deferredBodyR291 — тело ответа «отложено». Один формат для всех путей:
// клиент и UI проверяют `status`/`deferredReason`, а не разбирают текст.
type deferredBodyR291 struct {
	Status         string                 `json:"status"`
	Deferred       bool                   `json:"deferred"`
	DeferredReason string                 `json:"deferredReason"`
	Reason         string                 `json:"reason,omitempty"`
	Model          string                 `json:"model,omitempty"`
	Operation      string                 `json:"operation,omitempty"`
	RetryAfterSec  int                    `json:"retryAfterSec"`
	ActiveStreams  int                    `json:"activeStreams,omitempty"`
	Extra          map[string]interface{} `json:"-"`
}

// writeDeferredJSON — записать 503 + единый формат + Retry-After.
func writeDeferredJSON(w http.ResponseWriter, body deferredBodyR291) {
	if body.RetryAfterSec <= 0 {
		body.RetryAfterSec = deferredRetryAfterSecR291()
	}
	if body.Status == "" {
		body.Status = "deferred"
	}
	body.Deferred = true
	if body.Reason == "" {
		body.Reason = body.DeferredReason
	}
	payload := map[string]interface{}{
		"status":         body.Status,
		"deferred":       true,
		"deferredReason": body.DeferredReason,
		"reason":         body.Reason,
		"retryAfterSec":  body.RetryAfterSec,
	}
	if body.Model != "" {
		payload["model"] = body.Model
	}
	if body.Operation != "" {
		payload["operation"] = body.Operation
	}
	if body.ActiveStreams > 0 {
		payload["activeStreams"] = body.ActiveStreams
	}
	for k, v := range body.Extra {
		payload[k] = v
	}
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"status":"deferred","deferred":true,"deferredReason":"internal"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(body.RetryAfterSec))
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(data)
}

// writeReloadDeferredR291 — отказ «перезагрузка отложена» (активные стримы).
//
// Причина одна и та же на всех путях (`active streams`), чтобы UI и логи
// говорили одинаково: ручной reload из API, синхронный n_ctx-reload и
// AutoTune-путь.
func writeReloadDeferredR291(w http.ResponseWriter, model, operation string, activeStreams int) {
	writeDeferredJSON(w, deferredBodyR291{
		DeferredReason: "active streams",
		Model:          model,
		Operation:      operation,
		ActiveStreams:  activeStreams,
	})
}

// contextGrowthNumbers — «сколько нужно / сколько есть» для текста отказа.
//
// Нужна там, где ответ формируется не нашим JSON-форматом (preflight-путь
// возвращает msg+status вызывающему): без чисел оператор видит «reload already
// in progress» и не понимает, вырастет ли окно вообще. Возвращает нули, если
// оценка или загруженное окно неизвестны — тогда текст остаётся прежним, а не
// показывает выдуманные числа.
//
// R297 (2026-10-29): ЯВНО ЗАПРОШЕННОЕ КЛИЕНТОМ окно — часть ответа. Живой факт:
// клиент просил `num_ctx=32768`, а в теле отказа стояло `required=4098` (оценка
// промпта) при загруженном 16384 — оператор видел «нужно 4k» и не понимал, чего
// ему не хватает. Теперь `needed = max(оценка+бюджет, requested)`.
func (p *Proxy) contextGrowthNumbers(backendID, model string, body []byte, path string) (estimated, loaded, needed int) {
	if p == nil || backendID == "" || len(body) == 0 {
		return 0, 0, 0
	}
	meta := p.extractRequestMeta(body, path)
	if meta == nil {
		return 0, 0, 0
	}
	estimated = meta.EstimatedPromptTokens
	loaded = p.loadedPerSeqNCtx(backendID, model)

	if loaded <= 0 {
		// Загруженное окно неизвестно — числа «сколько нужно» были бы выдумкой
		// (не с чем сравнивать). Контракт прежний: возвращаем нули.
		return estimated, 0, 0
	}

	// R330 (2026-10-30): формула ОДНА на все guard'ы — neededWindowTokensR330.
	// Раньше здесь и в contextGrowthBlocker она была записана дважды, и любая
	// правка одной разводила вердикт с текстом отказа.
	needed = neededWindowTokensR330(meta)
	if needed == 0 {
		// Ни оценки, ни явного окна — чисел нет, текст остаётся прежним.
		return estimated, loaded, 0
	}
	return estimated, loaded, needed
}
