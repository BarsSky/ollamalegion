// nctx_reload_decide.go — R634 (2026-11-19): решения координатора: какую копию перезагружать, план отказа, пределы и округления.
//
// ЗАЧЕМ ВЫНЕСЕНО. `nctx_reload.go` держал 1865 строк: типы и конфигурацию,
// состояние координатора, решения о перезагрузке, HTTP-клиент, длинный DoReload,
// метрики и опрос прогресса. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"encoding/json"
	"fmt"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// DecideReloadBackend — решение о перезагрузке с потолком из КОНФИГА.
//
// R267 (2026-10-29): оставлен для обратной совместимости (тесты, внешние
// вызовы). Поток запроса идёт через DecideReloadBackendWithCeiling: потолок —
// не «сырое» значение env, а вычисленный операторский потолок
// (Proxy.effectiveAutoReloadCeiling), иначе решение координатора обходило
// выбор оператора в WebUI и молча резало окно до env-значения.
func (c *NCtxReloadCoordinator) DecideReloadBackend(
	backendID string,
	bridgeErr *NCtxBridgeError,
	requestedNCtxOverride int,
) *ReloadPlan {
	cfg := c.Config()
	return c.decideReloadBackend(backendID, bridgeErr, requestedNCtxOverride,
		ReloadCeiling{Effective: cfg.AutoReloadMaxNCtx, Env: cfg.AutoReloadMaxNCtx}, 1)
}

// DecideReloadBackendWithCeiling — R267: то же решение, но потолок авто-роста
// задаёт ВЫЗЫВАЮЩИЙ (одна формула — Proxy.effectiveAutoReloadCeiling).
//
// ЗАЧЕМ. Живая жалоба оператора: в WebUI выставлено окно 256000, модель обучена
// на 262144, VRAM хватает — а запрос агента на окно больше env-потолка получал
// отказ. Причина: обработчик потолок поднимал, а РЕШАТЕЛЬ о перезагрузке брал
// cfg.AutoReloadMaxNCtx напрямую, и типовой агентский путь (prompt_too_long)
// идёт именно через него. Теперь решатель не имеет собственного мнения о
// потолке: ему передают то же число, что и обработчику.
//
// R504 / аудит N-2: метод считает слоты как 1 (историческое поведение). Поток
// запроса идёт через DecideReloadBackendForModel — там слоты известны, и
// требование воркера (на слот) пересчитывается в суммарное окно.
func (c *NCtxReloadCoordinator) DecideReloadBackendWithCeiling(
	backendID string,
	bridgeErr *NCtxBridgeError,
	requestedNCtxOverride int,
	ceiling ReloadCeiling,
) *ReloadPlan {
	return c.decideReloadBackend(backendID, bridgeErr, requestedNCtxOverride, ceiling, 1)
}

// DecideReloadBackendForModel — R504 (аудит N-2): решение с числом СЛОТОВ модели.
//
// ЗАЧЕМ. Воркер сообщает требование НА СЛОТ (prompt + n_predict против окна слота),
// а `contextSize` в перезагрузке — СУММАРНОЕ окно модели. Раньше требование
// уходило в contextSize как есть: при parallel=2 балансер поднимал модель на
// 131072 там, где слоту нужно было 131072 — слот получал 65536, и следующий
// запрос снова получал 413 (бесконечный цикл «reload без выигрыша»).
//
// Слоты берутся из хинтов reload'а (ReloadHints.Parallel: профиль модели или
// фактическое число у загруженной модели), поэтому сигнатура и получила имя
// модели — без неё хинты не найти.
func (c *NCtxReloadCoordinator) DecideReloadBackendForModel(
	backendID, modelName string,
	bridgeErr *NCtxBridgeError,
	requestedNCtxOverride int,
	ceiling ReloadCeiling,
) *ReloadPlan {
	slots := 1
	if h := c.ReloadHintsFor(backendID, modelName); h.Parallel > 1 {
		slots = h.Parallel
	}
	return c.decideReloadBackend(backendID, bridgeErr, requestedNCtxOverride, ceiling, slots)
}

