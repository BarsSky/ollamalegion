package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// R248: узлы для ЗАГРУЗКИ модели упорядочиваются по пригодности VRAM.
//
// Живой дефект: Qwen3.8-27B (≈15 ГБ) уходил на узел с 8 ГБ, хотя рядом был
// свободный узел на 20 ГБ — P3 брал первый кандидат со свободным слотом.
func TestR248_OrderCandidatesByVramFit(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	restore := setModelSizeBytesForTest(func(model string) int64 {
		if model == "Qwen3.8-27B" {
			return 15 * gb
		}
		return 0
	})
	defer restore()

	p := &Proxy{}
	p.metricsMgr = &MetricsManager{llamaMetrics: map[string]*types.LlamaCppMetrics{
		// маленькая карта: 8 ГБ всего, 7 свободно
		"small": {VramKnown: true, AvailableVRAMMB: 7000},
		// большая карта: 20 ГБ всего, 19 свободно
		"big": {VramKnown: true, AvailableVRAMMB: 19456},
		// узел без метрик VRAM (например, агент не отчитывается)
		"unknown": {},
	}}

	got := p.orderCandidatesByVramFit([]string{"small", "unknown", "big"}, "Qwen3.8-27B")
	want := []string{"big", "small", "unknown"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, ожидалось %v (тяжёлая модель должна идти на узел, где влезает)", got, want)
		}
	}
}

// R248: если размер модели неизвестен — порядок НЕ меняется (сортировка без
// размера была бы догадкой).
func TestR248_OrderCandidatesUnknownSizeKeepsOrder(t *testing.T) {
	restore := setModelSizeBytesForTest(func(string) int64 { return 0 })
	defer restore()

	p := &Proxy{}
	p.metricsMgr = &MetricsManager{llamaMetrics: map[string]*types.LlamaCppMetrics{
		"small": {VramKnown: true, AvailableVRAMMB: 7000},
		"big":   {VramKnown: true, AvailableVRAMMB: 19456},
	}}

	got := p.orderCandidatesByVramFit([]string{"small", "big"}, "unknown-model")
	if got[0] != "small" || got[1] != "big" {
		t.Fatalf("order = %v, при неизвестном размере порядок должен сохраняться", got)
	}
}

// R248: когда модель не влезает НИКУДА, первым идёт узел с большей свободной
// VRAM (наименее плохой вариант), а не первый по списку.
func TestR248_OrderCandidatesNoFitPrefersLarger(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	restore := setModelSizeBytesForTest(func(string) int64 { return 30 * gb })
	defer restore()

	p := &Proxy{}
	p.metricsMgr = &MetricsManager{llamaMetrics: map[string]*types.LlamaCppMetrics{
		"small": {VramKnown: true, AvailableVRAMMB: 7000},
		"big":   {VramKnown: true, AvailableVRAMMB: 19456},
	}}

	got := p.orderCandidatesByVramFit([]string{"small", "big"}, "huge")
	if got[0] != "big" {
		t.Fatalf("order = %v, при нехватке VRAM везде первым должен быть узел с большей VRAM", got)
	}
}
