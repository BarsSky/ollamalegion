// nctx_ceiling_r265_test.go — R265 (2026-10-28): тех-долг из финального аудита.
//
// ЗАЧЕМ. В коде жили три «потолка n_ctx», помеченные в комментариях «MUST stay
// in sync»:
//  1. physicalNumCtxCeiling (num_ctx_resolver.go) — что физически возможно для
//     КЛИЕНТСКОГО запроса: min(GGUF, cap профиля, операторский потолок);
//  2. NCtxBackendState.physicalCeiling (preflight_nctx.go) — тот же потолок, но
//     уже ПОСЧИТАННЫЙ на момент сборки состояния (геттер, не расчёт);
//  3. resolveModelMaxContext (nctx_reload_handlers.go) — другой вопрос: какой
//     предел отдать РЕШЕНИЮ о reload (в hard-режиме профиль может быть ниже
//     физики, в auto — наоборот, профиль лишь подсказка).
//
// В R265 сведены 1 и 2: формула одна (resolvePhysicalMaxContext), сборка входов
// одна (profileCapCtx + getGGUFMaxContext + preflightAutoReloadMaxNCtx). 3
// сознательно оставлен отдельно — см. докстроку, почему слияние изменило бы
// поведение preflight.
//
// ЭТИ ТЕСТЫ — ХАРАКТЕРИЗАЦИОННЫЕ: они фиксируют ПРЕЖНЕЕ поведение на тех же
// входах. Если правка «улучшила» семантику, тест обязан упасть.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// ceilingTestProxy — прокси с заданным GGUF-потолком и профилями моделей.
func ceilingTestProxy(ggufMax int, profiles map[string]types.LlamaCppModelProfile) *Proxy {
	p := &Proxy{
		config:     &types.LoadBalancerConfig{LlamaCppModelProfiles: profiles},
		metricsMgr: NewMetricsManager(),
	}
	if ggufMax > 0 {
		p.metricsMgr.mu.Lock()
		p.metricsMgr.llamaMetrics["b1"] = &types.LlamaCppMetrics{GGUFMaxContext: ggufMax}
		p.metricsMgr.mu.Unlock()
	}
	return p
}

// TestR265_PhysicalNumCtxCeiling_Characterization — потолок клиентских запросов
// обязан остаться ровно прежним: min(GGUF, cap профиля|операторский потолок).
func TestR265_PhysicalNumCtxCeiling_Characterization(t *testing.T) {
	cases := []struct {
		name    string
		ggufMax int
		profile map[string]types.LlamaCppModelProfile
		model   string
		want    int
	}{
		{
			name:  "нет данных вообще → 0 (clamping не производится)",
			model: "m",
			want:  0,
		},
		{
			name:    "только GGUF → он и есть потолок",
			ggufMax: 262144, model: "m",
			want: 262144,
		},
		{
			name:    "GGUF меньше cap профиля → GGUF",
			ggufMax: 32768,
			profile: map[string]types.LlamaCppModelProfile{"m": {ContextLengthMax: 131072}},
			model:   "m",
			want:    32768,
		},
		{
			name:    "cap профиля меньше GGUF → cap профиля",
			ggufMax: 262144,
			profile: map[string]types.LlamaCppModelProfile{"m": {ContextLengthMax: 131072}},
			model:   "m",
			want:    131072,
		},
		{
			name:    "cap профиля при неизвестном GGUF → cap профиля",
			profile: map[string]types.LlamaCppModelProfile{"m": {ContextLengthMax: 8192}},
			model:   "m",
			want:    8192,
		},
		{
			name:    "ПРОФИЛЬ ПО КОРОТКОМУ ИМЕНИ: «gemma-4» для «gemma-4-E4B-it-Q4_K_M» находится (fold)",
			ggufMax: 262144,
			profile: map[string]types.LlamaCppModelProfile{"gemma-4": {ContextLengthMax: 131072}},
			model:   "gemma-4-E4B-it-Q4_K_M",
			want:    131072,
		},
		{
			name:    "чужой профиль не подхватывается → GGUF",
			ggufMax: 262144,
			profile: map[string]types.LlamaCppModelProfile{"qwen3": {ContextLengthMax: 4096}},
			model:   "gemma-4-E4B",
			want:    262144,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := ceilingTestProxy(c.ggufMax, c.profile)
			if got := p.physicalNumCtxCeiling(c.model, "b1"); got != c.want {
				t.Fatalf("physicalNumCtxCeiling(%q) = %d, want %d (прежнее поведение!)", c.model, got, c.want)
			}
		})
	}

	// Пустые входы: nil-прокси и пустой backendID → 0, без паники.
	var nilProxy *Proxy
	if got := nilProxy.physicalNumCtxCeiling("m", "b1"); got != 0 {
		t.Fatalf("nil proxy: %d, want 0", got)
	}
	p := ceilingTestProxy(0, nil)
	if got := p.physicalNumCtxCeiling("m", ""); got != 0 {
		t.Fatalf("пустой backendID: %d, want 0", got)
	}
}

