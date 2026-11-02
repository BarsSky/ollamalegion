// nctx_declared_above_max_r400_test.go — R400 (2026-11-05).
//
// ЖИВАЯ ЖАЛОБА. «Настроив загруженную модель под больший контекст, балансер не
// ищет доступную возможную, а встречает зашитый блок в ограничение 131072 по
// контекстному окну».
//
// ПРИЧИНА. В профиле два поля: contextLength («окно») и contextLengthMax
// («максимальный контекст» — верхняя граница авто-роста). Потолок окна брался
// ТОЛЬКО из contextLengthMax, поэтому окно 262144 при максимуме 131072
// урезалось до 131072 в трёх местах сразу:
//   - physicalNumCtxCeiling (клиентский запрос получал 413 «модель
//     поддерживает 131072»);
//   - resolveModelMaxContext, Tier 2 (cap = contextLengthMax);
//   - resolveModelMaxContext, Tier 1 (жёсткий режим клампил заявленное окно).
//
// ЧТО ИЗМЕНЕНО. Заявленное окно — такая же политика оператора, как максимум:
// потолок = max(contextLengthMax, contextLength). Максимум продолжает поднимать
// потолок авто-роста, но больше не может урезать окно, которое оператор сам
// заявил в карточке.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR400_DeclaredWindowAboveMaxIsHonoured — auto-режим: заявленное окно
// 262144 при максимуме 131072 больше не урезается.
func TestR400_DeclaredWindowAboveMaxIsHonoured(t *testing.T) {
	p := ceilingTestProxy(0, map[string]types.LlamaCppModelProfile{
		"m": {ContextLength: 262144, ContextLengthMax: 131072, ContextLengthAuto: true},
	})
	if got := p.resolveModelMaxContext("b1", "m", 262144, true, 131072, 0); got != 262144 {
		t.Fatalf("resolveModelMaxContext(auto) = %d, want 262144 (заявленное окно не режется устаревшим максимумом)", got)
	}
}

// TestR400_DeclaredWindowAboveMaxHardMode — жёсткий режим: окно задаёт
// contextLength, максимум его не переписывает.
func TestR400_DeclaredWindowAboveMaxHardMode(t *testing.T) {
	p := ceilingTestProxy(0, nil)
	if got := p.resolveModelMaxContext("b1", "m", 262144, false, 131072, 0); got != 262144 {
		t.Fatalf("resolveModelMaxContext(hard) = %d, want 262144 (contextLength авторитетен в жёстком режиме)", got)
	}
}

// TestR400_PhysicalCeilingHonoursDeclaredWindow — потолок клиентских запросов
// (physicalNumCtxCeiling) обязан пропустить окно, заявленное профилем.
func TestR400_PhysicalCeilingHonoursDeclaredWindow(t *testing.T) {
	p := ceilingTestProxy(262144, map[string]types.LlamaCppModelProfile{
		"m": {ContextLength: 262144, ContextLengthMax: 131072, ContextLengthAuto: true},
	})
	if got := p.physicalNumCtxCeiling("m", "b1"); got != 262144 {
		t.Fatalf("physicalNumCtxCeiling = %d, want 262144 (GGUF позволяет, профиль заявил 262144)", got)
	}

	// GGUF остаётся физическим верхом: заявленное окно его не поднимает.
	p2 := ceilingTestProxy(131072, map[string]types.LlamaCppModelProfile{
		"m": {ContextLength: 262144, ContextLengthMax: 131072, ContextLengthAuto: true},
	})
	if got := p2.physicalNumCtxCeiling("m", "b1"); got != 131072 {
		t.Fatalf("physicalNumCtxCeiling = %d, want 131072 (GGUF max модели)", got)
	}
}

// TestR400_ProfileCapCtxTakesMaxOfWindowAndMax — формула потолка профиля:
// максимум, но не ниже заявленного окна. Если максимум НЕ задан (0), профиль
// потолка не даёт вообще — контракт R68: окно это подсказка загрузки, а предел
// клиентских запросов берётся из GGUF/env.
func TestR400_ProfileCapCtxTakesMaxOfWindowAndMax(t *testing.T) {
	cases := []struct {
		name string
		prof types.LlamaCppModelProfile
		want int
	}{
		{"только окно (максимум не задан): профиль не ограничивает", types.LlamaCppModelProfile{ContextLength: 65536}, 0},
		{"только максимум", types.LlamaCppModelProfile{ContextLengthMax: 131072}, 131072},
		{"окно выше максимума", types.LlamaCppModelProfile{ContextLength: 262144, ContextLengthMax: 131072}, 262144},
		{"максимум выше окна", types.LlamaCppModelProfile{ContextLength: 32768, ContextLengthMax: 131072}, 131072},
		{"оба пусты", types.LlamaCppModelProfile{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := ceilingTestProxy(0, map[string]types.LlamaCppModelProfile{"m": c.prof})
			if got := p.profileCapCtx("m", false); got != c.want {
				t.Fatalf("profileCapCtx = %d, want %d", got, c.want)
			}
		})
	}
}
