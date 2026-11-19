package balancer

import (
	"testing"
	"time"
)

// R549 (T-6 аудита docs/plans/2026-11-19-consistency-audit.md §1.4).
//
// ЖИВОЙ ДЕФЕКТ. Потолок `Retry-After` при async reload был 120 с, а тот же
// балансер считал время перезагрузки по своей же формуле:
// `EstimateReloadTimeMs(16.4 ГБ, 131072)` = (16.4e9/100e6) + (32 блока × 0.5 с) +
// 2 с ≈ **186 с**, живые reload'ы на стенде — 280–480 с
// (docs/plans/2026-11-19-webui-and-balancer-fixes.md §8.1). Клиент, послушавшись
// `Retry-After: 120`, возвращался, пока reload ещё шёл, получал «reload in
// progress» и запускал ВТОРОЙ — ровно тот cascading retry, от которого защищал
// R60.6. Плюс формула «база → оценка → потолок» была скопирована в двух местах
// со своим литералом 120 в каждой копии.
//
// После правки: потолок по умолчанию 480 с (измеренный верх живых reload'ов),
// формула — одна функция `asyncRetryAfterFromEstimate`, явная настройка оператора
// сильнее.

// TestR549_EstimateNotCutTo120 — оценка reload для реальной модели доходит до
// клиента, а не срезается до 120 с.
func TestR549_EstimateNotCutTo120(t *testing.T) {
	cfg := DefaultNCtxReloadConfig()

	size := int64(17_600_000_000) // 16.4 ГБ — Qwen3.8-27B-UD-Q4_K_M
	estMs := EstimateReloadTimeMs(size, 131072)
	estSec := int((estMs + 999) / 1000)
	if estSec < 150 || estSec > 220 {
		t.Fatalf("оценка reload = %d с, ожидалась ~186 с — проверьте формулу, на неё опирается Retry-After", estSec)
	}

	got := asyncRetryAfterFromEstimate(cfg, estMs)
	if got != estSec {
		t.Fatalf("Retry-After = %d с, ожидалось %d с (сама оценка): прежние 120 с возвращали клиента "+
			"вторым reload'ом, пока первый ещё шёл", got, estSec)
	}
	if got <= 120 {
		t.Fatalf("Retry-After = %d с — это прежний потолок 120, дефект T-6 вернулся", got)
	}
}

// TestR549_CeilingIsMeasuredUpperBound — потолок по умолчанию = измеренный верх
// живых reload'ов (480 с), а не 120.
func TestR549_CeilingIsMeasuredUpperBound(t *testing.T) {
	cfg := DefaultNCtxReloadConfig()
	if got := cfg.effectiveAsyncRetryAfterCeiling(); got != PreflightAsyncRetryAfterCeilingDefaultSec {
		t.Fatalf("потолок = %d, ожидался %d", got, PreflightAsyncRetryAfterCeilingDefaultSec)
	}
	if PreflightAsyncRetryAfterCeilingDefaultSec < 280 {
		t.Fatalf("потолок %d с ниже живых reload'ов (280–480 с) — клиент снова вернётся раньше времени",
			PreflightAsyncRetryAfterCeilingDefaultSec)
	}

	// Абсурдно большая оценка срезается потолком (клиенту нельзя советовать «жди час»).
	if got := asyncRetryAfterFromEstimate(cfg, 3_600_000); got != PreflightAsyncRetryAfterCeilingDefaultSec {
		t.Fatalf("оценка 1 час → %d с, ожидался срез потолком %d", got, PreflightAsyncRetryAfterCeilingDefaultSec)
	}
}

// TestR549_OperatorOverrideWins — явная настройка сильнее и дефолта, и оценки.
func TestR549_OperatorOverrideWins(t *testing.T) {
	cfg := DefaultNCtxReloadConfig()
	cfg.PreflightAsyncRetryAfterMaxSec = 1200

	if got := asyncRetryAfterFromEstimate(cfg, 900_000); got != 900 {
		t.Fatalf("оценка 900 с при потолке 1200 → %d, ожидалось 900", got)
	}
	cfg.PreflightAsyncRetryAfterMaxSec = 60
	if got := asyncRetryAfterFromEstimate(cfg, 186_000); got != 60 {
		t.Fatalf("потолок оператора 60 → %d, ожидалось 60 (его решение сильнее)", got)
	}
}

// TestR549_NoEstimateUsesConfigBase — оценки нет: работает прежняя база конфига.
func TestR549_NoEstimateUsesConfigBase(t *testing.T) {
	cfg := DefaultNCtxReloadConfig()
	if got := asyncRetryAfterFromEstimate(cfg, 0); got != cfg.effectiveAsyncRetryAfter() {
		t.Fatalf("без оценки → %d, ожидалась база конфига %d", got, cfg.effectiveAsyncRetryAfter())
	}
	if got := asyncRetryAfterFromEstimate(cfg, 0); got != 5 {
		t.Fatalf("база по умолчанию = %d, ожидалось 5 с", got)
	}
}

const bytesPerGBr549 = 1024.0 * 1024.0 * 1024.0

// TestR549_EstimateMatchesLiveNumbers — сверка «оценка против живого» на числах
// аудита: оценка не должна быть МЕНЬШЕ паспортных 140 с холодной загрузки 16 ГБ.
func TestR549_EstimateMatchesLiveNumbers(t *testing.T) {
	for _, sizeGB := range []float64{16, 27} {
		size := int64(sizeGB * bytesPerGBr549)
		sec := EstimateReloadTimeMs(size, 131072) / 1000
		if sec < 140 {
			t.Fatalf("оценка для %.0f ГБ = %d с, а живые холодные загрузки 140–485 с: "+
				"Retry-After будет короче реальности", sizeGB, sec)
		}
		if time.Duration(sec)*time.Second > 600*time.Second {
			t.Fatalf("оценка для %.0f ГБ = %d с — подозрительно много, проверьте формулу", sizeGB, sec)
		}
	}
}
