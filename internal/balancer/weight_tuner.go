package balancer

import (
	"math"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// ScoringWeights — веса для формулы скоринга (копия для авто-тюнинга)
type TuningWeights struct {
	GPUFree     float64
	VRAMFree    float64
	CPUFree     float64
	ModelLoaded float64
	Prediction  float64
	ModelCap    float64
	QueueDepth  float64
	ErrorRate   float64
}

// TuningSample — запись о результатах запроса для анализа
type TuningSample struct {
	BackendID string
	ModelName string
	LatencyMs int64
	Success   bool
	Timestamp time.Time
}

// AdaptiveWeightTuner — автоматическая корректировка весов на основе метрик
type AdaptiveWeightTuner struct {
	proxy          *Proxy
	mu             sync.Mutex
	weights        TuningWeights
	baseline       TuningWeights
	history        []TuningSample
	windowSize     int
	adjustInterval time.Duration
	lastAdjust     time.Time
	stopCh         chan struct{}
	enabled        bool
}

// NewAdaptiveWeightTuner — создание тюнера весов
func NewAdaptiveWeightTuner(proxy *Proxy) *AdaptiveWeightTuner {
	awt := &AdaptiveWeightTuner{
		proxy: proxy,
		weights: TuningWeights{
			GPUFree:     0.30,
			VRAMFree:    0.20,
			CPUFree:     0.15,
			ModelLoaded: 0.15,
			Prediction:  0.10,
			ModelCap:    0.10,
			QueueDepth:  0.05,
			ErrorRate:   0.05,
		},
		windowSize:     100,
		adjustInterval: 10 * time.Minute,
		stopCh:         make(chan struct{}),
		enabled:        true,
	}

	// Сохраняем baseline
	awt.baseline = awt.weights

	return awt
}

// RecordOutcome — запись результата запроса для анализа
func (awt *AdaptiveWeightTuner) RecordOutcome(backendID, modelName string, latencyMs int64, success bool) {
	if !awt.enabled {
		return
	}

	awt.mu.Lock()
	defer awt.mu.Unlock()

	awt.history = append(awt.history, TuningSample{
		BackendID: backendID,
		ModelName: modelName,
		LatencyMs: latencyMs,
		Success:   success,
		Timestamp: time.Now(),
	})

	// Удерживаем скользящее окно
	if len(awt.history) > awt.windowSize*2 {
		awt.history = awt.history[len(awt.history)-awt.windowSize:]
	}
}

// Start — запуск фонового цикла корректировки
func (awt *AdaptiveWeightTuner) Start() {
	if !awt.enabled {
		return
	}
	go awt.loop()
}

// Stop — остановка тюнера
func (awt *AdaptiveWeightTuner) Stop() {
	if awt.stopCh != nil {
		select {
		case <-awt.stopCh:
		default:
			close(awt.stopCh)
		}
	}
}

// SetEnabled — включение/отключение тюнера
func (awt *AdaptiveWeightTuner) SetEnabled(enabled bool) {
	awt.mu.Lock()
	defer awt.mu.Unlock()

	if enabled && !awt.enabled {
		awt.enabled = true
		awt.stopCh = make(chan struct{})
		go awt.loop()
	} else if !enabled && awt.enabled {
		awt.enabled = false
		close(awt.stopCh)
	}
}

// loop — основной цикл тюнинга
func (awt *AdaptiveWeightTuner) loop() {
	ticker := time.NewTicker(awt.adjustInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			awt.tune()
		case <-awt.stopCh:
			return
		}
	}
}

