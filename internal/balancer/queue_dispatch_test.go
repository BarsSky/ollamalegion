package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func TestGetModelLoadTimeout(t *testing.T) {
	tests := []struct {
		name     string
		timeout  int
		expected time.Duration
	}{
		{"default (0)", 0, 120 * time.Second},
		{"default (-1)", -1, 120 * time.Second},
		{"custom 60s", 60, 60 * time.Second},
		{"custom 300s", 300, 300 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Proxy{
				config: &types.LoadBalancerConfig{
					Balancing: types.BalancingSettings{ModelLoadTimeout: tt.timeout},
				},
			}
			got := p.getModelLoadTimeout()
			if got != tt.expected {
				t.Errorf("getModelLoadTimeout() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestModelManagerGetLoadTimeout(t *testing.T) {
	tests := []struct {
		name     string
		timeout  int
		expected time.Duration
	}{
		// Round 8 (2026-07-10): default bumped to 10 min for 21GB MoE models.
		{"default (0) → 10m", 0, 600 * time.Second},
		{"default (-1) → 10m", -1, 600 * time.Second},
		{"custom 60s", 60, 60 * time.Second},
		{"custom 300s", 300, 300 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Proxy{
				config: &types.LoadBalancerConfig{
					Balancing: types.BalancingSettings{ModelLoadTimeout: tt.timeout},
				},
			}
			mm := NewModelManager(p)
			got := mm.getLoadTimeout()
			if got != tt.expected {
				t.Errorf("getLoadTimeout() = %v, want %v", got, tt.expected)
			}
		})
	}

	// Проверка nil-proxy fallback
	t.Run("nil proxy → 10m default", func(t *testing.T) {
		mm := &ModelManager{proxy: nil}
		got := mm.getLoadTimeout()
		if got != 600*time.Second {
			t.Errorf("getLoadTimeout() with nil proxy = %v, want 600s", got)
		}
	})
}

func TestCanAcceptRequest(t *testing.T) {
	p := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			Algorithm:            "resource-aware",
			ModelAffinity:        true,
			Prewarm:              types.PrewarmConfig{TriggerLoadThreshold: 0.80},
			SyncModelLoad:        types.SyncModelLoadConfig{Enabled: true, Timeout: "30s"},
			QueueMaxSize:         10,
			QueueWorkers:         2,
			QueueTimeout:         60,
			ResourceReservation:  types.ResourceReservationConfig{GPUHeadroomPercent: 0.10},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 99, MaxVRAMUsagePercent: 99},
			CPU:    types.CPULimits{MaxUsagePercent: 99},
			Memory: types.MemoryLimits{MaxUsagePercent: 99},
			Disk:   types.DiskLimits{MinFreeMB: 0},
		},
		Backends: []types.Backend{
			{ID: "b1", Name: "Backend 1", Host: "127.0.0.1", OllamaPort: 11434, MaxConcurrentReqs: 5, Status: types.StatusHealthy},
		},
	})

	// Устанавливаем метрики с достаточным VRAM
	p.UpdateMetrics("b1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1:8b", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 5, OllamaAvailable: true},
	})

	tests := []struct {
		name      string
		activeReq int
		want      bool
	}{
		{"empty backend", 0, true},
		{"below threshold (4/5 = 80%, но < threshold 0.80*5=4, threshold проверяет loadRatio < 0.80)", 3, true},
		{"at threshold boundary", 4, false}, // 4/5 = 0.80, loadRatio < 0.80 → false
		{"above threshold", 5, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Устанавливаем ActiveReqs напрямую
			p.backends["b1"].mu.Lock()
			p.backends["b1"].ActiveReqs = tt.activeReq
			p.backends["b1"].mu.Unlock()

			got := p.canAcceptRequest("b1")
			if got != tt.want {
				t.Errorf("canAcceptRequest() = %v, want %v (activeReq=%d)", got, tt.want, tt.activeReq)
			}
		})
	}

	// Проверка несуществующего бэкенда
	if p.canAcceptRequest("nonexistent") {
		t.Error("canAcceptRequest(nonexistent) should be false")
	}

	// Проверка unhealthy бэкенда
	p.UpdateBackendStatus("b1", types.StatusUnhealthy)
	if p.canAcceptRequest("b1") {
		t.Error("canAcceptRequest(unhealthy) should be false")
	}
}

