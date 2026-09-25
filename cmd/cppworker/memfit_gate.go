// memfit_gate.go — R83, шаг 3 миграции: гейт n_ctx принимает решение через
// internal/memfit.
//
// ЧТО БЫЛО. Гейт считал лимиты собственной формулой (CalculateResourceLimits →
// evaluateNCtxFeasibility), которая расходилась с решением о загрузке и с memfit:
// KV по всем блокам вместо attention-слоёв, head_dim из n_embd/n_heads вместо
// attention.key_length, сравнение с MemTotal вместо доступной RAM. Плюс ноль в
// потолке означал одновременно «неизвестно» и «веса не влезают», из-за чего
// нельзя было ни отказать, ни предупредить.
//
// ЧТО ТЕПЕРЬ. Решение принимает memfit.Evaluate. Форма ответа сохранена: 422
// `n_ctx_infeasible` с теми же полями (их читают балансер и WebUI), запись в
// load_failures и текст подсказки — из вердикта. NCtxFeasibility остаётся как
// транспортная структура для логов и ответа, но заполняется из Verdict.
package main

import (
	"strings"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/memfit"
	"ollama-loadbalancer/pkg/logger"
)

// feasibilityFromMemfit считает вердикт гейта и переносит его в NCtxFeasibility.
//
// ok == false означает «метаданных модели нет» — судить не о чем; вызывающий
// обязан решить сам (гейт в этом случае не отказывает, но и не молчит: пишет в лог).
func feasibilityFromMemfit(modelName string, requested int) (NCtxFeasibility, memfit.Verdict, bool) {
	var zero memfit.Verdict
	if backend == nil || requested <= 0 {
		return NCtxFeasibility{}, zero, false
	}
	spec, ok := backend.MemfitSpec(modelName)
	if !ok {
		return NCtxFeasibility{}, zero, false
	}
	kvType := cppbackend.MemfitKVType(backend.EffectiveKVCacheType(""))
	v := memfit.Evaluate(spec, memfit.Request{Ctx: requested, KVType: kvType},
		backend.MemfitBudget(), cppbackend.MemfitPolicy())

	minPositive := func(vals ...int) int {
		out := 0
		for _, x := range vals {
			if x > 0 && (out == 0 || x < out) {
				out = x
			}
		}
		return out
	}

	f := NCtxFeasibility{
		Model:          modelName,
		RequestedNCtx:  requested,
		MaxVRAMNCtx:    v.MaxExactFitCtx,
		MaxRAMNCtx:     v.MaxHardCtx,
		GGUFMaxContext: spec.TrainCtx,
		Known:          v.CeilingsKnown && spec.Complete(),
	}
	f.FeasibleMaxNCtx = minPositive(v.MaxExactFitCtx, v.MaxHardCtx, spec.TrainCtx)
	f.HardMaxNCtx = minPositive(v.MaxHardCtx, spec.TrainCtx)

	switch v.Stage {
	case memfit.StageExactFit:
		f.Stage = nctxStageExactFit
	case memfit.StagePartial, memfit.StageCPUOnly:
		f.Stage = nctxStageDegraded
		f.Degraded = true
	case memfit.StageDoesNotFit:
		f.Stage = nctxStageInfeasible
	default:
		f.Stage = nctxStageUnknown
	}
	// Обучающий контекст модели — жёсткая граница, независимо от памяти.
	if spec.TrainCtx > 0 && requested > spec.TrainCtx {
		f.Stage = nctxStageInfeasible
	}

	f.VerdictSuggestion = v.Suggestion
	if detail := v.ReasonDetail(memfit.ReasonKVQuantLever); detail != "" {
		f.VerdictSuggestion = strings.TrimSpace(f.VerdictSuggestion + " " + detail)
	}
	return f, v, true
}

// logMemfitVerdict — строка вердикта в лог: одна структура со всеми числами, из
// неё же строятся ответ и уведомление (см. Verdict.String()).
func logMemfitVerdict(v memfit.Verdict) {
	if logger.Get() == nil {
		return
	}
	logger.Get().Debugw("memfit: вердикт по памяти", "verdict", v.String())
}
