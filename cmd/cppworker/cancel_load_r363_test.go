// cancel_load_r363_test.go — R363 (2026-10-31), аудит abort A4.
//
// РЕШЕНИЕ ОПЕРАТОРА (дословно): «Да загрузку прерывать при отмене».
//
// ЧТО БЫЛО ДО. Отмена во время загрузки модели не работала по замыслу: загрузка отвязана
// от клиентского ctx (иначе первый ушедший клиент обрывал бы загрузку, которую ждут
// остальные), abort-флаг выставлялся только для генерации. Клиент нажимал Stop и получал
// 200 «no_active_generation», а модель продолжала читаться минутами, держа VRAM.
//
// R363: явная метка cancel_load в приказе. Её ставит ТОЛЬКО клиентская отмена (кнопка
// Stop), а не сторожевые обрывы — таймаут чужого стрима не должен рушить общую загрузку.
//
// Проверяется:
//  1. модель не грузится → отменять нечего (ничего не выдумываем);
//  2. флаг принят и загрузка отпустила → load_cancelled;
//  3. флаг принят, но загрузка ещё идёт → load_abort_requested (честно, не «готово»);
//  4. флаг НЕ приняли (ptr не выставлен) → load_abort_requested, а не «отменено»;
//  5. сторож: /api/cancel использует abortLoadingModelR363, и он опирается на
//     backend.AbortLoad (тот же путь, что force-выгрузка грузящейся модели, R215).
package main

import (
	"os"
	"strings"
	"testing"
)

// TestR363_OutcomeNotLoading — нечего отменять.
func TestR363_OutcomeNotLoading(t *testing.T) {
	if got := cancelLoadOutcomeR363(false, false, false); got != cancelLoadOutcomeNotLoadingR363 {
		t.Fatalf("outcome = %q, want пусто (модель не грузится)", got)
	}
}

// TestR363_OutcomeCancelled — флаг принят, загрузка отпустила.
func TestR363_OutcomeCancelled(t *testing.T) {
	if got := cancelLoadOutcomeR363(true, true, false); got != cancelLoadOutcomeCancelledR363 {
		t.Fatalf("outcome = %q, want load_cancelled", got)
	}
}

// TestR363_OutcomeStillLoading — флаг принят, но чтение весов ещё идёт.
func TestR363_OutcomeStillLoading(t *testing.T) {
	if got := cancelLoadOutcomeR363(true, true, true); got != cancelLoadOutcomeStillLoadingR363 {
		t.Fatalf("outcome = %q, want load_abort_requested: выдавать «отменено», пока "+
			"загрузка идёт, значит врать клиенту", got)
	}
}

// TestR363_OutcomeAbortNotApplied — флаг не приняли (C-ptr не выставлен).
func TestR363_OutcomeAbortNotApplied(t *testing.T) {
	if got := cancelLoadOutcomeR363(true, false, true); got != cancelLoadOutcomeStillLoadingR363 {
		t.Fatalf("outcome = %q, want load_abort_requested (честно: флаг не применён)", got)
	}
}

// TestR363_HandlerUsesAbortLoad_STEWARD — путь отмены загрузки один и тот же (R215).
func TestR363_HandlerUsesAbortLoad_STEWARD(t *testing.T) {
	read := func(name string) string {
		for _, p := range []string{name, "cmd/cppworker/" + name} {
			if b, err := os.ReadFile(p); err == nil {
				return string(b)
			}
		}
		return ""
	}

	handler := read("handlers_cancel.go")
	if handler == "" {
		t.Fatal("не удалось прочитать handlers_cancel.go")
	}
	if !strings.Contains(handler, "abortLoadingModelR363(") {
		t.Error("/api/cancel не зовёт abortLoadingModelR363: Stop во время загрузки снова " +
			"будет отвечать «no_active_generation» и ничего не остановит")
	}
	if !strings.Contains(handler, `json:"cancel_load"`) {
		t.Error("/api/cancel не принимает cancel_load — балансеру нечем разрешить отмену " +
			"загрузки (а сторожевые обрывы НЕ должны её прерывать)")
	}

	helper := read("cancel_load_r363.go")
	if helper == "" {
		t.Fatal("не удалось прочитать cancel_load_r363.go")
	}
	if !strings.Contains(helper, "backend.AbortLoad(") {
		t.Error("отмена загрузки не идёт через backend.AbortLoad: это единственный путь, " +
			"который выставляет флаг в C-мост (тем же пользуется force-выгрузка, R215)")
	}
	if !strings.Contains(helper, "backend.IsLoading(") {
		t.Error("нет проверки backend.IsLoading: отменять нечего, если модель не грузится")
	}
}
