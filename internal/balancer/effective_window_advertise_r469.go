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
//	eff      — минимум из известных ограничений (>0);
//	ggufMax  — потолок из метаданных модели (для отдельного заголовка), 0 если unknown.
//
// Все источники берутся из уже посчитанных значений: GGUF и max из снимка
// метрик, профиль — p.profileCapCtx, операторский cap — effectiveAutoReloadCeiling
// (он же учитывает заявленное оператором окно профиля, правило R255/R267).
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
	for _, v := range []int{ggufMax, perModelMax, operatorCap} {
		if v > 0 && (eff == 0 || v < eff) {
			eff = v
		}
	}
	return eff, ggufMax
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