func (c *NCtxReloadCoordinator) decideReloadBackend(
	backendID string,
	bridgeErr *NCtxBridgeError,
	requestedNCtxOverride int,
	ceiling ReloadCeiling,
	slotsIn int,
) *ReloadPlan {
	cfg := c.Config()
	// Единицы: bridgeErr.* — НА СЛОТ, ceiling.Effective и NewNCtx — СУММАРНЫЕ.
	// Пересчёт делается здесь один раз, чтобы в решении не смешивались единицы.
	slots := normalizedSlotsR504(slotsIn)

	// Kill-switch
	if !cfg.AutoReloadNCtx {
		return &ReloadPlan{
			Decision: DecisionNoOp,
			Reason:   "auto-reload disabled in config (auto_reload_n_ctx=false)",
		}
	}

	//

	//   current_n_ctx=32768, required_n_ctx=23816, n_ctx_override=16384
	//   prompt=15623 + n_predict=8192 + 1 = 23816 > 16384 (override) ? code 3
	//

	//

	if bridgeErr == nil {
		return &ReloadPlan{Decision: DecisionNoOp, Reason: "nil bridge error"}
	}

	if bridgeErr.Code == NCtxErrCodePromptTooLong {
		// BRIDGE_ERR_PROMPT_TOO_LONG (code 3) — prompt + n_predict exceeds current n_ctx.
		//
		// 2026-07-03 FIX: instead of rejecting immediately, try reload via adaptive strategy.
		// The adaptive loader can reduce gpuLayers to free VRAM for larger KV-cache,
		// enabling a larger n_ctx. Previously this was rejected because the model was
		// assumed to be at maximum n_ctx for the current gpuLayers.
		//
		// Calculate required n_ctx from actual prompt tokens + requested n_predict.
		required := bridgeErr.ActualTokens + bridgeErr.NPredict
		if required <= 0 {
			required = bridgeErr.RequiredNCtx
		}
		// R296 (2026-10-29): ЯВНО ЗАПРОШЕННОЕ клиентом окно — тоже нижняя
		// граница. Живой факт: клиент просил num_ctx=32768 при загруженных 16384,
		// но в ответе 503 стоял required=4098 (оценка ПРОМПТА), и по этому числу
		// нельзя было понять, сколько на самом деле нужно. Перезагружать модель
		// в окно МЕНЬШЕ запрошенного нельзя в принципе (R294), а сообщать клиенту
		// число меньше его запроса — значит врать о причине.
		if requestedNCtxOverride > required {
			required = requestedNCtxOverride
		}
		if required <= 0 {
			return c.makeRejectPlan(backendID, bridgeErr, required,
				"prompt_too_long: cannot determine required n_ctx from error info",
				"prompt_exceeds_context")
		}

		// Check configured max limit. R267: ceiling — эффективный потолок
		// (env, поднятый до заявленного окна оператора), а не сырое env-значение.
		//
		// R504 / аудит N-2: ceiling СУММАРНЫЙ, required — НА СЛОТ. Сравнивать их
		// напрямую значит пропускать перезагрузку, которая слоту ничего не даёт.
		requiredTotal := perClientToTotal(required, slots)
		if ceiling.Effective > 0 && requiredTotal > ceiling.Effective {
			return c.makeRejectPlan(backendID, bridgeErr, required,
				fmt.Sprintf("%s (нужно %d на слот × %d слота = %d суммарно)",
					ceiling.limitReason(requiredTotal, bridgeErr.MaxVRAMNCtx),
					required, slots, requiredTotal),
				"prompt_exceeds_context", ceiling)
		}

		// Allow reload even if required > max_vram_n_ctx (with current gpuLayers).
		// The adaptive strategy (queryAdaptiveStrategy in DoReload) will reduce
		// gpuLayers to free VRAM for the larger KV-cache.
		newNCtx := roundUpPow2(requiredTotal)
		clampedBy := ""
		if ceiling.Effective > 0 && newNCtx > ceiling.Effective {
			newNCtx = ceiling.Effective
			clampedBy = ceiling.source(bridgeErr.MaxVRAMNCtx)
		}

		plan := &ReloadPlan{
			Decision:         DecisionReload,
			NewNCtx:          newNCtx,
			EffectiveCeiling: ceiling.Effective,
			Reason: fmt.Sprintf("prompt_too_long: actual_tokens=%d n_predict=%d required_per_seq=%d slots=%d required_total=%d target=%d max_vram=%d",
				bridgeErr.ActualTokens, bridgeErr.NPredict, required, slots, requiredTotal, newNCtx, bridgeErr.MaxVRAMNCtx),
		}
		if clampedBy != "" {
			plan.LimitedBy = clampedBy
			plan.Reason += fmt.Sprintf(" limited_by=%s effective_ceiling=%d", clampedBy, ceiling.Effective)
		}
		return plan
	}

	if bridgeErr.Code != NCtxErrCodeNCtxNeedsReload {
		return &ReloadPlan{Decision: DecisionNoOp, Reason: "not a n_ctx-reload error"}

	}

	required := bridgeErr.RequiredNCtx
	if required <= 0 {
		required = requestedNCtxOverride
	}
	if required <= 0 {
		return &ReloadPlan{
			Decision: DecisionNoOp,
			Reason:   "no required n_ctx in error info (bridge bug?)",
		}
	}

	if bridgeErr.CurrentNCtx > 0 && required <= bridgeErr.CurrentNCtx {
		return &ReloadPlan{
			Decision: DecisionNoOp,
			Reason: fmt.Sprintf("required n_ctx=%d already covered by current=%d (no reload needed)",
				required, bridgeErr.CurrentNCtx),
		}
	}

	// R83-fix (2026-09-30): операторский потолок auto-reload НЕ должен отказывать
	// в запросе, который помещается в УЖЕ ЗАГРУЖЕННОЕ окно.
	//
	// Живой случай: модель загружена с 31841, cppworker вернул code=2 из-за
	// n_ctx_override=32768 (балансер посчитал потолком profile.contextLengthMax),
	// required=32768 > auto_reload_max_n_ctx=16384 → 413 «prompt + n_predict
	// exceeds n_ctx for this backend», хотя клиент просил 8192.
	//
	// Проверка выше (required <= CurrentNCtx → NoOp) уже реализует инвариант;
	// здесь — громкое предупреждение о конфигурации, из-за которой он нарушается.
	if ceiling.Effective > 0 && required > ceiling.Effective {
		if bridgeErr.CurrentNCtx > 0 && ceiling.Env > 0 && ceiling.Env < bridgeErr.CurrentNCtx {
			logger.Get().Errorw(i18n.T("balancer.nctx_reload.auto_reload_ceiling_below_loaded_error"),
				"backend", backendID,
				"loaded_n_ctx", bridgeErr.CurrentNCtx,
				"auto_reload_max_n_ctx", ceiling.Env,
				"effective_ceiling", ceiling.Effective,
				"required_n_ctx", required,
				"hint", i18n.T("balancer.nctx_reload.auto_reload_ceiling_below_loaded_hint"))
		}
		return c.makeRejectPlan(backendID, bridgeErr, required,
			ceiling.limitReason(required, bridgeErr.MaxVRAMNCtx),
			"n_ctx_too_large_for_backend", ceiling)
	}

	//

	//      head_dim = n_embd / n_heads.

	//

	maxVRAMNCtx := bridgeErr.MaxVRAMNCtx
	if maxVRAMNCtx <= 0 {

		if maxVRAMNCtx == 0 && bridgeErr.CurrentNCtx > 0 {
			return c.makeRejectPlan(backendID, bridgeErr, required,
				"model fits in VRAM but no space remains for KV-cache (max_vram_n_ctx=0). "+
					"Try reducing n_gpu_layers (CPU-offload) to free VRAM for context, "+
					"or use a smaller model / increase physical VRAM",
				"n_ctx_too_large_for_backend")
		}

		return c.makeRejectPlan(backendID, bridgeErr, required,
			"backend did not report max_vram_n_ctx (CPU-only, unknown GPU, or CUDA-query failed); cannot safely auto-reload",
			"n_ctx_too_large_for_backend", ceiling)
	}
	safety := cfg.effectiveSafetyFactor()
	safeMax := int(float64(maxVRAMNCtx) * safety)
	if required > safeMax {
		// Bug fix (Round 2): bridge.c рассчитывает max_vram_n_ctx для KV-cache f16
		// (4 bytes/token). q4_0 даёт 4x больше headroom (1 byte/token). Если
		// required укладывается в q4_0 + safety — пропускаем через reload:
		// cppworker /adaptive/strategy выберет q4_0 в DoReload
		// (см. nctx_reload.go:775 — queryAdaptiveStrategy).
		//
		// Также учитываем partial offload: если cfg.AutoReloadMaxNCtx настроен
		// >= required, считаем что пользователь явно готов на gpu_layers<all.
		safeMaxQ4 := int(float64(maxVRAMNCtx) * safety * 4)
		if required <= safeMaxQ4 {
			// R504 / аудит N-2: цель перезагрузки — СУММАРНОЕ окно, требование
			// воркера — на слот. Иначе слот получает цель/slots и снова 413.
			requiredTotal := perClientToTotal(required, slots)
			newNCtx := roundUpPow2(requiredTotal)
			if newNCtx < requiredTotal {
				newNCtx = requiredTotal
			}
			logger.Get().Infow("nctx_reload: required exceeds f16 safe_max but fits q4_0, accepting",
				"backend", backendID,
				"required_per_seq", required, "slots", slots, "required_total", requiredTotal,
				"safe_max_f16", safeMax, "safe_max_q4", safeMaxQ4)
			return &ReloadPlan{
				Decision: DecisionReload,
				NewNCtx:  newNCtx,
				Reason: fmt.Sprintf("required_per_seq=%d (slots=%d, total=%d) fits q4_0 (safe_max_f16=%d, q4_0=%d); adaptive loader will pick q4_0",
					required, slots, requiredTotal, safeMax, safeMaxQ4),
			}
		}
		return c.makeRejectPlan(backendID, bridgeErr, required,
			fmt.Sprintf("required n_ctx=%d exceeds safe VRAM limit=%d (max_vram_n_ctx=%d, safety=%.2f)",
				required, safeMax, maxVRAMNCtx, safety),
			"n_ctx_too_large_for_backend", ceiling)
	}

	// 4096 ? 4096, 5000 ? 8192, 8000 ? 8192, 12000 ? 16384
	//
	// R504 / аудит N-2: цель — СУММАРНОЕ окно (его получает воркер как
	// contextSize и делит между слотами), поэтому требование на слот
	// пересчитывается в суммарное ДО округления. `safeMax` — предел НА СЛОТ
	// (его считает bridge по max_vram_n_ctx), значит и сравнивать с ним надо в
	// тех же единицах: суммарный предел = safeMax × slots.
	requiredTotal := perClientToTotal(required, slots)
	newNCtx := roundUpPow2(requiredTotal)

	if newNCtx < requiredTotal {
		newNCtx = requiredTotal
	}

	if safeMaxTotal := perClientToTotal(safeMax, slots); newNCtx > safeMaxTotal {
		newNCtx = safeMaxTotal
	}

	// R267: тот же эффективный потолок, что и в ветке prompt_too_long —
	// иначе два выхода одного решателя принимали разные решения об одном окне.
	if ceiling.Effective > 0 && newNCtx > ceiling.Effective {
		newNCtx = ceiling.Effective
		limitedBy := ceiling.source(maxVRAMNCtx)
		return &ReloadPlan{
			Decision:         DecisionReload,
			NewNCtx:          newNCtx,
			EffectiveCeiling: ceiling.Effective,
			LimitedBy:        limitedBy,
			Reason: fmt.Sprintf("required=%d, current=%d, max_vram=%d, safe_max=%d, target=%d limited_by=%s effective_ceiling=%d",
				required, bridgeErr.CurrentNCtx, maxVRAMNCtx, safeMax, newNCtx, limitedBy, ceiling.Effective),
		}
	}

	return &ReloadPlan{
		Decision:         DecisionReload,
		NewNCtx:          newNCtx,
		EffectiveCeiling: ceiling.Effective,
		Reason: fmt.Sprintf("required=%d, current=%d, max_vram=%d, safe_max=%d, target=%d",
			required, bridgeErr.CurrentNCtx, maxVRAMNCtx, safeMax, newNCtx),
	}
}

