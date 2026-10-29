package balancer

// nctx_declared_r255.go — R255 (2026-10-28): «заявлено 256000 → загружено 32768».
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (стенд на qwen3.8). В карточке модели WebUI выставлено
// контекстное окно 256000, а модель загрузилась с 32768. Второй симптом: агент
// просит рост окна больше 131072, и балансер отказывает, хотя VRAM хватает.
//
// ГДЕ ТЕРЯЕТСЯ (разбор цепочки):
//  1. WebUI пишет в профиль поле `contextLength` (gguf-renderer-detail-render.js
//     MODEL_PARAM_SPECS: field=contextLength, loadKey=contextSize, max=1048576) —
//     здесь 256000 сохраняется без потерь.
//  2. В профиле ОДНОВРЕМЕННО живёт второе поле — `contextPerSeq` («окно НА
//     КЛИЕНТА», R83). Балансер отправляет воркеру ОБА поля
//     (model_management.go:1108-1144), а cppworker при `contextPerSeq > 0`
//     считает СУММАРНОЕ окно сам: total = PAD256(perSeq) × слоты
//     (cmd/cppworker/types.go:195-202, handlers_model.go:189-190).
//     То есть устаревшее per-seq значение МОЛЧА перебивает заявленное оператором
//     суммарное окно: 16384 × 2 слота = 32768 — ровно то число, которое увидел
//     оператор.
//  3. Второй симптом — потолок авто-РОСТА `LB_NCTX_RELOAD_MAX_N_CTX`
//     (config→AutoReloadMaxNCtx, nctx_reload_config_bridge.go:126): он режет
//     именно АВТО-рост и отказ на запрос агента (nctx_reload.go:686-688,
//     732-742, 811-813; nctx_reload_handlers.go:850). К ЗАЯВЛЕННОМУ оператором
//     окну он отношения иметь не должен — иначе оператор не может задать окно,
//     которое физика держит.
//
// РЕШЕНИЕ. Одно место, где два поля профиля согласуются ПРЕЖДЕ, чем уйти в
// воркер, и где оператору печатается ОДНА строка с числами: что заявлено, что
// реально уйдёт, какие слоты и кто из двух полей победил.

