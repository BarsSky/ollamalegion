// preflight_nctx_decide.go — R635 (2026-11-19): решение о преflight: growthWorthReload, DecidePreflight и мелкие преобразования окна.
//
// ЗАЧЕМ ВЫНЕСЕНО. `preflight_nctx.go` держал 1601 строку: разбор запроса и
// метаданных, решение о preflight, отказы и сам RunPreflight. Здесь — один из слоёв;
// поведение не менялось.
package balancer

import (
	"net/http"
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"time"
)

// growthWorthReload — R83-fix (2026-09-30): стоит ли вообще перезагружать
// модель ради большего окна.
//
// ЗАЧЕМ. Живой дефект на локальном стенде (gemma-4, RTX 3070 8 GB, клиент Cline
// с prompt ≈16k токенов и max_tokens=8192):
//
//  1. preflight сравнивает ОЦЕНКУ промпта (44 230 токенов, tokencount —
//     консервативная верхняя граница; реально в промпте 16 210) с окном слота
//     и решает растить окно;
//  2. cppworker выгружает модель и грузит заново;
//  3. адаптивная стратегия возвращает nCtx, РАВНЫЙ текущему окну, — и модель
//     всё равно перезагружается: 2–3 минуты простоя, 503/ожидание у клиента и
//     ни одного выигрыша (проверено: `new_ctx` в логе cppworker совпадал со
//     старым, менялось только число GPU-слоёв).
//
// Поэтому здесь проверяется единственное, что действительно делает reload
// бессмысленным: стратегия не может дать окно больше текущего.
//
// ЧЕГО ЗДЕСЬ СОЗНАТЕЛЬНО НЕТ. Проверки «стратегия предлагает меньше
// GPU-слоёв». Первая версия фикса отклоняла reload при stage=cpu_only
// (gpuLayers=0) — на живом стенде это оказалось неверно: `gpuLayers: 0` в
// reload-запросе cppworker трактует как «взять из профиля» (sync_profile.go:
// `else if *gpuLayers == 0 { *gpuLayers = profile.NumGPULayers }` → -1 = все
// слои → auto-раскладка по VRAM). Подтверждено логом: при стратегии cpu_only
// reload всё равно дал `new_gpu_layers:-1` → `gpuLayers=22` и
// `offloaded 22/43 layers` — то есть раскладка стала ЛУЧШЕ, а отклонение роста
// окна только лишило бы клиента нужного контекста. Явное понижение (например,
// 12 против 19) по-прежнему отсекается — но в DoReload, а не здесь
// (clampStrategyToCurrentLayout).
//
// Возвращает (worth, achievablePerClient, why):
//   - worth=true  → перезагрузка осмысленна (achievablePerClient не определён);
//   - worth=false → achievablePerClient — окно НА КЛИЕНТА, которое cppworker
//     реально сможет дать (0 = неизвестно), why — причина словами.
//
// worth=true при любой неопределённости (нет адреса, нет стратегии) — прежнее
// поведение сохраняется.
func (c *NCtxReloadCoordinator) growthWorthReload(
	backendAddr, backendID, modelName string,
	targetTotalNCtx, currentTotalNCtx, currentPerSeqNCtx, currentGPULayers, slots int,
) (bool, int, string) {
	if backendAddr == "" || modelName == "" {
		return true, 0, ""
	}
	hints := c.ReloadHintsFor(backendID, modelName)
	httpClient := &http.Client{Timeout: 3 * time.Second}
	strategy := queryAdaptiveStrategy(backendAddr, modelName, targetTotalNCtx, hints.KVCacheType, httpClient)
	if strategy == nil {
		// Стратегии нет (cppworker не ответил/404) — не берём на себя
		// решение: прежнее поведение (reload).
		return true, 0, ""
	}
	if strategy.Stage == "cpu_only" {
		// Диагностика для оператора: сама по себе не отменяет рост окна
		// (см. комментарий выше), но объясняет, почему раскладка может
		// измениться.
		logger.Get().Warnw(i18n.T("balancer.preflight_nctx.strategy_cpu_only_warn"),
			"backend_id", backendID, "model", modelName,
			"strategy_gpu_layers", strategy.GPULayers,
			"current_gpu_layers", currentGPULayers,
			"strategy_n_ctx", strategy.NCtx)
	}

	// Окно не увеличится. Проверяем только когда reload затевался РАДИ роста:
	// если target <= current, это не growth-перезагрузка (другая причина), и
	// решать за неё мы не имеем права.
	if targetTotalNCtx > currentTotalNCtx && strategy.NCtx > 0 && currentTotalNCtx > 0 &&
		strategy.NCtx <= currentTotalNCtx {
		// strategy.NCtx — СУММАРНОЕ окно: клиенту достанется его доля по слотам.
		achievablePerClient := strategy.NCtx
		if slots > 1 {
			achievablePerClient = strategy.NCtx / slots
		}
		return false, achievablePerClient, i18n.T(
			"balancer.preflight_nctx.growth_not_worth_reason",
			strategy.NCtx, currentTotalNCtx, currentPerSeqNCtx)
	}

	return true, 0, ""
}

