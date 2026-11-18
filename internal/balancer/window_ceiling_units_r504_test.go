//go:build llama_stub

// window_ceiling_units_r504_test.go — R504 (2026-11-19), аудит S4: N-13 и N-1.
//
// ДЕФЕКТ (одна величина в двух единицах). `Ceiling` объявлен «потолком НА
// КЛИЕНТА», но получает СУММАРНЫЙ физический потолок модели
// (`physicalNumCtxCeiling` = min(GGUF, профиль, LB_NCTX_RELOAD_MAX_N_CTX)).
// R466-урезание тот же потолок ДЕЛИЛО на слоты, а отказ и цель роста сравнивали
// с ним `Needed` (на клиента) без деления. При 2 слотах и потолке 131072 цель
// 131072 на клиента означала перезагрузку на 262144 — ВЫШЕ потолка (N-1:
// «27B, parallel=2, потолок 131072 → reload на 262144»).
//
// ЛОВУШКА ИЗ АУДИТА (N-13): «починить по комментарию» (разделить на слоты там,
// где стоит «на клиента») = разделить ДВАЖДЫ и отказать там, где окно влезает.
// Поэтому тест держит обе стороны: и «не превышать суммарный потолок», и
// «не отказать клиенту, которому доля потолка хватает».
package balancer

import (
	"strings"
	"testing"
)

// windowInR504 — минимальный вход решения: занятости нет, путь умеет расти.
func windowInR504(loaded, needed int, slots, ceiling int) windowInputR493 {
	return windowInputR493{
		Needed:    needed,
		Estimated: needed,
		Loaded:    loaded,
		Slots:     slots,
		Ceiling:   ceiling,
	}
}

// TestR504_TotalCeilingIsDividedBySlotsOnce — N-13: доля клиента считается РОВНО
// один раз: при 2 слотах и суммарном потолке 131072 клиенту доступно 65536.
func TestR504_TotalCeilingIsDividedBySlotsOnce(t *testing.T) {
	if got := derivedWindowCapR466(131072, 2); got != 65536 {
		t.Fatalf("доля клиента = %d, ожидалось 65536 (131072/2)", got)
	}

	// Клиенту 40000 — влезает в долю 65536: рост обязателен. Если долю разделить
	// ещё раз (32768), решение откажет — именно это ловит тест.
	d := decideWindowR493(windowInR504(32768, 40000, 2, 131072))
	if d.Kind != windowGrowR493 {
		t.Fatalf("нужно 40000 при доле потолка 65536 → вид %q, ожидался %q (причина: %s): "+
			"потолок поделён дважды (ловушка N-13)", d.Kind, windowGrowR493, d.Why)
	}
	if d.Target <= 0 || d.Target > 65536 {
		t.Fatalf("цель роста = %d, ожидалось (0; 65536]: цель выше доли клиента "+
			"означает перезагрузку выше суммарного потолка", d.Target)
	}
}

// TestR504_ExplicitRequestAbovePerClientCeilingIsRefused — N-1: клиент ЯВНО
// попросил окно больше доли клиента — рост бессмыслен (перезагрузка выше
// суммарного потолка), честный отказ с обоими числами.
func TestR504_ExplicitRequestAbovePerClientCeilingIsRefused(t *testing.T) {
	d := decideWindowR493(windowInputR493{
		Needed:    100000,
		Estimated: 100000,
		Requested: 100000, // клиент попросил явно: R466-урезание не применяется
		Loaded:    32768,
		Slots:     2,
		Ceiling:   131072,
	})
	if d.Kind != windowRefuseR493 {
		t.Fatalf("клиент просит 100000 при доле потолка 65536 → вид %q, ожидался %q "+
			"(иначе перезагрузка на 200000 выше суммарного потолка 131072)", d.Kind, windowRefuseR493)
	}
	if !strings.Contains(d.Why, "65536") || !strings.Contains(d.Why, "131072") {
		t.Fatalf("причина не называет оба числа (долю и суммарный потолок): %q", d.Why)
	}
}

// TestR504_DerivedNeedAboveShareGrowsToShare — N-1/N-13: НАША оценка выше доли
// клиента урезается до неё (R466) и рост идёт до неё же — перезагрузка ровно в
// суммарный потолок, без выхода за него.
func TestR504_DerivedNeedAboveShareGrowsToShare(t *testing.T) {
	d := decideWindowR493(windowInR504(32768, 100000, 2, 131072))
	if d.Kind != windowGrowR493 {
		t.Fatalf("выведенная потребность 100000 при доле 65536 → вид %q, ожидался %q (%s)",
			d.Kind, windowGrowR493, d.Why)
	}
	if !d.Capped {
		t.Fatal("выведенная потребность выше доли клиента обязана быть урезана (Capped)")
	}
	if d.Target > 65536 {
		t.Fatalf("цель = %d выше доли клиента 65536: перезагрузка уйдёт за суммарный потолок", d.Target)
	}
	if reloadTotal := perClientToTotal(d.Target, 2); reloadTotal > 131072 {
		t.Fatalf("перезагрузка = %d выше суммарного потолка 131072", reloadTotal)
	}
}

// TestR504_SingleSlotCeilingUnchanged — slots<=1: деление не меняет ничего.
func TestR504_SingleSlotCeilingUnchanged(t *testing.T) {
	d := decideWindowR493(windowInR504(32768, 100000, 1, 131072))
	if d.Kind != windowGrowR493 {
		t.Fatalf("один слот, нужно 100000 при потолке 131072 → вид %q, ожидался %q (%s)",
			d.Kind, windowGrowR493, d.Why)
	}
	if d.Target <= 0 || d.Target > 131072 {
		t.Fatalf("цель = %d, ожидалось (0; 131072]", d.Target)
	}
}

// TestR504_ReloadNeverExceedsTotalCeiling — инвариант N-1: пересчёт цели в
// суммарное окно (это делает перезагрузка) не превышает суммарный потолок.
func TestR504_ReloadNeverExceedsTotalCeiling(t *testing.T) {
	const totalCeiling = 131072
	for _, slots := range []int{1, 2, 3, 4} {
		per := derivedWindowCapR466(totalCeiling, slots)
		if per <= 0 {
			continue
		}
		for _, needed := range []int{4096, 16384, per - 4096, per} {
			if needed <= 0 {
				continue
			}
			d := decideWindowR493(windowInR504(2048, needed, slots, totalCeiling))
			if d.Kind != windowGrowR493 {
				continue
			}
			reloadTotal := perClientToTotal(d.Target, slots)
			if reloadTotal > totalCeiling {
				t.Fatalf("slots=%d needed=%d: цель %d на клиента → перезагрузка %d "+
					"ВЫШЕ суммарного потолка %d (аудит N-1)",
					slots, needed, d.Target, reloadTotal, totalCeiling)
			}
		}
	}
}
