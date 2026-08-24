// workload.go — Round 54.9 (2026-08-24): per-model workload tracking для AutoTune.
//
// Цель: AutoTune должен выбирать KV cache type на основе РЕАЛЬНОГО паттерна
// использования модели, а не только на основе её размера.
//
// Логика (R54.9 — workload-aware KV cache selection):
//   - Если workload "light" (p95 num_ctx < 25% of loaded) → f16 (high quality, low mem usage)
//   - Если workload "heavy" (p95 num_ctx > 75% of loaded) → q4_0 (max context)
//   - Иначе → старая логика (model size + free VRAM)
//
// Пример: 4B Q4_K_M model loaded с n_ctx=65536, но реально используется
// p95=4096 токенов → AutoTune переключит KV cache с q4_0 (current) на f16
// (higher quality, тот же mem budget). При burst p95=60000 → обратно на q4_0.
//
// Implementation:
//   - WorkloadTracker — sliding window per (backend, model) num_ctx samples
//   - Record() вызывается из proxyRequestLlamaCpp на каждом запросе
//   - Stats() возвращает WorkloadStats: SampleCount, AvgNumCtx, P95NumCtx, MaxNumCtx
//   - IsLight / IsHeavy — удобные хелперы для AutoTune
//
// Thread-safety: sync.RWMutex. Record = write lock (быстрый). Stats = read lock.
//
// Memory: maxSamples per (backend, model), default 100. С 10 backends × 5 models
// × 100 samples × 24 bytes = ~120KB. Идеально для in-memory.

package balancer

import (
	"sort"
	"sync"
	"time"
)

// WorkloadSample — одна точка данных: когда и сколько num_ctx запросили.
type WorkloadSample struct {
	Timestamp time.Time
	NumCtx    int
}

// WorkloadStats — агрегированная статистика по workload (per backend+model).
// Используется AutoTune для выбора optimal KV cache type.
type WorkloadStats struct {
	SampleCount  int       `json:"sampleCount"`
	AvgNumCtx    int       `json:"avgNumCtx"`
	P95NumCtx    int       `json:"p95NumCtx"`
	MaxNumCtx    int       `json:"maxNumCtx"`
	MinNumCtx    int       `json:"minNumCtx"`
	OldestSample time.Time `json:"oldestSample,omitempty"`
	NewestSample time.Time `json:"newestSample,omitempty"`
}

// IsLight — workload "light": p95 num_ctx < 25% of loaded n_ctx. Возвращает
// false если недостаточно samples (< minSamples) или нет loaded n_ctx.
func (s WorkloadStats) IsLight(loadedNCtx int, minSamples int) bool {
	if s.SampleCount < minSamples || loadedNCtx <= 0 {
		return false
	}
	return s.P95NumCtx < (loadedNCtx / 4)
}

// IsHeavy — workload "heavy": p95 num_ctx > 75% of loaded n_ctx.
func (s WorkloadStats) IsHeavy(loadedNCtx int, minSamples int) bool {
	if s.SampleCount < minSamples || loadedNCtx <= 0 {
		return false
	}
	return s.P95NumCtx > (3 * loadedNCtx / 4)
}

// WorkloadTracker — Round 54.9: sliding window per (backendID, modelName).
//
// Concurrency: sync.RWMutex. Record() = write lock. Stats() = read lock.
// AutoTune читает Stats() под read lock — много readers OK.
//
// Default config:
//   - maxSamples = 100 (≈ последние 100 запросов)
//   - minSamplesForRecommendation = 10 (нужно ≥ 10 samples чтобы доверять p95)
//
// Zero value: valid (lazy init в Stats/Record). nil tracker = no-op.
type WorkloadTracker struct {
	mu       sync.RWMutex
	samples  map[string][]WorkloadSample // key: "backendID|modelName"
	maxSamples int
}

// WorkloadTrackerConfig — Round 54.9: tunable.
type WorkloadTrackerConfig struct {
	// MaxSamples — sliding window size (per backend+model).
	MaxSamples int
	// MinSamplesForRecommendation — порог samples для workload-based decision.
	// Ниже этого — fallback к model-size-based logic (R54.1).
	MinSamplesForRecommendation int
}

// DefaultWorkloadTrackerConfig — sane defaults.
func DefaultWorkloadTrackerConfig() WorkloadTrackerConfig {
	return WorkloadTrackerConfig{
		MaxSamples:                  100,
		MinSamplesForRecommendation: 10,
	}
}

