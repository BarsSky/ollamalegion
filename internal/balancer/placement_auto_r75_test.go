package balancer

// R75 (placement policy, этап P1.5): auto выбирает стратегию по VRAM-fit (§4).
//
// Проверяем:
//   - single, когда модель влезает хотя бы на один бэкенд;
//   - replicated, когда prefer содержит replicated и влезает на minBackends;
//   - degraded single с числами, когда не влезает никуда;
//   - degraded single с честной причиной, когда размер модели неизвестен;
//   - пропуск sharded/rpc (этап P2) в reason;
//   - группу репликации для auto-правила, предпочитающего replicated.

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// placementAutoProxyR75 — прокси с двумя healthy бэкендами, заданным размером
// модели (профиль) и свободным VRAM в МЕГАБАЙТАХ (как их отдаёт агент).
func placementAutoProxyR75(t *testing.T, model string, sizeBytes int64, freeVRAMMB uint64, rule types.PlacementModelRule) *Proxy {
	t.Helper()
	cfg := createTestConfig() // два бэкенда
	if sizeBytes > 0 {
		cfg.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
			model: {SizeBytes: sizeBytes},
		}
	}
	cfg.Balancing.Placement = types.PlacementSettings{Enabled: true, Models: []types.PlacementModelRule{rule}}
	proxy := newProxyWithCleanup(t, cfg)
	if freeVRAMMB > 0 {
		for _, b := range proxy.GetAllBackends() {
			proxy.UpdateMetrics(b.ID, &types.BackendMetrics{
				GPU: types.GPUMetrics{MemoryTotal: freeVRAMMB * 2, MemoryFree: freeVRAMMB},
			})
		}
	}
	return proxy
}

func TestPlacementAuto_SingleWhenFits_R75(t *testing.T) {
	proxy := placementAutoProxyR75(t, "m-small", 4<<30, 20*1024, types.PlacementModelRule{
		Model:    "m-small",
		Strategy: string(types.PlacementAuto),
		Auto:     &types.PlacementAutoRule{Prefer: []string{"single"}},
	})

	d := proxy.ResolvePlacement("m-small", 4, "")
	if d.Strategy != types.PlacementSingle {
		t.Fatalf("strategy = %q, ожидалось single (reason=%s)", d.Strategy, d.Reason)
	}
	if d.Degraded {
		t.Errorf("вмещается на оба бэкенда — деградации быть не должно: %s", d.Reason)
	}
	if !strings.Contains(d.Reason, "влезает на") {
		t.Errorf("reason должен объяснять VRAM-fit: %s", d.Reason)
	}
	if !d.Executable || !d.Refined {
		t.Errorf("ожидалось Executable=true Refined=true, получено %+v", d)
	}
}

func TestPlacementAuto_ReplicatedWhenPreferAndFits_R75(t *testing.T) {
	proxy := placementAutoProxyR75(t, "m-big", 12<<30, 20*1024, types.PlacementModelRule{
		Model:    "m-big",
		Strategy: string(types.PlacementAuto),
		Auto:     &types.PlacementAutoRule{Prefer: []string{"replicated", "single"}, MinBackends: 2},
	})

	d := proxy.ResolvePlacement("m-big", 12, "")
	if d.Strategy != types.PlacementReplicated {
		t.Fatalf("strategy = %q, ожидалось replicated (reason=%s)", d.Strategy, d.Reason)
	}
	if d.Degraded {
		t.Errorf("два бэкенда вмещают модель — деградации быть не должно: %s", d.Reason)
	}
	if !strings.Contains(d.Reason, "minBackends=2") {
		t.Errorf("reason должен упоминать minBackends: %s", d.Reason)
	}

	// R75: для auto-правила с prefer=replicated группа репликации создаётся
	// заранее, чтобы выбранная стратегия была подкреплена репликами.
	if proxy.modelReplication == nil || proxy.modelReplication.GetGroup("m-big") == nil {
		t.Fatal("группа репликации для auto-правила не создана")
	}
	group := proxy.modelReplication.GetGroup("m-big")
	if group.MinInstances < 2 {
		t.Errorf("minInstances = %d, для auto→replicated ожидалось ≥ 2", group.MinInstances)
	}
}