func TestDispatchRequestAffinity(t *testing.T) {
	p := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			Algorithm:            "resource-aware",
			ModelAffinity:        true,
			Prewarm:              types.PrewarmConfig{TriggerLoadThreshold: 0.80},
			SyncModelLoad:        types.SyncModelLoadConfig{Enabled: true, Timeout: "30s"},
			UseEnhancedScoring:   true,
			QueueMaxSize:         10,
			QueueWorkers:         2,
			QueueTimeout:         60,
			ResourceReservation:  types.ResourceReservationConfig{GPUHeadroomPercent: 0.10},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 99, MaxVRAMUsagePercent: 99},
			CPU:    types.CPULimits{MaxUsagePercent: 99},
			Memory: types.MemoryLimits{MaxUsagePercent: 99},
			Disk:   types.DiskLimits{MinFreeMB: 0},
		},
		Backends: []types.Backend{
			{ID: "b1", Name: "Backend 1", Host: "127.0.0.1", OllamaPort: 11434, MaxConcurrentReqs: 5, Status: types.StatusHealthy},
			{ID: "b2", Name: "Backend 2", Host: "127.0.0.1", OllamaPort: 11435, MaxConcurrentReqs: 5, Status: types.StatusHealthy},
		},
	})

	// b1: модель загружена, свободен
	p.UpdateMetrics("b1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1:8b", VRAMUsage: 4096}}, MaxModels: 10, MaxConcurrentRequests: 5, OllamaAvailable: true},
	})

	// b2: модель НЕ загружена
	p.UpdateMetrics("b2", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 5, OllamaAvailable: true},
	})

	req := &QueuedRequest{Model: "llama3.1:8b"}
	result := p.dispatchRequest(req)

	if result.Error != nil {
		t.Fatalf("dispatchRequest() error = %v", result.Error)
	}
	if result.BackendID != "b1" {
		t.Errorf("dispatchRequest() backend = %v, want b1 (affinity)", result.BackendID)
	}
	if result.DispatchType != "affinity" {
		t.Errorf("dispatchRequest() type = %v, want affinity", result.DispatchType)
	}

	// Сбрасываем слот
	p.releaseSlot(result.BackendID)
}

func TestDispatchRequestNoBackendAvailable(t *testing.T) {
	p := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			Algorithm:            "resource-aware",
			ModelAffinity:        true,
			Prewarm:              types.PrewarmConfig{TriggerLoadThreshold: 0.80},
			SyncModelLoad:        types.SyncModelLoadConfig{Enabled: false}, // выключено
			UseEnhancedScoring:   true,
			QueueMaxSize:         10,
			QueueWorkers:         2,
			QueueTimeout:         60,
			ResourceReservation:  types.ResourceReservationConfig{GPUHeadroomPercent: 0.10},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 99, MaxVRAMUsagePercent: 99},
			CPU:    types.CPULimits{MaxUsagePercent: 99},
			Memory: types.MemoryLimits{MaxUsagePercent: 99},
			Disk:   types.DiskLimits{MinFreeMB: 0},
		},
		Backends: []types.Backend{
			{ID: "b1", Name: "Backend 1", Host: "127.0.0.1", OllamaPort: 11434, MaxConcurrentReqs: 5, Status: types.StatusHealthy},
		},
	})

	// b1: модель НЕ загружена, sync load выключено
	p.UpdateMetrics("b1", &types.BackendMetrics{
		GPU:    types.GPUMetrics{UsagePercent: 10, MemoryTotal: 24576, MemoryUsed: 4096, MemoryFree: 20480},
		System: types.SystemMetrics{CPUUsagePercent: 5, MemoryTotal: 65536, MemoryUsed: 8192, MemoryFree: 57344},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}, MaxModels: 10, MaxConcurrentRequests: 5, OllamaAvailable: true},
	})

	// Загружаем b1 полностью
	p.backends["b1"].mu.Lock()
	p.backends["b1"].ActiveReqs = 5
	p.backends["b1"].mu.Unlock()

	req := &QueuedRequest{Model: "llama3.1:8b"}
	result := p.dispatchRequest(req)

	if result.Error == nil {
		t.Error("dispatchRequest() expected error for no available backend")
	}
	if result.BackendID != "" {
		t.Errorf("dispatchRequest() backend = %v, want empty", result.BackendID)
	}
}

func TestWaitForModelReady(t *testing.T) {
	p := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			QueueMaxSize:         10,
			QueueWorkers:         2,
			QueueTimeout:         60,
			ResourceReservation:  types.ResourceReservationConfig{GPUHeadroomPercent: 0.10},
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 99, MaxVRAMUsagePercent: 99},
			CPU:    types.CPULimits{MaxUsagePercent: 99},
			Memory: types.MemoryLimits{MaxUsagePercent: 99},
			Disk:   types.DiskLimits{MinFreeMB: 0},
		},
		Backends: []types.Backend{
			{ID: "b1", Name: "Backend 1", Host: "127.0.0.1", OllamaPort: 11434, MaxConcurrentReqs: 5, Status: types.StatusHealthy},
		},
	})

	// Модель не загружена — таймаут
	t.Run("timeout", func(t *testing.T) {
		p.UpdateMetrics("b1", &types.BackendMetrics{
			Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}},
		})
		start := time.Now()
		got := p.waitForModelReady("b1", "missing-model", 100*time.Millisecond)
		elapsed := time.Since(start)
		if got {
			t.Error("waitForModelReady() = true, want false (timeout)")
		}
		if elapsed > 300*time.Millisecond {
			t.Errorf("waitForModelReady() took too long: %v", elapsed)
		}
	})

	// Модель уже загружена — мгновенный успех
	t.Run("already ready", func(t *testing.T) {
		p.UpdateMetrics("b1", &types.BackendMetrics{
			Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{{Name: "llama3.1:8b"}}},
		})
		start := time.Now()
		got := p.waitForModelReady("b1", "llama3.1:8b", 100*time.Millisecond)
		elapsed := time.Since(start)
		if !got {
			t.Error("waitForModelReady() = false, want true")
		}
		if elapsed > 50*time.Millisecond {
			t.Errorf("waitForModelReady() should be instant, took: %v", elapsed)
		}
	})
}