// DecidePreflight решает, что делать с запросом ДО отправки в cppworker.
//
// R83-политика (2026-10-01): решение принимается ТОЛЬКО по тому, что клиент
// ЯВНО указал, — а не по нашей оценке длины промпта.
//
// Почему так. Раньше окно «росло» по оценке промпта (tokencount — консервативная
// верхняя граница, завышающая в 2–4 раза): обычный запрос Cline с 16k-промптом
// давал оценку 44 230, окно слота 32 768 объявлялось малым, и модель уходила в
// перезагрузку на 2–3 минуты — иногда с тем же самым окном на выходе. Загрузка
// «по оценке» — это и есть принудительный элемент, который убирается: если
// клиент не сказал, какое окно ему нужно, мы не гадаем.
//
// Порядок решений:
//
//  1. Расхождение СЕМАНТИЧЕСКИХ параметров (reasoning/thinking) → отказ 409 с
//     перечислением отличий. Тихая подмена поведения недопустима: клиент просит
//     «думать», а модель загружена без reasoning — это другой ответ.
//  2. Клиент не указал num_ctx → NoOp. Никаких перезагрузок «на всякий случай»;
//     если история действительно не влезает, cppworker вернёт свою ошибку, а
//     балансер передаст её объяснение клиенту.
//  3. Модель ещё не загружена → NoOp: cppworker загрузит её С ОКНОМ КЛИЕНТА
//     (ensureModelLoaded принимает num_ctx запроса), а не с дефолтом профиля.
//  4. Загруженное окно на клиента >= запрошенного → NoOp. Загружено больше —
//     перезагружать под клиента не нужно и уведомлять не о чем.
//  5. Загруженное окно меньше → перезагрузка модели под окно клиента (остальные
//     параметры совпадают, поэтому это безопасно). Если запрошенное окно не
//     влезает в физический потолок (GGUF/операторский cap) — отказ с понятным
//     текстом: «столько не поместится, вот почему и что делать».
func DecidePreflight(meta *RequestMeta, state *NCtxBackendState, cfg NCtxReloadConfig) *PreflightResult {
	if meta == nil || state == nil {
		return &PreflightResult{Decision: PreflightNoOp}
	}
	logger.Get().Infow("preflight: DecidePreflight inputs",
		"request_id", meta.RequestID,
		"backend_id", state.BackendID,
		"model", meta.ModelName,
		"body_n_ctx", meta.RequestedNCtxOverride,
		"loaded_n_ctx", state.CurrentNCtx,
		"context_per_seq", state.CurrentContextPerSeq,
		"slots", state.effectiveSlots(),
		"model_max_n_ctx", state.ModelMaxContext,
		"max_vram_n_ctx", state.MaxVRAMNCtx,
		"estimated_prompt_tokens", meta.EstimatedPromptTokens,
		"n_predict", meta.RequestedNPredict,
		"has_tools", meta.HasTools,
		"requested_think", reqStr(meta.RequestedThink),
		"loaded_reasoning", reqStr(state.CurrentReasoningEnabled))

	// (1) Семантические параметры: клиент требует то, чего у загруженной модели
	// нет. Перезагрузкой это НЕ лечим (пользователь просил именно ошибку), и
	// молчать нельзя — ответ был бы другим по смыслу.
	if diffs := semanticParamDiffs(meta, state); len(diffs) > 0 {
		logger.Get().Warnw(i18n.T("balancer.preflight_nctx.param_mismatch_reject_warn"),
			"backend_id", state.BackendID,
			"model", meta.ModelName,
			"differences", len(diffs),
			"detail", diffs[0].Explain)
		return makeParamMismatchReject(state, meta, diffs)
	}

	reqWindow := meta.RequestedNCtxOverride

	// (2) Клиент не сообщил окно.
	//
	// R253 (2026-10-28): АГЕНТСКИЙ ХОД РАСТИТ ОКНО САМ.
	//
	// Раньше здесь был безусловный NoOp («клиент не сообщил окно — не гадаем»).
	// Cline num_ctx не присылает никогда, поэтому окно не росло, а cppworker резал
	// вывод по фактическому промпту: prompt ≥ n_ctx → пустой ответ, иначе clamp до
	// n_ctx-prompt-64. Снаружи — «сначала быстро, а как упёрлось — срыв сессии».
	//
	// Теперь при известном размере промпта выводим необходимое окно (промпт +
	// бюджет вывода + slack) и прогоняем его через ТЕ ЖЕ проверки (3)-(5), что и
	// клиентское: физический потолок модели и AutoReloadMaxNCtx. Если выведенное
	// окно не больше загруженного на слот, ветка (4) всё равно даст NoOp — никаких
	// перезагрузок «на всякий случай».
	agentDerived := false
	if reqWindow <= 0 {
		derived := 0
		if nctxGrowForAgentsEnabled() {
			// R294 (2026-10-29): «пол» — загруженное окно слота. Раньше выведенное
			// окно могло оказаться МЕНЬШЕ загруженного (промпт короткий, бюджет 0),
			// и reload уходил в меньшую сторону: cppworker выгружал модель с 8192
			// и грузил 4165, обрывая чужой ход. Рост не может быть уменьшением.
			loaded := state.effectivePerSeqNCtx()

			// R451 (2026-11-06): РОСТ — ТОЛЬКО ПО ТРЕБОВАНИЮ ЗАПРОСА.
			//
			// ЖИВОЙ ДЕФЕКТ (Cline на 9B, второй ход). Клиент прислал
			// `max_tokens=15393`; формула «оценка промпта + бюджет клиента + slack»
			// дала 24677+15393+64 = 40134 при загруженных 32768 → unload+load на
			// 243 с посреди хода. При этом промпт в окно ВЛЕЗАЛ (24677 из 32768,
			// свободно ~8k под ответ), а бюджет клиента — это «сколько я готов
			// принять», а не «сколько модели нужно места»: воркер урезает
			// n_predict по свободному месту сам.
			//
			// Правило то же, что в R447 для стримингового вердикта: растим, если
			// не влезает САМ промпт, клиент явно попросил больше, или под ответ не
			// остаётся осмысленного места. Иначе — обслуживаем загруженным окном.
			//
			// В режиме LB_AGENT_WINDOW_BUDGET=client гейт не применяется: оператор
			// этим режимом возвращает ПОЛНОСТЬЮ прежнее поведение (и решение, и
			// таргет), иначе «откат» возвращал бы только половину правила.
			if loaded > 0 && agentWindowBudgetMode() != "client" {
				// R664 (2026-11-20) ШАГ B: ДАННЫЕ НЕ ВЛЕЗАЮТ + ЗАЯВКИ НЕТ — ОТКАЗ
				// С ЧИСЛАМИ, а не рост под НАШУ оценку промпта.
				//
				// ЖИВОЙ ДЕФЕКТ. Cline num_ctx не присылает никогда, поэтому «окно
				// выведем сами» превращалось в перезагрузку модели на каждый
				// длинный промпт: 13023 токена мимо окна 8192 → unload+load и
				// минуты ожидания, причём выведенное окно считалось по эвристике
				// (завышает в 2–3 раза, R466), то есть обслуживание не
				// гарантировалось даже после перезагрузки.
				//
				// Правило одно на все пути — growthActionForR664:
				//   growthNoneR664     → делать нечего (обслуживаем загруженным);
				//   growthRefuseR664   → 413 context_window_exhausted_no_growth_request;
				//   growthRequiredR664 → рост как раньше (запас под ответ меньше
				//                        минимума — R447 п.3: промпт влезает).
				//
				// «Запас под ответ меньше минимума» остаётся причиной РАСТИ, а не
				// отказа: такой запрос воркер обслуживает укороченным ответом, и
				// отказ отнял бы у клиента работающий ход (window_serve_as_is.go).
				action, why := growthActionForR664(meta.EstimatedPromptTokens, loaded, meta.RequestedNCtxOverride)
				switch action {
				case growthNoneR664:
					logger.Get().Infow("preflight: рост окна под агентский ход не нужен (R451)",
						"backend_id", state.BackendID,
						"model", meta.ModelName,
						"estimated_prompt_tokens", meta.EstimatedPromptTokens,
						"loaded_per_seq_n_ctx", loaded,
						"client_output_budget", agentOutputBudget(meta),
						"window_budget_mode", agentWindowBudgetMode(),
						"reason", why)
					return &PreflightResult{Decision: PreflightNoOp}
				case growthRefuseR664:
					// Модель НЕ перезагружаем (R294: окно не уменьшаем; чужой ход не
					// рвём). Клиент получает числа: промпт, загруженное окно, минимум
					// к заявке и доступный потолок.
					required := minDeclarableWindowR664(meta.EstimatedPromptTokens)
					logger.Get().Warnw(i18n.T("balancer.preflight_nctx.context_window_exhausted_warn"),
						"backend_id", state.BackendID,
						"model", meta.ModelName,
						"request_id", meta.RequestID,
						"prompt_tokens", meta.EstimatedPromptTokens,
						"loaded_window_tokens", loaded,
						"required_window_tokens", required,
						"available_context_tokens", derivedWindowCapR466(state.physicalCeiling(), state.effectiveSlots()),
						"reason", why)
					return makePromptExceedsWindowRejectR664(state, meta,
						meta.EstimatedPromptTokens, loaded, required)
				}
			}
			derived = agentRequiredWindowAtLeast(meta, loaded)

			// R466 (2026-11-06): ВЫВЕДЕННОЕ ОКНО НЕ ОТКАЗЫВАЕТ ПО ФИЗИКЕ.
			//
			// ЖИВОЙ ДЕФЕКТ (второй стенд, A10 ×2, Qwen3.8-27B, Cline). Клиент
			// показывал занятый контекст 63.6k, а балансер отвечал «требуется
			// 151743 токенов на контекстное окно» и окно НЕ поднимал. Нужное окно
			// выводится из ОЦЕНКИ промпта (эвристика по символам, без
			// токенизатора), а она завышает в 2-3 раза: 151743 здесь — это оценка
			// нашего же счётчика, а не требование клиента. Клиент num_ctx не
			// присылал вовсе.
			//
			// Поэтому: отказ 413 по физическому потолку остаётся только для окна,
			// которое ЯВНО запросил клиент (ветка ниже, reqWindow из тела). Нашу
			// догадку урезаем до потолка и грузим её: реальный промпт в потолок
			// может и влезть, а отказ по завышенной оценке гарантированно ломает
			// ход. Рост не может быть уменьшением (R294): если потолок ниже
			// загруженного окна, ветка (4) всё равно вернёт NoOp.
			if capped, cut := capDerivedWindowR466(derived, state.physicalCeiling(), state.effectiveSlots()); cut {
				logger.Get().Warnw("preflight: R466 выведенное окно урезано до потолка (оценка завышена)",
					"request_id", meta.RequestID,
					"backend_id", state.BackendID,
					"model", meta.ModelName,
					"estimated_prompt_tokens", meta.EstimatedPromptTokens,
					"derived_window_per_client", derived,
					"capped_window_per_client", capped,
					"physical_ceiling", state.physicalCeiling(),
					"slots", state.effectiveSlots())
				derived = capped
			}
		}
		if derived <= 0 {
			logger.Get().Infow(i18n.T("balancer.preflight_nctx.client_silent_no_reload_info"),
				"backend_id", state.BackendID,
				"model", meta.ModelName,
				"loaded_n_ctx", state.CurrentNCtx,
				"estimated_prompt_tokens", meta.EstimatedPromptTokens,
				"grow_for_agents", nctxGrowForAgentsEnabled())
			return &PreflightResult{Decision: PreflightNoOp}
		}
		reqWindow = derived
		agentDerived = true
		logger.Get().Infow("preflight: agent window derived from prompt estimate",
			"backend_id", state.BackendID,
			"model", meta.ModelName,
			"estimated_prompt_tokens", meta.EstimatedPromptTokens,
			"client_output_budget", agentOutputBudget(meta),
			"window_reserve_tokens", agentWindowReserveTokens(meta),
			"window_budget_mode", agentWindowBudgetMode(),
			"requested_by_client", meta.RequestedNCtxOverride,
			"derived_window_per_client", derived,
			"loaded_per_seq_n_ctx", state.effectivePerSeqNCtx())
	}

	// (3) Модель не загружена — грузим её окном клиента. Решение о загрузке
	// принимает cppworker (ensureModelLoaded получает num_ctx запроса), поэтому
	// здесь именно NoOp: «проксируй запрос как есть».
	if state.effectivePerSeqNCtx() <= 0 && state.CurrentNCtx <= 0 {
		logger.Get().Infow(i18n.T("balancer.preflight_nctx.model_not_loaded_load_with_client_window_info"),
			"backend_id", state.BackendID,
			"model", meta.ModelName,
			"requested_n_ctx", reqWindow)
		return &PreflightResult{Decision: PreflightNoOp}
	}

	// (4) Загруженное окно на клиента уже не меньше запрошенного → NoOp.
	perSeq := state.effectivePerSeqNCtx()
	if perSeq > 0 && reqWindow <= perSeq {
		logger.Get().Infow(i18n.T("balancer.preflight_nctx.loaded_window_covers_request_info"),
			"backend_id", state.BackendID,
			"model", meta.ModelName,
			"requested_n_ctx", reqWindow,
			"context_per_seq", perSeq,
			"loaded_n_ctx", state.CurrentNCtx)
		return &PreflightResult{Decision: PreflightNoOp}
	}
	if perSeq <= 0 && state.CurrentNCtx > 0 && reqWindow <= state.CurrentNCtx {
		// Окно слота неизвестно (старые метрики) — сравниваем с суммарным.
		return &PreflightResult{Decision: PreflightNoOp}
	}

	// (5) Нужен рост окна под клиента. Проверяем только физику: поместится ли
	// запрошенное окно вообще (GGUF-потолок модели и операторский cap). Если нет
	// — понятный отказ вместо перезагрузки «в никуда».
	slots := state.effectiveSlots()
	targetTotal := perClientToTotal(reqWindow, slots)
	if ceil := state.physicalCeiling(); ceil > 0 && targetTotal > ceil {
		logger.Get().Warnw(i18n.T("balancer.preflight_nctx.requested_window_above_physical_ceiling_warn"),
			"backend_id", state.BackendID,
			"model", meta.ModelName,
			"requested_per_client", reqWindow,
			"requested_total", targetTotal,
			"slots", slots,
			"physical_ceiling", ceil)
		tooBig := makeWindowTooBigReject(state, meta, reqWindow, targetTotal, ceil,
			i18n.T("balancer.preflight_nctx.model_supports_max_context", ceil))
		// R253: пометка нужна и на отказе — оператор должен видеть, что окно
		// выведено из промпта, а не запрошено клиентом.
		tooBig.AgentDerivedWindow = agentDerived
		return tooBig
	}
	// R323 (2026-10-30): потолок оператора — НЕ сырое env-значение.
	//
	// R255/R267 ввели одно правило: действующий потолок авто-роста =
	// max(env, заявленное в профиле модели окно), потому что решение оператора
	// (окно в карточке модели) не должен перебивать защитный env. Решатель
	// перезагрузки это правило получил, а preflight — нет: он сравнивал с СЫРЫМ
	// cfg.AutoReloadMaxNCtx и отдавал 413 «auto_reload_ceiling_reason», даже
	// когда профиль заявлял окно БОЛЬШЕ env (живая жалоба: модель 131072,
	// профиль 131072, а запрос на 65536 получал отказ по env=32768).
	// Класс дефекта — ровно R267 («правка одного слоя не лечила другой»),
	// поэтому здесь используется то же значение, что и в решателе:
	// meta.EffectiveCeiling, посчитанный Proxy.effectiveAutoReloadCeiling.
	// Если поле не заполнено (юнит-тесты без Proxy) — падаем на сырое env,
	// как раньше.
	operatorCeiling := cfg.AutoReloadMaxNCtx
	if meta != nil && meta.EffectiveCeiling > 0 {
		operatorCeiling = meta.EffectiveCeiling
	}
	if operatorCeiling > 0 && targetTotal > operatorCeiling {
		logger.Get().Warnw(i18n.T("balancer.preflight_nctx.requested_window_above_operator_ceiling_warn"),
			"backend_id", state.BackendID,
			"model", meta.ModelName,
			"requested_total", targetTotal,
			"auto_reload_max_n_ctx", operatorCeiling)
		tooBig := makeWindowTooBigReject(state, meta, reqWindow, targetTotal, operatorCeiling,
			i18n.T("balancer.preflight_nctx.auto_reload_ceiling_reason", operatorCeiling))
		tooBig.AgentDerivedWindow = agentDerived
		return tooBig
	}

	logger.Get().Infow(i18n.T("balancer.preflight_nctx.reload_for_client_window_info"),
		"backend_id", state.BackendID,
		"model", meta.ModelName,
		"requested_per_client", reqWindow,
		"requested_total", targetTotal,
		"context_per_seq", perSeq,
		"loaded_n_ctx", state.CurrentNCtx,
		"slots", slots)
	// TargetNCtx — окно НА КЛИЕНТА: RunPreflight сам пересчитает его в суммарное
	// (total = окно × слоты), чтобы не умножить дважды.
	return &PreflightResult{
		Decision:   PreflightReload,
		TargetNCtx: reqWindow,
		// R253: окно выведено из оценки промпта (клиент num_ctx не присылал) —
		// вызывающий код пишет это в лог отдельной строкой, чтобы оператор видел
		// причину перезагрузки, а не «модель вдруг перезагрузилась».
		AgentDerivedWindow: agentDerived,
	}
}

