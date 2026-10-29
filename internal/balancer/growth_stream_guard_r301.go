// growth_stream_guard_r301.go — R301 (2026-10-29): «нужно окно больше
// загруженного» решается ОДИНАКОВО на всех путях, в том числе на стриминге,
// и решается ДО того, как запрос займёт слот.
//
// ЖИВОЙ ДЕФЕКТ (стенд 573a03a3, нативная поверхность Ollama :18080,
// POST /api/chat, модель загружена 8192 на слот, клиент шлёт options.num_ctx=16384):
//
//	HTTP 200 за 0.12 с, тело:
//	  {"done":false}
//	  {"bridge_code":2,"bridge_message":"stream inference failed with code 2:
//	   requested n_ctx=16384 exceeds model's effective n_ctx=8192 ..."}
//
// То есть ни один guard не сработал, запрос ушёл в воркер, и клиент получил
// «стрим», который состоит из кадра ошибки и не имеет терминального done:true.
// Для агента (Cline) это неотличимо от «модель ответила пустотой»: он не
// повторяет, не сообщает про контекст и не показывает причину.
//
// ПОЧЕМУ ТАК ВЫШЛО — три разных механизма, и каждый считал, что дело не его:
//
//  1. guard R299 (growthDeferR299) откладывает отказ ТОЛЬКО когда росту окна
//     что-то мешает (идёт стрим или уже идёт перезагрузка). Если не мешает
//     ничего, он намеренно отдаёт решение preflight'у: «пусть попробует
//     вырасти». Это правильно для не-стримингового пути.
//  2. Но preflight-транспорт (preflightNCtxReloadIfNeeded) для СТРИМИНГОВЫХ
//     запросов выходил раньше любой попытки роста (R60.x: перезагрузка перед
//     стримом рвала текущий стрим). Значит «preflight попробует» на этом пути
//     никогда не правда: попытки не будет вовсе.
//  3. На поверхности :18080 запрос доходит до preflight'а ПОСЛЕ admission:
//     сначала занимается слот (unified queue, ожидание до 900 с), и только
//     внутри proxyRequestLlamaCpp считаются окна. Поэтому «второй» запрос с
//     большим окном при занятой модели простоял в очереди 120 с и был отменён
//     клиентом, хотя ответ «окно не вырастет» был известен сразу.
//
// ЧТО ЗДЕСЬ. Один вердикт (growthVerdictR301) с ТРЕМЯ исходами, который
// используется и до очереди, и в preflight'е:
//
//   - Defer: отказ стандартным телом «отложено» (writePreflightDeferredR299 —
//     формат не новый, R291/R299: deferredReason=context_growth_pending плюс
//     числа requiredTokens/loadedWindowTokens/activeStreams/reloadPending);
//   - Served: обслужить с подменённым num_ctx (окно НЕ нужно растить: промпт с
//     бюджетом вывода влезает в загруженное окно, клиент просто попросил
//     больше — ровно так же поступает политика primary, R83);
//   - ничего: окно достаточно, решение не наше.
//
// УМЕЕТ ЛИ НАТИВНЫЙ ПУТЬ РАСТИТЬ ОКНО — ответ для оператора: НЕТ, не для
// стримингового запроса. Рост окна на этом пути требует перезагрузки модели,
// а перезагрузка под стримом рвёт стрим (R271: guard откладывает её, клиент
// получает «stream ended by upstream»). Поэтому для стриминга честный ответ —
// Served (если влезает) или Defer с числами и подсказкой «поднимите окно в
// карточке модели». Рост для стриминговых клиентов делает НЕ стриминговый
// preflight (не-стриминговый запрос той же модели) либо явная перезагрузка в
// WebUI/админке; после неё тот же запрос обслуживается с первого раза.
//
// ГРАНИЦА ОТКАЗА. Отказываем только когда в окно не влезает САМ ПРОМПТ: бюджет
// генерации воркер урезает сам, и отказ по «промпт + 4096» был бы отказом
// запросу, который обслуживается укороченным ответом. Запрошенное клиентом
// окно больше загруженного — не повод отказывать, а повод привести num_ctx к
// окну слота (Served).
package balancer

import (
	"fmt"

	"ollama-loadbalancer/pkg/logger"
)

// growthVerdictR301 — решение по окну для одного запроса.
//
// Числа (Estimated/Loaded/Needed) заполняются и при «ничего не делаем»: они
// нужны логу, чтобы живой разбор не требовал догадок.
type growthVerdictR301 struct {
	// Defer — отказ стандартным телом «отложено» до любой сети и любого слота.
	Defer bool
	// Served — обслужить с подменённым телом (Body): окно растить не нужно.
	Served bool
	// Body — тело запроса с num_ctx, приведённым к загруженному окну.
	Body []byte

	Estimated     int // оценка промпта
	Loaded        int // окно ОДНОГО слота загруженной модели
	Needed        int // сколько нужно: max(промпт + бюджет вывода, запрошенное окно)
	Active        int // сколько стримов идёт сейчас
	ReloadPending bool
	Streaming     bool
}

// decided — вердикт что-то предписывает (отказ или подмена тела).
func (v growthVerdictR301) decided() bool { return v.Defer || v.Served }