// tune — анализ корреляции весов с результатами и корректировка
func (awt *AdaptiveWeightTuner) tune() {
	awt.mu.Lock()
	defer awt.mu.Unlock()

	if len(awt.history) < awt.windowSize {
		return
	}

	// Анализируем последние результаты
	recentWindow := awt.history
	if len(recentWindow) > awt.windowSize {
		recentWindow = recentWindow[len(recentWindow)-awt.windowSize:]
	}

	var totalRequests int
	var successCount int
	var totalLatency int64

	// Группируем по бэкендам для анализа
	perBackend := make(map[string]struct {
		requests int
		success  int
		latency  int64
	})

	for _, s := range recentWindow {
		totalRequests++
		if s.Success {
			successCount++
		}
		totalLatency += s.LatencyMs

		b := perBackend[s.BackendID]
		b.requests++
		if s.Success {
			b.success++
		}
		b.latency += s.LatencyMs
		perBackend[s.BackendID] = b
	}

	if totalRequests == 0 {
		return
	}

	successRate := float64(successCount) / float64(totalRequests)
	avgLatency := float64(totalLatency) / float64(totalRequests)

	// Адаптивная логика:
	// - Если success rate < 95% → усиливаем ErrorRate penalty
	// - Если latency растёт → усиливаем QueueDepth penalty
	// - Если всё хорошо → возвращаем веса к baseline
	maxAdjust := 0.05 // максимум изменения веса за цикл (±5%)

	if successRate < 0.95 {
		// Увеличиваем penalty за ошибки
		awt.weights.ErrorRate = math.Min(awt.weights.ErrorRate+maxAdjust, 0.15)
		awt.weights.QueueDepth = math.Min(awt.weights.QueueDepth+maxAdjust*0.5, 0.10)
		logger.Get().Infow("weightTuner: increasing error/queue penalties due to low success rate",
			"successRate", successRate, "errorRateWeight", awt.weights.ErrorRate)
	} else if successRate > 0.98 && avgLatency < 1000 {
		// Возвращаем к baseline
		awt.weights = awt.approachBaseline(maxAdjust * 0.5)
		logger.Get().Debugw("weightTuner: approaching baseline weights",
			"successRate", successRate, "avgLatency", avgLatency)
	}

	// Увеличиваем ModelLoaded бонус если много успешных запросов на загруженных моделях
	if successRate > 0.95 {
		awt.weights.ModelLoaded = math.Min(awt.weights.ModelLoaded+maxAdjust*0.3, 0.25)
	}

	awt.lastAdjust = time.Now()
	logger.Get().Infow("weightTuner: weights adjusted",
		"gpuFree", awt.weights.GPUFree,
		"vramFree", awt.weights.VRAMFree,
		"cpuFree", awt.weights.CPUFree,
		"modelLoaded", awt.weights.ModelLoaded,
		"prediction", awt.weights.Prediction,
		"errorRate", awt.weights.ErrorRate,
		"queueDepth", awt.weights.QueueDepth,
	)
}

// approachBaseline — постепенное приближение к baseline весам
func (awt *AdaptiveWeightTuner) approachBaseline(step float64) TuningWeights {
	newWeights := awt.weights

	newWeights.GPUFree = approach(newWeights.GPUFree, awt.baseline.GPUFree, step)
	newWeights.VRAMFree = approach(newWeights.VRAMFree, awt.baseline.VRAMFree, step)
	newWeights.CPUFree = approach(newWeights.CPUFree, awt.baseline.CPUFree, step)
	newWeights.ModelLoaded = approach(newWeights.ModelLoaded, awt.baseline.ModelLoaded, step)
	newWeights.Prediction = approach(newWeights.Prediction, awt.baseline.Prediction, step)
	newWeights.ModelCap = approach(newWeights.ModelCap, awt.baseline.ModelCap, step)
	newWeights.QueueDepth = approach(newWeights.QueueDepth, awt.baseline.QueueDepth, step)
	newWeights.ErrorRate = approach(newWeights.ErrorRate, awt.baseline.ErrorRate, step)

	return newWeights
}

// approach — вспомогательная функция приближения одного значения к целевому
func approach(current, target, step float64) float64 {
	if math.Abs(current-target) <= step {
		return target
	}
	if current < target {
		return current + step
	}
	return current - step
}

// GetWeights — получение текущих весов
func (awt *AdaptiveWeightTuner) GetWeights() TuningWeights {
	awt.mu.Lock()
	defer awt.mu.Unlock()
	return awt.weights
}

// Reset — сброс к baseline
func (awt *AdaptiveWeightTuner) Reset() {
	awt.mu.Lock()
	defer awt.mu.Unlock()
	awt.weights = awt.baseline
	awt.history = nil
	logger.Get().Infow("weightTuner: reset to baseline weights")
}