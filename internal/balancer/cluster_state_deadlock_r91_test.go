package balancer

import (
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// cluster_state_deadlock_r91_test.go — R91 (2026-10-08): регрессия на дедлок,
// из-за которого при нагрузке НАВСЕГДА отваливалась вся admin-плоскость.
//
// ЧТО БЫЛО. GetClusterState() брал p.metricsMgr.mu.RLock() и держал его до конца
// функции, а внутри цикла по бэкендам вызывал SnapshotBackendMetrics/
// SnapshotLlamaCppMetrics — то есть брал ТОТ ЖЕ sync.RWMutex на чтение ПОВТОРНО.
// Документация sync.RWMutex прямо запрещает такой рекурсивный RLock: если между
// первым и вторым RLock встанет писатель, второй RLock не получит блокировку
// никогда — писатель ждёт первого читателя, а все новые читатели ждут писателя.
//
// Живое следствие (нагрузочный прогон): писателей у metricsMgr.mu много и они
// частые (llamaCppMetricsPoller каждые ~2 с, heartbeat агента,
// updateRunningModelInMetrics). При нагрузке дедлок наступал за секунды и не
// отпускал: GET /api/v1/metrics, /api/v1/backends, /api/v1/cluster,
// /api/v1/predictions не отвечали больше 13 минут (подтверждено дампом горутин по
// SIGQUIT: горутина ждала metricsMgr.mu.Lock 13 минут, читатели стояли на
// cluster_state.go:23), а metricsPublishLoop перестал публиковать состояние —
// панель оператора теряла кластер именно тогда, когда он был нужен.
//
// ТЕСТ. Держим рядом писателя (Lock/Unlock в цикле) и читателя
// (GetClusterState в цикле). С рекурсивным RLock читатель обязан встать навсегда;
// без него — проходит за миллисекунды. Поэтому проверка идёт с таймаутом, иначе
// регрессия выглядела бы как «тест висит», а не как внятный провал.

// TestGetClusterState_NoDeadlockWithConcurrentMetricsWriter — главная проверка.
func TestGetClusterState_NoDeadlockWithConcurrentMetricsWriter(t *testing.T) {
	p := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
	})
	if err := p.AddBackend(types.Backend{
		ID:            "bk-1",
		Host:          "127.0.0.1",
		Type:          types.BackendTypeLlamaCpp,
		CppWorkerPort: 18092,
		Status:        types.StatusHealthy,
	}); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	// Метрики от агента и отдельный срез llama.cpp: GetClusterState вызывает
	// SnapshotBackendMetrics и SnapshotLlamaCppMetrics — ровно те два места, где
	// раньше брался второй RLock.
	p.SetBackendMetrics("bk-1", &types.BackendMetrics{
		ID:       "bk-1",
		GPU:      types.GPUMetrics{MemoryTotal: 8192, MemoryUsed: 1024},
		System:   types.SystemMetrics{MemoryTotal: 24576, MemoryUsed: 2048},
		LlamaCpp: types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{Name: "gemma-4-E4B-it-Q4_K_M"}}},
	})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Писатель. Именно он «встаёт в очередь» между двумя рекурсивными RLock —
	// без него рекурсивный RLock по счастливой случайности проходит.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			p.metricsMgr.mu.Lock()
			p.metricsMgr.mu.Unlock()
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			if state := p.GetClusterState(); state == nil || len(state.Backends) != 1 {
				// Не проверка корректности (ею заняты другие тесты), а признак
				// того, что функция дошла до конца и вернула данные.
				return
			}
		}
	}()

	select {
	case <-done:
		// Успех: ни одного вечного ожидания.
	case <-time.After(15 * time.Second):
		close(stop)
		t.Fatal("GetClusterState() зависла при одновременной записи в metricsMgr.mu: " +
			"вернулся рекурсивный RLock на одном и том же sync.RWMutex (внешний RLock " +
			"в GetClusterState + RLock внутри SnapshotBackendMetrics/SnapshotLlamaCppMetrics). " +
			"Держите только внутренние блокировки: каждый Snapshot* берёт RLock сам")
	}

	close(stop)
	wg.Wait()
}
