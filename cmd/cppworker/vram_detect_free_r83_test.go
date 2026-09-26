//go:build llama_stub

// vram_detect_free_r83_test.go — R83 §9.4 шаг 4 (2026-09-26).
//
// Найденный дефект: legacy-фоллбэк раскладки слоёв спрашивал
// `availableVRAMBytes()` — а это ПОЛНАЯ ёмкость карты (bridge отдаёт
// VRAMTotalMB), не свободная. На живом стенде 3070 8 GB это 8191 MB против
// ~1033 MB фактически свободных, то есть оценка была оптимистична в 8 раз.
// Теперь auto_offload берёт `freeVRAMBytes()`, а функция переименована в
// `availableVRAMBytes()` — чтобы имя не обещало «доступно».
//
// Тест фиксирует семантику обеих функций через env-оверрайды (в stub-сборке
// NVML недоступен, поэтому источник ровно один — переменные окружения).
package main

import "testing"

// TestR83_VRAMHelpers_FreeVsTotal — «free» и «total» — разные величины, и
// каждая читает СВОЮ переменную окружения.
func TestR83_VRAMHelpers_FreeVsTotal(t *testing.T) {
	t.Setenv("CPPWORKER_FREE_VRAM_BYTES", "1083371520") // ~1033 MB (живой замер)
	t.Setenv("CPPWORKER_VRAM_BYTES", "8589934592")     // 8 GiB — ёмкость карты

	if got := freeVRAMBytes(); got != 1083371520 {
		t.Errorf("freeVRAMBytes() = %d, want 1083371520 (свободно, а не ёмкость)", got)
	}
	if got := availableVRAMBytes(); got != 8589934592 {
		t.Errorf("availableVRAMBytes() = %d, want 8589934592 (полная ёмкость)", got)
	}
	if freeVRAMBytes() >= availableVRAMBytes() {
		t.Error("на стенде с загруженной моделью свободной VRAM обязано быть МЕНЬШЕ ёмкости")
	}
}

// TestR83_VRAMHelpers_FreeOnlyNoTotal — если задан только free, total-функция
// берёт ёмкость из GPU (bridge/nvidia-smi), а не подсовывает free как ёмкость.
// Проверяем именно это: результат либо 0 (GPU недоступен), либо >= free, но
// НИКОГДА не равен ему «по ошибке источника».
func TestR83_VRAMHelpers_FreeOnlyNoTotal(t *testing.T) {
	t.Setenv("CPPWORKER_FREE_VRAM_BYTES", "1073741824")
	t.Setenv("CPPWORKER_VRAM_BYTES", "")

	free := freeVRAMBytes()
	if free != 1073741824 {
		t.Errorf("freeVRAMBytes() = %d, want 1073741824", free)
	}
	total := availableVRAMBytes()
	if total != 0 && total < free {
		t.Errorf("availableVRAMBytes() = %d — ёмкость карты меньше свободной VRAM (%d)", total, free)
	}
}

// TestR83_VRAMHelpers_TotalAsFreeFallback — если задан только
// CPPWORKER_VRAM_BYTES, free-функция по контракту использует его как оверрайд
// (оператор явно сказал «считай эту VRAM свободной»).
func TestR83_VRAMHelpers_TotalAsFreeFallback(t *testing.T) {
	t.Setenv("CPPWORKER_FREE_VRAM_BYTES", "")
	t.Setenv("CPPWORKER_VRAM_BYTES", "4294967296") // 4 GiB

	if got := freeVRAMBytes(); got != 4294967296 {
		t.Errorf("freeVRAMBytes() = %d, want 4294967296 (env-оверрайд)", got)
	}
}
