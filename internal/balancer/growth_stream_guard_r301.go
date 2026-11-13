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
	// RID — request_id хода (R476): без него решение о росте нельзя связать со
	// строкой equest trace и сводкой воркера. Пусто — вызов без HTTP-запроса.
	RID string
	// Defer — отказ стандартным телом «отложено» до любой сети и любого слота.
	Defer bool
	// Served — обслужить с подменённым телом (Body): окно растить не нужно.
	Served bool
	// Body — тело запроса с num_ctx, приведённым к загруженному окну.
	Body []byte
	// Grow — R385: нужно и МОЖНО поднять модель под большее окно. Стриминговый
	// запрос больше не «довольствуется» загруженным окном, если модель можно
	// перезагрузить: это требование оператора (см. stream_growth_r385.go).
	Grow bool

	Estimated int // оценка промпта
	Loaded    int // окно ОДНОГО слота загруженной модели
	Needed    int // сколько нужно: max(промпт + бюджет вывода, запрошенное окно)
	// Target — R493-12n: окно НА КЛИЕНТА, до которого надо поднять модель.
	// Заполняется единым решением (LB_WINDOW_UNIFIED=1/2); 0 — исторический путь,
	// и тогда цель считает streamGrowthPlanR385. Одно поле вместо второго расчёта:
	// «разрешено» и «до какого окна» не могут разойтись (R493-12h).
	Target        int
	Active        int // сколько стримов идёт сейчас
	ReloadPending bool
	Streaming     bool
	// RetryAfterSec — срок повтора, НАЗВАННЫЙ в тексте причины (R447). Writer
	// тела обязан писать это же значение: иначе клиент читает «retry in 60
	// seconds» рядом с `retryAfterSec: 30`.
	RetryAfterSec int
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
//
// R493-12n (этап 3 «два мозга»): при LB_WINDOW_UNIFIED=1 правила (R288/R447/
// R466/R385) считает ОДНА функция decideWindowR493, а эта — только переводит её
// вердикт в транспортное действие. Исторический расчёт остался ниже
// (growthVerdictLegacyR301) и работает при флаге 0 — это и есть путь отката.
func (p *Proxy) growthVerdictR301(rid, backendID, model string, body []byte, path string, streaming bool) growthVerdictR301 {
	mode := windowUnifiedModeR493()
	if mode == windowUnifiedOffR493 {
		return p.growthVerdictLegacyR301(rid, backendID, model, body, path, streaming)
	}
	unified, ok := p.growthVerdictUnifiedR493(rid, backendID, model, body, path, streaming)
	if !ok {
		return p.growthVerdictLegacyR301(rid, backendID, model, body, path, streaming)
	}
	if mode == windowUnifiedOnR493 {
		return unified
	}
	// Режим «тень»: считаем оба, расхождение пишем в лог, поведение — историческое.
	legacy := p.growthVerdictLegacyR301(rid, backendID, model, body, path, streaming)
	if windowVerdictKindLegacyR493(legacy) != windowVerdictKindLegacyR493(unified) {
		logWindowDivergenceR493("growth-verdict", backendID, model, path,
			windowVerdictKindLegacyR493(legacy), mustWindowDecisionR493(unified))
	}
	return legacy
}

// mustWindowDecisionR493 — восстановить решение из вердикта для строки лога
// расхождения: у «тени» нет под рукой исходного windowDecisionR493.
func mustWindowDecisionR493(v growthVerdictR301) windowDecisionR493 {
	return windowDecisionR493{
		Kind:          windowVerdictKindLegacyR493(v),
		Needed:        v.Needed,
		Estimated:     v.Estimated,
		Loaded:        v.Loaded,
		Target:        v.Needed,
		RetryAfterSec: v.RetryAfterSec,
	}
}

// growthVerdictUnifiedR493 — вердикт из единого решения (R493-12n).
func (p *Proxy) growthVerdictUnifiedR493(rid, backendID, model string, body []byte, path string, streaming bool) (growthVerdictR301, bool) {
	d, ok := p.windowDecisionForR493(backendID, model, body, path, streaming)
	if !ok {
		return growthVerdictR301{}, false
	}
	v, _ := windowVerdictFromDecisionR493(rid, d, streaming)
	// Тело с приведённым num_ctx нужно только для «обслужить» — и только когда
	// клиент попросил больше загруженного (R83).
	if v.Served {
		if patched := patchNumCtxInBody(body, d.Loaded); patched != nil {
			v.Body = patched
		} else {
			// Тело не разобралось — «обслужить» без подмены отправило бы воркеру
			// чужой num_ctx, и он отверг бы запрос кадром ошибки.
			v.Served = false
		}
	}
	logWindowDecisionR493("growth-verdict", backendID, model, path, d)
	return v, true
}

