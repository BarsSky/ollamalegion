package cppbackend

import ()

// ContextPadStep — шаг выравнивания n_ctx_seq в llama.cpp (GGML_PAD(_, 256)).
const ContextPadStep = 256

// padContext — выравнивание окна вверх до шага llama.cpp.
func padContext(n int) int {
	if n <= 0 {
		return 0
	}
	step := ContextPadStep
	return ((n + step - 1) / step) * step
}

// PadContext — экспортированный вариант padContext: балансер пересчитывает
// «окно на клиента» в суммарное тем же правилом, что и llama.cpp.
func PadContext(n int) int { return padContext(n) }

// ContextPerSlot — R83 (2026-09-30): сколько токенов окна реально получит ОДИН
// клиент (слот) при суммарном totalNCtx и slots слотах.
//
// Повторяет правило llama.cpp для kv_unified=false:
//
//	n_ctx_seq = GGML_PAD(n_ctx / n_seq_max, 256)
//
// (см. c/llama.cpp/src/llama-context.cpp — там же llama.cpp печатает
// «n_ctx_seq = ...» в лог, и это же число видно оператору).
//
// Зачем в Go: иначе WebUI/API показывают суммарное окно как будто оно на клиента,
// и оператор, выставив 16384 + parallel=2, молча получает по 8192.
func ContextPerSlot(totalNCtx, slots int) int {
	if totalNCtx <= 0 {
		return 0
	}
	if slots < 1 {
		slots = 1
	}
	return padContext(totalNCtx / slots)
}

// EffectiveContextSize — R83 (2026-09-30): единая точка, где «окно на клиента»
// превращается в суммарный n_ctx, который уходит в memfit и в C-bridge.
//
//   - perSeq > 0 → total = PAD256(perSeq) × slots (окно НА КЛИЕНТА задано явно);
//   - иначе      → total = ContextSize как есть (прежняя семантика: суммарное).
//
// Возвращает (total, perSeqActual): второе — фактическое окно на клиента для
// логов и /api/models (0, если считать не из чего).
// LoadWindowNCtx — R522 (§8.3.3): итоговое СУММАРНОЕ окно, с которым модель
// загрузится, и число слотов — для гейта ДО загрузки.
//
// ЕДИНСТВЕННАЯ формула пересчёта «окно на клиента → суммарное» живёт в
// EffectiveContextSize; здесь только резолв слотов (тот же resolveNParallel, что
// в LoadModelWithOpts). Второй копии расчёта быть не должно: именно расхождение
// «гейт проверил 32768, загрузка взяла 131072» и привело к CUDA OOM из архива
// 2026-11-18.
func LoadWindowNCtx(contextSize, contextPerSeq, parallel, defaultParallel int) (total, slots int) {
	slots = resolveNParallel(parallel, defaultParallel)
	total, _ = EffectiveContextSize(contextSize, contextPerSeq, slots)
	return total, parallelSlotsFor(slots)
}
func EffectiveContextSize(contextSize, perSeq, slots int) (int, int) {
	slots = parallelSlotsFor(slots)
	if perSeq > 0 {
		per := padContext(perSeq)
		return per * slots, per
	}
	if contextSize > 0 {
		return contextSize, ContextPerSlot(contextSize, slots)
	}
	return contextSize, 0
}
