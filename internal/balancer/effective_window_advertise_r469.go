// effective_window_advertise_r469.go — R469 (2026-11-06): клиенту нельзя
// обещать окно, которое балансер больше не даст.
//
// ЖИВОЙ СИМПТОМ (жалоба оператора, два измерения одного хода):
//   - Cline показывал занятый контекст 63.6k и НЕ запускал сжатие — по его
//     данным запас до 130k ещё есть;
//   - балансер в это же время отвечал «требуется 151743 на контекстное окно»
//     (до R466) и не поднимал окно выше операторского потолка
//     LB_NCTX_RELOAD_MAX_N_CTX=65536.
//
// Причина расхождения — НАШИ числа. На успешном ответе мы отдавали
// X-Model-Max-Context=131072 (это GGUF-потолок модели), а физически обслужить
// могли не больше 65536: min(GGUF, потолок профиля, операторский cap). Клиент,
// который строит историю по нашей подсказке, не имеет причин сжимать до 130k —
// и упирается в наш же потолок на 65k.
//
// ПРАВИЛО: X-Model-Max-Context = ДЕЙСТВУЮЩИЙ потолок обслуживания
// (min из GGUF, профиля модели и операторского cap). Сырой GGUF-потолок остаётся
// отдельным заголовком X-Model-Gguf-Max-Context — терять метаданные незачем,
// но подменять ими предел обслуживания нельзя.
package balancer

import "ollama-loadbalancer/pkg/types"

// advertisedMaxContextR469 — действующий потолок окна для КЛИЕНТСКОЙ подсказки.
//
// Возвращает:
//
//	eff      — минимум из известных ограничений (>0), в единице «НА СЛОТ»;
//	ggufMax  — потолок из метаданных модели (для отдельного заголовка), 0 если unknown.
//
// Все источники берутся из уже посчитанных значений: GGUF и max из снимка
// метрик, профиль — p.profileCapCtx, операторский cap — effectiveAutoReloadCeiling
// (он же учитывает заявленное оператором окно профиля, правило R255/R267).
//
// R505 / аудит N-8: единица — НА СЛОТ. Клиент трактует `num_ctx` как окно
// своего запроса (одного слота), а операторский cap и загруженное окно у нас
// СУММАРНЫЕ: объявлять их как есть — обещать вдвое больше при parallel=2. Клиент
// ставил наше же число и получал либо 413, либо вторую перезагрузку. GGUF-потолок
// (n_ctx_train) и MaxContext модели — уже на последовательность, поэтому берутся
// как есть. Суммарный потолок уходит отдельным заголовком
// (X-Model-Max-Context-Total) — см. advertisedMaxContextTotalR505.
func (p *Proxy) advertisedMaxContextR469(modelName string) (eff, ggufMax int) {
	if p == nil || modelName == "" {
		return 0, 0
	}
	_, maxCtx := p.modelContextInfo(modelName)
	ggufMax, perModelMax := p.modelContextLimitsR469(modelName)
	if ggufMax <= 0 {
		ggufMax = maxCtx
	}
	// Модели нет в метриках: её СОБСТВЕННЫЙ потолок неизвестен. Операторский cap
	// сам по себе не потолок этой модели (у неё может быть меньше), поэтому ничего
	// не обещаем — остаётся прежнее значение из caps.Headers().
	if ggufMax <= 0 && perModelMax <= 0 {
		return 0, 0
	}
	operatorCap := 0
	if p.nctxReload != nil {
		operatorCap = p.effectiveAutoReloadCeiling(modelName, p.nctxReload.Config().AutoReloadMaxNCtx)
	}
	// R505 / аудит N-8: суммарные источники приводим к единице «на слот» ОДИН раз.
	// ggufMax и perModelMax — уже на последовательность (n_ctx_train/MaxContext
	// модели), их не делим. Загруженное окно сюда НЕ входит: это текущее
	// состояние инстанса (его отдаёт X-Model-Context-Length), а не потолок —
	// иначе объявленный потолок падал бы до текущего окна и клиент сжимал бы
	// историю без причины.
	slots := p.windowSlotsR505(modelName)
	for _, v := range []int{
		ggufMax,
		perModelMax,
		derivedWindowCapR466(operatorCap, slots),
	} {
		if v > 0 && (eff == 0 || v < eff) {
			eff = v
		}
	}
	return eff, ggufMax
}

// advertisedMaxContextTotalR505 — СУММАРНЫЙ потолок окна для отдельного заголовка.
//
// R505 / аудит N-8: клиенту полезно знать и суммарное окно модели (сколько воркер
// поднимет в contextSize), но оно не должно подменять окно клиента: клиент
// ставит `num_ctx` как окно своего запроса, то есть на слот.
func (p *Proxy) advertisedMaxContextTotalR505(modelName string) int {
	perClient, _ := p.advertisedMaxContextR469(modelName)
	if perClient <= 0 {
		return 0
	}
	return perClientToTotal(perClient, p.windowSlotsR505(modelName))
}

// windowSlotsR505 — число слотов загруженной модели (0/1 = один).
//
// Источник: `maxSlots` из /api/models (cppworker отдаёт его с R83), иначе
// параллельность из профиля модели, иначе 1.
func (p *Proxy) windowSlotsR505(modelName string) int {
	if p == nil || modelName == "" {
		return 1
	}
	slots := 0
	if p.metricsMgr != nil {
		p.metricsMgr.mu.RLock()
		for _, lm := range p.metricsMgr.llamaMetrics {
			if lm == nil {
				continue
			}
			for i := range lm.LoadedModels {
				if !modelNameMatches(lm.LoadedModels[i].Name, modelName) {
					continue
				}
				if v := lm.LoadedModels[i].MaxSlots; v > slots {
					slots = v
				}
			}
		}
		p.metricsMgr.mu.RUnlock()
	}
	if slots <= 0 {
		if prof, ok := profileForReloadHints(p.profilesSnapshotR423(), modelName); ok && prof.Parallel > 0 {
			slots = prof.Parallel
		}
	}
	return normalizedSlotsR504(slots)
}

// modelContextLimitsR469 — GGUF-потолок и max из метрик загруженной модели.
//
// Отдельная функция (а не modelContextInfo) нужна, чтобы не менять уже
// используемое поведение того геттера: он склеивает max/GGUF в одно число, а
// здесь оба значения нужны по отдельности.
func (p *Proxy) modelContextLimitsR469(modelName string) (ggufMax, maxCtx int) {
	if p == nil || p.metricsMgr == nil || modelName == "" {
		return 0, 0
	}
	p.metricsMgr.mu.RLock()
	snapshot := make([]types.LlamaCppModel, 0, 8)
	for _, lm := range p.metricsMgr.llamaMetrics {
		if lm == nil {
			continue
		}
		snapshot = append(snapshot, lm.LoadedModels...)
	}
	p.metricsMgr.mu.RUnlock()
	for i := range snapshot {
		m := &snapshot[i]
		if !modelNameMatches(m.Name, modelName) {
			continue
		}
		return m.GGUFMaxContext, m.MaxContext
	}
	return 0, 0
}