import (
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// nctxAlignTolerance — допуск выравнивания при сравнении «суммарное» vs
// «на клиента × слоты». cppworker выравнивает окно слота вверх до 256, поэтому
// равенство проверяем с допуском на один шаг выравнивания на слот.
const nctxAlignTolerance = 256

// pad256 — выравнивание окна вверх до кратного 256 (как делает cppworker).
func pad256(n int) int {
	if n <= 0 {
		return 0
	}
	return ((n + 255) / 256) * 256
}

// resolveDeclaredWindow — R255: согласовать `contextLength` (суммарное) и
// `contextPerSeq` (на клиента) из профиля модели.
//
// Возвращает итоговые (total, perSeq) для тела загрузки и note — объяснение
// оператору, ЕСЛИ поля противоречили друг другу (пусто = противоречия не было).
// perSeq == 0 означает «поле не отправлять»: тогда воркер возьмёт суммарное окно
// как есть и не пересчитает его сам.
//
// Правила (минимальные и предсказуемые):
//   - задано только одно поле → оно и уходит (второе не выдумываем);
//   - оба заданы и согласованы (perSeq × слоты == total с точностью до
//     выравнивания) → уходят оба, как раньше;
//   - perSeq × слоты МЕНЬШЕ total → доверяем суммарному (иначе устаревшее
//     per-seq молча режет окно оператора: живой случай 256000 → 32768),
//     per-seq не отправляем;
//   - perSeq × слоты БОЛЬШЕ total → доверяем per-seq (оператор просил окно на
//     клиента, суммарное осталось старым), total поднимаем до perSeq × слоты.
func resolveDeclaredWindow(total, perSeq, slots int) (resolvedTotal, resolvedPerSeq int, note string) {
	if total <= 0 && perSeq <= 0 {
		return 0, 0, ""
	}
	if slots < 1 {
		slots = 1
	}
	if perSeq <= 0 {
		return total, 0, ""
	}
	implied := pad256(perSeq) * slots
	if total <= 0 {
		return implied, perSeq, ""
	}
	if absInt(implied-total) <= nctxAlignTolerance*slots {
		return total, perSeq, ""
	}
	// Оба поля заданы и расходятся. ПОБЕЖДАЕТ СУММАРНОЕ.
	//
	// Почему не «большее»: карточка модели показывает `contextLength` как
	// «n_ctx (окно контекста, СУММАРНОЕ)» — именно его оператор и правит, когда
	// говорит «я выставил 256000». Второе поле (`contextPerSeq`) остаётся от
	// прежних опытов и не должно ни резать заявленное окно (живой случай
	// 256000 → 32768), ни молча растить его сверх заявленного (лишняя VRAM и
	// риск OOM). Выбор «большего» был бы угадыванием за оператора.
	return total, 0, "заданы оба поля окна и они противоречат друг другу: доверяем суммарному contextLength, per-seq не отправляем"
}

// absInt — модуль целого (локальная копия, чтобы не тянуть math в hot path).
func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// logDeclaredWindow — ОДНА строка с числами: заявлено → уйдёт в загрузку.
//
// Зачем в лог, а не только в UI: оператор в следующий раз должен видеть ответ на
// вопрос «почему загрузилось 32768, если я задал 256000» в журнале балансера, а
// не искать его в переписке.
func logDeclaredWindow(model string, prof types.LlamaCppModelProfile, slots, total, perSeq int, note string) {
	if logger.Get() == nil {
		return
	}
	logger.Get().Infow("declared n_ctx window resolved",
		"model", model,
		"profile_context_length", prof.ContextLength,
		"profile_context_per_seq", prof.ContextPerSeq,
		"slots", slots,
		"effective_total", total,
		"effective_per_seq", perSeq,
		"note", note)
}

// effectiveAutoReloadCeiling — R255: потолок АВТО-роста с учётом того, что
// оператор уже заявил в профиле модели.
//
// ЧТО БЫЛО НЕ ТАК. `LB_NCTX_RELOAD_MAX_N_CTX` (AutoReloadMaxNCtx) — это ЗАЩИТА
// ОТ АВТО-роста, то есть от того, что клиентский запрос сам поднимет окно. Но
// когда оператор в карточке модели уже заявил окно больше (например 256000 при
// потолке 131072), отказ «выше потолка» противоречит его же решению: он эту
// политику и установил. Живая жалоба: «не смог удовлетворить запрос агента на
// увеличение контекста больше 131072, хотя VRAM достаточно».
//
// ПРАВИЛО. Потолок авто-роста = max(env-потолок, заявленное оператором окно).
// Физический предел (VRAM/GGUF) остаётся отдельной проверкой: если окно в
// физику не влезает, отказ по-прежнему будет — но уже с физической причиной, а
// не «операторский лимит».
//
// configured <= 0 означает «потолок не задан» — тогда заявленное окно и есть
// потолок (0 = без ограничения, как раньше).
func (p *Proxy) effectiveAutoReloadCeiling(modelName string, configured int) int {
	declared := p.declaredWindowTotal(modelName)
	if configured <= 0 {
		return 0 // ограничения нет
	}
	if declared <= configured {
		return configured
	}
	if logger.Get() != nil {
		logger.Get().Infow("auto-growth n_ctx ceiling raised to the operator-declared window",
			"model", modelName,
			"env_ceiling", configured,
			"declared_window", declared,
			"why", "профиль модели задан оператором: это его политика, а пас-тру потолок защищает от АВТО-роста по клиентскому запросу")
	}
	return declared
}

// declaredWindowTotal — суммарное окно, ЗАЯВЛЕННОЕ оператором в профиле модели.
//
// R297 (2026-10-29): вынесено из effectiveAutoReloadCeiling, потому что это же
// число понадобилось проверке выполнимости ручной перезагрузки
// (WindowFeasibility). Двух мнений о «заявленном окне» быть не должно: раньше
// копия этой логики уже приводила к тому, что ручной запрос отвергался по
// env-потолку при заявленном окне больше него (класс дефекта R267).
//
// 0 = оператор окно не заявлял.
func (p *Proxy) declaredWindowTotal(modelName string) int {
	if p == nil {
		return 0
	}
	prof, ok := p.GetModelProfile(modelName)
	if !ok {
		return 0
	}
	// R255: то же правило, что и при загрузке — суммарное окно оператора
	// авторитетно; per-seq участвует только если суммарного нет.
	slots := prof.Parallel
	if slots < 1 {
		slots = 1
	}
	total, _, _ := resolveDeclaredWindow(prof.ContextLength, prof.ContextPerSeq, slots)
	if total < 0 {
		return 0
	}
	return total
}
