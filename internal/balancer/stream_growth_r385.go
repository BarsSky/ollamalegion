// stream_growth_r385.go — R385 (2026-11-01): СТРИМИНГОВЫЙ запрос с большим
// окном ПОДНИМАЕТ модель, если это возможно. Прежнее правило «стриминг окно не
// растит» отменено как противоречащее требованию оператора.
//
// ЖИВАЯ ЖАЛОБА. Клиент пришёл с `options.num_ctx=131072`, модель загружена с
// 32768, VRAM/RAM хватает, доноры свободны — а балансер обслужил запрос с
// урезанным окном (или отказал «поднимите окно в карточке модели»). Оператор:
// «правило — если запрос приходит с большим окном, модель надо перезагрузить
// под новое окно, если она его поддерживает и нет ограничений в железе или
// параллельности».
//
// ЧЕМ ЭТО БЫЛО. R301 сознательно запрещал рост на стриминге: перезагрузка под
// живым стримом рвёт стрим (R271), поэтому для streaming оставались только
// «обслужить с загруженным окном» (Served) и «отказать с числами» (Defer).
// Тесты R301 это и закрепляли — то есть правило в тестах было, а
// операторского правила не было.
//
// ЧТО ТЕПЕРЬ. Рост разрешён, но только когда он НИКОМУ не вредит:
//   - нет активных стримов и нет уже идущей перезагрузки (growthBlockersR288);
//   - бэкенд не донор раскладки (своих копий он не держит, R304);
//   - окно физически возможно: GGUF-потолок, заявленное окно профиля и
//     ContextLengthMax (WindowFeasibility), плюс потолок авто-роста.
//
// R401 (2026-11-05): проверка primary УБРАНА. Раньше рост запрещался для
// primary-модели («окно зафиксировано администратором»), но флаг отключён как
// противоречащий политике: если окно физически возможно и никто не работает,
// клиентский запрос вправе его занять.
//
// Если хоть одно не выполнено — поведение прежнее: обслужить с загруженным
// окном, если промпт влезает, иначе отказ с числами и причиной.
//
// СЕМАНТИКА ОТВЕТА. Перезагрузка асинхронная (LB_NCTX_RELOAD_ASYNC), поэтому
// клиент получает 503 с Retry-After и числами «нужно/загружено», а следующий
// его запрос обслуживается уже с новым окном. Тот же контракт, что у
// не-стримингового пути: рост окна — это перезагрузка, а не мгновенное действие.
package balancer

import (
	"fmt"
	"os"
	"strings"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
)

