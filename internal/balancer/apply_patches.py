import sys
import os

path = r'c:\Ollama\ollamalegion\internal\balancer\proxy.go'

with open(path, 'r', encoding='utf-8') as f:
    data = f.read()

replacements = 0

# 1. Replace old selectBackend with 5-stage
old_select = '''// selectBackend - выбор бэкенда для запроса
func (p *Proxy) selectBackend(model string) string {
\tp.mu.RLock()

\tif p.config.Balancing.ModelAffinity && model != "" {
\t\tif backend := p.findBackendWithModel(model); backend != "" {
\t\t\tstate := p.backends[backend]
\t\t\tstate.mu.Lock()
\t\t\tactive := state.ActiveReqs
\t\t\tmaxReqs := state.Backend.MaxConcurrentReqs
\t\t\tstate.mu.Unlock()

\t\t\tloadRatio := 0.0
\t\t\tif maxReqs > 0 {
\t\t\t\tloadRatio = float64(active) / float64(maxReqs)
\t\t\t}

\t\t\tif loadRatio < 0.80 {
\t\t\t\tp.mu.RUnlock()
\t\t\t\treturn backend
\t\t\t}

\t\t\tlogger.Get().Warnw("model-affinity backend overloaded, trying resource-aware fallback",
\t\t\t\t"backend", backend, "model", model,
\t\t\t\t"load_ratio", loadRatio, "active", active, "max", maxReqs)
\t\t}
\t}
\tp.mu.RUnlock()

\tbackend := p.selectByResources()
\tif backend == "" {
\t\tlogger.Get().Warnw("no available backends - balancer waiting for recovery")
\t}
\treturn backend
}'''

new_select = '''// selectBackend - выбор бэкенда для запроса (5-этапный алгоритм)
func (p *Proxy) selectBackend(model string) string {
\tp.mu.RLock()
\tdefer p.mu.RUnlock()

\t// 1. Model Affinity (LOADED only)
\tif p.config.Balancing.ModelAffinity && model != "" {
\t\tif backend := p.findBackendWithModel(model); backend != "" {
\t\t\tstate := p.backends[backend]
\t\t\tstate.mu.Lock()
\t\t\tactive := state.ActiveReqs
\t\t\tmaxReqs := state.Backend.MaxConcurrentReqs
\t\t\tstate.mu.Unlock()

\t\t\tloadRatio := 0.0
\t\t\tif maxReqs > 0 { loadRatio = float64(active) / float64(maxReqs) }
\t\t\tif loadRatio < 0.80 { return backend }

\t\t\tlogger.Get().Warnw("model-affinity backend overloaded, trying fallback",
\t\t\t\t"backend", backend, "model", model, "load_ratio", loadRatio)
\t\t}
\t}

\t// 2. Model Warming (WARMING_UP + ETA < threshold)
\tif warmingBackend := p.findWarmingBackendForModelUnsafe(model); warmingBackend != "" {
\t\tstate := p.backends[warmingBackend]
\t\tstate.mu.Lock()
\t\tws, exists := state.WarmingUpModels[model]
\t\tstate.mu.Unlock()

\t\tif exists {
\t\t\teta := time.Until(ws.EstimatedReadyAt)
\t\t\tsyncTimeout := 30 * time.Second
\t\t\tif p.config.Balancing.SyncModelLoad.Timeout != "" {
\t\t\t\tif d, err := time.ParseDuration(p.config.Balancing.SyncModelLoad.Timeout); err == nil { syncTimeout = d }
\t\t\t}
\t\t\tif eta > 0 && eta < syncTimeout {
\t\t\t\tstart := time.Now()
\t\t\t\tfor time.Since(start) < syncTimeout {
\t\t\t\t\tif p.checkModelReadyUnsafe(warmingBackend, model) { return warmingBackend }
\t\t\t\t\ttime.Sleep(500 * time.Millisecond)
\t\t\t\t}
\t\t\t\tlogger.Get().Warnw("model warmup timeout", "backend", warmingBackend, "model", model)
\t\t\t}
\t\t}
\t}

\t// 3. Sync Model Load (запуск загрузки с таймаутом)
\tif p.config.Balancing.SyncModelLoad.Enabled {
\t\tfreeBackend := p.findFreeBackendForModelUnsafe(model)
\t\tif freeBackend != "" {
\t\t\tstate := p.backends[freeBackend]
\t\t\tp.warmupModel(freeBackend, state.Backend.Host, state.Backend.OllamaPort, model)
\t\t\tsyncTimeout := 30 * time.Second
\t\t\tif p.config.Balancing.SyncModelLoad.Timeout != "" {
\t\t\t\tif d, err := time.ParseDuration(p.config.Balancing.SyncModelLoad.Timeout); err == nil { syncTimeout = d }
\t\t\t}
\t\t\tstart := time.Now()
\t\t\tfor time.Since(start) < syncTimeout {
\t\t\t\tif p.checkModelReadyUnsafe(freeBackend, model) { return freeBackend }
\t\t\t\ttime.Sleep(500 * time.Millisecond)
\t\t\t}
\t\t\tlogger.Get().Warnw("sync model load timeout", "backend", freeBackend, "model", model)
\t\t}
\t}

\t// 4. selectByResources (scoring v2)
\treturn p.selectByResources()
}'''

