// prompt_calibration_r453_test.go — R453 (2026-11-06): калибровка не должна
// завышать оценку больших промптов, а короткие промпты не должны учить плотности.
//
// ЖИВОЙ ФАКТ (файл /app/data/prompt_calibration.json на стенде):
//
//	"Qwen3-4B-Instruct-2507-Q4_K_M": {"ratio": 1.84375, "samples": 55,
//	                                   "last_raw_estimated": 8, "last_actual": 11}
//
// То есть одним из замеров, сформировавших отношение 1.84, был промпт «Привет!»
// (raw=8 → actual=11): почти весь actual там — шаблон чата, BOS и схемы
// инструментов, а не токены текста. Потом это отношение умножалось на оценку
// больших промптов: 44137 символов английского текста → raw ≈ 27k → ×1.84 =
// 49731, тогда как фактический промпт был 9816 токенов. Ошибка в 5 раз — и
// окно росло там, где не нужно (рост до 61206 при промпте ~10k).
package balancer

import (
	"testing"
	"time"
)

// newR453Calib — калибровка с выученным отношением 1.84 (как на стенде) без
// записи на диск.
func newR453Calib(t *testing.T) *promptCalibration {
	t.Helper()
	c := newPromptCalibration("")
	now := time.Now()
	// Два полноценных замера формируют отношение: 17616/9550 ≈ 1.8446.
	for i := 0; i < 2; i++ {
		c.note("m", 9550, now)
		c.observe("m", 17616, now)
	}
	return c
}

// TestR453_ShortSampleDoesNotTeachRatio — замер на коротком промпте не учит.
func TestR453_ShortSampleDoesNotTeachRatio(t *testing.T) {
	c := newPromptCalibration("")
	now := time.Now()

	before, _, ok := c.observe("m", 11, now) // нет ожидания — не учимся
	if ok || before != 0 {
		t.Fatalf("замер без note принят: ratio=%v ok=%v", before, ok)
	}

	c.note("m", 8, now) // «Привет!» — 8 токенов по raw
	if ratio, samples, ok := c.observe("m", 11, now); ok {
		t.Fatalf("короткий промпт (raw=8) попал в обучение: ratio=%v samples=%d", ratio, samples)
	}
	if got, source := c.calibrate("m", 8, 0); got != 8 || source != "raw" {
		t.Fatalf("без обучения оценка короткого промпта изменилась: %d (source=%q, want 8/raw)", got, source)
	}
}

// TestR453_LargePromptIsNotAmplified — главный дефект: 1.84 не применяется к
// большому промпту (оценка уже консервативна).
func TestR453_LargePromptIsNotAmplified(t *testing.T) {
	c := newR453Calib(t)

	// Контроль: отношение действительно выучено и ниже порога применяется.
	if got, source := c.calibrate("m", 2000, 0); source != "calibrated" || got <= 2000 {
		t.Fatalf("подготовка теста: ratio не применяется ниже порога (got=%d source=%q)", got, source)
	}

	// Живые числа: raw ≈ 27000 (44137 символов английского) — оценка не растёт.
	if got, source := c.calibrate("m", 27000, 0); source != "raw_large" || got != 27000 {
		t.Fatalf("оценка большого промпта = %d (source=%q), want 27000 без умножения", got, source)
	}

	// И живой замер: raw 49731 из лога тоже не усиливается.
	if got, source := c.calibrate("m", 49731, 0); source != "raw_large" || got != 49731 {
		t.Fatalf("raw 49731 → %d (source=%q), want без изменения", got, source)
	}
}

// TestR453_SmallPromptStillCompensates — короткому промпту отношение по-прежнему
// нужно: там сидят накладные расходы шаблона.
func TestR453_SmallPromptStillCompensates(t *testing.T) {
	c := newR453Calib(t)
	got, source := c.calibrate("m", 100, 0)
	if source != "calibrated" {
		t.Fatalf("маленький промпт обязан калиброваться, source=%q", source)
	}
	if got <= 100 {
		t.Fatalf("оценка %d не больше raw 100 — компенсация накладных расходов пропала", got)
	}
}

// TestR453_ReductionStillApplies — снижение на большом промпте остаётся
// (не-ASCII запасной коэффициент и выученное отношение < 1).
func TestR453_ReductionStillApplies(t *testing.T) {
	// Не-ASCII: запасной коэффициент 1/2.5 применяется и к большому промпту.
	c := newPromptCalibration("")
	if got, source := c.calibrate("m", 10000, 0.9); source != "fallback_nonascii" || got != 4000 {
		t.Fatalf("не-ASCII запасной коэффициент на большом промпте: got=%d source=%q, want 4000", got, source)
	}

	// Выученное отношение < 1 — снижение тоже применяется.
	c2 := newPromptCalibration("")
	now := time.Now()
	for i := 0; i < 2; i++ {
		c2.note("m", 10000, now)
		c2.observe("m", 7000, now) // ratio 0.7
	}
	if got, source := c2.calibrate("m", 20000, 0); source != "calibrated" || got != 14000 {
		t.Fatalf("снижение большого промпта: got=%d source=%q, want 14000", got, source)
	}
}

// TestR453_ThresholdBoundaries — границы: 4095 усиливается, 4096 — нет.
func TestR453_ThresholdBoundaries(t *testing.T) {
	c := newR453Calib(t)

	below, sourceBelow := c.calibrate("m", promptCalibNoAmplifyRaw-1, 0)
	if sourceBelow != "calibrated" || below <= promptCalibNoAmplifyRaw-1 {
		t.Fatalf("ниже порога усиление обязано остаться: got=%d source=%q", below, sourceBelow)
	}
	at, sourceAt := c.calibrate("m", promptCalibNoAmplifyRaw, 0)
	if sourceAt != "raw_large" || at != promptCalibNoAmplifyRaw {
		t.Fatalf("на пороге усиления быть не должно: got=%d source=%q", at, sourceAt)
	}
}
