// cmd/cppworker/cli_feasible.go — Round 37 (2026-08-18) CLI.
//
// -feasible <model> — operator tool to print feasible n_ctx для модели
// БЕЗ загрузки. Используется для:
//   1. Валидации profile перед -load (не превышает ли profile hardware?)
//   2. Поиск оптимального n_ctx для auto-load
//   3. Отладка preflight 413 (почему "requested n_ctx=65536 exceeds max")
//
// Пример:
//
//	$ cppworker -feasible Qwen3.6-35B-A3B-UD-Q4_K_M
//	Model:           Qwen3.6-35B-A3B-UD-Q4_K_M
//	GGUF max:        262144
//	Max VRAM ctx:    0 (no free VRAM, model uses all 8GB)
//	Max RAM ctx:     131072 (20GB free / 40960 bytes per token)
//	KV cache type:   q4_0
//	KV per token:    10240 bytes
//	Source:          gguf
//
//	Auto-recommended n_ctx: 131072 (min of VRAM, RAM, GGUF)
//
// КРИТИЧНО: в Round 37 эта команда — единственный способ узнать feasible
// без загрузки модели (раньше нужно было load + measure + reload).
package main

import (
	"fmt"
	"io"
	"os"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
)

// runFeasible печатает FeasibleInfo для указанной модели и выходит с кодом 0.
//
// Exit codes:
//   0 — успех (модель найдена, feasible вычислен)
//   1 — backend не инициализирован, или ComputeFeasible упал
//   2 — usage error (modelName == "")
func runFeasible(modelName string, out io.Writer) int {
	log := logger.Get()
	if modelName == "" {
		fmt.Fprintln(out, "usage: cppworker -feasible <model>")
		return 2
	}

	b := cppbackend.GetBackend()
	if b == nil {
		fmt.Fprintln(out, "error: backend not initialized (start cppworker first or use -auto-init mode)")
		return 1
	}

	info, err := b.ComputeFeasible(modelName)
	if err != nil {
		log.Errorw("ComputeFeasible failed", "model", modelName, "error", err)
		fmt.Fprintf(out, "error: %v\n", err)
		return 1
	}

	// Pretty print
	fmt.Fprintf(out, "Model:           %s\n", modelName)
	fmt.Fprintf(out, "GGUF max:        %d\n", info.GGUFMax)
	if info.MaxVRAMCtx == 0 {
		fmt.Fprintf(out, "Max VRAM ctx:    0 (no free VRAM, model uses all available)\n")
	} else {
		fmt.Fprintf(out, "Max VRAM ctx:    %d\n", info.MaxVRAMCtx)
	}
	if info.MaxRAMCtx == 0 {
		fmt.Fprintf(out, "Max RAM ctx:     0 (no free RAM, system under pressure)\n")
	} else {
		fmt.Fprintf(out, "Max RAM ctx:     %d\n", info.MaxRAMCtx)
	}
	fmt.Fprintf(out, "KV cache type:   %s\n", info.KVCacheType)
	// R705 (2026-11-24): ширина головы и её ИСТОЧНИК — без этого «KV per token»
	// не проверить: до правки ширина считалась как n_embd/n_heads (Qwen3-4B —
	// 80 вместо 128), и число выглядело измеренным.
	fmt.Fprintf(out, "Head dim K/V:    %d/%d (%s)\n",
		info.HeadDimK, info.HeadDimV, feasibleHeadDimSourceLabelR705(info.HeadDimSource))
	fmt.Fprintf(out, "KV per token:    %d bytes\n", info.KVPerToken)
	fmt.Fprintf(out, "Free VRAM:       %d MB\n", info.FreeVRAMMB)
	fmt.Fprintf(out, "Free RAM:        %d MB\n", info.FreeRAMMB)
	fmt.Fprintf(out, "Source:          %s\n", info.Source)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Auto-recommended n_ctx: %d (min of VRAM, RAM, GGUF)\n",
		feasibleAutoRecommended(info))
	return 0
}

// feasibleHeadDimSourceLabelR705 — человекочитаемый источник ширины головы для
// `-feasible`: gguf = <arch>.attention.key_length из модели, computed = прежняя
// формула n_embd/n_heads (оценка), пусто = данных нет.
func feasibleHeadDimSourceLabelR705(source string) string {
	switch source {
	case bridge.HeadDimSourceGGUF:
		return "gguf: <arch>.attention.key_length"
	case bridge.HeadDimSourceComputed:
		return "computed: n_embd/n_heads (оценка)"
	default:
		return "неизвестно"
	}
}

// feasibleAutoRecommended вычисляет recommended n_ctx = min(VRAM, RAM, GGUF).
// 0-значения трактуются как "unknown" и пропускаются.
func feasibleAutoRecommended(info *cppbackend.FeasibleInfo) int {
	if info == nil {
		return 0
	}
	candidates := []int{}
	if info.MaxVRAMCtx > 0 {
		candidates = append(candidates, info.MaxVRAMCtx)
	}
	if info.MaxRAMCtx > 0 {
		candidates = append(candidates, info.MaxRAMCtx)
	}
	if info.GGUFMax > 0 {
		candidates = append(candidates, info.GGUFMax)
	}
	if len(candidates) == 0 {
		return 0
	}
	min := candidates[0]
	for _, c := range candidates[1:] {
		if c < min {
			min = c
		}
	}
	return min
}

// runFeasibleMain — entry point из main.go. Проверяет флаг и вызывает runFeasible.
// ВАЖНО: возвращает os.Exit code напрямую.
func runFeasibleMain(modelName string) {
	os.Exit(runFeasible(modelName, os.Stdout))
}