func TestPlacementAuto_DegradedWhenNothingFits_R75(t *testing.T) {
	// 20 ГБ весов (need ≈ 25 ГБ) при 4 ГБ свободного VRAM на каждом бэкенде.
	proxy := placementAutoProxyR75(t, "m-huge", 20<<30, 4*1024, types.PlacementModelRule{
		Model:    "m-huge",
		Strategy: string(types.PlacementAuto),
		Auto:     &types.PlacementAutoRule{Prefer: []string{"replicated", "single"}, MinBackends: 2},
	})

	d := proxy.ResolvePlacement("m-huge", 20, "")
	if d.Strategy != types.PlacementSingle {
		t.Fatalf("strategy = %q, ожидалось single (деградация)", d.Strategy)
	}
	if !d.Degraded {
		t.Error("ни один бэкенд не вмещает модель — ожидался degraded")
	}
	if !strings.Contains(d.Reason, "не вмещает") || !strings.Contains(d.Reason, "ГБ") {
		t.Errorf("в reason должны быть числа (need/свободно): %s", d.Reason)
	}
}

func TestPlacementAuto_UnknownSizeSingleWithNote_R75(t *testing.T) {
	// Размер модели неизвестен (нет профиля и метрик загруженной модели):
	// VRAM-fit проверить нельзя — выбираем single (дефолт) и сообщаем об этом в
	// причине; деградацией это не считаем (нет данных, а не «не влезает»).
	proxy := placementAutoProxyR75(t, "m-unknown", 0, 20*1024, types.PlacementModelRule{
		Model:    "m-unknown",
		Strategy: string(types.PlacementAuto),
	})

	d := proxy.ResolvePlacement("m-unknown", 0, "")
	if d.Strategy != types.PlacementSingle {
		t.Fatalf("ожидался single, получено %+v", d)
	}
	if !strings.Contains(d.Reason, "размер модели неизвестен") {
		t.Errorf("reason должен объяснять, что размер неизвестен: %s", d.Reason)
	}
	if d.Degraded {
		t.Errorf("без данных о размере это не деградация: %s", d.Reason)
	}
}

func TestPlacementAuto_ShardedSkipped_R75(t *testing.T) {
	proxy := placementAutoProxyR75(t, "m-any", 4<<30, 20*1024, types.PlacementModelRule{
		Model:    "m-any",
		Strategy: string(types.PlacementAuto),
		Auto:     &types.PlacementAutoRule{Prefer: []string{"sharded", "single"}},
	})

	d := proxy.ResolvePlacement("m-any", 4, "")
	if d.Strategy != types.PlacementSingle {
		t.Fatalf("strategy = %q, ожидалось single (sharded — этап P2)", d.Strategy)
	}
	if !strings.Contains(d.Reason, "P2") {
		t.Errorf("reason должен сообщать, что sharded пропущен до P2: %s", d.Reason)
	}
}

func TestPlacementAuto_LoadedModelFits_R75(t *testing.T) {
	// Модель уже загружена на бэкенде — влезает по определению, даже если
	// метрик свободного VRAM нет (getModelLoadedCtxFromMetrics > 0).
	cfg := createTestConfig()
	cfg.Balancing.Placement = types.PlacementSettings{
		Enabled: true,
		Models: []types.PlacementModelRule{{
			Model:    "m-loaded",
			Strategy: string(types.PlacementAuto),
			Auto:     &types.PlacementAutoRule{Prefer: []string{"single"}},
		}},
	}
	proxy := newProxyWithCleanup(t, cfg)
	backends := proxy.GetAllBackends()
	if len(backends) == 0 {
		t.Fatal("нет бэкендов")
	}
	proxy.UpdateMetrics(backends[0].ID, &types.BackendMetrics{
		GPU: types.GPUMetrics{MemoryTotal: 8 << 30, MemoryFree: 1 << 30}, // свободного VRAM мало
	})
	// Модель числится загруженной в llama.cpp-метриках бэкенда: R75 считает,
	// что она «влезает по определению», даже если свободного VRAM мало.
	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.llamaMetrics[backends[0].ID] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name: "m-loaded", State: "loaded", ContextLength: 4096,
		}},
	}
	proxy.metricsMgr.mu.Unlock()

	d := proxy.ResolvePlacement("m-loaded", 0, "")
	if d.Strategy != types.PlacementSingle {
		t.Fatalf("strategy = %q, ожидалось single", d.Strategy)
	}
	if d.Degraded {
		t.Errorf("модель загружена на бэкенде — влезает, деградации быть не должно: %s", d.Reason)
	}
}