// windowVerdictFromDecisionR493 — вердикт решения → транспортное действие.
//
// ПОЧЕМУ ОТДЕЛЬНАЯ ФУНКЦИЯ, А НЕ ВЕТКИ НА МЕСТЕ. Отображение «вид → действие»
// должно быть одно: у generic-пути и у транспортного preflight оно одинаковое,
// иначе один и тот же вердикт снова даст 200 на одной поверхности и 503 на
// другой. Второе возвращаемое значение — «вердикт вообще что-то предписывает».
//
// ЧЕМ ОТЛИЧАЮТСЯ ПУТИ (и только этим). Не-стриминговый запрос умеет ждать
// перезагрузку: ему «рост невозможен сейчас» говорит «повтори» (Defer) или
// «решение за preflight-движком» (числа без действия). Стриминговый перезагрузку
// под собой не делает (R271: это оборвало бы стрим), поэтому «рост невозможен» для
// него означает «обслужи загруженным окном, если промпт влезает» — ровно то, что
// делал R301 до единого решения.
func windowVerdictFromDecisionR493(rid string, d windowDecisionR493, streaming bool) (growthVerdictR301, bool) {
	v := growthVerdictR301{
		RID:           rid,
		Streaming:     streaming,
		Estimated:     d.Estimated,
		Loaded:        d.Loaded,
		Needed:        d.Needed,
		Target:        d.Target,
		Active:        d.Active,
		ReloadPending: d.ReloadPending,
		RetryAfterSec: d.RetryAfterSec,
	}
	// serveOrDeferR493 — общий для «роста не будет» исход на стриминге: промпт
	// влезает → обслужить с подменой num_ctx (R83), не влезает → отказ с числами.
	serveOrDeferR493 := func() (growthVerdictR301, bool) {
		if d.PromptFits {
			v.Served = d.Requested > d.Loaded
			return v, v.Served
		}
		v.Defer = true
		v.RetryAfterSec = growthDeferRetryAfterSecR447
		return v, true
	}

	switch d.Kind {
	case windowNoopR493:
		return v, false
	case windowServeR493:
		v.Served = true
		return v, true
	case windowDeferR493:
		if streaming {
			return serveOrDeferR493()
		}
		v.Defer = true
		return v, true
	case windowGrowR493:
		// Рост исполняет ТОЛЬКО путь, который это умеет: не-стриминговые запросы
		// отдают решение preflight-движку (он умеет выбрать свободный узел вместо
		// перезагрузки — R493 availability-first), поэтому им вердикт сообщает
		// числа и цель, но не предписывает 503.
		v.Grow = streaming
		return v, true
	default: // windowRefuseR493
		if streaming {
			return serveOrDeferR493()
		}
		// Не-стриминговому пути отказ по потолку/роли сообщает сам preflight-движок
		// (у него 413 с числами и предложением). Здесь вердикт только называет
		// числа — действие остаётся прежним.
		return v, false
	}
}

// windowVerdictKindLegacyR493 — вид исторического вердикта строкой: нужен только
// для сравнения в режиме «тень» и в логе расхождений.
func windowVerdictKindLegacyR493(v growthVerdictR301) windowKindR493 {
	switch {
	case v.Grow:
		return windowGrowR493
	case v.Defer:
		return windowDeferR493
	case v.Served:
		return windowServeR493
	}
	return windowNoopR493
}

