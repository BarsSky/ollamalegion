// limits_shadow_r592_test.go — R592 (2026-11-19): два числа одного потолка окна.
//
// ЗАЧЕМ. Одно и то же «сколько окна влезает» считалось двумя источниками по
// разным правилам: Go (свободная VRAM МИНУС резерв CPPWORKER_VRAM_OVERHEAD_MB,
// умноженная на долю утилизации) и C-мост (`c/bridge/bridge.c`: вся свободная
// VRAM, без резерва). Числа попадали в разные места: первое — в /api/models и в
// гейт окна, второе — в текст ошибки декода. На 27B/A10 (2026-11-19) это дало
// «max_vram_n_ctx=251254» в ошибке при окне 201216 и 414 MB свободной VRAM —
// и падение декода с `code 1`.
//
// Решение владельца: сначала тень — показывать оба числа, решения не менять.
// Здесь проверяется ровно это: применяемое число не изменилось, справочное
// отличается ТОЛЬКО из-за резерва, и при резерве 0 они совпадают.
//
//go:build llama_stub

package cppbackend

import (
	"testing"

	"ollama-loadbalancer/internal/memfit"
)

// shadowSpecR592 — 27B-подобная форма: 64 слоя всего, 8 из них с KV, head_dim 256.
// Размер весов 15 GiB.
func shadowSpecR592() memfit.ModelSpec {
	return memfit.ModelSpec{
		Name:      "shadow-27b",
		SizeBytes: memfit.MiBOf(4 * 1024),
		NLayers:   64,
		KVLayers:  8,
		NHeads:    24,
		NKvHeads:  4,
		NEmbd:     5120,
		KVHeadDim: 256,
		TrainCtx:  262144,
	}
}

// shadowBudgetR592 — известная VRAM 8 GiB свободно, настраиваемый резерв.
func shadowBudgetR592(reserveMiB int64) memfit.Budget {
	return memfit.Budget{
		VRAMTotal:   memfit.MiBOf(24 * 1024),
		VRAMFree:    memfit.MiBOf(8 * 1024),
		VRAMReserve: memfit.MiBOf(reserveMiB),
		VRAMKnown:   true,
		RAMTotal:    memfit.MiBOf(64 * 1024),
		RAMAvail:    memfit.MiBOf(64 * 1024),
		RAMReserve:  memfit.MiBOf(4 * 1024),
	}
}

// TestCeilingsShadow_R592_РезервУменьшаетПрименяемое — главное утверждение:
// справочное (без резерва) больше применяемого ровно на величину резерва.
func TestCeilingsShadow_R592_РезервУменьшаетПрименяемое(t *testing.T) {
	spec := shadowSpecR592()
	policy := memfit.DefaultPolicy()

	applied, unreserved := CeilingsWithReserveShadowR592(spec, memfit.KVQ8, shadowBudgetR592(2048), policy)

	if applied <= 0 {
		t.Fatalf("применяемое число = %d, want > 0 (без него тест ничего не значит)", applied)
	}
	if unreserved <= 0 {
		t.Fatalf("справочное число = %d, want > 0", unreserved)
	}
	if unreserved <= applied {
		t.Errorf("без резерва потолок (%d) обязан быть БОЛЬШЕ применяемого (%d)", unreserved, applied)
	}

	// Проверяем причину расхождения числом: 2048 MiB резерва / KV на токен.
	// Допуск ±1 токен — следствие целочисленного деления в ДВУХ местах (потолок с
	// резервом и без него считаются независимо), а не разной формулы.
	kvPT := memfit.KVBytesPerToken(spec, memfit.KVQ8)
	if kvPT <= 0 {
		t.Fatalf("KVBytesPerToken = %d, want > 0", kvPT)
	}
	wantDiff := int(memfit.MiBOf(2048) / kvPT)
	if gotDiff := unreserved - applied; gotDiff < wantDiff-1 || gotDiff > wantDiff+1 {
		t.Errorf("разница потолков = %d токенов, want %d±1 (2048 MiB / %d B на токен)",
			gotDiff, wantDiff, kvPT)
	}
}

// TestCeilingsShadow_R592_БезРезерваСовпадают — при резерве 0 второй формулы нет:
// оба числа обязаны совпасть. Мутация «всегда вычитать 2 GiB» этот тест ломает.
func TestCeilingsShadow_R592_БезРезерваСовпадают(t *testing.T) {
	spec := shadowSpecR592()
	applied, unreserved := CeilingsWithReserveShadowR592(spec, memfit.KVQ8, shadowBudgetR592(0), memfit.DefaultPolicy())
	if applied != unreserved {
		t.Errorf("при резерве 0 применяемое (%d) и справочное (%d) обязаны совпадать", applied, unreserved)
	}
}

// TestCeilingsShadow_R592_НетВесовНетЧисел — вырожденный вход не выдумывает числа.
func TestCeilingsShadow_R592_НетВесовНетЧисел(t *testing.T) {
	applied, unreserved := CeilingsWithReserveShadowR592(memfit.ModelSpec{}, memfit.KVQ8, shadowBudgetR592(2048), memfit.DefaultPolicy())
	if applied != 0 || unreserved != 0 {
		t.Errorf("без спецификации модели потолки = %d/%d, want 0/0", applied, unreserved)
	}
}

// TestVRAMReserveMB_R592_ДефолтНеИзменился — «тень» не должна незаметно менять
// применяемое число через дефолт резерва.
func TestVRAMReserveMB_R592_ДефолтНеИзменился(t *testing.T) {
	if got := vramOverheadMBConfig(); got != 2048 {
		t.Errorf("vramOverheadMBConfig() = %d, want 2048", got)
	}
}
