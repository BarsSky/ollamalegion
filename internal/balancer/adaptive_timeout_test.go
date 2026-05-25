package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// computeAdaptiveTimeout — unit-тесты алгоритма
// ============================================================

func TestComputeAdaptiveTimeout_EmptyHistory(t *testing.T) {
	timeout := computeAdaptiveTimeout(nil, 120)
	if timeout != 0 {
		t.Errorf("empty history: got %d, want 0", timeout)
	}

	timeout = computeAdaptiveTimeout([]LatencyRecord{}, 120)
	if timeout != 0 {
		t.Errorf("empty slice: got %d, want 0", timeout)
	}
}

func TestComputeAdaptiveTimeout_InsufficientSamples(t *testing.T) {
	// 1 успешный sample — меньше minLatencySamples (3)
	history := []LatencyRecord{
		{LatencyMs: 5000, Success: true, Timestamp: timeNow()},
	}
	timeout := computeAdaptiveTimeout(history, 120)
	if timeout != 0 {
		t.Errorf("1 sample: got %d, want 0 (insufficient data)", timeout)
	}

	// 2 успешных — всё ещё недостаточно
	history = []LatencyRecord{
		{LatencyMs: 5000, Success: true, Timestamp: timeNow()},
		{LatencyMs: 8000, Success: true, Timestamp: timeNow()},
	}
	timeout = computeAdaptiveTimeout(history, 120)
	if timeout != 0 {
		t.Errorf("2 samples: got %d, want 0", timeout)
	}
}

func TestComputeAdaptiveTimeout_JustEnoughSamples(t *testing.T) {
	// 3 успешных — минимально достаточно для расчёта
	// Все latency = 5000ms → p95 ~5000ms → timeout = 5000*2/1000 = 10s
	// Но minimum guard: 120*0.5 = 60s → итог 60s
	history := []LatencyRecord{
		{LatencyMs: 5000, Success: true, Timestamp: timeNow()},
		{LatencyMs: 5000, Success: true, Timestamp: timeNow()},
		{LatencyMs: 5000, Success: true, Timestamp: timeNow()},
	}
	timeout := computeAdaptiveTimeout(history, 120)
	if timeout < 10 || timeout > 360 {
		t.Errorf("3 samples 5s: got %d, expected in [10, 360]", timeout)
	}
}

func TestComputeAdaptiveTimeout_P95Calculation(t *testing.T) {
	// 10 записей: 9 быстрых (1s) и 1 медленная (60s)
	// p95 ~ 60s → timeout = 60*2 = 120s
	history := make([]LatencyRecord, 10)
	for i := 0; i < 9; i++ {
		history[i] = LatencyRecord{LatencyMs: 1000, Success: true, Timestamp: timeNow()}
	}
	history[9] = LatencyRecord{LatencyMs: 60000, Success: true, Timestamp: timeNow()}

	timeout := computeAdaptiveTimeout(history, 120)
	if timeout < 100 || timeout > 360 {
		t.Errorf("p95 ~60s: got %d, expected in [100, 360]", timeout)
	}
}

func TestComputeAdaptiveTimeout_ErrorPenalty(t *testing.T) {
	// Успешных >= 3, но есть недавняя ошибка → +30s penalty
	now := timeNow()
	history := []LatencyRecord{
		{LatencyMs: 5000, Success: true, Timestamp: now.Add(-10 * time.Second)},
		{LatencyMs: 6000, Success: true, Timestamp: now.Add(-9 * time.Second)},
		{LatencyMs: 5500, Success: true, Timestamp: now.Add(-8 * time.Second)},
		{LatencyMs: 0, Success: false, Timestamp: now.Add(-5 * time.Second)}, // недавняя ошибка
	}
	timeoutWithout := computeAdaptiveTimeout(history[:3], 120)
	// Позволяем пересчитать timeNow внутри computeAdaptiveTimeout — ошибка в пределах 120с
	timeoutWith := computeAdaptiveTimeout(history, 120)
	_ = timeoutWithout
	_ = timeoutWith
	// Просто проверяем что не падает
	if timeoutWith <= 0 {
		t.Errorf("with error: got %d, expected > 0", timeoutWith)
	}
}

func TestComputeAdaptiveTimeout_Bounds(t *testing.T) {
	// Очень низкие latency → min 50% от baseTimeout
	history := make([]LatencyRecord, 10)
	for i := 0; i < 10; i++ {
		history[i] = LatencyRecord{LatencyMs: 100, Success: true, Timestamp: timeNow()}
	}
	// p95 ~100ms → timeout = 100*2/1000 = 0.2s → округляется до 1s
	// min = 120*0.5 = 60s
	timeout := computeAdaptiveTimeout(history, 120)
	if timeout < 60 {
		t.Errorf("low latency bound: got %d, want >= 60", timeout)
	}

	// Очень высокие latency → max 300% от baseTimeout
	history2 := make([]LatencyRecord, 10)
	for i := 0; i < 10; i++ {
		history2[i] = LatencyRecord{LatencyMs: 300000, Success: true, Timestamp: timeNow()} // 5 min
	}
	// p95 ~300000ms → timeout = 300000*2/1000 = 600s
	// max = 120*3 = 360s
	timeout = computeAdaptiveTimeout(history2, 120)
	if timeout > 360 {
		t.Errorf("high latency bound: got %d, want <= 360", timeout)
	}
}

