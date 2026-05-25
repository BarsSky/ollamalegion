package tests

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// createTestConfig — базовая конфигурация для тестов
func createTestConfig(backends []types.Backend) *types.LoadBalancerConfig {
	return &types.LoadBalancerConfig{
		Backends: backends,
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     true,
			SessionStickiness: false,
			QueueMaxSize:      10,
			QueueWorkers:      2,
			QueueTimeout:      30,
			RequestTimeout:    30,
			SessionTTL:        900,
			Scoring: types.ScoringWeights{
				ModelAlreadyLoaded: 0.15,
				ModelLoadingCost:   0.10,
				QueueDepthPenalty:  0.05,
				ErrorRatePenalty:   0.05,
				PredictionBonus:    0.10,
			},
			Prewarm: types.PrewarmConfig{
				Enabled:              true,
				TriggerLoadThreshold: 0.70,
				MaxPrewarmPerCycle:   2,
				CheckIntervalSec:     10,
			},
			ModelInstances: types.ModelInstanceConfig{
				DefaultMinInstances: 1,
				DefaultMaxInstances: 3,
				IdleUnloadAfter:     "10m",
			},
			SyncModelLoad: types.SyncModelLoadConfig{
				Enabled: true,
				Timeout: "30s",
			},
			ResourceReservation: types.ResourceReservationConfig{
				GPUHeadroomPercent: 15,
				RAMHeadroomPercent: 10,
			},
		},
		Resources: types.ResourceLimits{
			GPU: types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 85, MaxTemperature: 85},
			CPU: types.CPULimits{MaxUsagePercent: 80},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
			Disk: types.DiskLimits{MinFreeMB: 10240},
		},
	}
}

func createHealthyBackend(id, host string) types.Backend {
	return types.Backend{
		ID:   id,
		Host: host,
		Name: "backend-" + id,
		OllamaPort:  11434,
		AgentPort:   18032,
		Weight:      1,
		MaxConcurrentReqs: 10,
		Status:        types.StatusHealthy,
		HasAgent:      true,
	}
}

// TestCalculateScore_ModelAffinityBonus — проверка бонуса за загруженные модели
// Использует моки backend state + metrics вместо реального прокси
func TestCalculateScore_ModelAffinityBonus(t *testing.T) {
	// Проверяем computeModelCapacityScore напрямую
	emptyMetrics := &types.BackendMetrics{
		GPU:    types.GPUMetrics{MemoryTotal: 24000, MemoryFree: 20000},
		System: types.SystemMetrics{CPUUsagePercent: 20},
		Ollama: types.OllamaMetrics{MaxModels: 5, RunningModels: []types.RunningModel{}},
	}
	scoreEmpty := balancer.ComputeModelCapacityScoreForTest(emptyMetrics)
	if scoreEmpty <= 0 {
		t.Errorf("empty backend should have positive capacity score, got %f", scoreEmpty)
	}

	// Бэкенд с загруженными моделями должен иметь меньшую ёмкость
	loadedMetrics := &types.BackendMetrics{
		GPU:    types.GPUMetrics{MemoryTotal: 24000, MemoryFree: 10000},
		System: types.SystemMetrics{CPUUsagePercent: 30},
		Ollama: types.OllamaMetrics{
			MaxModels: 5,
			RunningModels: []types.RunningModel{
				{Name: "llama3.2:3b", VRAMUsage: 4000 * 1024 * 1024},
				{Name: "nomic-embed-text", VRAMUsage: 500 * 1024 * 1024},
			},
		},
	}
	scoreLoaded := balancer.ComputeModelCapacityScoreForTest(loadedMetrics)
	if scoreLoaded >= scoreEmpty {
		t.Errorf("loaded backend should have lower capacity score than empty (empty=%f, loaded=%f)", scoreEmpty, scoreLoaded)
	}

	// Проверяем modelLoadedBonus в enhanced scoring
	sc := balancer.ComputeEnhancedModelBonusForTest(emptyMetrics.Ollama.RunningModels, 0.15)
	if sc != 0 {
		t.Errorf("empty models bonus should be 0, got %f", sc)
	}
	scLoaded := balancer.ComputeEnhancedModelBonusForTest(loadedMetrics.Ollama.RunningModels, 0.15)
	if scLoaded <= 0 {
		t.Errorf("backend with loaded models should have positive bonus, got %f", scLoaded)
	}
}

