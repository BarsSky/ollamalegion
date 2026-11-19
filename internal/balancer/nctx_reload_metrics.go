// nctx_reload_metrics.go — R631 (2026-11-19): Метрики для окна: загруженное окно, потолок VRAM, разрешение максимального контекста модели (профиль/GGUF/метрики).
//
// ЗАЧЕМ ВЫНЕСЕНО. `nctx_reload_handlers.go` держал 2316 строк и четыре
// независимых слоя (обработчики, preflight, кэш наблюдённого окна, исполнение
// и метрики). Здесь — один из них; поведение не менялось, код перенесён как есть.
package balancer

import (
	"encoding/json"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"strings"
)

// reloadDedupNoUnloadR296 — воркер ответил dedup'ом, то есть НЕ выгружал модель.
//
// ЗАЧЕМ (R296). Причина обрыва стрима обязана определяться фактом («модель
// реально снимали»), а не фактом «мы попросили её снять». `/api/models/load`
// отвечает `{"status":"already_loaded"}`, когда параметры совпали: cppworker
// пропускает unload+load, ни один стрим от этой команды оборваться не мог.
// Засчитывать такой ответ за подтверждённую выгрузку — значит сохранить живой
// дефект: `docker kill` воркера подписывался как «балансер перезагрузил модель».
func reloadDedupNoUnloadR296(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var probe struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(probe.Status), "already_loaded")
}