// NewWorkloadTracker — Round 54.9: constructor.
func NewWorkloadTracker(cfg WorkloadTrackerConfig) *WorkloadTracker {
	if cfg.MaxSamples <= 0 {
		cfg.MaxSamples = 100
	}
	if cfg.MinSamplesForRecommendation <= 0 {
		cfg.MinSamplesForRecommendation = 10
	}
	return &WorkloadTracker{
		samples:    make(map[string][]WorkloadSample),
		maxSamples: cfg.MaxSamples,
	}
}

// Record — добавляет sample. Если NumCtx == 0 (клиент не задал), sample
// игнорируется (нет данных = не записываем).
func (t *WorkloadTracker) Record(backendID, modelName string, numCtx int) {
	if t == nil || numCtx <= 0 {
		return
	}
	key := backendID + "|" + modelName
	t.mu.Lock()
	defer t.mu.Unlock()
	samples := t.samples[key]
	samples = append(samples, WorkloadSample{
		Timestamp: time.Now(),
		NumCtx:    numCtx,
	})
	// Sliding window: keep only last maxSamples
	if len(samples) > t.maxSamples {
		samples = samples[len(samples)-t.maxSamples:]
	}
	t.samples[key] = samples
}

// Stats — возвращает aggregated stats. Zero value если нет samples.
func (t *WorkloadTracker) Stats(backendID, modelName string) WorkloadStats {
	if t == nil {
		return WorkloadStats{}
	}
	key := backendID + "|" + modelName
	t.mu.RLock()
	samples := t.samples[key]
	t.mu.RUnlock()
	if len(samples) == 0 {
		return WorkloadStats{}
	}

	// Copy + sort to compute p95
	sorted := make([]int, len(samples))
	total := 0
	maxVal := 0
	for i, s := range samples {
		sorted[i] = s.NumCtx
		total += s.NumCtx
		if s.NumCtx > maxVal {
			maxVal = s.NumCtx
		}
	}
	sort.Ints(sorted)

	p95Idx := int(float64(len(sorted)) * 0.95)
	if p95Idx >= len(sorted) {
		p95Idx = len(sorted) - 1
	}

	return WorkloadStats{
		SampleCount:  len(samples),
		AvgNumCtx:    total / len(samples),
		P95NumCtx:    sorted[p95Idx],
		MaxNumCtx:    maxVal,
		MinNumCtx:    sorted[0],
		OldestSample: samples[0].Timestamp,
		NewestSample: samples[len(samples)-1].Timestamp,
	}
}

// Reset — очищает samples для (backend, model). Используется при reload модели
// (новые параметры = новый workload pattern).
func (t *WorkloadTracker) Reset(backendID, modelName string) {
	if t == nil {
		return
	}
	key := backendID + "|" + modelName
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.samples, key)
}

// Snapshot — debug helper для admin endpoint. Возвращает map всех stats.
func (t *WorkloadTracker) Snapshot() map[string]WorkloadStats {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make(map[string]WorkloadStats, len(t.samples))
	for k := range t.samples {
		// Re-parse key
		idx := -1
		for i, c := range k {
			if c == '|' {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		backendID := k[:idx]
		modelName := k[idx+1:]
		result[k] = t.statsLocked(backendID, modelName)
	}
	return result
}

// statsLocked — internal helper, caller must hold read lock.
func (t *WorkloadTracker) statsLocked(backendID, modelName string) WorkloadStats {
	key := backendID + "|" + modelName
	samples := t.samples[key]
	if len(samples) == 0 {
		return WorkloadStats{}
	}
	sorted := make([]int, len(samples))
	total := 0
	maxVal := 0
	for i, s := range samples {
		sorted[i] = s.NumCtx
		total += s.NumCtx
		if s.NumCtx > maxVal {
			maxVal = s.NumCtx
		}
	}
	sort.Ints(sorted)
	p95Idx := int(float64(len(sorted)) * 0.95)
	if p95Idx >= len(sorted) {
		p95Idx = len(sorted) - 1
	}
	return WorkloadStats{
		SampleCount:  len(samples),
		AvgNumCtx:    total / len(samples),
		P95NumCtx:    sorted[p95Idx],
		MaxNumCtx:    maxVal,
		MinNumCtx:    sorted[0],
		OldestSample: samples[0].Timestamp,
		NewestSample: samples[len(samples)-1].Timestamp,
	}
}

// extractNumCtxFromBody removed in R54.9 — using existing ExtractNumCtxFromBody
// from num_ctx_resolver.go (better: handles JSON float decoding, options.num_ctx
// priority, etc.). See TestExtractNumCtxFromBody_* in num_ctx_resolver_test.go.
