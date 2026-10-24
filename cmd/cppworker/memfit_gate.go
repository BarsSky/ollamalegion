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
	"ollama-loadbalancer/internal/i18n"
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
		// Явный параметр приходит из плана загрузки (опции хендлера). Логируем на
		// уровне INFO: именно здесь видно, что гейт считает KV тем же типом, что
		// и раскладка — то есть что §3.4 не вернулся (раньше здесь молча
		// подставлялся дефолт конфига, обычно q4_0 при профиле q8_0).
		if logger.Get() != nil {
			logger.Get().Infow(i18n.T("cppworker.memfit_gate.kv_from_plan"),
				"model", modelName, "kv_cache_type", explicit)
		}
		return explicit
	}
	if profileSyncer != nil {
		if prof := profileSyncer.applyProfileOnLoad(modelName); prof != nil &&
			isValidKVCacheType(prof.KVCacheType) {
			// Профиль перебивает дефолт конфига. Путь используется хендлерами,
			// которые не применяют профиль в opts до гейта (handleLoadModel,
			// handleReloadModel) — поэтому резолв делается здесь.
			if logger.Get() != nil {
				logger.Get().Infow(i18n.T("cppworker.memfit_gate.kv_from_profile"),
					"model", modelName, "kv_cache_type", prof.KVCacheType)
			}
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
//
// ВНИМАНИЕ: этот вариант МОЖЕТ дозвониться до узлов раскладки (счётчик узлов для
// раскладки решения берётся у моста). Он для путей РЕШЕНИЯ (гейт n_ctx перед
// загрузкой). Справочные ручки обязаны звать feasibilityFromMemfitCached.
func feasibilityFromMemfit(spec memfit.ModelSpec, budget memfit.Budget,
	kvType string, requested int) (NCtxFeasibility, memfit.Verdict, bool) {
	return feasibilityFromMemfitMode(spec, budget, kvType, requested, true)
}

// feasibilityFromMemfitCached — то же, но узлы раскладки НЕ опрашиваются: их
// число берётся из уже собранного снимка (R132, 2026-10-16).
//
// ЖИВОЙ ДЕФЕКТ, ЗАКРЫТЫЙ ЭТИМ. `GET /api/models/effective` (карточка модели в
// WebUI) звал feasibilityFromMemfit, а внутри него — RPCCapacityBytes(). На узле,
// который принимает TCP и молчит, вызов уходил в C-мост, и ручка переставала
// отвечать (замер: 30 с без ответа, дублёр узла принял соединение). Правка
// вызывающего (MemfitBudgetCached) этого не лечила: дозвон был внутри помощника.
func feasibilityFromMemfitCached(spec memfit.ModelSpec, budget memfit.Budget,
	kvType string, requested int) (NCtxFeasibility, memfit.Verdict, bool) {
	return feasibilityFromMemfitMode(spec, budget, kvType, requested, false)
}

// feasibilityFromMemfitMode — общий расчёт; withRPC решает, спрашивать ли узлы.
func feasibilityFromMemfitMode(spec memfit.ModelSpec, budget memfit.Budget,
	kvType string, requested int, withRPC bool) (NCtxFeasibility, memfit.Verdict, bool) {
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

	// R115: раскладка решения. Узлы RPC считаем отдельно: без этого отказ по
	// памяти не отличим от «удалённый узел отвалился» — а это разные действия
	// оператора (уменьшить контекст против починить узел). Счётчик берём у моста
	// через тот же кэш снимка устройств, что и бюджет.
	//
	// R132: справочный вариант берёт счётчик из КЭША и не дозванивается —
	// RPCCapacityBytes на молчащем узле уходит в мост и вешает ручку.
	rpcDevices := 0
	if backend != nil {
		n := 0
		if withRPC {
			_, _, n = backend.RPCCapacityBytes()
		} else {
			n = backend.CachedRemoteDeviceCount()
		}
		if n > 0 {
			rpcDevices = n
		}
	}
	bd := buildMemoryBreakdown(budget, v, rpcDevices)
	f.Breakdown = &bd
	return f, v, true
}

// logResolvedKVCacheType — одна строка о том, каким типом KV считает гейт.
// Нужна, чтобы расхождение «гейт q4_0 против раскладки q8_0» было видно в логе
// без чтения кода (это и был симптом §3.4).
func logResolvedKVCacheType(modelName, kvType string) {
	if logger.Get() == nil {
		return
	}
	logger.Get().Debugw(i18n.T("cppworker.memfit_gate.kv_for_estimate"),
		"model", modelName, "kv_cache_type", kvType)
}

// logMemfitVerdict — строка вердикта в лог: одна структура со всеми числами, из
// неё же строятся ответ и уведомление (см. Verdict.String()).
func logMemfitVerdict(v memfit.Verdict) {
	if logger.Get() == nil {
		return
	}
	logger.Get().Debugw(i18n.T("cppworker.memfit_gate.verdict"), "verdict", v.String())
}