//

//

func (c *NCtxReloadCoordinator) makeRejectPlan(
	backendID string,
	bridgeErr *NCtxBridgeError,
	required int,
	reason string,
	errorCode string,
	ceilings ...ReloadCeiling,
) *ReloadPlan {
	if errorCode == "" {
		errorCode = "n_ctx_too_large_for_backend"
	}
	cfg := c.Config()
	safe := 0
	if bridgeErr.MaxVRAMNCtx > 0 {
		safe = int(float64(bridgeErr.MaxVRAMNCtx) * cfg.effectiveSafetyFactor())
	}

	// R267: в тело отказа кладём эффективный потолок и его источник — оператор
	// должен видеть «заявлено X, ограничено Y, причина Z» в ответе, а не только
	// в логе балансера.
	ceiling := ReloadCeiling{Effective: cfg.AutoReloadMaxNCtx, Env: cfg.AutoReloadMaxNCtx}
	if len(ceilings) > 0 {
		ceiling = ceilings[0]
	}
	limitedBy := ceiling.source(bridgeErr.MaxVRAMNCtx)

	// R402 (2026-11-05): текст подсказки больше не отправляет оператора
	// «сохранить профиль и перезагрузить вручную» — балансер растит окно сам
	// (R400), поэтому отказ здесь означает, что вырасти НЕКУДА: упёрлись в
	// training context модели или в память. Объясняем единицы и реальные рычаги.
	//
	// Отдельно про parallel: при parallel=2 окно модели делится между слотами, и
	// одна сессия получает половину. Это частая причина «не даёт поднять окно».
	suggestion := "the balancer already tried to grow the window automatically: requested total n_ctx " +
		"exceeds the model/backend ceiling (training context or available VRAM/RAM). " +
		"Reduce conversation history, tools[] and the system prompt, or reload the model with a smaller " +
		"parallel value: slots divide the window, so parallel=1 gives a single session the whole training context."
	if errorCode == "prompt_exceeds_context" {
		suggestion = "reduce conversation history / tools[] / system prompt, or use a smaller model. " +
			"Reload with the same or larger n_ctx will NOT help — the model is already loaded at maximum."
	}
	rej := map[string]interface{}{
		"error":             errorCode,
		"backend_id":        backendID,
		"reason":            reason,
		"requested_n_ctx":   required,
		"current_n_ctx":     bridgeErr.CurrentNCtx,
		"max_vram_n_ctx":    bridgeErr.MaxVRAMNCtx,
		"safe_max_n_ctx":    safe,
		"suggestion":        suggestion,
		"profile_endpoint":  "/api/profiles",
		"reload_endpoint":   "/api/models/reload",
		"bridge_code":       bridgeErr.Code,
		"bridge_message":    bridgeErr.Message,
		"effective_ceiling": ceiling.Effective,
		"limited_by":        limitedBy,
	}
	b, _ := json.Marshal(rej)
	return &ReloadPlan{
		Decision:         DecisionReject,
		Reason:           reason,
		RejectMsg:        string(b),
		EffectiveCeiling: ceiling.Effective,
		LimitedBy:        limitedBy,
	}
}