if old_select in data:
    data = data.replace(old_select, new_select, 1)
    replacements += 1
    print("Replaced selectBackend")
else:
    print("ERROR: selectBackend old pattern not found")
    sys.exit(1)

# 2. Add Unsafe helpers after findBackendWithModel
old_find = '''// findBackendWithModel - поиск бэкенда с загруженной моделью
func (p *Proxy) findBackendWithModel(modelName string) string {
\tp.mu.RLock()
\tdefer p.mu.RUnlock()
\t
\tvar bestBackendID string
\tvar bestScore float64 = -1
\t
\tfor id, state := range p.backends {
\t\tif state.Backend.Status != types.StatusHealthy {
\t\t\tcontinue
\t\t}

\t\tif !p.checkResourceLimits(id) {
\t\t\tcontinue
\t\t}

\t\tstate.mu.Lock()
\t\tif state.ActiveReqs >= state.Backend.MaxConcurrentReqs {
\t\t\tstate.mu.Unlock()
\t\t\tcontinue
\t\t}
\t\tstate.mu.Unlock()
\t\t
\t\tpred := state.Prediction
\t\tif pred.SecondsToCritical > 0 && pred.SecondsToCritical < 300 {
\t\t\tcontinue
\t\t}
\t\t
\t\tp.metricsMgr.mu.RLock()
\t\tmetrics, hasMetrics := p.metricsMgr.metrics[id]
\t\tp.metricsMgr.mu.RUnlock()
\t\t
\t\tif !hasMetrics {
\t\t\tcontinue
\t\t}
\t\t
\t\thasModel := false
\t\tfor _, m := range metrics.Ollama.RunningModels {
\t\t\tif m.Name == modelName || strings.Contains(m.Name, modelName) {
\t\t\t\thasModel = true
\t\t\t\tbreak
\t\t\t}
\t\t}
\t\tif !hasModel {
\t\t\tcontinue
\t\t}
\t\t
\t\tscore := p.calculateScore(id)
\t\tif score > bestScore {
\t\t\tbestScore = score
\t\t\tbestBackendID = id
\t\t}
\t}
\t
\treturn bestBackendID
}'''

