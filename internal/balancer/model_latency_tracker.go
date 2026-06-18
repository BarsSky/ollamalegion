// Package balancer — ModelLatencyTracker для per-model адаптивных таймаутов.
//
// ModelLatencyTracker собирает per-model метрики генерации:
//   - tokens/sec (скорость генерации)
//   - inter-token latency (задержка между чанками)
//   - first-byte latency (время до первого токена)
//   - количество ошибок (таймауты, OOM, обрывы)
//
// На основе этих метрик балансировщик может:
//   - вычислять per-model StreamingTimeout и StreamingIdleTimeout
//   - предсказывать превышение таймаута для тяжёлых моделей (CPU offload)
//   - адаптивно увеличивать/уменьшать таймауты без ручной конфигурации
package balancer

import (
	"math"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// Default timeout constants for models without profile or history.
const (
	// defaultStreamTimeout is the fallback when no per-model or global timeout is set.
	defaultStreamTimeout = 600 * time.Second // 10 min
	// defaultIdleTimeout is the fallback for streaming idle timeout.
	defaultIdleTimeout = 120 * time.Second // 2 min
	// defaultRequestTimeout is the fallback for non-streaming request timeout.
	defaultRequestTimeout = 120 * time.Second // 2 min

	// minSamplesForStats — минимальное количество семплов для статистики.
	minSamplesForStats = 3

	// maxSamplesPerModel — максимум записей в истории на модель.
	maxSamplesPerModel = 100

	// heavyModelMultiplier — множитель таймаута для "тяжёлых" моделей
	// (CPU-only или partial offload с низким tokens/sec).
	heavyModelMultiplier = 2.0

	// fastModelMultiplier — множитель таймаута для "быстрых" моделей (GPU-only).
	fastModelMultiplier = 1.2

	// mediumModelMultiplier — множитель для смешанных сценариев.
	mediumModelMultiplier = 1.5
)

// ModelSample — один замер генерации модели.
type ModelSample struct {
	Timestamp        time.Time `json:"timestamp"`
	TokensGenerated  int       `json:"tokensGenerated"`
	DurationMs       int64     `json:"durationMs"`
	FirstByteLatencyMs int64   `json:"firstByteLatencyMs"`
	MaxInterTokenGapMs int64   `json:"maxInterTokenGapMs"`
	Error            bool      `json:"error"`
	ErrorType        string    `json:"errorType,omitempty"` // "timeout", "oom", "disconnect"
	NumGPULayers     int       `json:"numGpuLayers"`
}

// ModelLatencyStats — агрегированная статистика latency для модели.
type ModelLatencyStats struct {
	// AvgTokensPerSec — средняя скорость генерации токенов.
	AvgTokensPerSec float64 `json:"avgTokensPerSec"`
	// P95TokensPerSec — 95-й перцентиль скорости генерации.
	P95TokensPerSec float64 `json:"p95TokensPerSec"`
	// AvgFirstByteMs — среднее время до первого токена (ms).
	AvgFirstByteMs float64 `json:"avgFirstByteMs"`
	// P95FirstByteMs — 95-й перцентиль времени до первого токена (ms).
	P95FirstByteMs float64 `json:"p95FirstByteMs"`
	// MaxInterTokenGapMs — максимальная задержка между чанками (ms).
	MaxInterTokenGapMs float64 `json:"maxInterTokenGapMs"`
	// P95InterTokenGapMs — 95-й перцентиль межчанковой задержки (ms).
	P95InterTokenGapMs float64 `json:"p95InterTokenGapMs"`
	// SampleCount — количество семплов.
	SampleCount int `json:"sampleCount"`
	// ErrorRate — доля ошибочных запросов (0.0–1.0).
	ErrorRate float64 `json:"errorRate"`
	// IsHeavy — true если модель классифицируется как «тяжёлая»
	// (CPU-only или partial GPU offload с низким tokens/sec).
	IsHeavy bool `json:"isHeavy"`
	// RecommendedStreamTimeoutSec — рекомендуемый общий таймаут стриминга (сек).
	RecommendedStreamTimeoutSec int `json:"recommendedStreamTimeoutSec"`
	// RecommendedIdleTimeoutSec — рекомендуемый idle-таймаут стриминга (сек).
	RecommendedIdleTimeoutSec int `json:"recommendedIdleTimeoutSec"`
	// RecommendedRequestTimeoutSec — рекомендуемый таймаут non-streaming (сек).
	RecommendedRequestTimeoutSec int `json:"recommendedRequestTimeoutSec"`
	// RecommendedFirstByteTimeoutSec — рекомендуемый таймаут ожидания первого байта
	// (сек). Учитывает время загрузки модели в VRAM + prompt processing.
	// Для неизвестных/некогревых моделей (нет семплов) — 600s (10 min).
	RecommendedFirstByteTimeoutSec int `json:"recommendedFirstByteTimeoutSec"`
}

// ModelLatencyTracker собирает per-model метрики генерации.
type ModelLatencyTracker struct {
	mu      sync.RWMutex
	models  map[string][]ModelSample // modelName → история замеров
}

// NewModelLatencyTracker создаёт новый трекер.
func NewModelLatencyTracker() *ModelLatencyTracker {
	return &ModelLatencyTracker{
		models: make(map[string][]ModelSample),
	}
}

// RecordSample добавляет замер генерации модели.
func (t *ModelLatencyTracker) RecordSample(modelName string, sample ModelSample) {
	if modelName == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	samples := t.models[modelName]
	samples = append(samples, sample)

	// Обрезаем до максимума
	if len(samples) > maxSamplesPerModel {
		samples = samples[len(samples)-maxSamplesPerModel:]
	}
	t.models[modelName] = samples

	logger.Get().Debugw("ModelLatencyTracker: recorded sample",
		"model", modelName,
		"tokens", sample.TokensGenerated,
		"duration_ms", sample.DurationMs,
		"samples", len(samples),
		"error", sample.Error,
	)
}

// GetStats возвращает агрегированную статистику для модели.
func (t *ModelLatencyTracker) GetStats(modelName string) ModelLatencyStats {
	t.mu.RLock()
	defer t.mu.RUnlock()

	samples := t.models[modelName]
	if len(samples) == 0 {
		return ModelLatencyStats{}
	}

	return computeModelStats(samples)
}

// computeModelStats вычисляет статистику из истории семплов.
func computeModelStats(samples []ModelSample) ModelLatencyStats {
	if len(samples) == 0 {
		return ModelLatencyStats{}
	}

	// Собираем метрики
	var tokensPerSec []float64
	var firstByteMs []float64
	var interTokenGaps []float64
	errCount := 0

	// Определяем, есть ли GPU layers.
	// Если хотя бы один семпл имеет NumGPULayers <= 0, считаем модель CPU-only.
	hasGPU := false
	allCPU := true
	for _, s := range samples {
		if s.NumGPULayers > 0 {
			hasGPU = true
			allCPU = false
		}
		if s.Error {
			errCount++
		}
		if s.TokensGenerated > 0 && s.DurationMs > 0 {
			tps := float64(s.TokensGenerated) / (float64(s.DurationMs) / 1000.0)
			tokensPerSec = append(tokensPerSec, tps)
		}
		if s.FirstByteLatencyMs > 0 {
			firstByteMs = append(firstByteMs, float64(s.FirstByteLatencyMs))
		}
		if s.MaxInterTokenGapMs > 0 {
			interTokenGaps = append(interTokenGaps, float64(s.MaxInterTokenGapMs))
		}
	}

	stats := ModelLatencyStats{
		SampleCount: len(samples),
		ErrorRate:   float64(errCount) / float64(len(samples)),
	}

	if len(tokensPerSec) > 0 {
		stats.AvgTokensPerSec = avgFloat64(tokensPerSec)
		stats.P95TokensPerSec = p95Float64(tokensPerSec)
	}
	if len(firstByteMs) > 0 {
		stats.AvgFirstByteMs = avgFloat64(firstByteMs)
		stats.P95FirstByteMs = p95Float64(firstByteMs)
	}
	if len(interTokenGaps) > 0 {
		stats.MaxInterTokenGapMs = maxFloat64(interTokenGaps)
		stats.P95InterTokenGapMs = p95Float64(interTokenGaps)
	}

	// Классификация модели: heavy, medium, fast.
	// - CPU-only (allCPU && !hasGPU) → heavy
	// - partial offload с низким TPS (< 5 tok/s) → heavy
	// - GPU-only с высоким TPS (>= 20 tok/s) → fast
	// - остальные → medium
	isHeavy := false
	if allCPU && !hasGPU {
		isHeavy = true
	} else if stats.AvgTokensPerSec > 0 && stats.AvgTokensPerSec < 5.0 {
		isHeavy = true
	}

	isFast := false
	if !allCPU && hasGPU && stats.AvgTokensPerSec >= 20.0 {
		isFast = true
	}

	stats.IsHeavy = isHeavy

	// Рекомендуемые таймауты
	var streamMult, idleMult, reqMult float64
	switch {
	case isHeavy:
		streamMult = heavyModelMultiplier
		idleMult = heavyModelMultiplier
		reqMult = heavyModelMultiplier
	case isFast:
		streamMult = fastModelMultiplier
		idleMult = fastModelMultiplier
		reqMult = fastModelMultiplier
	default:
		streamMult = mediumModelMultiplier
		idleMult = mediumModelMultiplier
		reqMult = mediumModelMultiplier
	}

	// Базовые таймауты: отталкиваемся от p95 latency, но не меньше дефолтов
	baseStream := defaultStreamTimeout
	baseIdle := defaultIdleTimeout
	baseReq := defaultRequestTimeout

	// Если есть данные по first-byte latency — используем их для RequestTimeout
	if stats.P95FirstByteMs > 0 {
		estimatedReq := time.Duration(stats.P95FirstByteMs*reqMult) * time.Millisecond
		if estimatedReq > baseReq {
			baseReq = estimatedReq
		}
	}

	// Если есть данные по inter-token gap — используем для idle timeout
	if stats.P95InterTokenGapMs > 0 {
		estimatedIdle := time.Duration(stats.P95InterTokenGapMs*idleMult*1.5) * time.Millisecond
		if estimatedIdle > baseIdle {
			baseIdle = estimatedIdle
		}
	}

	// Streaming timeout: база + ожидаемое число токенов * среднее время токена
	if stats.AvgTokensPerSec > 0 {
		// Предполагаем максимум 4096 токенов на ответ
		expectedTokens := 4096.0
		// время на генерацию всех токенов (сек) = expectedTokens / avgTokensPerSec
		genTimeSec := expectedTokens / stats.AvgTokensPerSec
		estimatedStream := time.Duration(genTimeSec*streamMult) * time.Second
		// плюс first-byte latency
		if stats.P95FirstByteMs > 0 {
			estimatedStream += time.Duration(stats.P95FirstByteMs) * time.Millisecond
		}
		if estimatedStream > baseStream {
			baseStream = estimatedStream
		}
	}

	// Дополнительный запас для heavy моделей
	if isHeavy {
		baseStream = time.Duration(math.Max(
			float64(baseStream),
			float64(1800*time.Second), // минимум 30 мин для CPU
		))
		baseIdle = time.Duration(math.Max(
			float64(baseIdle),
			float64(300*time.Second), // минимум 5 мин idle для CPU
		))
		baseReq = time.Duration(math.Max(
			float64(baseReq),
			float64(300*time.Second), // минимум 5 мин non-streaming для CPU
		))
	}

	// FirstByteTimeout: время ожидания первого байта (HTTP заголовков).
	// Учитывает загрузку модели в VRAM + prompt processing (первый токен).
	// Для неизвестных моделей без семплов используем консервативные значения:
	//   - heavy (CPU/partial offload): 600s (10 min)
	//   - medium: 300s (5 min)
	//   - fast: 120s (2 min)
	// Если есть семплы first-byte latency: P95FirstByteMs * 2.0 + 60s запас.
	var baseFirstByte time.Duration
	switch {
	case isHeavy:
		baseFirstByte = 600 * time.Second // 10 min для CPU-загрузки
	case isFast:
		baseFirstByte = 120 * time.Second // 2 min для GPU
	default:
		baseFirstByte = 300 * time.Second // 5 min для medium
	}
	if stats.P95FirstByteMs > 0 {
		estimatedFb := time.Duration(stats.P95FirstByteMs*2.0+60000) * time.Millisecond
		if estimatedFb > baseFirstByte {
			baseFirstByte = estimatedFb
		}
	}

	// Учёт ошибок: если error rate > 0.3, увеличиваем таймауты на 50%
	if stats.ErrorRate > 0.3 {
		baseStream = time.Duration(float64(baseStream) * 1.5)
		baseIdle = time.Duration(float64(baseIdle) * 1.5)
		baseReq = time.Duration(float64(baseReq) * 1.5)
		baseFirstByte = time.Duration(float64(baseFirstByte) * 1.5)
	}

	stats.RecommendedStreamTimeoutSec = int(baseStream.Seconds())
	stats.RecommendedIdleTimeoutSec = int(baseIdle.Seconds())
	stats.RecommendedRequestTimeoutSec = int(baseReq.Seconds())
	stats.RecommendedFirstByteTimeoutSec = int(baseFirstByte.Seconds())

	return stats
}

// GetOrComputeTimeout возвращает таймаут для модели:
//   - если в профиле модели задан явный таймаут (> 0) — используем его
//   - иначе вычисляем на основе статистики (если есть семплы)
//   - иначе оцениваем по размеру GGUF файла (если передан modelSizeBytes > 0)
//   - иначе возвращаем глобальный дефолт
//
// modelSizeBytes — размер .gguf файла в байтах (0 = неизвестен).
// Используется для эвристической оценки таймаута для моделей без истории генерации.
func (t *ModelLatencyTracker) GetOrComputeTimeout(
	modelName string,
	profileStreamTimeoutSec int,
	globalStreamTimeoutSec int,
	modelSizeBytes ...int64,
) time.Duration {
	// Явный per-model таймаут из профиля
	if profileStreamTimeoutSec > 0 {
		return time.Duration(profileStreamTimeoutSec) * time.Second
	}

	// Пытаемся вычислить из статистики
	stats := t.GetStats(modelName)
	if stats.RecommendedStreamTimeoutSec > 0 {
		return time.Duration(stats.RecommendedStreamTimeoutSec) * time.Second
	}

	// Эвристика по размеру GGUF файла (для моделей без истории)
	if len(modelSizeBytes) > 0 && modelSizeBytes[0] > 0 {
		if estimated := EstimateStreamTimeoutFromModelSize(modelSizeBytes[0]); estimated > 0 {
			return estimated
		}
	}

	// Глобальный дефолт
	if globalStreamTimeoutSec > 0 {
		return time.Duration(globalStreamTimeoutSec) * time.Second
	}
	return defaultStreamTimeout
}

// GetOrComputeIdleTimeout — как GetOrComputeTimeout, но для idle timeout.
func (t *ModelLatencyTracker) GetOrComputeIdleTimeout(
	modelName string,
	profileIdleTimeoutSec int,
	globalIdleTimeoutSec int,
) time.Duration {
	if profileIdleTimeoutSec > 0 {
		return time.Duration(profileIdleTimeoutSec) * time.Second
	}
	stats := t.GetStats(modelName)
	if stats.RecommendedIdleTimeoutSec > 0 {
		return time.Duration(stats.RecommendedIdleTimeoutSec) * time.Second
	}
	if globalIdleTimeoutSec > 0 {
		return time.Duration(globalIdleTimeoutSec) * time.Second
	}
	return defaultIdleTimeout
}

// GetOrComputeFirstByteTimeout — как GetOrComputeTimeout, но для FirstByte timeout.
// Возвращает рекомендуемый таймаут ожидания первого байта (HTTP-заголовков).
// Первым проверяется per-model профиль, затем статистика, затем эвристика
// по размеру GGUF файла, затем глобальный конфиг, и наконец дефолт 120s.
//
// modelSizeBytes — размер .gguf файла в байтах (0 = неизвестен).
// Используется для эвристической оценки first-byte таймаута для моделей
// без истории генерации.
func (t *ModelLatencyTracker) GetOrComputeFirstByteTimeout(
	modelName string,
	profileFirstByteTimeoutSec int,
	globalFirstByteTimeoutSec int,
	modelSizeBytes ...int64,
) time.Duration {
	if profileFirstByteTimeoutSec > 0 {
		return time.Duration(profileFirstByteTimeoutSec) * time.Second
	}
	stats := t.GetStats(modelName)
	if stats.RecommendedFirstByteTimeoutSec > 0 {
		return time.Duration(stats.RecommendedFirstByteTimeoutSec) * time.Second
	}

	// Эвристика по размеру GGUF файла (для моделей без истории)
	if len(modelSizeBytes) > 0 && modelSizeBytes[0] > 0 {
		if estimated := EstimateFirstByteTimeoutFromModelSize(modelSizeBytes[0]); estimated > 0 {
			return estimated
		}
	}

	if globalFirstByteTimeoutSec > 0 {
		return time.Duration(globalFirstByteTimeoutSec) * time.Second
	}
	return 120 * time.Second // default 2 min
}

// GetOrComputeRequestTimeout — как GetOrComputeTimeout, но для non-streaming.
func (t *ModelLatencyTracker) GetOrComputeRequestTimeout(
	modelName string,
	profileRequestTimeoutSec int,
	globalRequestTimeoutSec int,
) time.Duration {
	if profileRequestTimeoutSec > 0 {
		return time.Duration(profileRequestTimeoutSec) * time.Second
	}
	stats := t.GetStats(modelName)
	if stats.RecommendedRequestTimeoutSec > 0 {
		return time.Duration(stats.RecommendedRequestTimeoutSec) * time.Second
	}
	if globalRequestTimeoutSec > 0 {
		return time.Duration(globalRequestTimeoutSec) * time.Second
	}
	return defaultRequestTimeout
}

// EstimateStreamTimeoutFromModelSize оценивает разумный таймаут стриминга (первый байт + генерация)
// на основе размера .gguf файла. Используется как эвристика для моделей без истории генерации.
//
// Эвристика:
//   - < 2 GB  (Q2_K, 1-3B param):    120s  — маленькие модели, быстрая загрузка
//   - 2-5 GB  (Q4_K_M, 3-8B param):   300s  — средние модели
//   - 5-12 GB (Q4_K_M, 8-20B param):  600s  — большие модели (10+ min)
//   - 12-24 GB (Q4_K_M, 20-40B param): 900s — очень большие (15+ min)
//   - > 24 GB (> 40B param):           1200s — гигантские (20+ min)
func EstimateStreamTimeoutFromModelSize(sizeBytes int64) time.Duration {
	gb := float64(sizeBytes) / (1024 * 1024 * 1024)
	switch {
	case gb <= 0:
		return 0
	case gb < 2.0:
		return 120 * time.Second
	case gb < 5.0:
		return 300 * time.Second
	case gb < 12.0:
		return 600 * time.Second
	case gb < 24.0:
		return 900 * time.Second
	default:
		return 1200 * time.Second
	}
}

// EstimateFirstByteTimeoutFromModelSize оценивает таймаут ожидания первого байта (HTTP-заголовков)
// на основе размера .gguf файла. Учитывает время загрузки модели в VRAM + prompt processing.
// Используется как эвристика для моделей без истории генерации.
//
// Эвристика:
//   - < 2 GB:   60s  — маленькие модели
//   - 2-5 GB:   120s — средние модели
//   - 5-12 GB:  300s — большие модели
//   - 12-24 GB: 600s — очень большие
//   - > 24 GB:  900s — гигантские
func EstimateFirstByteTimeoutFromModelSize(sizeBytes int64) time.Duration {
	gb := float64(sizeBytes) / (1024 * 1024 * 1024)
	switch {
	case gb <= 0:
		return 0
	case gb < 2.0:
		return 60 * time.Second
	case gb < 5.0:
		return 120 * time.Second
	case gb < 12.0:
		return 300 * time.Second
	case gb < 24.0:
		return 600 * time.Second
	default:
		return 900 * time.Second
	}
}

// GetModelNames возвращает список имён моделей, по которым есть данные.
func (t *ModelLatencyTracker) GetModelNames() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	names := make([]string, 0, len(t.models))
	for name := range t.models {
		names = append(names, name)
	}
	return names
}

// --- утилиты ---

func avgFloat64(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func maxFloat64(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	max := vals[0]
	for _, v := range vals[1:] {
		if v > max {
			max = v
		}
	}
	return max
}

func p95Float64(vals []float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return vals[0]
	}

	// Копируем и сортируем
	sorted := make([]float64, n)
	copy(sorted, vals)
	sortFloat64(sorted)

	idx := int(math.Ceil(float64(n)*0.95) - 1)
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// sortFloat64 — сортировка []float64 (чтобы не импортировать "sort" зря).
func sortFloat64(a []float64) {
	n := len(a)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}
