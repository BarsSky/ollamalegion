package balancer

// R78 (P3): хвосты placement policy + вывод legacy-очереди из эксплуатации.
//
//   - сводка политики для GET /api/v1/metrics (PlacementMetricsSummary);
//   - пересборка раскладки при изменении состава бэкендов
//     (schedulePlacementResync → EnsureInstances);
//   - QueueManager больше не очередь: current_size отражает ожидающих в единой
//     (admission-)очереди, канала и worker'ов нет.

import (
	"context"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func TestPlacementMetricsSummary_NotConfigured_R78(t *testing.T) {
	proxy := newProxyWithCleanup(t, createTestConfig())
	summary := proxy.PlacementMetricsSummary()

	if summary["configured"] != false {
		t.Errorf("configured = %v, ожидалось false (политика не настроена)", summary["configured"])
	}
	if summary["enabled"] != false {
		t.Errorf("enabled = %v, ожидалось false", summary["enabled"])
	}
	if _, has := summary["byStrategy"]; has {
		t.Error("для ненастроенной политики сводка не должна считать стратегии")
	}
}

func TestPlacementMetricsSummary_CountsDegradedAndReplication_R78(t *testing.T) {
	cfg := createTestConfig()
	cfg.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"m-big": {SizeBytes: 40 << 30}, // не влезет: свободно мало
	}
	cfg.Balancing.Placement = types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackError,
		Models: []types.PlacementModelRule{
			{
				Model:    "m-big",
				Strategy: string(types.PlacementAuto),
				Auto:     &types.PlacementAutoRule{Prefer: []string{"replicated", "single"}, MinBackends: 2},
			},
			{Model: "m-sharded", Strategy: string(types.PlacementSharded)},
		},
	}
	proxy := newProxyWithCleanup(t, cfg)
	for _, b := range proxy.GetAllBackends() {
		proxy.UpdateMetrics(b.ID, &types.BackendMetrics{
			GPU: types.GPUMetrics{MemoryTotal: 8 * 1024, MemoryFree: 512},
		})
	}

	summary := proxy.PlacementMetricsSummary()
	if summary["configured"] != true || summary["enabled"] != true {
		t.Fatalf("сводка должна быть настроенной и включённой: %v", summary)
	}
	if summary["rules"] != 2 {
		t.Errorf("rules = %v, ожидалось 2", summary["rules"])
	}
	if summary["degraded"] != 1 {
		t.Errorf("degraded = %v, ожидалось 1 (m-big не влезает)", summary["degraded"])
	}
	if summary["notExecutable"] != 1 {
		t.Errorf("notExecutable = %v, ожидалось 1 (m-sharded — этап P2)", summary["notExecutable"])
	}
	byStrategy, ok := summary["byStrategy"].(map[string]int)
	if !ok {
		t.Fatalf("byStrategy отсутствует: %T", summary["byStrategy"])
	}
	// Глобальный дефолт «*» + два правила модели.
	if byStrategy["single"] < 2 || byStrategy["sharded"] != 1 {
		t.Errorf("byStrategy = %v, ожидалось ≥2 single и 1 sharded", byStrategy)
	}
	if summary["replicationReady"] != true {
		t.Errorf("replicationReady = %v, ожидалось true (группа m-big создаётся политикой)",
			summary["replicationReady"])
	}
	if summary["replicationGroups"] != 1 {
		t.Errorf("replicationGroups = %v, ожидалось 1", summary["replicationGroups"])
	}
}

func TestPlacementResync_NewBackendGetsReplica_R78(t *testing.T) {
	cfg := createTestConfig()
	cfg.Backends = cfg.Backends[:1] // стартуем с одного бэкенда
	cfg.Balancing.ModelReplication.Enabled = false
	cfg.Balancing.Placement = types.PlacementSettings{
		Enabled: true,
		Models: []types.PlacementModelRule{{
			Model:    "m-repl",
			Strategy: string(types.PlacementReplicated),
			Auto:     &types.PlacementAutoRule{MinBackends: 1, MaxShardCount: 3},
		}},
	}
	proxy := newProxyWithCleanup(t, cfg)

	if proxy.modelReplication == nil || proxy.modelReplication.GetGroup("m-repl") == nil {
		t.Fatal("группа репликации не создана при старте")
	}
	// Ждём появления инстанса на единственном бэкенде (warmup-горутина).
	waitFor(t, 3*time.Second, func() bool {
		return len(proxy.modelReplication.GetInstanceStates("m-repl")) >= 1
	}, "инстанс на первом бэкенде")

	// Добавляем второй бэкенд — R78 должен пересобрать раскладку (асинхронно).
	second := cfg.Backends[0]
	second.ID = "backend-r78-2"
	second.OllamaPort++
	if err := proxy.AddBackend(second); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}

	// Пересборка запускается горутиной: ждём, пока у группы появится второй инстанс.
	waitFor(t, 5*time.Second, func() bool {
		return len(proxy.modelReplication.GetInstanceStates("m-repl")) >= 2
	}, "инстанс на добавленном бэкенде")
}

func TestQueueManager_CurrentSizeFollowsAdmissionQueue_R78(t *testing.T) {
	// Один бэкенд с одним слотом (как в R73): занимаем слот, следующий запрос
	// обязан встать в единую очередь.
	proxy := newUnifiedQueueProxyR73(t, 1)
	proxy.setAdmissionWait(3 * time.Second)

	backends := proxy.GetAllBackends()
	if len(backends) == 0 {
		t.Fatal("нет бэкендов")
	}
	for _, b := range backends {
		if !proxy.tryAcquireSlot(b.ID) {
			t.Fatalf("не удалось занять слот на %s", b.ID)
		}
	}
	if waiting := proxy.admissionWaiting(); waiting != 0 {
		t.Fatalf("очередь должна быть пуста, waiting=%d", waiting)
	}

	done := make(chan struct{})
	go func() {
		_, release, _, _, _ := proxy.waitForInferenceBackend(
			context.Background(), "m-r78", types.BackendTypeOllama, "X-User-Id:r78")
		if release != nil {
			release()
		}
		close(done)
	}()

	waitFor(t, 3*time.Second, func() bool { return proxy.admissionWaiting() == 1 }, "запрос в очереди")
	if stats := proxy.GetQueueStats(); stats.CurrentSize != 1 {
		t.Errorf("CurrentSize = %d, ожидалось 1 (ожидающие единой очереди)", stats.CurrentSize)
	}

	for _, b := range backends {
		proxy.releaseSlot(b.ID)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ожидающий запрос не завершился после освобождения слотов")
	}
	waitFor(t, 3*time.Second, func() bool { return proxy.admissionWaiting() == 0 }, "очередь опустела")
}

// waitFor — ожидание условия с таймаутом (для асинхронных пересборок раскладки).
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("не дождались: %s (за %v)", what, timeout)
}
