//go:build llama_stub

// load_window_nctx_test.go — R522 (§8.3.3): гейт обязан проверять ФИНАЛЬНОЕ
// окно загрузки.
//
// ЖИВОЙ ФАКТ (архив 2026-11-18, A10 24 ГБ, 27B Q4_K_M): в логе рядом
// «гейт requested_n_ctx=32768 stage=exact_fit feasible_max_context=182340» и
// через 52 мс «total_n_ctx=131072». Гейт проверял профильный хинт, загрузка
// брала окно клиента на слот × слоты; на compute buffer места не осталось, и
// первый decode упал с CUDA OOM.
package cppbackend

import "testing"

func TestR522_GateSeesFinalTotalWindow(t *testing.T) {
	// Ровно архив: профиль 32768 (total как есть), клиент просит 131072 НА КЛИЕНТА,
	// один слот → загрузится 131072, и именно это обязан проверить гейт.
	total, slots := LoadWindowNCtx(32768, 131072, 1, 1)
	if total != 131072 || slots != 1 {
		t.Fatalf("LoadWindowNCtx(32768, 131072, 1, 1) = (%d, %d), want (131072, 1)", total, slots)
	}
	if total == 32768 {
		t.Fatalf("гейт снова увидит 32768 при загрузке в 131072 — это и был дефект §8.3.3")
	}
}

func TestR522_PerSeqTimesSlots(t *testing.T) {
	total, slots := LoadWindowNCtx(32768, 16384, 2, 1)
	if total != 32768 || slots != 2 {
		t.Fatalf("(32768, 16384, parallel=2) = (%d, %d), want (32768, 2)", total, slots)
	}
}

func TestR522_PaddingAppliedLikeLlamaCpp(t *testing.T) {
	total, _ := LoadWindowNCtx(0, 1000, 1, 1)
	if total != 1024 {
		t.Fatalf("окно на клиента 1000 обязано выровняться до 1024 (GGML_PAD 256), получено %d", total)
	}
}

func TestR522_NoPerSeqMeansTotalAsIs(t *testing.T) {
	total, slots := LoadWindowNCtx(65536, 0, 0, 4)
	if total != 65536 || slots != 4 {
		t.Fatalf("(65536, perSeq=0, parallel=0, default=4) = (%d, %d), want (65536, 4)", total, slots)
	}
}

func TestR522_DefaultParallelUsedWhenUnset(t *testing.T) {
	// parallel не задан, дефолт конфига 4: 8192 на клиента → 32768 суммарно.
	total, slots := LoadWindowNCtx(0, 8192, 0, 4)
	if total != 32768 || slots != 4 {
		t.Fatalf("(perSeq=8192, default=4) = (%d, %d), want (32768, 4)", total, slots)
	}
	// И без дефолта — один слот.
	total, slots = LoadWindowNCtx(32768, 0, 0, 0)
	if total != 32768 || slots != 1 {
		t.Fatalf("без слотов = (%d, %d), want (32768, 1)", total, slots)
	}
}
