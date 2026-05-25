package balancer

import (
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// scoreComponents - компоненты score для бэкенда
type scoreComponents struct {
	baseScore          float64
	requestPenalty     float64
	modelCapacityScore float64
	modelLoadedBonus   float64
	queueDepthPenalty  float64
	errorRatePenalty   float64
	predictionBonus    float64
	weightFactor       float64
}

// computeBaseScoreAndPenalty вычисляет базовый score и штраф за загрузку
// из метрик бэкенда. Используется и simple, и enhanced scoring.
func computeBaseScoreAndPenalty(m *types.BackendMetrics) (baseScore, requestPenalty float64) {
	gpuFree := 100 - m.GPU.UsagePercent
	vramFree := 100.0
	if m.GPU.MemoryTotal > 0 {
		vramFree = float64(m.GPU.MemoryFree) * 100 / float64(m.GPU.MemoryTotal)
	}
	cpuFree := 100 - m.System.CPUUsagePercent
	baseScore = gpuFree*0.30 + vramFree*0.20 + cpuFree*0.15

	if m.Ollama.MaxConcurrentRequests > 0 {
		requestPenalty = float64(m.Ollama.ActiveRequests) / float64(m.Ollama.MaxConcurrentRequests) * 15.0
	} else if m.Ollama.ActiveRequests > 5 {
		requestPenalty = float64(m.Ollama.ActiveRequests) * 1.5
	}
	return
}

// computeNoMetricsScore вычисляет score когда метрики недоступны
func computeNoMetricsScore(state *BackendState) float64 {
	state.mu.Lock()
	active := state.ActiveReqs
	maxReqs := state.Backend.MaxConcurrentReqs
	state.mu.Unlock()

	score := float64(state.Backend.Weight)
	if maxReqs > 0 {
		score -= float64(active) / float64(maxReqs) * 20.0
	}
	if score < 1 {
		score = 1
	}
	return score
}

// computeWeightFactor возвращает вес бэкенда с защитой от нуля
func computeWeightFactor(state *BackendState) float64 {
	wf := float64(state.Backend.Weight)
	if wf <= 0 {
		return 1
	}
	return wf
}

// computeModelCapacityScore оценивает оставшуюся ёмкость для моделей
func computeModelCapacityScore(m *types.BackendMetrics) float64 {
	if m.Ollama.MaxModels > 0 {
		loaded := len(m.Ollama.RunningModels)
		return float64(m.Ollama.MaxModels-loaded) / float64(m.Ollama.MaxModels) * 10.0
	}

	if m.Ollama.BackendCapacity.LoadableModelCount > 0 {
		switch {
		case m.Ollama.BackendCapacity.LoadableModelCount >= 5:
			return 10.0
		case m.Ollama.BackendCapacity.LoadableModelCount >= 3:
			return 7.0
		case m.Ollama.BackendCapacity.LoadableModelCount >= 1:
			return 4.0
		default:
			return -3.0
		}
	}

	if m.GPU.MemoryTotal > 0 {
		var loadedVRAM uint64
		for _, model := range m.Ollama.RunningModels {
			loadedVRAM += model.VRAMUsage
		}
		vramRatio := float64(loadedVRAM) * 100 / float64(m.GPU.MemoryTotal)
		switch {
		case vramRatio > 80:
			return -5.0
		case vramRatio > 50:
			return 2.0
		default:
			return 8.0
		}
	}

	return 0
}

// computeEnhancedComponents вычисляет все компоненты enhanced scoring
func (p *Proxy) computeEnhancedComponents(metrics *types.BackendMetrics, state *BackendState) scoreComponents {
	sc := p.config.Balancing.Scoring

	wModelLoaded := defaultIfZero(sc.ModelAlreadyLoaded, 0.15)
	wQueueDepth := defaultIfZero(sc.QueueDepthPenalty, 0.05)
	wErrorRate := defaultIfZero(sc.ErrorRatePenalty, 0.05)
	wPrediction := defaultIfZero(sc.PredictionBonus, 0.10)

	baseScore, requestPenalty := computeBaseScoreAndPenalty(metrics)

	modelCapacityScore := computeModelCapacityScore(metrics)
	modelLoadedBonus := float64(len(metrics.Ollama.RunningModels)) * wModelLoaded * 10.0
	queueDepthPenalty := float64(len(p.queueMgr.queue)) * wQueueDepth

	// Error rate penalty
	errorRatePenalty := 0.0
	state.mu.Lock()
	if state.TotalAttempts > 10 {
		errorRatePenalty = float64(state.ErrorCount) / float64(state.TotalAttempts) * wErrorRate * 100
	}
	state.mu.Unlock()

	// Prediction bonus
	predictionBonus := 0.0
	if pred := state.Prediction; pred.SecondsToCritical < 0 || pred.SecondsToCritical >= 600 {
		predictionBonus = 3.0 * wPrediction
	} else if pred.SecondsToCritical >= 300 {
		predictionBonus = 1.5 * wPrediction
	} else if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 120 {
		predictionBonus = -5.0 * wPrediction
	}

	return scoreComponents{
		baseScore:          baseScore,
		requestPenalty:     requestPenalty,
		modelCapacityScore: modelCapacityScore,
		modelLoadedBonus:   modelLoadedBonus,
		queueDepthPenalty:  queueDepthPenalty,
		errorRatePenalty:   errorRatePenalty,
		predictionBonus:    predictionBonus,
		weightFactor:       computeWeightFactor(state),
	}
}

// ComputeModelCapacityScoreForTest — публичная обёртка для тестов
func ComputeModelCapacityScoreForTest(m *types.BackendMetrics) float64 {
	return computeModelCapacityScore(m)
}

// ComputeEnhancedModelBonusForTest — публичная обёртка для тестов
func ComputeEnhancedModelBonusForTest(models []types.RunningModel, wModelLoaded float64) float64 {
	return float64(len(models)) * wModelLoaded * 10.0
}

// defaultIfZero возвращает def если val <= 0
func defaultIfZero(val, def float64) float64 {
	if val <= 0 {
		return def
	}
	return val
}

// calculateScoreSimple - упрощённое вычисление score (без enhanced-весов)
// Используется когда UseEnhancedScoring=false для обратной совместимости
func (p *Proxy) calculateScoreSimple(backendID string) float64 {
	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()

	state := p.backends[backendID]
	if state == nil {
		return 0
	}

	if !ok {
		return computeNoMetricsScore(state)
	}

	baseScore, requestPenalty := computeBaseScoreAndPenalty(metrics)
	weightFactor := computeWeightFactor(state)

	score := (baseScore - requestPenalty) * weightFactor
	if score < 0 {
		score = 0
	}
	return score
}

// calculateScore - вычисление scores для бэкенда (v2: model affinity, queue depth, error rate)
// При UseEnhancedScoring=true использует полную формулу с весами из конфига.
// При UseEnhancedScoring=false делегирует calculateScoreSimple для обратной совместимости.
func (p *Proxy) calculateScore(backendID string) float64 {
	if !p.config.Balancing.UseEnhancedScoring {
		return p.calculateScoreSimple(backendID)
	}

	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()

	state := p.backends[backendID]
	if state == nil {
		return 0
	}

	if !ok {
		return computeNoMetricsScore(state)
	}

	comp := p.computeEnhancedComponents(metrics, state)

	score := (comp.baseScore - comp.requestPenalty + comp.modelCapacityScore + comp.modelLoadedBonus -
		comp.queueDepthPenalty - comp.errorRatePenalty + comp.predictionBonus) * comp.weightFactor
	if score < 0 {
		score = 0
	}
	return score
}

// calculateModelLoadPenalty - оценка стоимости загрузки модели на бэкенд
func (p *Proxy) calculateModelLoadPenalty(backendID string, modelName string) float64 {
	if modelName == "" {
		return 0
	}

	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()
	if !ok {
		return 5.0
	}

	for _, m := range metrics.Ollama.RunningModels {
		if m.Name == modelName || strings.Contains(m.Name, modelName) {
			return 0
		}
	}

	penalty := 5.0
	if metrics.GPU.MemoryTotal > 0 {
		vramUsedPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
		if vramUsedPercent > 80 {
			penalty = 20.0
		} else if vramUsedPercent > 60 {
			penalty = 10.0
		}
	}

	return penalty
}

// getBackendModelCapacity - оценка оставшихся слотов для моделей на бэкенде
func (p *Proxy) getBackendModelCapacity(backendID string) int {
	p.metricsMgr.mu.RLock()
	metrics, ok := p.metricsMgr.metrics[backendID]
	p.metricsMgr.mu.RUnlock()
	if !ok {
		return 0
	}

	if metrics.Ollama.MaxModels > 0 {
		available := metrics.Ollama.MaxModels - len(metrics.Ollama.RunningModels)
		if available < 0 {
			return 0
		}
		return available
	}

	if metrics.GPU.MemoryTotal > 0 && metrics.GPU.MemoryTotal > metrics.GPU.MemoryUsed {
		freeVRAM := metrics.GPU.MemoryTotal - metrics.GPU.MemoryUsed
		avgModelSize := uint64(4096)
		return int(freeVRAM / avgModelSize)
	}

	return 0
}

// estimateModelVRAM - оценка VRAM, необходимого для модели
func estimateModelVRAM(modelName string) uint64 {
	lower := strings.ToLower(modelName)

	var sizeGB uint64 = 4

	sizeMap := map[string]uint64{
		":0.5b": 1, ":1b": 1, ":1.5b": 2,
		":3b": 3, ":4b": 4, ":7b": 5, ":8b": 6,
		":13b": 9, ":14b": 10, ":20b": 14,
		":32b": 22, ":34b": 24, ":40b": 28,
		":65b": 45, ":70b": 48, ":72b": 50,
		":110b": 75, ":405b": 250,
	}

	for suffix, vram := range sizeMap {
		if strings.Contains(lower, suffix) {
			sizeGB = vram
			break
		}
	}

	if strings.Contains(lower, "q4") || strings.Contains(lower, "4bit") {
		sizeGB = sizeGB * 6 / 10
	} else if strings.Contains(lower, "q5") || strings.Contains(lower, "5bit") {
		sizeGB = sizeGB * 7 / 10
	} else if strings.Contains(lower, "q8") || strings.Contains(lower, "8bit") {
		sizeGB = sizeGB * 8 / 10
	} else if strings.Contains(lower, "q2") {
		sizeGB = sizeGB * 4 / 10
	}

	if sizeGB < 1 {
		sizeGB = 1
	}

	return sizeGB * 1024
}