// streamGrowthEnabledR385 — выключатель роста окна на стриминге (по умолчанию
// включён: именно это и просил оператор).
func streamGrowthEnabledR385() bool {
	// R388: значение живёт в реестре горячих настроек (панель WebUI):
	// переопределение → env → умолчание. Так выключатель виден оператору, а не
	// только в .env.
	if v, ok := settings.HotInt("LB_NCTX_GROW_FOR_STREAM"); ok {
		return v > 0
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LB_NCTX_GROW_FOR_STREAM"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// streamGrowthPlanR493 — решение о росте окна на стриминге: цель, растим ли, и
// ПОЧЕМУ нет.
//
// ЗАЧЕМ ТИП, А НЕ ТРИ ВОЗВРАЩАЕМЫХ ЗНАЧЕНИЯ. Есть два разных «роста не будет»:
//
//   - отказ (Refuse): рост нужен, но невозможен (выключатель, донор раскладки,
//     потолок). Клиент получит 503 — причину обязан назвать WARN в логе (R472).
//   - нечего растить (Note): нужное окно уже покрыто выровненным загруженным
//     (149809 при 150016). Это НЕ отказ: запрос обслуживается загруженным окном,
//     а WARN «окно не растим» на ровном месте только пугал бы оператора.
//
// Смешивать их в одной строке нельзя: guard делает WARN по непустой причине, и
// пустой строкой «нет отказа» он как раз и отличается.
type streamGrowthPlanR493 struct {
	Note   string // пояснение для лога, когда роста просто не нужно
	Refuse string // причина ОТКАЗА, когда рост нужен, но невозможен
	Target int    // окно НА КЛИЕНТА, до которого растим
	Grow   bool
}

// streamGrowthCheckR385 — можно ли поднять модель под нужное окно ради
// стримингового запроса. Возвращает (можно, причина_отказа).
//
// R493-12h: проверка и запуск обязаны считать ЦЕЛЬ ОДИНАКОВО — иначе «разрешено»
// и «до какого окна» разойдутся (именно это дало перезагрузки вниз на 94–207
// токенов из архива 09:53). Обе стороны зовут growthTargetR493.
func (p *Proxy) streamGrowthCheckR385(backendID, model string, needed int) (bool, string) {
	plan := p.streamGrowthPlanR385(backendID, model, needed)
	return plan.Grow, plan.Refuse
}

// streamGrowthPlanR385 — цель перезагрузки и решение «растим ли».
//
// ПОЧЕМУ ОТДЕЛЬНАЯ ФУНКЦИЯ. Решение «можно» и число «до какого окна» считаются
// ОДНИМ И ТЕМ ЖЕ кодом: иначе проверка разрешала рост по нужному числу, а
// перезагрузка уходила на другое (в архиве — цель 149809 при загруженных 150016,
// то есть перезагрузка ВНИЗ).
func (p *Proxy) streamGrowthPlanR385(backendID, model string, needed int) streamGrowthPlanR493 {
	refuse := func(why string) streamGrowthPlanR493 {
		return streamGrowthPlanR493{Refuse: why}
	}
	if p == nil || model == "" {
		return refuse("модель не определена")
	}
	if !streamGrowthEnabledR385() {
		return refuse("рост окна на стриминге выключен (LB_NCTX_GROW_FOR_STREAM=0)")
	}
	if needed <= 0 {
		return refuse("нужное окно неизвестно")
	}
	if backendID == "" {
		return refuse("бэкенд не определён")
	}
	if p.cannotHostModelR304(backendID) {
		return refuse("бэкенд — донор раскладки, своих копий он не держит")
	}
	// Физика и операторский потолок авто-роста.
	//
	// R466 (2026-11-06): потолок — СУММАРНЫЙ (так его считает
	// physicalNumCtxCeiling и так его делит воркер между слотами), а needed
	// приходит НА КЛИЕНТА. Сравнивать их напрямую значило бы разрешать рост,
	// который воркер не сможет выполнить (или урезать окно слота вдвое).
	//
	// R472 (2026-11-06): ЗДЕСЬ БОЛЬШЕ НЕ СПРАШИВАЕМ WindowFeasibility.
	//
	// ЖИВОЙ ДЕФЕКТ (Cline → балансер, gemma-4-E4B). Клиент просил num_ctx=32768,
	// промпт требовал ~40.7k (наша оценка), загруженное окно было 16384. Рост
	// отказывался с причиной
	//   «окно 40697 физически невозможно: … (потолок 32768)»,
	// хотя операторский потолок авто-роста — 65536, а профиль модели заявляет
	// contextLengthMax=131072. «Потолок 32768» брался из WindowFeasibility, а там
	// участвует `declaredWindowTotal` — окно, ЗАЯВЛЕННОЕ В ПРОФИЛЕ ДЛЯ ЗАГРУЗКИ
	// (contextLength=32768). Это ХИНТ загрузки, а не потолок обслуживания: тот же
	// класс, что R267/R255 («два мнения об одном потолке»).
	//
	// Правильный потолок для клиентского роста — physicalNumCtxCeiling:
	// min(GGUF, contextLengthMax профиля, операторский LB_NCTX_RELOAD_MAX_N_CTX).
	// Для ручной перезагрузки (R297) WindowFeasibility остаётся как был: там окно
	// задаёт оператор, и его же заявленное значение разумно считать пределом.
	ceiling := p.physicalNumCtxCeiling(model, backendID)
	loaded := p.loadedPerSeqNCtx(backendID, model)
	target, grow := growthTargetR493(needed, loaded, ceiling)
	if !grow {
		if ceiling > 0 && needed > ceiling {
			return refuse(fmt.Sprintf("нужно %d, потолок авто-роста %d", needed, ceiling))
		}
		// Не отказ: выровненное нужное окно уже внутри загруженного. Именно этот
		// случай в архиве 09:53 и перезагружал 27B ВНИЗ на 94–207 токенов.
		return streamGrowthPlanR493{Note: fmt.Sprintf(
			"окно уже покрывает запрос: нужно %d, выровненное %d, загружено %d",
			needed, cppbackend.PadContext(needed), loaded)}
	}
	if ceiling <= 0 {
		// Потолок неизвестен — не отказываем по догадке: пусть решает загрузка
		// (воркер вернёт честную ошибку, если окно не влезет).
		if ok, _, why := p.WindowFeasibility(backendID, model, perClientToTotal(target, p.loadedSlotsNCtx(backendID, model))); !ok && why != "" {
			logger.Get().Warnw("R472: потолок модели неизвестен, но WindowFeasibility отказал — всё равно пробуем рост",
				"backend", backendID, "model", model, "target", target, "why", why)
		}
	}
	return streamGrowthPlanR493{Target: target, Grow: true}
}

// triggerStreamGrowthR385 — запустить перезагрузку модели под нужное окно.
//
// targetPerClient — окно НА КЛИЕНТА из решения (R493-12n). 0 означает
// исторический путь: цель считается здесь через streamGrowthPlanR385 (флаг
// LB_WINDOW_UNIFIED=0). Два расчёта на один запрос не нужны: при едином решении
// цель уже посчитана той же функцией, которая сказала «растим».
func (p *Proxy) triggerStreamGrowthR385(backendID, model string, needed, targetPerClient int) {
	if p == nil || model == "" || needed <= 0 {
		return
	}
	backend := p.backendByIDR383(backendID)
	if backend == nil {
		logger.Get().Warnw("R385: рост окна не запущен — запись бэкенда не найдена",
			"backend", backendID, "model", model, "needed_tokens", needed)
		return
	}
	// Адрес воркера обязателен: перезагрузка — это HTTP-вызов ему. Без проверки
	// «неполная» запись бэкенда (тест/полу-зарегистрированный узел) роняет
	// процесс паникой в http.Client, а не отвечает клиенту отказом.
	if strings.TrimSpace(backend.Host) == "" || backend.CppWorkerPort <= 0 {
		logger.Get().Warnw("R385: рост окна не запущен — у бэкенда нет адреса воркера",
			"backend", backendID, "model", model, "host", backend.Host,
			"port", backend.CppWorkerPort, "needed_tokens", needed)
		return
	}
	// R493-12h: цель берём ТОЙ ЖЕ функцией, что и решение о росте. Раньше здесь
	// уходило «нужное» число как есть: 149809 при загруженных 150016 — воркер
	// видел «parameters changed» и перезагружал модель ВНИЗ (207 токенов), а
	// выровненное 150016 никогда не совпадало с целью.
	if targetPerClient <= 0 {
		plan := p.streamGrowthPlanR385(backendID, model, needed)
		if !plan.Grow {
			logger.Get().Infow("R385: рост окна не запускается",
				"backend", backendID, "model", model, "needed_tokens", needed,
				"refused", plan.Refuse != "", "reason", plan.Refuse, "note", plan.Note)
			return
		}
		targetPerClient = plan.Target
	}
	logger.Get().Infow("R385: стриминговый запрос требует большего окна — запускаем перезагрузку модели",
		"backend", backendID, "model", model, "needed_tokens", needed, "target_per_client", targetPerClient)
	// R466 (2026-11-06): executeAsyncReload ждёт СУММАРНОЕ окно (см.
	// asyncReloadLoadBody — contextSize получает воркер и делит его между
	// слотами), а план даёт окно НА КЛИЕНТА. Без пересчёта модель с двумя слотами
	// поднималась бы до окна слота вдвое меньше нужного.
	target := perClientToTotal(targetPerClient, p.loadedSlotsNCtx(backendID, model))
	if slots := p.loadedSlotsNCtx(backendID, model); slots > 1 {
		logger.Get().Infow("R385: окно пересчитано в суммарное",
			"backend", backendID, "model", model,
			"per_client", targetPerClient, "slots", slots, "total", target)
	}
	p.executeAsyncReload(backendID, model, target, backend)
}

// growthStartsMessageR385 — текст ответа клиенту: что происходит и когда
// повторить. Числа обязательны (R291/R299): «retry later» без них оператор не
// может отличить рост от вечного отказа.
func growthStartsMessageR385(model string, v growthVerdictR301) string {
	return fmt.Sprintf("model %q needs %d tokens of context; the loaded window is %d — "+
		"the balancer has started reloading the model to the requested window, retry in %d seconds",
		model, v.Needed, v.Loaded, streamGrowthRetryAfterSecR385)
}

// streamGrowthRetryAfterSecR385 — сколько подождать до повтора. Загрузка
// 27B-модели на живой машине занимает десятки секунд; 60 — верхняя граница
// обычного случая, повтор раньше просто снова получит 503 с числом.
const streamGrowthRetryAfterSecR385 = 60