new_helpers = '''// findBackendWithModel - поиск бэкенда с загруженной моделью
func (p *Proxy) findBackendWithModel(modelName string) string {
\tp.mu.RLock()
\tdefer p.mu.RUnlock()
\t
\tvar bestBackendID string
\tvar bestScore float64 = -1
\t
\tfor id, state := range p.backends {
\t\tif state.Backend.Status != types.StatusHealthy {
\t\t\tcontinue
\t\t}

\t\tif !p.checkResourceLimits(id) {
\t\t\tcontinue
\t\t}

\t\tstate.mu.Lock()
\t\tif state.ActiveReqs >= state.Backend.MaxConcurrentReqs {
\t\t\tstate.mu.Unlock()
\t\t\tcontinue
\t\t}
\t\tstate.mu.Unlock()
\t\t
\t\tpred := state.Prediction
\t\tif pred.SecondsToCritical > 0 && pred.SecondsToCritical < 300 {
\t\t\tcontinue
\t\t}
\t\t
\t\tp.metricsMgr.mu.RLock()
\t\tmetrics, hasMetrics := p.metricsMgr.metrics[id]
\t\tp.metricsMgr.mu.RUnlock()
\t\t
\t\tif !hasMetrics {
\t\t\tcontinue
\t\t}
\t\t
\t\thasModel := false
\t\tfor _, m := range metrics.Ollama.RunningModels {
\t\t\tif m.Name == modelName || strings.Contains(m.Name, modelName) {
\t\t\t\thasModel = true
\t\t\t\tbreak
\t\t\t}
\t\t}
\t\tif !hasModel {
\t\t\tcontinue
\t\t}
\t\t
\t\tscore := p.calculateScore(id)
\t\tif score > bestScore {
\t\t\tbestScore = score
\t\t\tbestBackendID = id
\t\t}
\t}
\t
\treturn bestBackendID
}

// findWarmingBackendForModelUnsafe — поиск WARMING_UP бэкенда (без блокировки, вызывается под p.mu.RLock)
func (p *Proxy) findWarmingBackendForModelUnsafe(model string) string {
\tfor id, state := range p.backends {
\t\tif state.Backend.Status != types.StatusHealthy { continue }
\t\tstate.mu.Lock()
\t\tws, exists := state.WarmingUpModels[model]
\t\tstate.mu.Unlock()
\t\tif exists && ws != nil && time.Now().Before(ws.EstimatedReadyAt) { return id }
\t}
\treturn ""
}

// findFreeBackendForModelUnsafe — свободный бэкенд с достаточным VRAM (без блокировки)
func (p *Proxy) findFreeBackendForModelUnsafe(model string) string {
\tvar best string
\tvar maxFree uint64
\tfor id, state := range p.backends {
\t\tif state.Backend.Status != types.StatusHealthy { continue }
\t\tif !p.checkResourceLimits(id) { continue }
\t\tstate.mu.Lock()
\t\tactive := state.ActiveReqs
\t\tmaxReqs := state.Backend.MaxConcurrentReqs
\t\tstate.mu.Unlock()
\t\tif active > 0 || (maxReqs > 0 && active >= maxReqs) { continue }
\t\tmetrics, ok := p.metricsMgr.metrics[id]
\t\tif !ok || metrics.GPU.MemoryTotal == 0 { continue }
\t\tfreeVRAM := metrics.GPU.MemoryTotal - metrics.GPU.MemoryUsed
\t\tneeded := estimateModelVRAM(model)
\t\tif freeVRAM <= needed { continue }
\t\thasModel := false
\t\tfor _, m := range metrics.Ollama.RunningModels {
\t\t\tif m.Name == model || strings.Contains(m.Name, model) { hasModel = true; break }
\t\t}
\t\tif !hasModel && freeVRAM > maxFree { maxFree = freeVRAM; best = id }
\t}
\treturn best
}

// checkModelReadyUnsafe — готова ли модель на бэкенде (без блокировки)
func (p *Proxy) checkModelReadyUnsafe(backendID, model string) bool {
\tmetrics, ok := p.metricsMgr.metrics[backendID]
\tif !ok { return false }
\tfor _, m := range metrics.Ollama.RunningModels {
\t\tif m.Name == model || strings.Contains(m.Name, model) { return true }
\t}
\treturn false
}'''

if old_find in data:
    data = data.replace(old_find, new_helpers, 1)
    replacements += 1
    print("Added Unsafe helpers")
else:
    print("ERROR: findBackendWithModel old pattern not found")
    sys.exit(1)

