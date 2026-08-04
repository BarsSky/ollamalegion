package cppbackend

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestModelMetrics_RecordDuration_Percentiles_Basic — фиксированный набор
// durations, проверяем p50/p95/p99 через nearest-rank.
func TestModelMetrics_RecordDuration_Percentiles_Basic(t *testing.T) {
	t.Parallel()
	mm := &ModelMetrics{Name: "test-model"}
	// 100 сэмплов: 10, 20, 30, ..., 1000
	for i := int64(10); i <= 1000; i += 10 {
		mm.recordDuration(i)
	}
	p50, p95, p99, count := mm.Percentiles()
	if count != 100 {
		t.Fatalf("expected count=100, got %d", count)
	}
	// Nearest-rank: sorted[idx*50/100] = sorted[50] = 510
	if p50 != 510 {
		t.Errorf("p50: expected 510, got %d", p50)
	}
	// sorted[95] = 960
	if p95 != 960 {
		t.Errorf("p95: expected 960, got %d", p95)
	}
	// sorted[99] = 1000
	if p99 != 1000 {
		t.Errorf("p99: expected 1000, got %d", p99)
	}
}

// TestModelMetrics_RecordDuration_RingBufferOverflow — больше 256 samples,
// старые должны перезаписываться (FIFO).
func TestModelMetrics_RecordDuration_RingBufferOverflow(t *testing.T) {
	t.Parallel()
	mm := &ModelMetrics{Name: "test-model"}
	// 300 сэмплов: 1..300
	for i := int64(1); i <= 300; i++ {
		mm.recordDuration(i)
	}
	// Должно остаться последние 256: 45..300
	p50, p95, p99, count := mm.Percentiles()
	if count != 256 {
		t.Fatalf("expected count=256 (ring buffer full), got %d", count)
	}
	// p50 = sorted[128] = 45 + 128 - 1 = 172
	if p50 != 172 {
		t.Errorf("p50: expected 172, got %d", p50)
	}
	// p95 = sorted[243] = 45 + 243 - 1 = 287
	if p95 != 287 {
		t.Errorf("p95: expected 287, got %d", p95)
	}
	// p99 = sorted[254] = 45 + 254 - 1 = 298
	if p99 != 298 {
		t.Errorf("p99: expected 298, got %d", p99)
	}
}

// TestModelMetrics_PercentilesEmpty — нет сэмплов → все 0, count=0.
func TestModelMetrics_PercentilesEmpty(t *testing.T) {
	t.Parallel()
	mm := &ModelMetrics{Name: "test-model"}
	p50, p95, p99, count := mm.Percentiles()
	if p50 != 0 || p95 != 0 || p99 != 0 || count != 0 {
		t.Fatalf("expected all zero, got p50=%d p95=%d p99=%d count=%d", p50, p95, p99, count)
	}
}

// TestModelMetrics_RecordDurationConcurrent — 100 горутин, 100 sample каждая.
// После завершения count=256 (буфер полон), все percentiles — конечные.
func TestModelMetrics_RecordDurationConcurrent(t *testing.T) {
	t.Parallel()
	mm := &ModelMetrics{Name: "test-model"}
	var wg sync.WaitGroup
	const goroutines = 100
	const samplesPerGoroutine = 100
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < samplesPerGoroutine; i++ {
				mm.recordDuration(int64(g*1000 + i))
			}
		}(g)
	}
	wg.Wait()
	_, _, _, count := mm.Percentiles()
	if count != 256 {
		t.Fatalf("expected 256 samples after concurrent writes (ring buffer full), got %d", count)
	}
}

// TestMetrics_RecordRequest_PercentilesPopulated — RecordRequest должен
// заполнять per-model ring buffer (duration > 0).
func TestMetrics_RecordRequest_PercentilesPopulated(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	// 50 запросов по 100ms
	for i := 0; i < 50; i++ {
		m.RecordRequest("model-a", 100, 100*time.Millisecond, true)
	}
	snap := m.GetModelMetricsSnapshot()
	entry, ok := snap["model-a"]
	if !ok {
		t.Fatal("model-a should be in snapshot")
	}
	if entry.Requests != 50 {
		t.Errorf("requests: expected 50, got %d", entry.Requests)
	}
	if entry.P50Ms != 100 {
		t.Errorf("p50_ms: expected 100, got %d", entry.P50Ms)
	}
	if entry.P95Ms != 100 {
		t.Errorf("p95_ms: expected 100, got %d", entry.P95Ms)
	}
	if entry.AvgMs != 100 {
		t.Errorf("avg_ms: expected 100, got %d", entry.AvgMs)
	}
	if entry.LastUsedAt == "" {
		t.Error("last_used_at should be set")
	}
}