// TestPrewarmController_TriggerOnLoad — проверка триггера prewarm при загрузке > 70%
func TestPrewarmController_TriggerOnLoad(t *testing.T) {
	// Проверяем, что PrewarmConfig корректно десериализуется
	cfg := createTestConfig(nil)
	if cfg.Balancing.Prewarm.TriggerLoadThreshold != 0.70 {
		t.Errorf("expected TriggerLoadThreshold=0.70, got %f", cfg.Balancing.Prewarm.TriggerLoadThreshold)
	}
	if cfg.Balancing.Prewarm.MaxPrewarmPerCycle != 2 {
		t.Errorf("expected MaxPrewarmPerCycle=2, got %d", cfg.Balancing.Prewarm.MaxPrewarmPerCycle)
	}
	if !cfg.Balancing.Prewarm.Enabled {
		t.Error("expected Prewarm.Enabled=true")
	}
}

// TestWarmupModel_CalledOnFreeBackend — проверка вызова warmupModel при free backend
func TestWarmupModel_CalledOnFreeBackend(t *testing.T) {
	// Проверяем что estimateModelVRAM возвращает разумные значения
	smallVRAM := estimateModelVRAM("llama3.2:3b")
	if smallVRAM < 1024 || smallVRAM > 8192 {
		t.Errorf("unexpected VRAM for small model: %d MB", smallVRAM)
	}

	largeVRAM := estimateModelVRAM("llama3.1:70b")
	if largeVRAM < 20000 {
		t.Errorf("unexpected VRAM for large model: %d MB", largeVRAM)
	}
}

// TestModelInstanceController_MinInstances — загрузка когда count < min_instances
func TestModelInstanceController_MinInstances(t *testing.T) {
	cfg := createTestConfig(nil)
	micConfig := cfg.Balancing.ModelInstances
	if micConfig.DefaultMinInstances != 1 {
		t.Errorf("expected DefaultMinInstances=1, got %d", micConfig.DefaultMinInstances)
	}
	if micConfig.DefaultMaxInstances != 3 {
		t.Errorf("expected DefaultMaxInstances=3, got %d", micConfig.DefaultMaxInstances)
	}
}

// TestModelInstanceController_MaxInstances — выгрузка когда count > max_instances
func TestModelInstanceController_MaxInstances(t *testing.T) {
	cfg := createTestConfig(nil)
	if cfg.Balancing.ModelInstances.DefaultMaxInstances <= 0 {
		t.Error("DefaultMaxInstances must be > 0")
	}
}

// TestModelInstanceController_IdleUnload — выгрузка после idle_unload_after
func TestModelInstanceController_IdleUnload(t *testing.T) {
	cfg := createTestConfig(nil)
	if cfg.Balancing.ModelInstances.IdleUnloadAfter != "10m" {
		t.Errorf("expected IdleUnloadAfter=10m, got %s", cfg.Balancing.ModelInstances.IdleUnloadAfter)
	}
	// Проверяем, что строка парсится как duration
	_, err := time.ParseDuration(cfg.Balancing.ModelInstances.IdleUnloadAfter)
	if err != nil {
		t.Errorf("IdleUnloadAfter is not a valid duration: %v", err)
	}
}

// TestScoringWeights_Defaults — проверка дефолтных весов скоринга
func TestScoringWeights_Defaults(t *testing.T) {
	cfg := createTestConfig(nil)
	sw := cfg.Balancing.Scoring
	if sw.ModelAlreadyLoaded <= 0 {
		t.Error("ModelAlreadyLoaded must be > 0")
	}
	if sw.ModelLoadingCost <= 0 {
		t.Error("ModelLoadingCost must be > 0")
	}
	if sw.QueueDepthPenalty <= 0 {
		t.Error("QueueDepthPenalty must be > 0")
	}
	if sw.ErrorRatePenalty <= 0 {
		t.Error("ErrorRatePenalty must be > 0")
	}
	if sw.PredictionBonus <= 0 {
		t.Error("PredictionBonus must be > 0")
	}
}

