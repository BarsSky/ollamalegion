// nctx_feasibility.go — R83 (2026-09-25): пред-загрузочная оценка n_ctx.
//
// ЗАЧЕМ. Живая проверка на A10 (Qwen3.8-27B-UD-Q4_K_M, 15.3 GB, 64 слоя):
// при n_ctx=32768 модель отвечает без ошибок, при 65536 и 128000 — нет.
// По коду граница объясняется сменой РЕЖИМА, а не тем, что «не влезло»:
//
//   - 32768 → SelectStrategyWithKV выбирает exact_fit (все слои на GPU);
//   - 65536 → уже не укладывается в safeVRAM → partial_offload (часть слоёв
//     в RAM через mmap): резко медленнее, упирается в таймауты клиента;
//   - дальше → cpu_only / fallback_no_fit.
//
// При этом MaxViableNCtx в обеих ветках просто равен requestedNCtx
// (adaptive_loader.go), то есть наружу НЕ сообщается, что режим деградировал:
// оператор видит «загрузилось» и не понимает, почему стало плохо.
//
// ГИБРИДНАЯ ПОЛИТИКА (выбор владельца проекта, R83):
//   1. режим показываем ВСЕГДА (эта функция + лог; UI — отдельным шагом);
//   2. отказ (422) — только когда запрошенное физически невыполнимо, то есть
//      выше RAM-границы или выше обучающего контекста модели.
//      partial_offload / cpu_only остаются РАЗРЕШЁННЫМИ: это легитимный способ
//      запустить большую модель на маленьком GPU, запрещать его нельзя.
//
// ВАЖНО про провод: cppworker декодирует тела строгим декодером
// (types.DecodeJSONRequest, DisallowUnknownFields), и в репозитории есть
// guard-тесты (internal/balancer/contract/cppworker_contract_test.go,
// nctx_reload_payload_test.go), которые требуют, чтобы балансер НЕ слал поля
// `reason` / `adaptiveStage`. Поэтому новые поля нельзя просто «добавить в
// запрос» — сначала структура на обеих сторонах, потом тесты. Эта функция
// намеренно ничего не меняет на проводе: она только считает и логирует.

package main

