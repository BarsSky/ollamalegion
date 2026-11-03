//go:build llama_stub

// autotune_profile_floor_r425_test.go — R425 (2026-11-03): окно профиля модели —
// НИЖНЯЯ граница для AutoTune, а не «овер-аллокация».
//
// ЖИВОЙ ДЕФЕКТ (стенд, балансер r261103-2245, модель Qwen3-4B-Instruct-2507).
// Сразу после первого запроса БЕЗ num_ctx AutoTune выгрузил модель и поднял её
// заново с окном 20654 вместо 32768:
//
//	"autotune: triggering async reload", "reason": "R54.4: n_ctx 32768 → 20654
//	(over-allocation fix)"
//
// В профиле модели оператор заявил contextLength=32768 и contextLengthMax=65536.
// Клиент, пришедший с окном 32768, получил бы 20654, и агентская сессия
// оборвалась бы по контексту — при том что оператор ничего не менял.
//
// Контракт: рекомендация ниже заявленного окна профиля не применяется
// (skip с логом), выше — как раньше.

package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// autotuneAnalysisContextR425 — минимальный анализ с одной рекомендацией по окну.
func autotuneAnalysisContextR425(recommended int) *AutoTuneAnalysis {
	return &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "context", RecommendedNumCtx: recommended, Severity: "warning"},
		},
	}
}

func TestR425_AutotuneDoesNotShrinkBelowClientRequest(t *testing.T) {
	loaded := types.LlamaCppModel{Name: "m", ContextLength: 32768}
	plan := PlanApplyAutoTuneWithMinContext(autotuneAnalysisContextR425(20654), loaded, 32768)
	if plan != nil {
		t.Fatalf("план не должен уменьшать окно ниже клиентского запроса 32768, получено %+v", plan)
	}
}

func TestR425_AutotuneShrinkAllowedWithoutFloor(t *testing.T) {
	loaded := types.LlamaCppModel{Name: "m", ContextLength: 32768}
	plan := PlanApplyAutoTuneWithMinContext(autotuneAnalysisContextR425(20654), loaded, 0)
	if plan == nil || plan.ContextSize != 20654 {
		t.Fatalf("без нижней границы прежнее поведение (20654) должно сохраниться, получено %+v", plan)
	}
}

// TestR425_TriggerPassesProfileWindowAsFloor — нижняя граница для плана берётся
// как максимум из клиентского запроса и окна профиля (autoTuneMinContextR425).
// Без этого тест выше не спасёт: на стенде minContext приходил НУЛЕВЫМ (запрос
// без num_ctx), и план уменьшал окно.
func TestR425_TriggerPassesProfileWindowAsFloor(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			"Qwen3-4B-Instruct-2507-Q4_K_M": {ContextLength: 32768, ContextLengthMax: 65536},
		},
	}}
	if got := p.GetModelProfileNumCtx("Qwen3-4B-Instruct-2507-Q4_K_M"); got != 32768 {
		t.Fatalf("профиль не читается: %d", got)
	}
	cases := []struct {
		client, profile, want int
	}{
		{0, 32768, 32768},     // ровно случай стенда: клиент окно не просил
		{65536, 32768, 65536}, // клиент просит больше профиля — уступаем только вверх
		{0, 0, 0},             // профиля нет — прежнее поведение
		{16384, 0, 16384},     // профиля нет, клиент просил — уважаем
	}
	for _, c := range cases {
		if got := autoTuneMinContextR425(c.client, c.profile); got != c.want {
			t.Errorf("autoTuneMinContextR425(%d, %d) = %d, want %d", c.client, c.profile, got, c.want)
		}
	}

	minContext := autoTuneMinContextR425(0, p.GetModelProfileNumCtx("Qwen3-4B-Instruct-2507-Q4_K_M"))
	loaded := types.LlamaCppModel{Name: "Qwen3-4B-Instruct-2507-Q4_K_M", ContextLength: 32768}
	if plan := PlanApplyAutoTuneWithMinContext(autotuneAnalysisContextR425(20654), loaded, minContext); plan != nil {
		t.Fatalf("AutoTune не должен опускать окно ниже профиля, получено %+v", plan)
	}
}
