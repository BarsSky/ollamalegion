package balancer

// placement_replication_free_r379_test.go — R379 (2026-11-01, P2-2).
//
// Тесты фиксируют две ошибки прежнего правила «свободен = ActiveReqs == 0»:
//  1. бэкенд с нулём запросов считался свободным, даже когда модель на него
//     физически не помещается (чужая модель заняла VRAM) — репликация падала;
//  2. бэкенд с одним запросом считался занятым, хотя VRAM свободна — репликация
//     туда не шла никогда.

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// newReplicationFreeProxyR379 — прокси с N здоровыми llama.cpp-бэкендами и без
// групп репликации: тест проверяет только выбор «куда можно грузить копию».
func newReplicationFreeProxyR379(t *testing.T, ids ...string) *Proxy {
	t.Helper()
	backends := make([]types.Backend, 0, len(ids))
	for _, id := range ids {
		backends = append(backends, types.Backend{
			ID:                id,
			Name:              "test " + id,
			Host:              "127.0.0.1",
			CppWorkerPort:     18092,
			Type:              types.BackendTypeLlamaCpp,
			Weight:            1,
			Status:            types.StatusHealthy,
			MaxConcurrentReqs: 4,
		})
	}
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends:     backends,
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			HealthCheckInterval: 60,
			MetricsInterval:     60,
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false},
	}
	return newProxyWithCleanup(t, cfg)
}

// setBackendVRAMR379 — метрики VRAM бэкенда (значения в МБ, как у cppworker).
func setBackendVRAMR379(p *Proxy, id string, totalMB, freeMB uint64) {
	p.UpdateMetrics(id, &types.BackendMetrics{
		GPU: types.GPUMetrics{MemoryTotal: totalMB, MemoryFree: freeMB},
	})
}

// setBackendActiveReqsR379 — счётчик активных запросов бэкенда.
func setBackendActiveReqsR379(p *Proxy, id string, n int) {
	p.mu.RLock()
	state := p.backends[id]
	p.mu.RUnlock()
	if state == nil {
		return
	}
	state.mu.Lock()
	state.ActiveReqs = n
	state.mu.Unlock()
}

// TestR379_FreeBackend_MemoryNotActiveReqs — «свободен» решает память.
func TestR379_FreeBackend_MemoryNotActiveReqs(t *testing.T) {
	const model = "r379-model"
	restore := setModelSizeBytesForTest(func(name string) int64 {
		if name == model {
			return 4 << 30 // 4 GiB → копии нужно 5 GiB с запасом 1.25
		}
		return 0
	})
	defer restore()

	p := newReplicationFreeProxyR379(t, "b1-idle-fit", "b2-idle-no-room", "b3-busy-fit")

	// b1: простаивает, VRAM пуста — помещается.
	setBackendVRAMR379(p, "b1-idle-fit", 8192, 8192)
	// b2: простаивает, но свободного VRAM 1 ГБ — копия НЕ помещается. Раньше
	// такой бэкенд считался свободным только из-за ActiveReqs == 0.
	setBackendVRAMR379(p, "b2-idle-no-room", 8192, 1024)
	// b3: два активных запроса, но VRAM пуста — помещается. Раньше такой бэкенд
	// исключался из репликации навсегда.
	setBackendVRAMR379(p, "b3-busy-fit", 8192, 8192)
	setBackendActiveReqsR379(p, "b3-busy-fit", 2)

	got := p.freeBackendsForReplicationR379(model, nil)
	want := []string{"b1-idle-fit", "b3-busy-fit"} // простаивающие первыми
	if len(got) != len(want) {
		t.Fatalf("кандидаты репликации = %v, ожидались %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("кандидаты репликации = %v, ожидался порядок %v", got, want)
		}
	}
}

// TestR379_FreeBackend_AlreadyLoadedCounts — если копия уже загружена, свободной
// памяти может не быть: реплика уже на месте, и бэкенд обязан остаться
// кандидатом (иначе группа выгрузит копию и загрузит её снова).
func TestR379_FreeBackend_AlreadyLoadedCounts(t *testing.T) {
	const model = "r379-loaded"
	restore := setModelSizeBytesForTest(func(string) int64 { return 4 << 30 })
	defer restore()

	p := newReplicationFreeProxyR379(t, "b1-loaded")

	p.UpdateMetrics("b1-loaded", &types.BackendMetrics{
		GPU:      types.GPUMetrics{MemoryTotal: 8192, MemoryFree: 256},
		LlamaCpp: types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{Name: model}}},
	})

	got := p.freeBackendsForReplicationR379(model, nil)
	if len(got) != 1 || got[0] != "b1-loaded" {
		t.Fatalf("бэкенд с загруженной копией обязан быть кандидатом, got %v", got)
	}
}

// TestR379_FreeBackend_RespectsTargets — список целей группы по-прежнему
// ограничивает выбор: реплика не должна появиться вне разрешённых бэкендов.
func TestR379_FreeBackend_RespectsTargets(t *testing.T) {
	const model = "r379-targets"
	restore := setModelSizeBytesForTest(func(string) int64 { return 1 << 30 })
	defer restore()

	p := newReplicationFreeProxyR379(t, "b1", "b2")
	setBackendVRAMR379(p, "b1", 8192, 8192)
	setBackendVRAMR379(p, "b2", 8192, 8192)

	got := p.freeBackendsForReplicationR379(model, []string{"b2"})
	if len(got) != 1 || got[0] != "b2" {
		t.Fatalf("кандидаты = %v, ожидался только разрешённый b2", got)
	}
}
