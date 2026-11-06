//go:build llama_stub

// profile_hint_not_ceiling_r472_test.go — R472 (2026-11-06): окно, заявленное в
// профиле ДЛЯ ЗАГРУЗКИ (contextLength), не является потолком обслуживания.
//
// ЖИВОЙ ДЕФЕКТ (Cline → балансер, gemma-4-E4B). Клиент просил num_ctx=32768,
// промпт требовал ~40.7k (наша оценка), загруженное окно слота — 16384. Рост окна
// отклонялся с причиной:
//
//	окно 40697 физически невозможно: requested context window exceeds
//	model/backend ceiling (потолок 32768)
//
// «Потолок 32768» приходил из WindowFeasibility, где участвует declaredWindowTotal
// — contextLength профиля (32768). Но это хинт загрузки: операторский потолок
// авто-роста 65536, contextLengthMax профиля 131072. В результате окно не росло,
// клиент получал вечный 503 «a streaming request does not reload the model»,
// хотя рост включён и ничто его не блокировало.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// r472Proxy — gemma на стенде: профиль contextLength=32768 (хинт загрузки),
// contextLengthMax=131072, операторский потолок 65536, GGUF 131072.
func r472Proxy() *Proxy {
	const model = "gemma-4-E4B-it-Q4_K_M"
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	backendID := "b-main"
	p.backends[backendID] = &BackendState{Backend: &types.Backend{
		ID: backendID, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}}
	p.ReplaceModelProfiles(map[string]types.LlamaCppModelProfile{
		// Как на стенде: в профиле задано ОКНО ЗАГРУЗКИ 32768, отдельного
		// contextLengthMax нет — потолок обслуживания даёт операторский cap (65536).
	})
	p.nctxReload = NewNCtxReloadCoordinator(NCtxReloadConfig{AutoReloadMaxNCtx: 65536})
	p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name:           model,
			State:          "loaded",
			ContextLength:  16384,
			ContextPerSeq:  16384,
			GGUFMaxContext: 131072,
			MaxSlots:       1,
			Parallel:       1,
		}},
	}
	return p
}

// TestR472_GrowthAllowsWithinOperatorCeiling — ровно живой случай: нужное окно
// 40 697 при потолке 65 536 обязано разрешать рост.
func TestR472_GrowthAllowsWithinOperatorCeiling(t *testing.T) {
	const model = "gemma-4-E4B-it-Q4_K_M"
	p := r472Proxy()

	ok, why := p.streamGrowthCheckR385("b-main", model, 40697)
	if !ok {
		t.Fatalf("рост окна до 40697 при потолке 65536 обязан быть разрешён, отказ: %s", why)
	}
	// И потолок действительно тот, который задал оператор, а не хинт профиля.
	if ceiling := p.physicalNumCtxCeiling(model, "b-main"); ceiling != 65536 {
		t.Fatalf("physicalNumCtxCeiling = %d, want 65536", ceiling)
	}
}

// TestR472_GrowthStillRefusedAboveCeiling — выше операторского потолка отказ
// остаётся: это не «разрешить всё».
func TestR472_GrowthStillRefusedAboveCeiling(t *testing.T) {
	p := r472Proxy()
	if ok, why := p.streamGrowthCheckR385("b-main", "gemma-4-E4B-it-Q4_K_M", 70000); ok {
		t.Fatalf("70 000 выше потолка 65 536 — рост обязан быть запрещён (причина: %s)", why)
	}
}
