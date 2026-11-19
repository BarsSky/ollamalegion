// nctx_shadow_fields_r592_test.go — R592 (2026-11-19): «тень» резерва VRAM
// в ответе /api/models.
//
// ЗАЧЕМ. Решение владельца (2026-11-19): сначала ПОКАЗАТЬ оба числа потолка окна,
// решения не менять. Живой факт, ради которого это появилось (27B/A10,
// diag-архив 2026-11-19): мост сообщил max_vram_n_ctx=251254 (без резерва), окно
// поднялось до 201216, после загрузки свободно 414 MB из 22 502, декод промпта
// 33 812 токенов упал с `llama_decode ... code 1` при измеренном
// `CUDA0 compute buffer = 316.75 MiB`. Оператор не видел ни одного числа, из
// которого понятно, что окно съело почти всю карту.
//
//go:build llama_stub

package main

import "testing"

// TestNctxShadowFieldsR592_Поля — обе величины и размер резерва присутствуют
// всегда, даже когда VRAM неизвестна: поле-ноль честнее отсутствующего поля
// (отсутствие читалось как «мы это не считаем»).
func TestNctxShadowFieldsR592_Поля(t *testing.T) {
	got := nctxShadowFieldsR592(251254, 2048, 201216, true)

	if v, ok := got["max_vram_n_ctx_unreserved"]; !ok || v != 251254 {
		t.Errorf("max_vram_n_ctx_unreserved = %v (есть=%v), want 251254", v, ok)
	}
	if v, ok := got["vram_reserve_mb"]; !ok || v != 2048 {
		t.Errorf("vram_reserve_mb = %v (есть=%v), want 2048", v, ok)
	}
	// 201216 < 251254 → окно НЕ выше безрезервного потолка; живой отказ был в
	// другом: 201216 съело всю карту, потому что потолок считался без буферов.
	if v, ok := got["window_over_unreserved"]; !ok {
		t.Error("window_over_unreserved отсутствует при известной VRAM и ненулевом потолке")
	} else if v != false {
		t.Errorf("window_over_unreserved = %v, want false (201216 < 251254)", v)
	}
}

// TestNctxShadowFieldsR592_ОкноВышеБезрезервного — различающая проверка: если
// окно больше безрезервного потолка, флаг обязан быть true. Без этого теста
// флаг можно «сломать в константу false» и никто не заметит.
func TestNctxShadowFieldsR592_ОкноВышеБезрезервного(t *testing.T) {
	got := nctxShadowFieldsR592(262144, 2048, 262144+1, true)
	if v, _ := got["window_over_unreserved"]; v != true {
		t.Errorf("window_over_unreserved = %v, want true (окно 262145 > потолок 262144)", v)
	}
}

// TestNctxShadowFieldsR592_НеизвестнаяVRAM — при VRAMKnown=false потолки нулевые
// и флаг не выдумывается: «неизвестно» не должно выглядеть как «не влезает».
func TestNctxShadowFieldsR592_НеизвестнаяVRAM(t *testing.T) {
	got := nctxShadowFieldsR592(0, 0, 32768, false)
	if _, ok := got["window_over_unreserved"]; ok {
		t.Error("window_over_unreserved не должен появляться при неизвестной VRAM")
	}
	if v := got["max_vram_n_ctx_unreserved"]; v != 0 {
		t.Errorf("max_vram_n_ctx_unreserved = %v, want 0", v)
	}
}

// TestNctxShadowFieldsR592_НулевойПотолок — модель не влезает ни при каком окне:
// флага быть не должно, иначе он читался бы как «влезает, но окно больше».
func TestNctxShadowFieldsR592_НулевойПотолок(t *testing.T) {
	got := nctxShadowFieldsR592(0, 2048, 8192, true)
	if _, ok := got["window_over_unreserved"]; ok {
		t.Error("window_over_unreserved при нулевом потолке не выставляется")
	}
}