// gateModelNameOr / requestedWindowOr — безопасные чтения meta для логов.
func gateModelNameOr(meta *RequestMeta) string {
	if meta == nil {
		return ""
	}
	return meta.ModelName
}

func requestedWindowOr(meta *RequestMeta) int {
	if meta == nil {
		return 0
	}
	return meta.RequestedNCtxOverride
}

// perClientToTotal — пересчёт «окно на клиента» → «суммарное окно модели».
//
// llama.cpp при kv_unified=false делит суммарное окно между слотами:
// n_ctx_seq = PAD(n_ctx / n_seq_max, 256). Значит для N клиентов нужно
// суммарно ≈ окно × N (с выравниванием вверх до 256).
//
// R493-12h: выравнивание нужно и при ОДНОМ слоте. Живой архив второго стенда
// 09:53 (Qwen3.8-27B): балансер считал целевым окном 149809 (оценка промпта,
// невыровненная), воркер загружал 150016 (GGML_PAD по 256) и держал в
// /api/models именно 150016, а балансер записывал в кэш метрик своё 149809.
// Дальше любое сравнение «нужно / загружено» шло между разными числами одного и
// того же окна: 149809 ≠ 150016 → «параметры изменились» → перезагрузка ВНИЗ на
// 207 токенов (выгрузка 27B, avgDurationMs 215875 мс).
func perClientToTotal(perSeq, slots int) int {
	if perSeq <= 0 {
		return perSeq
	}
	perSeq = cppbackend.PadContext(perSeq)
	if slots <= 1 {
		return perSeq
	}
	return perSeq * slots
}
