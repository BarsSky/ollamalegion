package balancer

// nctx_aligned_target_r493_test.go — R493-12h: окно, которое балансер просит и
// кэширует, совпадает с окном, которое воркер реально загрузит.
//
// ЖИВОЙ ФАКТ (архив второго стенда 09:53, Qwen3.8-27B, один слот):
//
//	просили 149809 → воркер загрузил 150016 (GGML_PAD 256) → кэш балансера
//	держал 149809 → следующий ход снова просил «до 149809» → воркер видел
//	contextSize 150016→149809 и перезагружал модель ВНИЗ на 207 токенов.
//
// Тесты держат два свойства: (1) пересчёт «на клиента → суммарное» выравнивает
// окно при ЛЮБОМ числе слотов; (2) «окно уже покрыто» — это НЕ отказ, иначе
// guard пишет WARN об отказе там, где запрос просто обслуживается.

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestR493_PerClientToTotalAligned(t *testing.T) {
	cases := []struct {
		name   string
		perSeq int
		slots  int
		want   int
	}{
		{"архив: 149809 на клиента, один слот", 149809, 1, 150016},
		{"архив: 160674 на клиента, один слот", 160674, 1, 160768},
		{"уже выровненное не меняется", 131072, 1, 131072},
		{"два слота: окно × 2", 149809, 2, 300032},
		{"три слота", 40000, 3, 120576},
		{"окно не задано", 0, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := perClientToTotal(tc.perSeq, tc.slots); got != tc.want {
				t.Fatalf("perClientToTotal(%d, %d) = %d, ожидалось %d",
					tc.perSeq, tc.slots, got, tc.want)
			}
		})
	}
}

func TestR493_ReloadTargetForStateAligned(t *testing.T) {
	one := &NCtxBackendState{BackendID: "b1", CurrentNCtx: 150016, CurrentSlots: 1}
	two := &NCtxBackendState{BackendID: "b1", CurrentNCtx: 300032, CurrentSlots: 2}

	if got := reloadTargetForState(149809, one); got != 150016 {
		t.Fatalf("один слот: %d, ожидалось 150016 (воркер загрузит именно его)", got)
	}
	if got := reloadTargetForState(149809, two); got != 300032 {
		t.Fatalf("два слота: %d, ожидалось 300032", got)
	}
}

// TestR493_CoveredWindowIsNotRefusal — окно уже покрыто: роста нет, но и ОТКАЗА
// нет (пустая причина). Guard делает WARN по непустой причине, поэтому пустая
// строка = «обслуживаем загруженным окном».
func TestR493_CoveredWindowIsNotRefusal(t *testing.T) {
	const model = "m"
	p := &Proxy{config: &types.LoadBalancerConfig{}, backends: map[string]*BackendState{},
		metricsMgr: NewMetricsManager()}
	p.backends["b1"] = &BackendState{Backend: &types.Backend{
		ID: "b1", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy}}
	p.nctxReload = NewNCtxReloadCoordinator(NCtxReloadConfig{AutoReloadMaxNCtx: 262144})
	p.metricsMgr.llamaMetrics["b1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name: model, State: "loaded",
			// Архив: воркер держит ВЫРОВНЕННОЕ окно 150016.
			ContextLength: 150016, ContextPerSeq: 150016,
			GGUFMaxContext: 262144, MaxSlots: 1, Parallel: 1,
		}},
	}

	// Просят 149809 — меньше загруженного, но выравнивается в него же.
	ok, why := p.streamGrowthCheckR385("b1", model, 149809)
	if ok {
		t.Fatal("145-токенная разница не повод перезагружать 27B")
	}
	if why != "" {
		t.Fatalf("«окно уже покрыто» — не отказ; guard написал бы WARN об отказе: %q", why)
	}

	// Просят заметно больше — рост нужен, ступень 8k, причина отказа пуста.
	ok, why = p.streamGrowthCheckR385("b1", model, 160000)
	if !ok {
		t.Fatalf("160000 при загруженных 150016 обязано требовать роста, отказ: %q", why)
	}

	// Просят выше потолка — здесь уже настоящий отказ с причиной.
	ok, why = p.streamGrowthCheckR385("b1", model, 300000)
	if ok {
		t.Fatal("300000 при потолке 262144 — рост запрещён")
	}
	if why == "" {
		t.Fatal("отказ по потолку обязан называть причину")
	}
}
