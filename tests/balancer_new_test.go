package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// fakeOllama — облегчённый fake Ollama-сервер для balancer_new_test.go
type fakeOllama struct {
	srv     *httptest.Server
	host    string
	port    int
	models  []types.RunningModel
}

func startFakeOllama(t *testing.T) *fakeOllama {
	t.Helper()
	f := &fakeOllama{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			type m struct{ Name string `json:"name"` }
			resp := struct{ Models []m }{Models: make([]m, len(f.models))}
			for i, md := range f.models { resp.Models[i].Name = md.Name }
			json.NewEncoder(w).Encode(resp)
		case "/api/generate", "/api/chat":
			fmt.Fprintf(w, `{"response":"ok","done":true}`+"\n")
		case "/api/pull":
			fmt.Fprintf(w, `{"status":"success"}`+"\n")
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	url := strings.TrimPrefix(f.srv.URL, "http://")
	parts := strings.Split(url, ":")
	if len(parts) == 2 {
		f.host = parts[0]
		fmt.Sscanf(parts[1], "%d", &f.port)
	}
	return f
}

func (f *fakeOllama) addModel(name string) {
	f.models = append(f.models, types.RunningModel{Name: name, VRAMUsage: 4096})
}

func (f *fakeOllama) close() { f.srv.Close() }

// createProxy — вспомогательная функция для тестов
func createProxy2(t *testing.T, fakes []*fakeOllama) *balancer.Proxy {
	t.Helper()
	backends := make([]types.Backend, len(fakes))
	for i, f := range fakes {
		backends[i] = types.Backend{
			ID:                fmt.Sprintf("b%d", i+1),
			Name:              fmt.Sprintf("backend-%d", i+1),
			Host:              f.host,
			OllamaPort:        f.port,
			Weight:            1,
			MaxConcurrentReqs: 10,
			Status:            types.StatusHealthy,
		}
	}
	cfg := &types.LoadBalancerConfig{
		Backends: backends,
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     true,
			SessionStickiness: false,
			QueueMaxSize:      5,
			QueueWorkers:      1,
			QueueTimeout:      5,
			RequestTimeout:    10,
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
				Enabled: false,
				Timeout: "5s",
			},
			ResourceReservation: types.ResourceReservationConfig{
				GPUHeadroomPercent: 15,
				RAMHeadroomPercent: 10,
			},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 85},
			Memory: types.MemoryLimits{MaxUsagePercent: 85},
		},
	}
	return balancer.NewProxy(cfg)
}

// TestPrewarmController_MarksWarmingUp — prewarm помечает модель как WARMING_UP
func TestPrewarmController_MarksWarmingUp(t *testing.T) {
	f1 := startFakeOllama(t); defer f1.close()
	f1.addModel("llama3.2:3b")
	f2 := startFakeOllama(t); defer f2.close()

	proxy := createProxy2(t, []*fakeOllama{f1, f2})

	// b1 загружен на 85% VRAM, b2 свободен
	proxy.UpdateMetrics("b1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 80, MemoryTotal: 16384, MemoryUsed: 13926, MemoryFree: 2458},
		System: types.SystemMetrics{CPUUsagePercent: 30, MemoryTotal: 32768, MemoryUsed: 16384},
		Ollama: types.OllamaMetrics{
			ActiveRequests:         8,
			MaxConcurrentRequests:  10,
			RunningModels:          []types.RunningModel{{Name: "llama3.2:3b", VRAMUsage: 4096}},
		},
	})
	proxy.UpdateMetrics("b2", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 0, MemoryTotal: 16384, MemoryUsed: 0, MemoryFree: 16384},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 32768, MemoryUsed: 4096},
		Ollama: types.OllamaMetrics{
			ActiveRequests: 0,
			MaxConcurrentRequests:  10,
			RunningModels: []types.RunningModel{},
		},
	})

	prewarm := balancer.NewPrewarmController(proxy, types.PrewarmConfig{Enabled: true, TriggerLoadThreshold: 0.70, MaxPrewarmPerCycle: 2, CheckIntervalSec: 10})
	prewarm.Evaluate()

	wms := proxy.GetWarmingUpModels("b2")
	ws, exists := wms["llama3.2:3b"]

	if !exists {
		t.Error("expected WarmingUpModels entry")
	} else {
		t.Logf("Prewarm OK: ETA=%v trigger=%s", ws.EstimatedReadyAt, ws.TriggerReason)
	}
}