# 3. Replace calculateScore with v2
old_score = '''// calculateScore - вычисление scores для бэкенда с учётом Ollama-специфических метрик
func (p *Proxy) calculateScore(backendID string) float64 {
\tp.metricsMgr.mu.RLock()
\tmetrics, ok := p.metricsMgr.metrics[backendID]
\tp.metricsMgr.mu.RUnlock()

\tstate := p.backends[backendID]
\tif state == nil {
\t\treturn 0
\t}

\tif !ok {
\t\tstate.mu.Lock()
\t\tactive := state.ActiveReqs
\t\tmaxReqs := state.Backend.MaxConcurrentReqs
\t\tstate.mu.Unlock()

\t\tscore := float64(state.Backend.Weight)
\t\tif maxReqs > 0 {
\t\t\tloadRatio := float64(active) / float64(maxReqs)
\t\t\tscore -= loadRatio * 20.0
\t\t}
\t\tif score < 1 {
\t\t\tscore = 1
\t\t}
\t\treturn score
\t}

\tgpuFree := 100 - metrics.GPU.UsagePercent
\tvar vramFree float64 = 100
\tif metrics.GPU.MemoryTotal > 0 {
\t\tvramFree = float64(metrics.GPU.MemoryFree) * 100 / float64(metrics.GPU.MemoryTotal)
\t}
\tcpuFree := 100 - metrics.System.CPUUsagePercent

\tbaseScore := gpuFree*0.35 + vramFree*0.25 + cpuFree*0.20

\trequestPenalty := 0.0
\tif metrics.Ollama.MaxConcurrentRequests > 0 {
\t\treqRatio := float64(metrics.Ollama.ActiveRequests) / float64(metrics.Ollama.MaxConcurrentRequests)
\t\trequestPenalty = reqRatio * 15.0
\t} else {
\t\tif metrics.Ollama.ActiveRequests > 5 {
\t\t\trequestPenalty = float64(metrics.Ollama.ActiveRequests) * 1.5
\t\t}
\t}

\tmodelCapacityScore := 0.0
\tif metrics.Ollama.MaxModels > 0 {
\t\tloaded := len(metrics.Ollama.RunningModels)
\t\tcapacityRatio := float64(metrics.Ollama.MaxModels-loaded) / float64(metrics.Ollama.MaxModels)
\t\tmodelCapacityScore = capacityRatio * 10.0
\t} else if metrics.Ollama.BackendCapacity.LoadableModelCount > 0 {
\t\tloadableCount := metrics.Ollama.BackendCapacity.LoadableModelCount
\t\tif loadableCount >= 5 {
\t\t\tmodelCapacityScore = 10.0
\t\t} else if loadableCount >= 3 {
\t\t\tmodelCapacityScore = 7.0
\t\t} else if loadableCount >= 1 {
\t\t\tmodelCapacityScore = 4.0
\t\t} else {
\t\t\tmodelCapacityScore = -3.0
\t\t}
\t} else {
\t\tif metrics.GPU.MemoryTotal > 0 {
\t\t\tvar loadedModelVRAM uint64
\t\t\tfor _, m := range metrics.Ollama.RunningModels {
\t\t\t\tloadedModelVRAM += m.VRAMUsage
\t\t\t}
\t\t\tif metrics.GPU.MemoryTotal > 0 {
\t\t\t\tvramUsedByModels := float64(loadedModelVRAM) * 100 / float64(metrics.GPU.MemoryTotal)
\t\t\t\tif vramUsedByModels > 80 {
\t\t\t\t\tmodelCapacityScore = -5.0
\t\t\t\t} else if vramUsedByModels > 50 {
\t\t\t\t\tmodelCapacityScore = 2.0
\t\t\t\t} else {
\t\t\t\t\tmodelCapacityScore = 8.0
\t\t\t\t}
\t\t\t}
\t\t}
\t}

\tpredictionBonus := 0.0
\tpred := state.Prediction
\tif pred.SecondsToCritical < 0 || pred.SecondsToCritical >= 600 {
\t\tpredictionBonus = 3.0
\t} else if pred.SecondsToCritical >= 300 {
\t\tpredictionBonus = 1.5
\t} else if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 120 {
\t\tpredictionBonus = -5.0
\t}

\tweightFactor := float64(state.Backend.Weight)
\tif weightFactor <= 0 {
\t\tweightFactor = 1
\t}
\tscore := (baseScore - requestPenalty + modelCapacityScore + predictionBonus) * weightFactor

\tif score < 0 {
\t\tscore = 0
\t}

\treturn score
}'''