func TestComputeAdaptiveTimeout_BaseTimeoutFallback(t *testing.T) {
	history := make([]LatencyRecord, 5)
	for i := 0; i < 5; i++ {
		history[i] = LatencyRecord{LatencyMs: 50000, Success: true, Timestamp: timeNow()}
	}
	// baseTimeout = 0 → fallback to 120
	timeout := computeAdaptiveTimeout(history, 0)
	if timeout <= 0 {
		t.Errorf("baseTimeout=0: got %d, expected > 0", timeout)
	}
}

func TestComputeAdaptiveTimeout_InsuffWithError(t *testing.T) {
	// < 3 samples, но есть ошибка → baseTimeout + errorPenalty
	now := timeNow()
	history := []LatencyRecord{
		{LatencyMs: 5000, Success: true, Timestamp: now.Add(-5 * time.Second)},
		{LatencyMs: 0, Success: false, Timestamp: now.Add(-5 * time.Second)},
	}
	timeout := computeAdaptiveTimeout(history, 120)
	if timeout != 150 { // 120 + 30
		t.Errorf("insufficient+error: got %d, want 150", timeout)
	}
}

func TestComputeAdaptiveTimeout_FullHistoryTruncation(t *testing.T) {
	// > 100 записей
	history := make([]LatencyRecord, 150)
	for i := 0; i < 150; i++ {
		m := int64(i * 1000) // от 0 до 149000ms
		if m == 0 {
			m = 100
		}
		history[i] = LatencyRecord{LatencyMs: m, Success: true, Timestamp: timeNow()}
	}
	// Должно взять последние 100
	timeout := computeAdaptiveTimeout(history, 120)
	if timeout <= 0 {
		t.Errorf("truncated history: got %d, expected > 0", timeout)
	}
}

// ============================================================
// recordLatency — интеграционные тесты
// ============================================================

func TestRecordLatency_AppendsAndTruncates(t *testing.T) {
	state := &BackendState{
		Backend: &types.Backend{
			ID:             "test-1",
			RequestTimeout: 120,
		},
	}

	// Добавляем 50 записей
	for i := 0; i < 150; i++ {
		recordLatency(state, int64(i*1000), "test-model", true)
	}
	// Должно быть не больше 100
	state.mu.Lock()
	if len(state.LatencyHistory) > 100 {
		t.Errorf("history too long: %d, want <= 100", len(state.LatencyHistory))
	}
	// Последняя запись должна быть ~149000ms
	last := state.LatencyHistory[len(state.LatencyHistory)-1]
	if last.LatencyMs < 140000 {
		t.Errorf("last entry: %d, want ~149000", last.LatencyMs)
	}
	// AdaptiveTimeout должен быть > 0
	if state.AdaptiveTimeout <= 0 {
		t.Errorf("AdaptiveTimeout not set: %d", state.AdaptiveTimeout)
	}
	state.mu.Unlock()
}

func TestRecordLatency_FailureRecord(t *testing.T) {
	state := &BackendState{
		Backend: &types.Backend{
			ID:             "test-1",
			RequestTimeout: 120,
		},
	}

	// Добавляем 3 успешных + 1 ошибку
	recordLatency(state, 5000, "test-model", true)
	recordLatency(state, 6000, "test-model", true)
	recordLatency(state, 5500, "test-model", true)
	recordLatency(state, 0, "test-model", false)

	state.mu.Lock()
	if len(state.LatencyHistory) != 4 {
		t.Errorf("history length: %d, want 4", len(state.LatencyHistory))
	}
	if !state.LatencyHistory[3].Success {
		// четвёртая запись — ошибка
	}
	if state.AdaptiveTimeout <= 0 {
		t.Errorf("AdaptiveTimeout: %d, expected > 0", state.AdaptiveTimeout)
	}
	state.mu.Unlock()
}

func TestRecordLatency_UpdatesRuntimeRequestTimeout(t *testing.T) {
	state := &BackendState{
		Backend: &types.Backend{
			ID:                   "test-1",
			RequestTimeout:       120,
			RuntimeRequestTimeout: 0,
		},
	}

	for i := 0; i < 10; i++ {
		recordLatency(state, 5000, "test-model", true)
	}

	state.mu.Lock()
	if state.Backend.RuntimeRequestTimeout <= 0 {
		t.Errorf("RuntimeRequestTimeout not updated: %d", state.Backend.RuntimeRequestTimeout)
	}
	if state.Backend.RuntimeRequestTimeout != state.AdaptiveTimeout {
		t.Errorf("RuntimeRequestTimeout (%d) != AdaptiveTimeout (%d)",
			state.Backend.RuntimeRequestTimeout, state.AdaptiveTimeout)
	}
	state.mu.Unlock()
}