// TestR265_ProfileCapCtx_FoldVersusExact — различие сопоставления имени профиля
// вынесено в явный параметр и закреплено здесь: preflight читает ТОЧНЫЙ ключ,
// физический потолок клиентских запросов терпим к короткому имени.
func TestR265_ProfileCapCtx_FoldVersusExact(t *testing.T) {
	profiles := map[string]types.LlamaCppModelProfile{
		"gemma-4":        {ContextLengthMax: 131072},
		"exact-model-70": {ContextLengthMax: 65536},
	}
	p := ceilingTestProxy(0, profiles)

	if got := p.profileCapCtx("gemma-4-E4B-it-Q4_K_M", false); got != 0 {
		t.Fatalf("fold=false обязан не находить короткий ключ, got %d", got)
	}
	if got := p.profileCapCtx("gemma-4-E4B-it-Q4_K_M", true); got != 131072 {
		t.Fatalf("fold=true обязан найти короткий ключ, got %d", got)
	}
	if got := p.profileCapCtx("exact-model-70", false); got != 65536 {
		t.Fatalf("точный ключ обязан находиться при fold=false, got %d", got)
	}
	var nilProxy *Proxy
	if got := nilProxy.profileCapCtx("x", true); got != 0 {
		t.Fatalf("nil proxy: %d, want 0", got)
	}
}

// TestR265_StatePhysicalCeiling_Characterization — геттер состояния отдаёт
// PhysicalMaxContext, иначе ModelMaxContext (старые вызовы/тесты с одним полем).
func TestR265_StatePhysicalCeiling_Characterization(t *testing.T) {
	cases := []struct {
		name  string
		state *NCtxBackendState
		want  int
	}{
		{"nil state → 0", nil, 0},
		{"PhysicalMaxContext задан → он", &NCtxBackendState{PhysicalMaxContext: 131072, ModelMaxContext: 32768}, 131072},
		{"PhysicalMaxContext пуст → ModelMaxContext", &NCtxBackendState{ModelMaxContext: 32768}, 32768},
		{"оба пусты → 0", &NCtxBackendState{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.state.physicalCeiling(); got != c.want {
				t.Fatalf("physicalCeiling() = %d, want %d", got, c.want)
			}
		})
	}
}

// TestR265_StatePhysicalCeiling_MatchesSharedCore — геттер и расчёт не должны
// расходиться: state, собранный из тех же входов, что и physicalNumCtxCeiling,
// обязан дать то же число.
func TestR265_StatePhysicalCeiling_MatchesSharedCore(t *testing.T) {
	p := ceilingTestProxy(262144, map[string]types.LlamaCppModelProfile{
		"gemma-4": {ContextLengthMax: 131072},
	})
	state := &NCtxBackendState{
		GGUFMaxContext:     p.getGGUFMaxContext("b1"),
		PhysicalMaxContext: p.physicalNumCtxCeiling("gemma-4-E4B-it-Q4_K_M", "b1"),
	}
	if got, want := state.physicalCeiling(), 131072; got != want {
		t.Fatalf("physicalCeiling() = %d, want %d (свод с общим ядром)", got, want)
	}
}

// TestR265_ResolveModelMaxContext_Characterization — третья точка НЕ сведена
// (она отвечает на другой вопрос), поэтому её прежнее поведение фиксируется
// отдельно: hard-режим может быть НИЖЕ физики, auto-режим — min(GGUF, cap).
func TestR265_ResolveModelMaxContext_Characterization(t *testing.T) {
	p := ceilingTestProxy(262144, nil)

	cases := []struct {
		name              string
		profileMaxCtx     int
		contextLengthAuto bool
		contextLengthMax  int
		feasible          int
		want              int
	}{
		{
			name: "нет профиля → GGUF (R43: не падаем в дефолтный профиль)",
			want: 262144,
		},
		{
			name:          "hard-режим: профиль ниже физики → профиль (это CAP)",
			profileMaxCtx: 32768, contextLengthMax: 131072, want: 32768,
		},
		{
			name:          "hard-режим: профиль выше операторского cap → cap",
			profileMaxCtx: 200000, contextLengthMax: 131072, want: 131072,
		},
		{
			name:          "auto-режим: профиль — подсказка, cap = min(GGUF, contextLengthMax)",
			profileMaxCtx: 32768, contextLengthAuto: true, contextLengthMax: 131072, want: 131072,
		},
		{
			name:          "auto-режим: feasible НЕ является cap (R43)",
			profileMaxCtx: 32768, contextLengthAuto: true, contextLengthMax: 131072, feasible: 32719, want: 131072,
		},
		{
			name:          "auto-режим без cap и без GGUF данных → 0 (доверяем воркеру)",
			profileMaxCtx: 32768, contextLengthAuto: true, want: 262144,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := p.resolveModelMaxContext("b1", "m", c.profileMaxCtx, c.contextLengthAuto, c.contextLengthMax, c.feasible)
			if got != c.want {
				t.Fatalf("resolveModelMaxContext = %d, want %d (прежнее поведение!)", got, c.want)
			}
		})
	}
}