// getLoadedNCtxFromMetrics — возвращает загруженный n_ctx модели из метрик.
// Используется в handleChat для предотвращения понижения n_ctx:
// если модель загружена с n_ctx=131072, а клиент шлёт num_ctx=8192,
// мы сохраняем загруженное значение.
func (p *Proxy) getLoadedNCtxFromMetrics(backendID, modelName string) int {
	if p.metricsMgr == nil {
		return 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	lm, hasLm := p.metricsMgr.llamaMetrics[backendID]
	if !hasLm || lm == nil {
		return 0
	}
	for _, m := range lm.LoadedModels {
		if m.Name == modelName || containsFold(m.Name, modelName) || containsFold(modelName, m.Name) {
			// R505 / аудит N-6: функция отдаёт окно НА СЛОТ (его сравнивают с
			// промптом одного клиента), поэтому ContextPerSeq главнее суммарного
			// ContextLength; без ContextPerSeq — делим суммарное на слоты.
			if m.ContextPerSeq > 0 {
				return m.ContextPerSeq
			}
			return perSeqFromTotal(m.ContextLength, m.MaxSlots)
		}
	}
	return 0
}

// getMaxVRAMNCtxFromMetrics возвращает max_vram_n_ctx из llama.cpp метрик бэкенда.
// Позволяет preflight узнать реальный VRAM-потолок без round-trip к cppworker.
func (p *Proxy) getMaxVRAMNCtxFromMetrics(backendID string) int {
	if p == nil || p.metricsMgr == nil {
		return 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	if lm, ok := p.metricsMgr.llamaMetrics[backendID]; ok && lm != nil {
		return lm.MaxVRAMNCtx
	}
	return 0
}

// maxVRAMNCtxKnownFromMetrics — то же значение + признак «VRAM измерена» (R519).
//
// Нужен, потому что 0 у max_vram_n_ctx означает и «неизвестно» (vram_known=false),
// и «свободной VRAM не хватает ни на какое окно» (vram_known=true). Отказ по этим
// двум случаям обязан звучать по-разному: во втором советовать поднять
// LB_NCTX_RELOAD_MAX_N_CTX бесполезно.
func (p *Proxy) maxVRAMNCtxKnownFromMetrics(backendID string) (int, bool) {
	if p == nil || p.metricsMgr == nil {
		return 0, false
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	lm, ok := p.metricsMgr.llamaMetrics[backendID]
	if !ok || lm == nil {
		return 0, false
	}
	// R520 (2026-11-19): признак «0 — это ДЕЙСТВИТЕЛЬНО нет места» требует ещё и
	// загруженной модели. Воркер считает exact-fit ПО ЗАГРУЖЕННЫМ моделям
	// (handleListModels), поэтому на пустом узле max_vram_n_ctx=0 значит «не
	// считали», а не «нет места». Живой факт: свежая установка стенда (0 моделей,
	// 7 ГБ свободной VRAM) отдаёт 0 при vram_known=true — первая версия R519
	// объявляла это «места нет», что было неправдой.
	known := lm.VramKnown && len(lm.LoadedModels) > 0
	return lm.MaxVRAMNCtx, known
}

// resolveModelMaxContext выбирает modelMaxContext для preflight v3 (Round 43).
//
// 3-tier resolution с conceptual fix (R43, 2026-08-19):
//
//  1. profile.contextLengthAuto == false (default для backward compat):
//     profile.contextLength — жёсткий cap, но min(profile, contextLengthMax) —
//     operator cap всегда уважается. Pre-R43: contextLengthMax в schema
//     был задекларирован, но resolver его ИГНОРИРОВАЛ. R43 fix: применён.
//  2. profile.contextLengthAuto == true (auto-adapt, opt-in per profile):
//     profile — HINT (не cap). Реальный cap = MIN(ggufMax, contextLengthMax).
//     perModelFeasible — HINT (current-state), не cap. Причина: feasible
//     отражает "что влезает с ТЕКУЩИМИ gpu_layers". После auto-tune (partial
//     offload в RAM) feasible вырастет. Если использовать feasible как cap,
//     preflight будет reject'ить запросы которые cppworker фактически может
//     обработать после reload с auto-offload. R43 fix: feasible — HINT, не cap.
//     Round 37 PRODUCTION BUG: pre-bug balancer reject'ил Cline num_ctx=65536
//     потому что feasible=32719 (max_vram_n_ctx при gpu_layers=-1) < 65536.
//     Post-R43: feasible игнорируется в auto mode, cap = min(ggufMax=262144,
//     contextLengthMax=131072) = 131072 → 65536 проходит.
//  3. Fallback (нет profile): ggufMax (hard upper bound from training).
//     Pre-R43: fallback на default profile (32768) — это conservative HINT
//     которая блокировала 65536 даже для моделей без явного profile. R43 fix:
//     fall through to ggufMax; если и его нет — to metrics.
//
// КЛЮЧЕВОЙ ПРИНЦИП R43: "балансер должен автоматом все определят и выделять
// под нужды модели". Никаких hardcoded n_ctx в resolver chain. Все значения
// либо auto-derived (ggufMax from training), либо явный operator policy
// (contextLengthMax). profile.contextLength — HINT для initial load, не cap.
//
// Параметр profileMaxContext — contextLength из per-model profile (0 = no profile).
// Параметр contextLengthAuto — true если profile разрешает auto-adapt.
// Параметр contextLengthMax — operator soft cap (0 = unlimited up to ggufMax).
// Параметр perModelFeasible — per-model feasible (из LoadedModels), HINT в auto mode.
//
// R265 (2026-10-28): ПОЧЕМУ ЭТО НЕ СВЕДЕНО С resolvePhysicalMaxContext.
// Вопрос здесь другой: «какой предел окна отдать решению о reload», а не
// «что физически возможно». Различия содержательные, а не косметические:
//   - в hard-режиме contextLength (профиль) — CAP, то есть значение МОЖЕТ быть
//     НИЖЕ физики (оператор сознательно ограничил модель), а физический потолок
//     по определению ниже не опускается;
//   - в auto-режиме наоборот: профиль — HINT, а cap = min(ggufMax, contextLengthMax);
//   - здесь же живут диагностические предупреждения оператору (профиль
//     консервативнее физики, потолок >4x текущей достижимости), которых в
//     физическом потолке нет по смыслу.
//
// Сведение изменило бы поведение preflight, поэтому в R265 оставлено обёрткой-
// комментарием и закреплено характеризационными тестами.
func (p *Proxy) resolveModelMaxContext(backendID, model string, profileMaxContext int, contextLengthAuto bool, contextLengthMax int, perModelFeasible int) int {
	// Tier 1+2: profile present, decide based on auto flag
	if profileMaxContext > 0 {
		if !contextLengthAuto {
			// Tier 1: hard cap (backward compat).
			//
			// R400 (2026-11-05): contextLengthMax больше НЕ урезает заявленное
			// окно. Живая жалоба: окно в карточке поднято до 262144, максимум
			// остался 131072 — и клиент получал отказ «модель поддерживает
			// 131072». Максимум ограничивает АВТО-рост (Tier 2), а в жёстком
			// режиме окно задаёт contextLength; противоречие фиксируем
			// предупреждением, но политику оператора не переписываем молча.
			cap := profileMaxContext
			if contextLengthMax > 0 && cap > contextLengthMax {
				logger.Get().Warnw("resolveModelMaxContext: declared window exceeds contextLengthMax, honouring declared window",
					"backend", backendID, "model", model,
					"profile_n_ctx", profileMaxContext,
					"context_length_max", contextLengthMax,
				)
			}
			// Diagnostic: warn если feasible >> profile (явно conservative profile).
			feasible := perModelFeasible
			if feasible == 0 {
				feasible = p.getFeasibleMaxContext(backendID)
			}
			if feasible > 0 && feasible > cap*3/2 {
				logger.Get().Warnw("resolveModelMaxContext: profile conservative (no auto-adaptation enabled)",
					"backend", backendID, "model", model,
					"profile_n_ctx", cap,
					"feasible_n_ctx", feasible,
					"headroom_ratio", float64(feasible)/float64(cap),
					"recommendation", i18n.T("balancer.nctx_reload_handlers.profile_conservative_recommendation"),
				)
			}
			return cap
		}
		// Tier 2: auto-adapt. profile — HINT. Real cap = MIN(ggufMax, потолок),
		// где потолок = max(contextLengthMax, заявленное окно) (R400: заявленное
		// окно не может быть урезано устаревшим максимумом).
		// CRITICAL R43 CHANGE: perModelFeasible is HINT, not cap. After cppworker
		// auto-tunes gpu_layers, feasible grows. Using it as cap blocks legit requests.
		cap := contextLengthMax // operator policy (soft cap, default 0 = unlimited)
		if cap > 0 && profileMaxContext > cap {
			// R400: максимум не может урезать заявленное окно (см. Tier 1).
			// 0 (не задан) по-прежнему означает «не ограничивать» — доверяем
			// GGUF и воркеру, как и раньше.
			cap = profileMaxContext
		}
		// R545 (F9): GGUF ИМЕННО этой модели, а не максимум по загруженным
		// моделям узла (см. ggufMaxContextForModel).
		ggufMax := p.ggufMaxContextForModel(backendID, model)
		if ggufMax > 0 && (cap == 0 || ggufMax < cap) {
			cap = ggufMax
		}
		// If both contextLengthMax and ggufMax are 0, we have NO data.
		// Return 0 (no cap) and let cppworker decide. This is the
		// "trust the model" path: if operator set auto=true, they trust
		// the system to figure it out. cppworker will validate against
		// its own context_size (computed from GGUF header).
		if cap == 0 {
			return 0
		}
		// If perModelFeasible is reported AND is BIGGER than profileMaxContext,
		// that's a confirmation that auto-relax is right. We don't use it as
		// a cap, but we can validate against it (warn if operator's cap is
		// more permissive than current achievable).
		if perModelFeasible > 0 && cap > perModelFeasible*4 {
			// Operator cap (e.g. 131072) is way more than current feasible
			// (e.g. 32719). After auto-tune, feasible will grow. Don't reject.
			// Just info-log so operator knows.
			logger.Get().Infow("resolveModelMaxContext: operator cap > 4x current feasible, will rely on cppworker auto-tune",
				"backend", backendID, "model", model,
				"resolved_cap", cap,
				"current_feasible", perModelFeasible,
				"note", "cppworker will reduce gpu_layers on reload to grow feasible via RAM offload")
		}
		return cap
	}
	// Tier 3: no profile. R43 fix: fall through to ggufMax (model's hard upper
	// bound from training) instead of default profile (conservative 32768).
	// Pre-R43: Tier 3 returned getMetricsModelMaxContext() which is also
	// conservative (cppworker reports max_vram_n_ctx, not GGUFMax).
	// R545 (F9): GGUF по имени модели.
	ggufMax := p.ggufMaxContextForModel(backendID, model)
	if ggufMax > 0 {
		return ggufMax
	}
	// Final fallback: metrics ModelMaxContext (preserved for backward compat).
	return p.getMetricsModelMaxContext(backendID)
}

// getFeasibleMaxContext — возвращает MaxFeasibleContext из llamaMetrics
// (заполняется из cppworker /api/models response, поле feasible_max_context).
// 0 = unknown (cppworker не сообщил).
func (p *Proxy) getFeasibleMaxContext(backendID string) int {
	if p == nil || p.metricsMgr == nil {
		return 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	if lm, ok := p.metricsMgr.llamaMetrics[backendID]; ok && lm != nil {
		return lm.MaxFeasibleContext // Round 37: 0 если не заполнено
	}
	return 0
}

// getGGUFMaxContext — возвращает GGUFMaxContext из llamaMetrics (top-level).
// Per-model override передаётся явно через resolveModelMaxContext parameter.
func (p *Proxy) getGGUFMaxContext(backendID string) int {
	if p == nil || p.metricsMgr == nil {
		return 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	if lm, ok := p.metricsMgr.llamaMetrics[backendID]; ok && lm != nil {
		return lm.GGUFMaxContext
	}
	return 0
}

// getMetricsModelMaxContext — старое fallback (ModelMaxContext из poller'а).
func (p *Proxy) getMetricsModelMaxContext(backendID string) int {
	if p == nil || p.metricsMgr == nil {
		return 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	if lm, ok := p.metricsMgr.llamaMetrics[backendID]; ok && lm != nil {
		return lm.ModelMaxContext
	}
	return 0
}
