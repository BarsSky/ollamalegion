//go:build llama_stub

// nctx_feasibility_r83_test.go — R83 (2026-09-25).
//
// Проверяет гибридную политику R83 (см. nctx_feasibility.go):
//   - режим классифицируется всегда, когда метрики есть;
//   - отказ (infeasible) — только когда запрошенное физически невыполнимо:
//     выше RAM-границы или выше обучающего контекста модели;
//   - partial_offload (запрос выше VRAM-границы, но в пределах RAM) — НЕ отказ,
//     а деградация: это легитимный способ запустить большую модель на малом GPU;
//   - неизвестные метрики (нули) не превращаются ни в отказ, ни в ложный exact_fit.
package main

import (
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

func TestR83_EvaluateNCtxFeasibility(t *testing.T) {
	cases := []struct {
		name        string
		limits      cppbackend.ResourceLimits
		requested   int
		wantStage   string
		wantFeasib  int
		wantHard    int
		wantKnown   bool
		wantDegrade bool
	}{
		{
			// Живой случай с A10: модель (27B Q4_K_M, 15.3 GB) и 32768 —
			// всё влезает в VRAM, это и есть рабочий режим.
			name:       "A10 / 32768 — exact_fit",
			limits:     cppbackend.ResourceLimits{MaxVRAMNCtx: 40000, MaxRAMNCtx: 200000, ModelMaxContext: 262144},
			requested:  32768,
			wantStage:  nctxStageExactFit,
			wantFeasib: 40000,
			wantHard:   200000,
			wantKnown:  true,
		},
		{
			// Тот же стенд, 65536: в VRAM не влезает, но в RAM — влезает.
			// Это НЕ отказ: стратегия уйдёт в partial_offload, ответ станет
			// медленнее. Оператор должен увидеть это в логе, но запрос не рубим.
			name:        "A10 / 65536 — degraded (partial_offload), не отказ",
			limits:      cppbackend.ResourceLimits{MaxVRAMNCtx: 40000, MaxRAMNCtx: 200000, ModelMaxContext: 262144},
			requested:   65536,
			wantStage:   nctxStageDegraded,
			wantFeasib:  40000,
			wantHard:    200000,
			wantKnown:   true,
			wantDegrade: true,
		},
		{
			name:       "выше RAM-границы — infeasible",
			limits:     cppbackend.ResourceLimits{MaxVRAMNCtx: 40000, MaxRAMNCtx: 100000, ModelMaxContext: 262144},
			requested:  128000,
			wantStage:  nctxStageInfeasible,
			wantFeasib: 40000,
			wantHard:   100000,
			wantKnown:  true,
		},
		{
			name:       "выше обучающего контекста модели — infeasible",
			limits:     cppbackend.ResourceLimits{MaxVRAMNCtx: 262144, MaxRAMNCtx: 200000, ModelMaxContext: 32768},
			requested:  65536,
			wantStage:  nctxStageInfeasible,
			wantFeasib: 32768,
			wantHard:   32768,
			wantKnown:  true,
		},
		{
			// GGUF-метаданных нет (ContextLength=0). Это НЕ повод отказывать:
			// hard-граница считается по RAM, значит 128000 при RAM=200000 — degraded.
			name:        "GGUF неизвестен — не отказ, а деградация",
			limits:      cppbackend.ResourceLimits{MaxVRAMNCtx: 40000, MaxRAMNCtx: 200000, ModelMaxContext: 0},
			requested:   128000,
			wantStage:   nctxStageDegraded,
			wantFeasib:  40000,
			wantHard:    200000,
			wantKnown:   true,
			wantDegrade: true,
		},
		{
			// VRAM неизвестна: нельзя утверждать exact_fit, но и отказывать не за что.
			name:       "VRAM неизвестна — unknown, не exact_fit",
			limits:     cppbackend.ResourceLimits{MaxVRAMNCtx: 0, MaxRAMNCtx: 200000, ModelMaxContext: 262144},
			requested:  65536,
			wantStage:  nctxStageUnknown,
			wantFeasib: 200000,
			wantHard:   200000,
			wantKnown:  true,
		},
		{
			// Метрик нет вообще (stub без GPU). Отказывать нельзя — иначе
			// сломаются стенды и тесты, где GPU отсутствует.
			name:       "метрик нет — unknown, fail-open",
			limits:     cppbackend.ResourceLimits{},
			requested:  131072,
			wantStage:  nctxStageUnknown,
			wantFeasib: 0,
			wantHard:   0,
			wantKnown:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateNCtxFeasibility("Qwen3.8-27B-UD-Q4_K_M", tc.limits, tc.requested)
			if got.Stage != tc.wantStage {
				t.Errorf("Stage = %q, want %q", got.Stage, tc.wantStage)
			}
			if got.FeasibleMaxNCtx != tc.wantFeasib {
				t.Errorf("FeasibleMaxNCtx = %d, want %d", got.FeasibleMaxNCtx, tc.wantFeasib)
			}
			if got.HardMaxNCtx != tc.wantHard {
				t.Errorf("HardMaxNCtx = %d, want %d", got.HardMaxNCtx, tc.wantHard)
			}
			if got.Known != tc.wantKnown {
				t.Errorf("Known = %v, want %v", got.Known, tc.wantKnown)
			}
			if got.Degraded != tc.wantDegrade {
				t.Errorf("Degraded = %v, want %v", got.Degraded, tc.wantDegrade)
			}
			if got.RequestedNCtx != tc.requested {
				t.Errorf("RequestedNCtx = %d, want %d", got.RequestedNCtx, tc.requested)
			}
		})
	}
}

// TestR83_Feasibility_NeverRefusesOnUnknownMetrics — отдельный гард на самое
// опасное поведение: отказ из-за отсутствия метрик. Живой стенд без GPU-метрик
// (или без GGUF-заголовка) обязан продолжать работать как раньше.
func TestR83_Feasibility_NeverRefusesOnUnknownMetrics(t *testing.T) {
	for _, req := range []int{4096, 32768, 65536, 131072, 262144, 1000000} {
		got := evaluateNCtxFeasibility("some-model", cppbackend.ResourceLimits{}, req)
		if got.Stage == nctxStageInfeasible {
			t.Errorf("requested=%d: получен infeasible без метрик — стенд сломается", req)
		}
		if got.Known {
			t.Errorf("requested=%d: Known=true без метрик", req)
		}
	}
}

// TestR83_Feasibility_ExactFitBoundary — граница exact_fit/degraded проходит
// ровно по MaxVRAMNCtx (не «на глазок»).
func TestR83_Feasibility_ExactFitBoundary(t *testing.T) {
	limits := cppbackend.ResourceLimits{MaxVRAMNCtx: 32768, MaxRAMNCtx: 200000, ModelMaxContext: 262144}

	at := evaluateNCtxFeasibility("m", limits, 32768)
	if at.Stage != nctxStageExactFit {
		t.Errorf("ровно на границе: Stage = %q, want %q", at.Stage, nctxStageExactFit)
	}
	above := evaluateNCtxFeasibility("m", limits, 32769)
	if above.Stage != nctxStageDegraded || !above.Degraded {
		t.Errorf("на 1 выше границы: Stage = %q / Degraded = %v, want %q / true",
			above.Stage, above.Degraded, nctxStageDegraded)
	}
}
