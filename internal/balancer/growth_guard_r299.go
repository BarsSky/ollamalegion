// growth_guard_r299.go — R299 (2026-10-29): «нужно больше окна, а вырасти
// сейчас нельзя» решается ДО preflight, а отказ говорит правду числами.
//
// ЖИВОЙ ДЕФЕКТ (стенд 793db393). Запрос с num_ctx больше загруженного окна при
// чужом активном стриме:
//
//   - простоял 88.2 с ВНУТРИ preflight (RunPreflight пытался вырастить окно,
//     которого вырастить нельзя, пока идёт стрим),
//   - и только потом получил 503 телом старого формата:
//     {"error":"...","decision":"async_reload","retry_after_seconds":30},
//   - в тексте стояло `active streams=0`.
//
// Два урока, и оба стали кодом ниже.
//
//  1. Решение известно ДО сетевого preflight. Окно меньше нужного + стрим идёт
//     (или перезагрузка уже в процессе) = ждать нечего: guard R271 всё равно
//     отложит перезагрузку. Поэтому проверка стоит на ВХОДЕ обоих preflight-путей
//     (раунтер → runInferencePreflight, транспорт → preflightNCtxReloadIfNeeded),
//     а не после того, как первый уже потратил полторы минуты.
//
//  2. Числа в отказе должны быть правдой. `active streams=0` рядом с «reload in
//     progress» — не «стримов нет», а «на момент замера учёт был пуст»: запрос,
//     который сам ждёт в preflight, стримом ещё не зарегистрирован
//     (beginClientStreamR271 вызывается ПОСЛЕ admission). Поэтому в теле отказа
//     теперь два независимых поля: activeStreams (живой счётчик, максимум из
//     учёта по бэкенду и по модели) и reloadPending (идёт ли перезагрузка).
//     Ноль стримов при reloadPending=true — честная картина, а не «никто не
//     мешает».
package balancer

import "net/http"

// growthBlockersR288 — кто мешает росту окна прямо сейчас.
//
// active — максимум из учёта по бэкенду и по модели. Почему максимум, а не
// сумма и не одно число: учёт по модели ведётся по ключу (бэкенд+имя модели),
// и при расхождении имени (алиас против загруженного имени) он может не увидеть
// стрим, который видит учёт по бэкенду. Отказ с заниженным числом оператор
// читает как ошибку балансера, поэтому берём наибольшее из двух наблюдений.
func growthBlockersR288(p *Proxy, backendID, model string) (active int, reloadPending bool) {
	if p == nil || backendID == "" {
		return 0, false
	}
	active = p.activeStreamsOnBackend(backendID)
	if onModel := p.activeStreamsOnModel(backendID, model); onModel > active {
		active = onModel
	}
	if p.nctxReload != nil {
		reloadPending = p.nctxReload.IsReloadPending(backendID, model)
	}
	return active, reloadPending
}

// growthDeferR299 — вердикт «отвечаем сразу, preflight не запускаем».
//
// Возвращает true только когда оба условия выполнены:
//   - окно действительно мало (needed > loaded, см. contextGrowthBlocker);
//   - росту сейчас что-то мешает (идут стримы или перезагрузка уже в процессе).
//
// Второе условие обязательно: если не мешает ничего, preflight ОБЯЗАН попробовать
// вырастить окно — иначе мы отказывали бы запросу, который сами же могли
// обслужить.
func (p *Proxy) growthDeferR299(backendID, model string, body []byte, path string) (
	shouldDefer bool, estimated, loaded, needed, active int, reloadPending bool,
) {
	if p == nil || backendID == "" || len(body) == 0 {
		return false, 0, 0, 0, 0, false
	}
	meta := ExtractRequestMeta(body, path)
	if meta == nil {
		return false, 0, 0, 0, 0, false
	}
	// Числа считаются ТЕМ ЖЕ правилом, что и в остальных отказах
	// (needed = max(оценка промпта + бюджет, явно запрошенное окно)).
	estimated, loaded, needed = p.contextGrowthNumbers(backendID, model, body, path)
	if needed <= 0 || loaded <= 0 || needed <= loaded {
		return false, estimated, loaded, 0, 0, false
	}
	active, reloadPending = growthBlockersR288(p, backendID, model)
	if active == 0 && !reloadPending {
		// Рост возможен — пусть preflight попробует.
		return false, estimated, loaded, needed, 0, false
	}
	return true, estimated, loaded, needed, active, reloadPending
}

// writePreflightDeferredR299 — единое тело отказа preflight-путей.
//
// Заменяет два «самописных» тела старого формата
// (`{"decision":"async_reload","retry_after_seconds":30}`) в потоковом и
// нестриминговом транспорте. Причина одна на все пути:
// deferredReason=context_growth_pending (R291 формат), а поля — те самые числа,
// которых не хватало оператору: requiredTokens / loadedWindowTokens /
// activeStreams / reloadPending.
//
// msg (текст прежнего отказа) остаётся в теле как `error` — клиенты, которые
// показывали его пользователю, не теряют текст.
func writePreflightDeferredR299(w http.ResponseWriter, p *Proxy, backendID, model string,
	body []byte, path, msg string, retryAfter int) {
	estimated, loaded, needed := p.contextGrowthNumbers(backendID, model, body, path)
	active, reloadPending := growthBlockersR288(p, backendID, model)
	extra := map[string]interface{}{
		"reloadPending": reloadPending,
		"activeStreams": active,
		"hint":          "raise the model context window (WebUI → model card) or shorten the history",
	}
	if msg != "" {
		extra["error"] = msg
	}
	if needed > 0 {
		extra["requiredTokens"] = needed
	}
	if loaded > 0 {
		extra["loadedWindowTokens"] = loaded
	}
	if estimated > 0 {
		extra["estimatedPromptTokens"] = estimated
	}
	writeDeferredJSON(w, deferredBodyR291{
		DeferredReason: "context_growth_pending",
		Model:          model,
		RetryAfterSec:  retryAfter,
		Extra:          extra,
	})
}