// growthVerdictLegacyR301 — историческое решение (LB_WINDOW_UNIFIED=0).
//
// Оставлено как путь отката флага и как эталон для «тени»: пока единый расчёт не
// сходится с ним на реальном трафике, включать его нельзя.
func (p *Proxy) growthVerdictLegacyR301(rid, backendID, model string, body []byte, path string, streaming bool) growthVerdictR301 {
	v := growthVerdictR301{Streaming: streaming, RID: rid}
	if p == nil || backendID == "" || len(body) == 0 {
		return v
	}
	meta := p.extractRequestMeta(body, path)
	if meta == nil {
		return v
	}
	estimated, loaded, needed := p.contextGrowthNumbers(backendID, model, body, path)

	// R466 (2026-11-06): ЗАВЫШЕННАЯ ОЦЕНКА НЕ ОТМЕНЯЕТ РОСТ.
	//
	// needed = оценка промпта + бюджет вывода, а оценка — эвристика по символам
	// (без токенизатора), которая на живых стендах завышает в 2–3 раза: «нужно
	// 151743 при занятых 63.6k». Дальше это число сравнивалось с физическим
	// потолком и рост отменялся — при том что клиент не просил ни окна, ни
	// такого бюджета, и реальный промпт в потолок мог влезть.
	//
	// Правило: если окно выведено нами (клиент num_ctx не присылал), урезаем его
	// до потолка на клиента и пробуем рост; явно запрошенное клиентом окно
	// сверх потолка по-прежнему честно отказывается (это его требование, а не
	// наша догадка).
	if meta.RequestedNCtxOverride <= 0 && needed > loaded {
		ceil := p.physicalNumCtxCeiling(model, backendID)
		if capped, cut := capDerivedWindowR466(needed, ceil, p.loadedSlotsNCtx(backendID, model)); cut && capped > loaded {
			logger.Get().Warnw("R466: нужное окно урезано до потолка (оценка промпта завышена)",
				"request_id", v.RID,
				"backend", backendID, "model", model, "path", path,
				"estimated_prompt_tokens", estimated,
				"needed_tokens", needed, "capped_tokens", capped,
				"loaded_window_tokens", loaded, "physical_ceiling", ceil)
			needed = capped
		}
	}
	v.Estimated, v.Loaded, v.Needed = estimated, loaded, needed
	if loaded <= 0 || needed <= 0 || needed <= loaded {
		// Окно известно и его хватает (или неизвестно — числами не спекулируем).
		return v
	}

	// R447 (2026-11-06): «окна мало» ≠ «окно надо растить».
	//
	// needed = оценка промпта + бюджет вывода, и бюджет — не требование модели:
	// воркер урезает num_predict по свободному месту сам. Поэтому короткий
	// запрос без явно запрошенного окна обслуживается загруженным окном как
	// есть: ни роста (перезагрузка модели = медленно и с обрывом чужого
	// процесса), ни отказа (503 «retry in 60 s» на вопрос в 3 токена).
	//
	// Раньше здесь падали в рост/отказ ВСЕГДА, когда needed > loaded. Живой
	// дефект: «model "test-model" needs 4099 tokens of context; the loaded window
	// is 2048 — the balancer has started reloading the model… retry in 60
	// seconds» на промпт «Привет!». Тот же вход давал 200 или 503 в зависимости
	// от свободной VRAM (рост разрешён только пока физика позволяет) — это
	// классический флак, и решать его подгонкой теста было бы подлогом.
	//
	// Растим окно только по требованию самого запроса: промпт не влезает ИЛИ
	// клиент явно попросил окно больше загруженного (R385).
	if needGrowth, needWhy := growthNeedR447(estimated, loaded, meta.RequestedNCtxOverride); !needGrowth {
		logger.Get().Debugw("R447: рост окна не нужен — обслуживаем загруженным окном",
			"backend", backendID, "model", model, "path", path, "streaming", streaming,
			"estimated_prompt_tokens", estimated, "loaded_window_tokens", loaded,
			"required_tokens", needed, "reason", needWhy)
		return v
	}

	v.Active, v.ReloadPending = growthBlockersR288(p, backendID, model)

	if !streaming {
		// Не-стриминговый путь: рост возможен, пока его ничто не блокирует
		// (прежнее правило R299 — рост пробует preflight).
		if v.Active > 0 || v.ReloadPending {
			v.Defer = true
			v.RetryAfterSec = growthDeferRetryAfterSecR447
		}
		return v
	}

	// R385 (2026-11-01): СНАЧАЛА пробуем ПОДНЯТЬ модель под нужное окно.
	//
	// Прежнее правило «стриминг окно не растит» отменено: оператор потребовал,
	// чтобы запрос с большим окном перезагружал модель, если она это
	// поддерживает и нет ограничений в железе/параллельности. Здесь решается
	// только «можно ли» (нет активных стримов, не primary, не донор, физика
	// позволяет) — саму перезагрузку запускает вызывающий (triggerStreamGrowthR385).
	//
	// Гейт по занятости: рост разрешён только когда ему ничто не мешает —
	// идущий стрим или уже начатая перезагрузка по-прежнему означают «ждать»,
	// а не «перезагрузить поверх живого ответа» (R271).
	if v.Active == 0 && !v.ReloadPending {
		if ok, why := p.streamGrowthCheckR385(backendID, model, v.Needed); ok {
			v.Grow = true
			// R447: текст причины называет срок повтора — writer тела обязан
			// взять ровно его (раньше в теле стояло 30 при тексте «60 seconds»).
			v.RetryAfterSec = streamGrowthRetryAfterSecR385
			return v
		} else if why != "" {
			// R472 (2026-11-06): причина отказа роста — WARN, а не Debug.
			//
			// ЖИВОЙ ДЕФЕКТ: клиент получал 503 «a streaming request does not reload
			// the model», хотя рост на стриминге ВКЛЮЧЁН и ничто его не блокировало.
			// Настоящая причина отказа (WindowFeasibility/потолок/выключатель) была
			// видна только на Debug, поэтому по логу нельзя было понять, что именно
			// решает. Оператор видит отказ — лог обязан назвать его причину.
			logger.Get().Warnw("R385: окно на стриминге не растим", "backend", backendID,
				"model", model, "needed_tokens", v.Needed, "loaded_tokens", v.Loaded, "reason", why)
		}
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
	v.RetryAfterSec = growthDeferRetryAfterSecR447
	return v
}

// growthDeferRetryAfterSecR447 — срок повтора для отказа по окну. Один и тот же
// в тексте причины и в теле (`retryAfterSec`); раньше транспорт писал свои 30,
// а текст для роста окна говорил «retry in 60 seconds».
const growthDeferRetryAfterSecR447 = 30

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
		"request_id", v.RID,
		"where", where,
		"backend", backendID, "model", model, "path", path,
		"streaming", v.Streaming,
		"defer", v.Defer, "grow", v.Grow, "served_with_loaded_window", v.Served,
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
