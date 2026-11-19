package balancer

import (
	"testing"
	"time"
)

// R550 (T-10 аудита docs/plans/2026-11-19-consistency-audit.md §1.4).
//
// ЖИВОЙ ДЕФЕКТ: `ModelLatencyTracker.RecordSample` вызывался только из тестов,
// поэтому «измеренная скорость декода» на карточке группы не могла появиться
// никогда (всегда «н/д»), а комментарии про 3-tier приоритет таймаутов описывали
// механизм без данных.
//
// После правки замер пишется из живого завершения стрима, и цепочка
// «замер → трекер → карточка группы» проверяется здесь end-to-end.

// TestR550_SampleFeedsGroupCard — замер доходит до карточки группы как ЧИСЛО.
func TestR550_SampleFeedsGroupCard(t *testing.T) {
	p := newProxyWithCleanup(t, createTestConfig())

	// 100 токенов за 10 с = 10 tok/s. Старт «10 с назад» — как у реального стрима.
	start := time.Now().Add(-10 * time.Second)
	p.recordGenerationSample("qwen3-8b", 100, start, start.Add(2*time.Second))

	stats := p.modelLatencyTracker.GetStats("qwen3-8b")
	if stats.AvgTokensPerSec <= 0 {
		t.Fatalf("после живого замера AvgTokensPerSec = %v, ожидалось > 0: "+
			"именно это значение карточка группы показывала как «н/д»", stats.AvgTokensPerSec)
	}
	if stats.AvgTokensPerSec < 5 || stats.AvgTokensPerSec > 20 {
		t.Fatalf("AvgTokensPerSec = %.2f, ожидалось ~10 (100 токенов за 10 с)", stats.AvgTokensPerSec)
	}

	info := p.groupModelInfoR104("qwen3-8b")
	if !info.TokensKnown {
		t.Fatal("карточка группы: TokensKnown=false после замера — «измеренная скорость» так и не появилась")
	}
	if info.TokensPerSecond < 5 || info.TokensPerSecond > 20 {
		t.Fatalf("карточка группы: %.2f tok/s, ожидалось ~10", info.TokensPerSecond)
	}
}

// TestR550_NoSampleNoInventedNumber — без замера карточка честно «н/д»
// (нуля не выдумываем: ноль означал бы «измерено и очень медленно»).
func TestR550_NoSampleNoInventedNumber(t *testing.T) {
	p := newProxyWithCleanup(t, createTestConfig())

	info := p.groupModelInfoR104("qwen3-8b")
	if info.TokensKnown {
		t.Fatalf("без единого замера карточка сообщает известную скорость %.2f — число выдумано",
			info.TokensPerSecond)
	}
}

// TestR550_BadSamplesIgnored — «служебные» замеры не записываются, иначе tok/s
// получается 10⁴–10⁵ и «измерение» врёт сильнее, чем его отсутствие.
func TestR550_BadSamplesIgnored(t *testing.T) {
	p := newProxyWithCleanup(t, createTestConfig())

	// Нулевые токены (tool-call без текста) — не генерация.
	p.recordGenerationSample("m1", 0, time.Now().Add(-time.Second), time.Time{})
	// Слишком короткий ответ: 5 токенов за 3 мс = ~1600 tok/s — шум планировщика.
	start := time.Now().Add(-3 * time.Millisecond)
	p.recordGenerationSample("m2", 5, start, time.Time{})
	// Нулевой старт (время неизвестно) — считать нечего.
	p.recordGenerationSample("m3", 100, time.Time{}, time.Time{})

	for _, model := range []string{"m1", "m2", "m3"} {
		if got := p.groupModelInfoR104(model); got.TokensKnown {
			t.Fatalf("%s: записан замер, которого не должно быть (%.2f tok/s) — "+
				"выдуманные числа хуже отсутствия числа", model, got.TokensPerSecond)
		}
	}
}

// TestR550_FirstByteLatencyRecorded — задержка первого дельта попадает в замер
// (это второй источник «почему медленно»: prefill против декода).
func TestR550_FirstByteLatencyRecorded(t *testing.T) {
	p := newProxyWithCleanup(t, createTestConfig())

	start := time.Now().Add(-10 * time.Second)
	p.recordGenerationSample("qwen3-8b", 100, start, start.Add(3*time.Second))

	stats := p.modelLatencyTracker.GetStats("qwen3-8b")
	if stats.AvgFirstByteMs <= 0 {
		t.Fatalf("AvgFirstByteMs = %v, ожидалось > 0 (первый дельт через 3 с)", stats.AvgFirstByteMs)
	}
	if stats.AvgFirstByteMs < 2000 || stats.AvgFirstByteMs > 4500 {
		t.Fatalf("AvgFirstByteMs = %.0f, ожидалось ~3000", stats.AvgFirstByteMs)
	}
}
