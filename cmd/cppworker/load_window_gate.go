// load_window_gate.go — R522 (2026-11-19, §8.3.3 плана): гейт n_ctx обязан
// проверять ФИНАЛЬНОЕ окно загрузки, а не «то, что лежало в opts до пересчёта».
//
// ЖИВОЙ ФАКТ (архив 2026-11-18, A10 24 ГБ, 27B Q4_K_M): в логе загрузки рядом
// стоят две строки — гейт «requested_n_ctx=32768 stage=exact_fit
// feasible_max_context=182340» и через 52 мс «total_n_ctx=131072,
// ctxSize=131072». То есть гейт проверил профильный хинт 32768, а загрузили
// 131072: пересчёт «окно на клиента → суммарное» (PAD256(per) × слоты) живёт
// ВНУТРИ LoadModelWithOpts (backend_load.go:95-112), уже ПОСЛЕ проверки. KV
// вырос вчетверо, на compute buffer места не осталось — первый же llama_decode
// упал с CUDA OOM.
//
// Здесь та же формула (cppbackend.LoadWindowNCtx) применяется ДО гейта: одна
// формула на проверку и на загрузку, второй копии нет.
package main

import (
	"ollama-loadbalancer/internal/cppbackend"
)

// loadWindowForGate — суммарное окно и число слотов, с которыми модель реально
// загрузится.
//
// contextPerSeq > 0 (R83: «окно на клиента») → total = PAD256(per) × слоты;
// иначе total = ContextSize как есть. Слоты: per-model parallel, затем дефолт
// конфига, затем 1 — тот же порядок, что в LoadModelWithOpts.
func loadWindowForGate(opts cppbackend.LoadModelOpts) (total, slots int) {
	defaultParallel := 0
	if currentConfig != nil {
		defaultParallel = currentConfig.DefaultNParallel
	}
	return cppbackend.LoadWindowNCtx(opts.ContextSize, opts.ContextPerSeq, opts.Parallel, defaultParallel)
}