import (
	"fmt"
	"net/http"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// Стадии режима загрузки. Строки совпадают со `Stage` из LoadStrategyResult
// (adaptive_loader.go) там, где это возможно, чтобы лог читался одинаково.
const (
	nctxStageExactFit   = "exact_fit"  // всё влезает в VRAM
	nctxStageDegraded   = "degraded"   // partial_offload / cpu_only: часть в RAM
	nctxStageInfeasible = "infeasible" // выше RAM-границы или обучающего контекста
	nctxStageUnknown    = "unknown"    // метаданных нет — не судим (fail-open)
)

// NCtxFeasibility — результат пред-загрузочной оценки.
type NCtxFeasibility struct {
	Model           string
	RequestedNCtx   int
	MaxVRAMNCtx     int // макс. n_ctx, влезающий в свободную VRAM (exact_fit)
	MaxRAMNCtx      int // макс. n_ctx с выгрузкой весов в RAM
	GGUFMaxContext  int // обучающий контекст модели (жёсткий верх)
	FeasibleMaxNCtx int // min(VRAM, RAM, GGUF) — то, что уходит в /api/models
	HardMaxNCtx     int // min(RAM, GGUF) — физический предел без учёта VRAM
	Degraded        bool
	Known           bool   // удалось ли вообще что-то посчитать
	Stage           string // см. nctxStage*
	// VerdictSuggestion — текст подсказки из internal/memfit (R83 шаг 3). Если
	// задан, он предпочтительнее собранного здесь: memfit добавляет к нему причину
	// «поможет квантизация KV», которую прежняя логика не знала.
	VerdictSuggestion string
}

// evaluateNCtxFeasibility применяет гибридную политику R83 к посчитанным
// лимитам ресурсов. Ноль в любом из полей означает «неизвестно» и НЕ участвует
// в минимумах — иначе отсутствие метрик GPU превращалось бы в отказ всем.
func evaluateNCtxFeasibility(model string, l cppbackend.ResourceLimits, requested int) NCtxFeasibility {
	f := NCtxFeasibility{
		Model:          model,
		RequestedNCtx:  requested,
		MaxVRAMNCtx:    l.MaxVRAMNCtx,
		MaxRAMNCtx:     l.MaxRAMNCtx,
		GGUFMaxContext: l.ModelMaxContext,
	}

	minPositive := func(vals ...int) int {
		out := 0
		for _, v := range vals {
			if v > 0 && (out == 0 || v < out) {
				out = v
			}
		}
		return out
	}

	f.FeasibleMaxNCtx = minPositive(l.MaxVRAMNCtx, l.MaxRAMNCtx, l.ModelMaxContext)
	f.HardMaxNCtx = minPositive(l.MaxRAMNCtx, l.ModelMaxContext)
	f.Known = f.FeasibleMaxNCtx > 0

	switch {
	case !f.Known:
		// Метрик нет (нет GPU/GGUF/агента) — не отказываем и не врём про режим.
		f.Stage = nctxStageUnknown
	case f.HardMaxNCtx > 0 && requested > f.HardMaxNCtx:
		f.Stage = nctxStageInfeasible
	case f.MaxVRAMNCtx == 0:
		// VRAM неизвестна (например, нет GPU-метрик): про режим сказать нечего,
		// поэтому НЕ выдаём exact_fit — иначе лог утверждал бы то, чего мы не знаем.
		f.Stage = nctxStageUnknown
	case requested > f.MaxVRAMNCtx:
		f.Stage = nctxStageDegraded
		f.Degraded = true
	default:
		f.Stage = nctxStageExactFit
	}
	return f
}

// logNCtxFeasibility пишет оценку в лог. Это «нулевая половина» гибридной
// политики: режим становится видимым, ничего не ломая. Отказ (422) и вывод в UI
// добавляются отдельно.
//
// Раньше этот случай не логировался вообще: /api/models/load рапортовал успех,
// а оператор видел только «стало медленно» без причины.
func logNCtxFeasibility(f NCtxFeasibility) {
	log := logger.Get()
	fields := []interface{}{
		"model", f.Model,
		"requested_n_ctx", f.RequestedNCtx,
		"stage", f.Stage,
		"max_vram_n_ctx", f.MaxVRAMNCtx,
		"max_ram_n_ctx", f.MaxRAMNCtx,
		"gguf_max_context", f.GGUFMaxContext,
		"feasible_max_context", f.FeasibleMaxNCtx,
	}
	switch f.Stage {
	case nctxStageInfeasible:
		log.Warnw("R83 n_ctx превышает физический предел: загрузка обречена "+
			"(выше RAM-границы или обучающего контекста модели)", fields...)
	case nctxStageDegraded:
		log.Warnw("R83 n_ctx выше VRAM-границы: загрузка уйдёт в partial_offload/cpu_only "+
			"(часть слоёв в RAM, ответ будет заметно медленнее; для A10 24 GB "+
			"используйте n_ctx в пределах max_vram_n_ctx и kvCacheType=q4_0)", fields...)
	case nctxStageExactFit:
		log.Infow("R83 n_ctx укладывается в VRAM (exact_fit)", fields...)
	default:
		log.Debugw("R83 n_ctx-оценка недоступна: нет метрик VRAM/RAM/GGUF", fields...)
	}
}

// nctxSuggestion — что оператору/клиенту сделать вместо отклонённого n_ctx.
// Возвращает конкретные числа, а не общий совет: это интерфейс между отказом и
// уведомлением в WebUI (блок 6 плана).
func nctxSuggestion(f NCtxFeasibility) string {
	// R83 шаг 3: если решение принял memfit, его подсказка конкретнее (в ней есть
	// и потолок, и — когда применимо — выигрыш от kvCacheType=q4_0).
	if f.VerdictSuggestion != "" {
		return f.VerdictSuggestion
	}
	switch {
	case f.GGUFMaxContext > 0 && f.HardMaxNCtx == f.GGUFMaxContext:
		return fmt.Sprintf("n_ctx ограничен обучающим контекстом модели: %d. "+
			"Укажите n_ctx <= %d.", f.GGUFMaxContext, f.GGUFMaxContext)
	case f.FeasibleMaxNCtx > 0:
		return fmt.Sprintf("Для загрузки целиком в VRAM используйте n_ctx <= %d "+
			"(feasible_max_context). До %d возможно с выгрузкой части слоёв в RAM "+
			"(partial_offload) — это заметно медленнее.",
			f.FeasibleMaxNCtx, f.HardMaxNCtx)
	default:
		return "Снизьте n_ctx или используйте модель/железо с большим объёмом памяти."
	}
}

// writeNCtxInfeasibleResponse — HTTP 422 со всеми числами.
//
// ВАЖНО про код ответа: 422, а НЕ 503. Балансер ретраит только 503 с признаком
// «model is loading» (internal/balancer/model_management.go:1223); на любой
// другой не-2xx он возвращает ошибку сразу («Другая ошибка — не повторяем»,
// :1245). Поэтому 422 доходит до клиента как внятный отказ, а не превращается
// в retry-шторм и не даёт 503-поллинг до 3-15 минут (см. load_failures.go).
//
// Тело намеренно содержит поле "error" — его читают и балансер (:1218), и
// WebUI/клиенты, не разбирающие структуру.
func writeNCtxInfeasibleResponse(w http.ResponseWriter, f NCtxFeasibility) {
	// R83 (2026-09-25): запоминаем причину провала с числами — её заберёт agent
	// из /api/models и покажет оператору уведомление (cppworker → agent → webui).
	// Пишем здесь, а не у вызывающих: это единственная воронка этого исхода.
	loadFailures.recordDetailed(f.Model, LoadFailureConfigOutOfBounds,
		fmt.Errorf("requested n_ctx=%d exceeds the physically feasible maximum %d",
			f.RequestedNCtx, f.HardMaxNCtx),
		map[string]interface{}{
			"requested_n_ctx":      f.RequestedNCtx,
			"feasible_max_context": f.FeasibleMaxNCtx,
			"max_vram_n_ctx":       f.MaxVRAMNCtx,
			"max_ram_n_ctx":        f.MaxRAMNCtx,
			"gguf_max_context":     f.GGUFMaxContext,
			"hard_max_n_ctx":       f.HardMaxNCtx,
			"suggestion":           nctxSuggestion(f),
		})

	writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
		"error": fmt.Sprintf(
			"requested n_ctx=%d exceeds the physically feasible maximum %d for model %q "+
				"on this hardware (max_vram_n_ctx=%d, max_ram_n_ctx=%d, gguf_max_context=%d)",
			f.RequestedNCtx, f.HardMaxNCtx, f.Model,
			f.MaxVRAMNCtx, f.MaxRAMNCtx, f.GGUFMaxContext),
		"code":                 "n_ctx_infeasible",
		"model":                f.Model,
		"requested_n_ctx":      f.RequestedNCtx,
		"feasible_max_context": f.FeasibleMaxNCtx,
		"max_vram_n_ctx":       f.MaxVRAMNCtx,
		"max_ram_n_ctx":        f.MaxRAMNCtx,
		"gguf_max_context":     f.GGUFMaxContext,
		"hard_max_n_ctx":       f.HardMaxNCtx,
		"suggestion":           nctxSuggestion(f),
	})
}