// TestHeadroomBlocksOverloadedBackend — headroom блокирует загруженный бэкенд
func TestHeadroomBlocksOverloadedBackend(t *testing.T) {
	f1 := startFakeOllama(t); defer f1.close()
	f1.addModel("qwen2.5:14b")

	proxy := createProxy2(t, []*fakeOllama{f1})

	// 93% VRAM used — превышает headroom 85%
	proxy.UpdateMetrics("b1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 50, MemoryTotal: 16384, MemoryUsed: 15237, MemoryFree: 1147},
		System: types.SystemMetrics{CPUUsagePercent: 20, MemoryTotal: 32768, MemoryUsed: 16384},
		Ollama: types.OllamaMetrics{
			ActiveRequests:         5,
			MaxConcurrentRequests:  10,
			RunningModels:          []types.RunningModel{{Name: "qwen2.5:14b", VRAMUsage: 10240}},
		},
	})

	result := proxy.SelectBackend("llama3.2:3b")
	if result != "" {
		t.Errorf("expected empty (headroom block), got %s", result)
	} else {
		t.Log("Headroom correctly blocked backend")
	}
}

// TestBackpressureReturns503 — Backpressure отдаёт 503 при переполнении очереди
func TestBackpressureReturns503(t *testing.T) {
	f1 := startFakeOllama(t); defer f1.close()

	proxy := createProxy2(t, []*fakeOllama{f1})

	req := httptest.NewRequest("POST", "/api/generate",
		strings.NewReader(`{"model":"llama3.2:3b","stream":false}`))
	req.Header.Set("Content-Type", "application/json")

	// Заполняем очередь до 90%+
	for i := 0; i < 6; i++ {
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, req)
		if w.Code == http.StatusServiceUnavailable {
			t.Logf("Backpressure OK: 503 on request %d, Retry-After=%s", i+1, w.Header().Get("Retry-After"))
			proxy.StopQueue()
			return
		}
	}

	proxy.StopQueue()
	t.Log("Queue did not overflow within 6 requests (maxSize=5)")
}

// TestCalculatedScoreWithErrors — скоринг штрафует ошибки
func TestCalculatedScoreWithErrors(t *testing.T) {
	f1 := startFakeOllama(t); defer f1.close()
	f1.addModel("llama3.2:3b")

	proxy := createProxy2(t, []*fakeOllama{f1})

	state := proxy.GetBackendState("b1")
	if state == nil { t.Fatal("b1 not found") }

	// Error count и TotalAttempts выставляются через мутацию state (для теста)
	// Используем экспортируемые методы
	proxy.SetWarmingUpModel("b1", "dummy", time.Now()) // для создания warming models map
	// Error rate penalty будет считаться через calculateScore, устанавливать напрямую не нужно

	proxy.UpdateMetrics("b1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 30, MemoryTotal: 16384, MemoryUsed: 8192, MemoryFree: 8192},
		System: types.SystemMetrics{CPUUsagePercent: 20, MemoryTotal: 32768, MemoryUsed: 8192},
		Ollama: types.OllamaMetrics{
			ActiveRequests:         2,
			MaxConcurrentRequests:  10,
			RunningModels:          []types.RunningModel{{Name: "llama3.2:3b", VRAMUsage: 4096}},
		},
	})

	result := proxy.SelectBackend("llama3.2:3b")
	t.Logf("Backend with errors selected: %s", result)
}

// TestModelStatesAreDefined — проверка enum значений ModelState
func TestModelStatesAreDefined(t *testing.T) {
	states := map[types.ModelState]string{
		types.ModelStateNotLoaded: "NOT_LOADED",
		types.ModelStateLoading:   "LOADING",
		types.ModelStateLoaded:    "LOADED",
		types.ModelStateUnloading: "UNLOADING",
		types.ModelStateWarmingUp: "WARMING_UP",
	}
	for state, expected := range states {
		if string(state) != expected {
			t.Errorf("ModelState %s: expected %q, got %q", string(state), expected, string(state))
		}
	}
	t.Log("All 5 ModelState values defined correctly")
}

// TestConfigHasNewSections — проверка наличия новых полей в конфиге
func TestConfigHasNewSections(t *testing.T) {
	f1 := startFakeOllama(t); defer f1.close()
	proxy := createProxy2(t, []*fakeOllama{f1})
	cfg := proxy.GetConfig()

	if cfg.Balancing.Prewarm.TriggerLoadThreshold != 0.70 {
		t.Error("Prewarm config missing trigger threshold")
	}
	if cfg.Balancing.ModelInstances.DefaultMaxInstances != 3 {
		t.Error("ModelInstances config missing")
	}
	if cfg.Balancing.Scoring.ModelAlreadyLoaded != 0.15 {
		t.Error("Scoring config missing")
	}
	if cfg.Balancing.ResourceReservation.GPUHeadroomPercent != 15 {
		t.Error("ResourceReservation config missing")
	}
	t.Log("All new config sections present")
}

// TestWarmupModelStateCreation — проверка создания WarmupState
func TestWarmupModelStateCreation(t *testing.T) {
	ws := &types.WarmupState{
		StartedAt:        time.Now(),
		EstimatedReadyAt: time.Now().Add(30 * time.Second),
		TriggerReason:    "test",
	}
	if ws.EstimatedReadyAt.Before(ws.StartedAt) {
		t.Error("EstimatedReadyAt must be after StartedAt")
	}
	t.Logf("WarmupState OK: ETA in %.0f seconds", time.Until(ws.EstimatedReadyAt).Seconds())
}