// nCtxLimitReason — причина отказа «окно больше потолка авто-перезагрузки».
//
// R140 (2026-10-16): предел зависит от двух чисел, и называть надо то, которое
// реально связывает. max_vram_n_ctx воркер считает по фактически свободной VRAM,
// поэтому он учитывает и другие загруженные модели. Если связывает он, совет
// «поднять LB_NCTX_RELOAD_MAX_N_CTX» невыполним — окно не влезет. Когда
// физический потолок неизвестен (0), текст прежний: число не выдумываем.
// Структурированное тело отказа (makeRejectPlan) несёт оба числа полями
// max_vram_n_ctx и safe_max_n_ctx независимо от этой строки.
func nCtxLimitReason(required, operatorMax, physicalMax int) string {
	if physicalMax > 0 && physicalMax < operatorMax {
		return fmt.Sprintf("required n_ctx=%d exceeds the backend's physical limit max_vram_n_ctx=%d "+
			"(free VRAM with the models loaded now); operator limit auto_reload_max_n_ctx=%d is higher",
			required, physicalMax, operatorMax)
	}
	return fmt.Sprintf("required n_ctx=%d exceeds configured max=%d", required, operatorMax)
}

func roundUpPow2(v int) int {
	if v <= 0 {
		return 512
	}
	if v < 512 {
		return 512
	}
	p := 1
	for p < v {
		p <<= 1
	}
	return p
}

// ============================================================

// ============================================================