new_score = '''// calculateScore - вычисление scores для бэкенда (v2: model affinity, queue depth, error rate)
func (p *Proxy) calculateScore(backendID string) float64 {
\tp.metricsMgr.mu.RLock()
\tmetrics, ok := p.metricsMgr.metrics[backendID]
\tp.metricsMgr.mu.RUnlock()

\tstate := p.backends[backendID]
\tif state == nil {
\t\treturn 0
\t}

\t// Веса из конфига (с дефолтами)
\tsc := p.config.Balancing.Scoring
\twModelLoaded := sc.ModelAlreadyLoaded; if wModelLoaded <= 0 { wModelLoaded = 0.15 }
\twModelCost := sc.ModelLoadingCost; if wModelCost <= 0 { wModelCost = 0.10 }
\twQueueDepth := sc.QueueDepthPenalty; if wQueueDepth <= 0 { wQueueDepth = 0.05 }
\twErrorRate := sc.ErrorRatePenalty; if wErrorRate <= 0 { wErrorRate = 0.05 }
\twPrediction := sc.PredictionBonus; if wPrediction <= 0 { wPrediction = 0.10 }

\tif !ok {
\t\tstate.mu.Lock()
\t\tactive := state.ActiveReqs
\t\tmaxReqs := state.Backend.MaxConcurrentReqs
\t\tstate.mu.Unlock()
\t\tscore := float64(state.Backend.Weight)
\t\tif maxReqs > 0 {
\t\t\tscore -= float64(active) / float64(maxReqs) * 20.0
\t\t}
\t\tif score < 1 { score = 1 }
\t\treturn score
\t}

\t// Базовые ресурсы (обновлённые веса: GPU*0.30, VRAM*0.20, CPU*0.15)
\tgpuFree := 100 - metrics.GPU.UsagePercent
\tvramFree := 100.0
\tif metrics.GPU.MemoryTotal > 0 { vramFree = float64(metrics.GPU.MemoryFree) * 100 / float64(metrics.GPU.MemoryTotal) }
\tcpuFree := 100 - metrics.System.CPUUsagePercent
\tbaseScore := gpuFree*0.30 + vramFree*0.20 + cpuFree*0.15

\t// Request penalty
\trequestPenalty := 0.0
\tif metrics.Ollama.MaxConcurrentRequests > 0 {
\t\trequestPenalty = float64(metrics.Ollama.ActiveRequests) / float64(metrics.Ollama.MaxConcurrentRequests) * 15.0
\t} else if metrics.Ollama.ActiveRequests > 5 {
\t\trequestPenalty = float64(metrics.Ollama.ActiveRequests) * 1.5
\t}

\t// Model capacity score (сохранено)
\tmodelCapacityScore := 0.0
\tif metrics.Ollama.MaxModels > 0 {
\t\tloaded := len(metrics.Ollama.RunningModels)
\t\tmodelCapacityScore = float64(metrics.Ollama.MaxModels-loaded) / float64(metrics.Ollama.MaxModels) * 10.0
\t} else if metrics.Ollama.BackendCapacity.LoadableModelCount > 0 {
\t\tswitch {
\t\tcase metrics.Ollama.BackendCapacity.LoadableModelCount >= 5: modelCapacityScore = 10.0
\t\tcase metrics.Ollama.BackendCapacity.LoadableModelCount >= 3: modelCapacityScore = 7.0
\t\tcase metrics.Ollama.BackendCapacity.LoadableModelCount >= 1: modelCapacityScore = 4.0
\t\tdefault: modelCapacityScore = -3.0
\t\t}
\t} else if metrics.GPU.MemoryTotal > 0 {
\t\tvar loadedVRAM uint64
\t\tfor _, m := range metrics.Ollama.RunningModels { loadedVRAM += m.VRAMUsage }
\t\tvramRatio := float64(loadedVRAM) * 100 / float64(metrics.GPU.MemoryTotal)
\t\tswitch {
\t\tcase vramRatio > 80: modelCapacityScore = -5.0
\t\tcase vramRatio > 50: modelCapacityScore = 2.0
\t\tdefault: modelCapacityScore = 8.0
\t\t}
\t}

\t// Model already loaded bonus (model affinity)
\tmodelLoadedBonus := float64(len(metrics.Ollama.RunningModels)) * wModelLoaded * 10.0

\t// Queue depth penalty (глобальная очередь)
\tqueueDepthPenalty := float64(len(p.queueMgr.queue)) * wQueueDepth

\t// Error rate penalty
\terrorRatePenalty := 0.0
\tstate.mu.Lock()
\tif state.TotalAttempts > 10 {
\t\terrorRatePenalty = float64(state.ErrorCount) / float64(state.TotalAttempts) * wErrorRate * 100
\t}
\tstate.mu.Unlock()

\t// Prediction bonus
\tpredictionBonus := 0.0
\tif pred := state.Prediction; pred.SecondsToCritical < 0 || pred.SecondsToCritical >= 600 {
\t\tpredictionBonus = 3.0 * wPrediction
\t} else if pred.SecondsToCritical >= 300 {
\t\tpredictionBonus = 1.5 * wPrediction
\t} else if pred.SecondsToCritical > 0 && pred.SecondsToCritical < 120 {
\t\tpredictionBonus = -5.0 * wPrediction
\t}

\tweightFactor := float64(state.Backend.Weight)
\tif weightFactor <= 0 { weightFactor = 1 }
\tscore := (baseScore - requestPenalty + modelCapacityScore + modelLoadedBonus - queueDepthPenalty - errorRatePenalty + predictionBonus) * weightFactor
\tif score < 0 { score = 0 }
\treturn score
}'''

if old_score in data:
    data = data.replace(old_score, new_score, 1)
    replacements += 1
    print("Replaced calculateScore with v2")
else:
    print("ERROR: calculateScore old pattern not found")
    sys.exit(1)

with open(path, 'w', encoding='utf-8') as f:
    f.write(data)

print(f"All patches applied ({replacements} replacements). Done.")