// checkNCtxBeforeLoad — общая точка входа для всех load/reload-хендлеров
// (handleLoadModel, handleLoadWithParams, handleReloadModel).
//
// Возвращает false, если запрос ОТКЛОНЁН — ответ уже записан, вызывающий обязан
// сделать return. Всегда логирует оценку (в т.ч. degraded), чтобы режим загрузки
// был виден оператору до её старта.
//
// kvCacheType — тип KV-cache, с которым пойдёт ИМЕННО эта загрузка ("" = взять
// дефолт конфига). Гейт обязан считать KV тем же типом, что и раскладка слоёв
// (R83 §3.4): иначе для модели, у которой профиль задаёт q8_0, а дефолт q4_0,
// потолок VRAM расходится в 1.88 раза.
func checkNCtxBeforeLoad(w http.ResponseWriter, modelName string, requestedNCtx int, kvCacheType string) bool {
	// R83, шаг 3: решение принимает internal/memfit (memfit_gate.go). Прежний
	// расчёт здесь удалён — он расходился с решением о загрузке.
	if backend == nil {
		return true
	}
	spec, ok := backend.MemfitSpec(modelName)
	if !ok {
		// Метаданных модели нет — судить не о чем. Не отказываем (как и раньше),
		// но говорим об этом явно, а не молча (прежний fail-open был неотличим от
		// «всё хорошо»).
		if logger.Get() != nil {
			logger.Get().Debugw("R83 гейт n_ctx: метаданные модели недоступны — проверка пропущена",
				"model", modelName, "requested_n_ctx", requestedNCtx)
		}
		return true
	}
	resolvedKV := effectiveKVCacheTypeForLoad(modelName, kvCacheType)
	logResolvedKVCacheType(modelName, resolvedKV)
	f, verdict, ok := feasibilityFromMemfit(spec, backend.MemfitBudget(), resolvedKV, requestedNCtx)
	if !ok {
		return true
	}
	logMemfitVerdict(verdict)
	logNCtxFeasibility(f)

	// R83 (C1): предупреждаем, если ПОТОЛОК ИЗ КОНФИГА недостижим на этом железе.
	//
	// Живой случай: CPPWORKER_RAM_FALLBACK_MAX_N_CTX=128000 и
	// LB_NCTX_RELOAD_MAX_N_CTX=131072 при том, что для 27B Q4_K_M на A10 24 GB
	// физический предел заметно ниже. Конфиг обещал то, чего железо не даёт, и
	// оператор не понимал, почему 65536/128000 «не работают».
	//
	// Отказ при этом выдаётся по ФАКТИЧЕСКОМУ feasible (выше в этой функции),
	// а не по конфигу — значение из .env остаётся верхней границей, но перестаёт
	// молча обещать невозможное.
	if *ramFallbackMaxNCtx > 0 && f.HardMaxNCtx > 0 && *ramFallbackMaxNCtx > f.HardMaxNCtx {
		logger.Get().Warnw("R83 потолок n_ctx из конфига недостижим на этом железе: "+
			"CPPWORKER_RAM_FALLBACK_MAX_N_CTX больше физического предела — "+
			"уменьшите его, иначе конфиг обещает то, чего не будет",
			"model", f.Model,
			"configured_max_n_ctx", *ramFallbackMaxNCtx,
			"hard_max_n_ctx", f.HardMaxNCtx,
			"feasible_max_context", f.FeasibleMaxNCtx,
			"max_vram_n_ctx", f.MaxVRAMNCtx,
			"max_ram_n_ctx", f.MaxRAMNCtx)
	}

	if f.Stage == nctxStageInfeasible {
		writeNCtxInfeasibleResponse(w, f)
		return false
	}
	return true
}

// nctxModeFields — поля РЕЖИМА загрузки для одной модели в /api/models.
//
// Зачем: context_size модели уже отдавался, но не было видно, ВЛЕЗ ли он в VRAM.
// При 32768 стратегия выбирает exact_fit, при 65536 — partial_offload (часть
// слоёв в RAM, ответ медленнее), и наружу это никак не сообщалось: оператор
// видел «загружено» в обоих случаях. Здесь режим становится машинночитаемым,
// чтобы WebUI показал его, а не только лог.
//
// vramKnown различает «VRAM неизвестна» и «известна, но веса не влезают»:
// раньше ноль в vramMax означал и то и другое, и n_ctx_degraded молчал.
func nctxModeFields(vramMax, ggufMax, feasibleMax, contextSize int, vramKnown bool) map[string]interface{} {
	fields := map[string]interface{}{
		"gguf_max_context":     ggufMax,
		"feasible_max_context": feasibleMax,
		"max_vram_n_ctx":       vramMax,
		"vram_known":           vramKnown,
		"n_ctx_degraded":       vramKnown && contextSize > vramMax,
	}
	if vramKnown {
		fields["n_ctx_headroom"] = vramMax - contextSize
	}
	return fields
}
