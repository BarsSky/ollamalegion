//go:build llama_stub

// advertise_feasible_r521_test.go — R521 (2026-11-19, §8.3.2): объявляемое
// клиенту окно не может превышать то, что ВЛЕЗАЕТ В ПАМЯТЬ.
//
// ЖИВОЙ ФАКТ (архив 2026-11-18, A10 24 ГБ, 27B Q4_K_M): X-Model-Max-Context
// был 131072 (min из GGUF, профиля и env), клиент законно просил 131072, модель
// грузилась с этим окном — и первый decode на 80k-промпте падал с CUDA OOM.
// Feasible воркера в тот же момент был ~60 000 (AutoTune рекомендовал 60 114).
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// r521AdvertiseProxy — один загруженный бэкенд с моделью и её потолками.
func r521AdvertiseProxy(t *testing.T, feasible, slots int) *Proxy {
	t.Helper()
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.nctxReload = NewNCtxReloadCoordinator(NCtxReloadConfig{AutoReloadMaxNCtx: 131072})
	p.metricsMgr.llamaMetrics["b1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name:               "Qwen3.8-27B-UD-Q4_K_M",
			State:              "loaded",
			GGUFMaxContext:     262144,
			MaxContext:         262144,
			FeasibleMaxContext: feasible,
			ContextLength:      131072,
			ContextPerSeq:      131072,
			MaxSlots:           slots,
			Parallel:           slots,
		}},
	}
	return p
}

// TestR521_AdvertisedWindowIsCappedByFeasible — потолок, который не влезает, не
// объявляется.
func TestR521_AdvertisedWindowIsCappedByFeasible(t *testing.T) {
	p := r521AdvertiseProxy(t, 60000, 1)
	eff, _ := p.advertisedMaxContextR469("Qwen3.8-27B-UD-Q4_K_M")
	if eff != 60000 {
		t.Fatalf("объявленный потолок = %d, want 60000 (feasible): обещать 131072 при влезающих 60k — прямой путь к CUDA OOM", eff)
	}
}

// TestR521_AdvertisedWindowFeasibleIsPerSlot — feasible суммарный, объявление на слот.
func TestR521_AdvertisedWindowFeasibleIsPerSlot(t *testing.T) {
	p := r521AdvertiseProxy(t, 60000, 2)
	eff, _ := p.advertisedMaxContextR469("Qwen3.8-27B-UD-Q4_K_M")
	want := derivedWindowCapR466(60000, 2) // 29952 после выравнивания на 256
	if eff != want {
		t.Fatalf("при 2 слотах объявлено %d, want %d (feasible — суммарный, на слот делим)", eff, want)
	}
}

// TestR521_UnknownFeasibleDoesNotLowerWindow — feasible неизвестен (0): прежнее
// поведение, окно не занижаем выдуманным числом.
func TestR521_UnknownFeasibleDoesNotLowerWindow(t *testing.T) {
	p := r521AdvertiseProxy(t, 0, 1)
	eff, _ := p.advertisedMaxContextR469("Qwen3.8-27B-UD-Q4_K_M")
	if eff != 131072 {
		t.Fatalf("при неизвестном feasible объявлено %d, want 131072 (прежний min из GGUF/профиля/env)", eff)
	}
}