// growthVerdictR301 — единственное место, где решается «растим / обслуживаем /
// отказываем» для запроса, которому нужно больше окна, чем загружено.
//
// streaming=true означает: этот путь НЕ перезагружает модель под запрос (см.
// шапку файла), поэтому «пусть preflight попробует вырасти» здесь запрещено —
// иначе мы отдаём воркеру заведомо невыполнимый запрос и клиент получает 200
// с кадром ошибки вместо терминального ответа.
func (p *Proxy) growthVerdictR301(backendID, model string, body []byte, path string, streaming bool) growthVerdictR301 {
	v := growthVerdictR301{Streaming: streaming}
	if p == nil || backendID == "" || len(body) == 0 {
		return v
	}
	meta := ExtractRequestMeta(body, path)
	if meta == nil {
		return v
	}
	estimated, loaded, needed := p.contextGrowthNumbers(backendID, model, body, path)
	v.Estimated, v.Loaded, v.Needed = estimated, loaded, needed
	if loaded <= 0 || needed <= 0 || needed <= loaded {
		// Окно известно и его хватает (или неизвестно — числами не спекулируем).
		return v
	}

	v.Active, v.ReloadPending = growthBlockersR288(p, backendID, model)

	if !streaming {
		// Не-стриминговый путь: рост возможен, пока его ничто не блокирует
		// (прежнее правило R299 — рост пробует preflight).
		if v.Active > 0 || v.ReloadPending {
			v.Defer = true
		}
		return v
	}

	// Стриминг: роста не будет. Решение принимается по ПРОМПТУ, а не по
	// «промпт + бюджет вывода»: бюджет генерации воркер урезает сам (клампит
	// num_predict по свободному месту), и отказ по нему был бы отказом запросу,
	// который воркер обслужил бы укороченным ответом. Отказываем только тогда,
	// когда в окно не влезает сам промпт — такой запрос воркер гарантированно
	// отвергнет кадром ошибки внутри 200 (живой дефект этого файла).
	requested := meta.RequestedNCtxOverride
	if estimated <= 0 || estimated <= loaded {
		// Промпт влезает. Если клиент попросил окно больше загруженного —
		// обслуживаем, приведя num_ctx к окну слота (окно растить не нужно):
		// ровно это и было в живом дефекте — просили 16384 «на всякий случай»
		// при промпте в несколько токенов.
		if requested > loaded {
			if patched := patchNumCtxInBody(body, loaded); patched != nil {
				v.Served = true
				v.Body = patched
			}
		}
		return v
	}
	// Промпт физически не влезает в загруженное окно. Число в отказе — то же,
	// что посчитает тело (contextGrowthNumbers: max(промпт + бюджет, запрошенное
	// окно)): оператор не должен видеть в тексте одно число, а в requiredTokens
	// другое. Отказ при этом принимается по ПРОМПТУ (см. выше), поэтому запрос,
	// который обслуживается укороченным ответом, отказа не получит.
	v.Defer = true
	return v
}

// growthDeferMessageR301 — текст причины для стандартного тела отказа.
//
// Текст отдельный от R299 (там «cannot grow right now» — про занятость), потому
// что здесь причина другая и постоянная: стриминговый путь окно не растит.
// Числа в тексте те же, что в теле (нужно / загружено) — оператор должен
// увидеть, чего именно не хватает.
func growthDeferMessageR301(model string, v growthVerdictR301) string {
	if v.Streaming {
		return fmt.Sprintf("model %q needs %d tokens of context, the loaded window is %d tokens; "+
			"a streaming request does not reload the model, so the window cannot grow for this request "+
			"(raise the model context window administratively, then retry)", model, v.Needed, v.Loaded)
	}
	return fmt.Sprintf("model %q needs %d tokens of context, the loaded window is %d and cannot grow right now "+
		"(active streams=%d, reload pending=%t), retry later", model, v.Needed, v.Loaded, v.Active, v.ReloadPending)
}

// logGrowthVerdictR301 — одна INFO-строка на решение (where: точка входа).
//
// Без неё живой разбор «почему клиент получил отказ на :18080» требовал чтения
// исходников: до этой правки в логе не было ни одного числа.
func logGrowthVerdictR301(where, backendID, model, path string, v growthVerdictR301) {
	if !v.decided() {
		return
	}
	logger.Get().Infow("context growth decision (R301)",
		"where", where,
		"backend", backendID, "model", model, "path", path,
		"streaming", v.Streaming,
		"defer", v.Defer, "served_with_loaded_window", v.Served,
		"estimated_prompt_tokens", v.Estimated,
		"required_tokens", v.Needed,
		"loaded_window_tokens", v.Loaded,
		"active_streams", v.Active,
		"reload_pending", v.ReloadPending)
}

// backendWithLoadedModelR301 — бэкенд, где эта модель загружена с САМЫМ ШИРОКИМ
// окном слота.
//
// Нужен проверке ДО admission-очереди: на поверхности :18080 слот ещё не
// занят и sticky-бэкенда может не быть вовсе (первый запрос клиента), поэтому
// «окно загруженной модели» неоткуда взять. Смотрим только кэш метрик (чтение,
// без выбора бэкенда и без побочных эффектов вроде синхронной загрузки): если
// модель нигде не загружена — вердикта нет, пусть решает preflight.
//
// ПОЧЕМУ МАКСИМУМ, А НЕ STICKY-КОПИЯ. Живой факт (стенд 146307dd): копий две,
// и метрики одной из них устарели (мёртвый донор). Отказ, посчитанный по
// меньшей копии, запретил бы запрос, который другая копия обслуживает; отказ —
// это действие, которое нельзя отменить, поэтому он опирается на ЛУЧШЕЕ из
// известных окон, а не на случайно выбранное. Какой бэкенд дал числа, видно в
// логе контекстного решения (logGrowthVerdictR301).
func (p *Proxy) backendWithLoadedModelR301(model string) string {
	if p == nil || model == "" {
		return ""
	}
	p.mu.RLock()
	ids := make([]string, 0, len(p.backends))
	for id := range p.backends {
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	best, bestWindow := "", 0
	for _, id := range ids {
		if w := p.loadedPerSeqNCtx(id, model); w > bestWindow {
			best, bestWindow = id, w
		}
	}
	return best
}
