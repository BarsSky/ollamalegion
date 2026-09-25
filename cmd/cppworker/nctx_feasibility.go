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
