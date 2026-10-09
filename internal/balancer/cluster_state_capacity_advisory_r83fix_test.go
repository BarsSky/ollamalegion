package balancer

import (
	"path/filepath"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// R83-fix (2026-10-09): «потенциал параллельности не раскрыт».
//
// Живой кейс: cppworker поднял модель с n_parallel=2 (RuntimeModelSlots=2), а
// статический лимит бэкенда MaxConcurrentReqs=1, и авто-привязка вместимости к
// слотам выключена (LB_CAPACITY_FROM_MODEL_SLOTS — opt-in). Балансер честно
// пропускает 1 запрос, но половина слотов воркера простаивает.
//
// WebUI обязан ПРЕДУПРЕДИТЬ оператора и предложить настройки, но не менять их
// сам. Чтобы баннер не был ложным (при включённом флаге вместимость = слотам),
// кластерный ответ отдаёт оба числа: effectiveMaxConcurrentRequests (то, что
// реально применяет резолвер) и runtimeModelSlots (факт от воркера).
func TestClusterState_CapacityAdvisoryFields_R83fix(t *testing.T) {
	p := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 8080, APIPort: 8081,
			StatePath: filepath.Join(t.TempDir(), "state.json"),
		},
		Backends: []types.Backend{{
			ID: "bk-adv", Name: "adv", Host: "127.0.0.1", CppWorkerPort: 18092,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
			MaxConcurrentReqs: 1,
			// Факт от воркера: модель загружена с двумя слотами.
			RuntimeModelSlots: 2,
		}},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmResourceAware, OperatingMode: "standard"},
	})

	b := clusterBackend(t, p, "bk-adv")
	if b.RuntimeModelSlots != 2 {
		t.Errorf("runtimeModelSlots = %d, ожидалось 2 (факт от воркера)", b.RuntimeModelSlots)
	}
	// Флаг выключен (по умолчанию): вместимость берётся из статического лимита,
	// то есть 1 из 2 слотов — именно этот разрыв и показывает WebUI.
	if b.EffectiveMaxConcurrentRequests != 1 {
		t.Errorf("effectiveMaxConcurrentRequests = %d, ожидалось 1 при выключенном LB_CAPACITY_FROM_MODEL_SLOTS",
			b.EffectiveMaxConcurrentRequests)
	}

	// Флаг включён: резолвер берёт слоты, разрыва нет — WebUI не должен
	// предупреждать (условие баннера: effective < slots).
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "true")
	b2 := clusterBackend(t, p, "bk-adv")
	if b2.EffectiveMaxConcurrentRequests != 2 {
		t.Errorf("effectiveMaxConcurrentRequests = %d, ожидалось 2 при LB_CAPACITY_FROM_MODEL_SLOTS=true",
			b2.EffectiveMaxConcurrentRequests)
	}
	if b2.RuntimeModelSlots != 2 {
		t.Errorf("runtimeModelSlots = %d, ожидалось 2 (факт от воркера не зависит от флага)", b2.RuntimeModelSlots)
	}

	// ЖИВОЙ путь: у бэкенда ЕСТЬ agent-метрики. GetClusterState перезаписывает
	// структуру целиком (`metrics = *agentMetrics`) и восстанавливает из
	// конфигурации только перечисленные поля — оба новых обязаны пережить
	// перезапись, иначе /api/v1/cluster их не отдаёт и баннер не появляется
	// (ровно это и было воспроизведено на стенде 2026-10-09).
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["bk-adv"] = &types.BackendMetrics{
		ID: "bk-adv",
		// Timestamp обязателен: снимок с нулевым временем считаётся устаревшим и
		// SnapshotBackendMetrics его не отдаёт (см. stale_agent_metrics_test.go),
		// то есть ветка merge вообще не выполнялась бы.
		Timestamp: time.Now().UTC(),
		Status:    types.StatusHealthy,
		GPU:       types.GPUMetrics{Temperature: 42},
	}
	p.metricsMgr.mu.Unlock()

	b3 := clusterBackend(t, p, "bk-adv")
	if b3.RuntimeModelSlots != 2 {
		t.Errorf("после merge agent-метрик runtimeModelSlots = %d, ожидалось 2",
			b3.RuntimeModelSlots)
	}
	if b3.EffectiveMaxConcurrentRequests != 1 {
		t.Errorf("после merge agent-метрик effectiveMaxConcurrentRequests = %d, ожидалось 1",
			b3.EffectiveMaxConcurrentRequests)
	}
	if b3.GPU.Temperature != 42 {
		t.Errorf("agent-метрики не применились (temperature = %d, ожидалось 42)", b3.GPU.Temperature)
	}
}