// TestMetrics_RecordRequest_ZeroDurationExcluded — RecordRequest с duration=0
// НЕ должен писать в ring buffer (иначе p50 станет 0).
func TestMetrics_RecordRequest_ZeroDurationExcluded(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	// 10 запросов с duration=0 (например, ошибка до inference)
	for i := 0; i < 10; i++ {
		m.RecordRequest("model-a", 0, 0, false)
	}
	// + 1 запрос с duration=100ms
	m.RecordRequest("model-a", 0, 100*time.Millisecond, true)
	snap := m.GetModelMetricsSnapshot()
	entry := snap["model-a"]
	if entry.Requests != 11 {
		t.Errorf("requests: expected 11 (10 + 1), got %d", entry.Requests)
	}
	if entry.Errors != 10 {
		t.Errorf("errors: expected 10, got %d", entry.Errors)
	}
	// Только 1 sample в ring buffer (duration > 0)
	if entry.P50Ms != 100 {
		t.Errorf("p50_ms: expected 100 (only non-zero durations in buffer), got %d", entry.P50Ms)
	}
}

// TestMetrics_RecordRequest_ErrorRate — error_rate = errors / requests.
func TestMetrics_RecordRequest_ErrorRate(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	for i := 0; i < 10; i++ {
		m.RecordRequest("model-a", 0, 100*time.Millisecond, i < 8) // 8 success, 2 errors
	}
	snap := m.GetModelMetricsSnapshot()
	entry := snap["model-a"]
	if entry.ErrorRate < 0.199 || entry.ErrorRate > 0.201 {
		t.Errorf("error_rate: expected ~0.2 (2/10), got %f", entry.ErrorRate)
	}
}

// TestMetrics_GetModelMetricsSnapshot_NilSafe — nil receiver → пустая map, не nil.
func TestMetrics_GetModelMetricsSnapshot_NilSafe(t *testing.T) {
	t.Parallel()
	var m *Metrics
	snap := m.GetModelMetricsSnapshot()
	if snap == nil {
		t.Fatal("nil receiver should return empty map, not nil")
	}
	if len(snap) != 0 {
		t.Fatalf("expected empty, got %v", snap)
	}
}

// TestMetrics_GetModelMetricsSnapshot_MultipleModels — несколько моделей,
// каждая со своими метриками.
func TestMetrics_GetModelMetricsSnapshot_MultipleModels(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	m.RecordRequest("qwen3-4b", 100, 200*time.Millisecond, true)
	m.RecordRequest("qwen3-4b", 100, 300*time.Millisecond, true)
	m.RecordRequest("gemma-4b", 100, 100*time.Millisecond, false)
	m.RecordRequest("gemma-4b", 100, 150*time.Millisecond, true)

	snap := m.GetModelMetricsSnapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 models, got %d", len(snap))
	}
	if snap["qwen3-4b"].Requests != 2 {
		t.Errorf("qwen3-4b requests: expected 2, got %d", snap["qwen3-4b"].Requests)
	}
	if snap["gemma-4b"].Requests != 2 {
		t.Errorf("gemma-4b requests: expected 2, got %d", snap["gemma-4b"].Requests)
	}
	if snap["gemma-4b"].Errors != 1 {
		t.Errorf("gemma-4b errors: expected 1, got %d", snap["gemma-4b"].Errors)
	}
	if snap["gemma-4b"].ErrorRate != 0.5 {
		t.Errorf("gemma-4b error_rate: expected 0.5, got %f", snap["gemma-4b"].ErrorRate)
	}
}

// TestModelMetrics_Percentiles_LockNoLeak — Percentiles() НЕ держать mutex
// дольше чем нужно (sanity: нельзя deadlock'нуть concurrent recordDuration).
// 1000 горутин: 500 recordDuration + 500 Percentiles. Должно завершиться быстро.
func TestModelMetrics_Percentiles_LockNoLeak(t *testing.T) {
	t.Parallel()
	mm := &ModelMetrics{Name: "test-model"}
	// seed: 100 samples
	for i := int64(1); i <= 100; i++ {
		mm.recordDuration(i)
	}
	var wg sync.WaitGroup
	const goroutines = 500
	wg.Add(goroutines * 2)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			mm.recordDuration(int64(i))
		}(i)
		go func() {
			defer wg.Done()
			_, _, _, _ = mm.Percentiles()
		}()
	}
	wg.Wait()
}

// TestModelMetrics_AtomicCounters_ConcurrentSafe — счётчики Request/Errors/etc.
// должны быть thread-safe (atomic.Int64 гарантирует).
func TestModelMetrics_AtomicCounters_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	mm := &ModelMetrics{Name: "test-model"}
	var wg sync.WaitGroup
	const goroutines = 100
	const incsPerGoroutine = 1000
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < incsPerGoroutine; j++ {
				mm.Requests.Add(1)
				mm.Tokens.Add(10)
				mm.TotalDuration.Add(100)
			}
		}()
	}
	wg.Wait()
	expected := int64(goroutines * incsPerGoroutine)
	if got := mm.Requests.Load(); got != expected {
		t.Errorf("Requests: expected %d, got %d", expected, got)
	}
	if got := mm.Tokens.Load(); got != expected*10 {
		t.Errorf("Tokens: expected %d, got %d", expected*10, got)
	}
	// Sanity: atomic package import не unused
	_ = atomic.Int64{}
}