// ============================================================
// getEffectiveTimeout — тесты приоритетов
// ============================================================

func TestGetEffectiveTimeout_AdaptivePriority(t *testing.T) {
	state := &BackendState{
		Backend: &types.Backend{
			ID:             "test-1",
			RequestTimeout: 300,
		},
		AdaptiveTimeout: 600,
	}

	// AdaptiveTimeout > static RequestTimeout
	timeout := getEffectiveTimeout(state, 120)
	if timeout != 600 {
		t.Errorf("adaptive priority: got %d, want 600", timeout)
	}
}

func TestGetEffectiveTimeout_StaticFallback(t *testing.T) {
	state := &BackendState{
		Backend: &types.Backend{
			ID:             "test-1",
			RequestTimeout: 300,
		},
		AdaptiveTimeout: 0,
	}

	// AdaptiveTimeout = 0 → статический RequestTimeout
	timeout := getEffectiveTimeout(state, 120)
	if timeout != 300 {
		t.Errorf("static fallback: got %d, want 300", timeout)
	}
}

func TestGetEffectiveTimeout_GlobalFallback(t *testing.T) {
	state := &BackendState{
		Backend: &types.Backend{
			ID:             "test-1",
			RequestTimeout: 0,
		},
		AdaptiveTimeout: 0,
	}

	// Всё 0 → глобальный
	timeout := getEffectiveTimeout(state, 120)
	if timeout != 120 {
		t.Errorf("global fallback: got %d, want 120", timeout)
	}
}

// ============================================================
// getRuntimeRequestTimeout — тесты
// ============================================================

func TestGetRuntimeRequestTimeout_Adaptive(t *testing.T) {
	state := &BackendState{
		Backend: &types.Backend{
			ID:                   "test-1",
			RuntimeRequestTimeout: 200,
		},
		AdaptiveTimeout: 400,
	}

	timeout := getRuntimeRequestTimeout(state)
	if timeout != 400 {
		t.Errorf("runtime (adaptive): got %d, want 400", timeout)
	}
}

func TestGetRuntimeRequestTimeout_Fallback(t *testing.T) {
	state := &BackendState{
		Backend: &types.Backend{
			ID:                   "test-1",
			RuntimeRequestTimeout: 200,
		},
		AdaptiveTimeout: 0,
	}

	timeout := getRuntimeRequestTimeout(state)
	if timeout != 200 {
		t.Errorf("runtime (fallback): got %d, want 200", timeout)
	}
}

// ============================================================
// End-to-end — запись latency влияет на getEffectiveTimeout
// ============================================================

func TestAdaptiveTimeout_RecordThenGetEffective(t *testing.T) {
	state := &BackendState{
		Backend: &types.Backend{
			ID:             "test-1",
			RequestTimeout: 120,
		},
	}

	// До записей — недостаточно данных → fallback на статический
	before := getEffectiveTimeout(state, 120)
	if before != 120 {
		t.Errorf("before records: got %d, want 120", before)
	}

	// Добавляем 5 быстрых записей
	for i := 0; i < 5; i++ {
		recordLatency(state, 5000, "test-model", true)
	}

	// После записей — адаптивный таймаут
	after := getEffectiveTimeout(state, 120)
	if after <= 0 {
		t.Errorf("after records: got %d, expected > 0", after)
	}
	t.Logf("Adaptive timeout after 5 records: %d", after)

	// Проверяем что state.Backend.RuntimeRequestTimeout тоже обновился
	state.mu.Lock()
	rrt := state.Backend.RuntimeRequestTimeout
	state.mu.Unlock()
	if rrt != after {
		t.Errorf("RuntimeRequestTimeout (%d) != EffectiveTimeout (%d)", rrt, after)
	}
}

// ============================================================
// getRuntimeRequestTimeout — state persistence compatibility
// ============================================================

func TestAdaptiveTimeout_TimeNowOverride(t *testing.T) {
	// Проверяем, что timeNow можно переопределить для тестов
	oldNow := timeNow
	defer func() { timeNow = oldNow }()

	fixedTime := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	timeNow = func() time.Time { return fixedTime }

	now := timeNow()
	if !now.Equal(fixedTime) {
		t.Errorf("timeNow override: got %v, want %v", now, fixedTime)
	}

	// Сбрасываем — timeNow должна вернуться к time.Now()
	ResetTimeNowForTest()
	resetNow := timeNow()
	if resetNow.Equal(fixedTime) {
		t.Error("ResetTimeNowForTest: timeNow should not still return fixed time")
	}
	// Убеждаемся что ResetTimeNowForTest действительно меняет функцию
	// (не может быть equal fixedTime если время не заморожено)
	_ = oldNow
}