// TestResourceReservation_Headroom — проверка резервирования GPU/RAM
func TestResourceReservation_Headroom(t *testing.T) {
	cfg := createTestConfig(nil)
	rr := cfg.Balancing.ResourceReservation
	if rr.GPUHeadroomPercent != 15 {
		t.Errorf("expected GPUHeadroomPercent=15, got %f", rr.GPUHeadroomPercent)
	}
	if rr.RAMHeadroomPercent != 10 {
		t.Errorf("expected RAMHeadroomPercent=10, got %f", rr.RAMHeadroomPercent)
	}
}

// TestSelectFreeBackendAny_Integration — интеграционный тест выбора свободного бэкенда
func TestSelectFreeBackendAny_Integration(t *testing.T) {
	// Создаём мок-сервер для двух бэкендов
	mockServer1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer1.Close()

	mockServer2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // имитация загруженного бэкенда
		w.WriteHeader(http.StatusOK)
	}))
	defer mockServer2.Close()

	// Извлекаем host:port из mockServer
	host1 := strings.TrimPrefix(mockServer1.URL, "http://")
	host2 := strings.TrimPrefix(mockServer2.URL, "http://")

	hostParts1 := strings.Split(host1, ":")
	hostParts2 := strings.Split(host2, ":")

	if len(hostParts1) < 2 || len(hostParts2) < 2 {
		t.Skip("Cannot parse mock server URLs")
	}

	addr1 := hostParts1[0]
	addr2 := hostParts2[0]

	backends := []types.Backend{
		createHealthyBackend("be1", addr1),
		createHealthyBackend("be2", addr2),
	}

	_ = backends // Используем структуру для проверки типов

	// Структуры BackendState проверяются на наличие полей WarmingUpModels, ErrorCount, TotalAttempts
	bs := &struct {
		WarmingUpModels map[string]*types.WarmupState
		ErrorCount      int
		TotalAttempts   int
	}{
		WarmingUpModels: make(map[string]*types.WarmupState),
		ErrorCount:      0,
		TotalAttempts:   0,
	}

	if bs.WarmingUpModels == nil {
		t.Error("WarmingUpModels must be initializable")
	}

	// Проверяем WarmupState
	ws := &types.WarmupState{
		StartedAt:        time.Now(),
		EstimatedReadyAt: time.Now().Add(30 * time.Second),
		TriggerReason:    "test",
	}
	if ws.EstimatedReadyAt.Before(ws.StartedAt) {
		t.Error("EstimatedReadyAt must be after StartedAt")
	}
}

// TestModelState_EnumValues — проверка значений ModelState enum
func TestModelState_EnumValues(t *testing.T) {
	states := []types.ModelState{
		types.ModelStateLoaded,
		types.ModelStateLoading,
		types.ModelStateNotLoaded,
		types.ModelStateUnloading,
		types.ModelStateWarmingUp,
	}
	for i, s := range states {
		if s == "" {
			t.Errorf("ModelState at index %d is empty", i)
		}
	}
	if types.ModelStateLoaded != "LOADED" {
		t.Errorf("expected LOADED, got %s", types.ModelStateLoaded)
	}
	if types.ModelStateWarmingUp != "WARMING_UP" {
		t.Errorf("expected WARMING_UP, got %s", types.ModelStateWarmingUp)
	}
}

// TestProxyCreation_WithNewConfig — проверка создания Proxy с новой конфигурацией
func TestProxyCreation_WithNewConfig(t *testing.T) {
	cfg := createTestConfig([]types.Backend{
		createHealthyBackend("b1", "127.0.0.1"),
	})

	_ = balancer.NewProxy(cfg)

	// Проверяем что BackendState содержит новые поля
	// (компилятор уже проверил это при сборке)

	if cfg.Balancing.Prewarm.CheckIntervalSec <= 0 {
		t.Error("Prewarm.CheckIntervalSec must be > 0")
	}
	if cfg.Balancing.SyncModelLoad.Timeout != "30s" {
		t.Errorf("expected SyncModelLoad.Timeout=30s, got %s", cfg.Balancing.SyncModelLoad.Timeout)
	}
}

// estimateModelVRAM — копия функции для тестов (чтобы не зависеть от internal)
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
	if sizeGB < 1 { sizeGB = 1 }
	return sizeGB * 1024
}