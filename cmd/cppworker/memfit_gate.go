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

// effectiveKVCacheTypeForLoad — какой тип KV-cache реально применит ЗАГРУЗКА для
// этой модели. Тот же порядок, что в хендлерах:
//
//	явный параметр запроса → профиль модели (config/cppworker-*.json /
//	профиль от балансера) → дефолт cppworker (config.DefaultKVCacheType).
//
// Зачем отдельная функция: гейт n_ctx обязан считать KV тем же типом, что и
// раскладка слоёв. Наивный EffectiveKVCacheType("") берёт дефолт конфига и
// игнорирует per-model профиль: для Qwen3.8 дефолт q4_0, а профиль задаёт
// q8_0 — тип расходится в 1.88 раза, и гейт пропускает n_ctx, который
// раскладка считает невлезающим (или наоборот).
func effectiveKVCacheTypeForLoad(modelName, explicit string) string {
	if isValidKVCacheType(explicit) {
		return explicit
	}
	if profileSyncer != nil {
		if prof := profileSyncer.applyProfileOnLoad(modelName); prof != nil &&
			isValidKVCacheType(prof.KVCacheType) {
			return prof.KVCacheType
		}
	}
	if backend != nil {
		return backend.EffectiveKVCacheType("")
	}
	return "f16"
}

// feasibilityFromMemfit — вердикт гейта по уже собранным входным данным.
//
// kvType — итоговый тип KV-cache ("" = взять дефолт конфига). Параметр
// обязателен: без него гейт считал бы KV по одному типу, а раскладка — по
// другому (R83 §3.4).
func feasibilityFromMemfit(spec memfit.ModelSpec, budget memfit.Budget,
	kvType string, requested int) (NCtxFeasibility, memfit.Verdict, bool) {
	var zero memfit.Verdict
	if requested <= 0 {
		return NCtxFeasibility{}, zero, false
	}
	if backend != nil && strings.TrimSpace(kvType) == "" {
		kvType = backend.EffectiveKVCacheType("")
	}
	v := memfit.Evaluate(spec, memfit.Request{
		Ctx:    requested,
		KVType: cppbackend.MemfitKVType(kvType),
	}, budget, cppbackend.MemfitPolicy())

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
		Model:          spec.Name,
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

// logResolvedKVCacheType — одна строка о том, каким типом KV считает гейт.
// Нужна, чтобы расхождение «гейт q4_0 против раскладки q8_0» было видно в логе
// без чтения кода (это и был симптом §3.4).
func logResolvedKVCacheType(modelName, kvType string) {
	if logger.Get() == nil {
		return
	}
	logger.Get().Debugw("R83 гейт n_ctx: тип KV-cache для оценки",
		"model", modelName, "kv_cache_type", kvType)
}

// logMemfitVerdict — строка вердикта в лог: одна структура со всеми числами, из
// неё же строятся ответ и уведомление (см. Verdict.String()).
func logMemfitVerdict(v memfit.Verdict) {
	if logger.Get() == nil {
		return
	}
	logger.Get().Debugw("memfit: вердикт по памяти", "verdict", v.String())
}
