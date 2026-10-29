package balancer

// placement_over_reload_r293_test.go — R293: размещение модели на свободном узле
// вместо перезагрузки того, где она загружена.
//
// Живой случай со второго стенда: два узла, на первом идёт стрим агента, второй
// пуст и файл модели у него есть. Второй агент пришёл с другим окном — балансер
// перезагрузил модель на первом узле и оборвал стрим первого агента
// («stream ended by upstream before completion»). Тесты фиксируют правило:
// уходить на другой узел только когда есть причина, и только если узел реально
// готов принять модель.

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

const placementTestModel = "gemma-4-E4B-it-Q4_K_M"

// newPlacementTestProxy — два узла: current (модель загружена) и alt (пуст).
func newPlacementTestProxy(t *testing.T) *Proxy {
	t.Helper()
	p := &Proxy{
		backends: map[string]*BackendState{
			"node-current": {
				Backend: &types.Backend{
					ID: "node-current", Host: "10.0.0.1", MaxConcurrentReqs: 1,
					Weight: 1, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp,
				},
			},
			"node-alt": {
				Backend: &types.Backend{
					ID: "node-alt", Host: "10.0.0.2", MaxConcurrentReqs: 1,
					Weight: 1, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp,
				},
			},
		},
		metricsMgr:   NewMetricsManager(),
		modelCatalog: newModelCatalog(),
		queueMgr:     NewQueueManager(nil, 10, 1, 30),
		config:       &types.LoadBalancerConfig{},
		statePath:    t.TempDir() + "/state.json",
	}
	// Узел current: модель загружена с окном 8192.
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["node-current"] = &types.BackendMetrics{
		ID:  "node-current",
		GPU: types.GPUMetrics{MemoryTotal: 8192, MemoryUsed: 6000, MemoryFree: 2192},
		Ollama: types.OllamaMetrics{
			MaxConcurrentRequests: 1,
		},
	}
	p.metricsMgr.llamaMetrics["node-current"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: placementTestModel, ContextLength: 8192, ContextPerSeq: 8192}},
	}
	// Узел alt: пуст, но VRAM свободна и потолок модели известен.
	p.metricsMgr.metrics["node-alt"] = &types.BackendMetrics{
		ID:  "node-alt",
		GPU: types.GPUMetrics{MemoryTotal: 16384, MemoryUsed: 1000, MemoryFree: 15384},
		Ollama: types.OllamaMetrics{
			MaxConcurrentRequests: 1,
		},
	}
	p.metricsMgr.llamaMetrics["node-alt"] = &types.LlamaCppMetrics{
		GGUFMaxContext:  32768,
		VramKnown:       true,
		AvailableVRAMMB: 15384,
	}
	p.metricsMgr.mu.Unlock()

	// Файл модели есть на обоих узлах (каталог опрошен).
	p.modelCatalog.store("node-current", []string{placementTestModel}, "", time.Now())
	p.modelCatalog.store("node-alt", []string{placementTestModel}, "", time.Now())
	return p
}

// TestR293_PlacementPrefersFreeNodeWhenCurrentIsStreaming — главный живой случай.
func TestR293_PlacementPrefersFreeNodeWhenCurrentIsStreaming(t *testing.T) {
	p := newPlacementTestProxy(t)

	stop := beginClientStreamR271(p, "node-current", placementTestModel)
	defer stop()

	alt, reason := p.findPlacementOverReloadR293(placementTestModel, 16384, "node-current")
	if alt != "node-alt" {
		t.Fatalf("размещение = %q (reason=%q), want node-alt: перезагрузка под живым стримом рвёт чужой запрос", alt, reason)
	}
	if reason == "" {
		t.Error("причина переезда не заполнена — оператор не увидит, почему уехало")
	}
}

// TestR293_NoPlacementWhenCurrentIsIdle — свободный узел дешевле перезагрузить.
func TestR293_NoPlacementWhenCurrentIsIdle(t *testing.T) {
	p := newPlacementTestProxy(t)

	if alt, reason := p.findPlacementOverReloadR293(placementTestModel, 16384, "node-current"); alt != "" {
		t.Fatalf("без причины уехали на %q (reason=%q), ожидалось «ничего» (перезагрузка на месте)", alt, reason)
	}
}

// TestR293_NoPlacementWithoutFileOnDisk — узел без файла модели не кандидат.
func TestR293_NoPlacementWithoutFileOnDisk(t *testing.T) {
	p := newPlacementTestProxy(t)
	p.modelCatalog = newModelCatalog() // каталог пуст: «знаем, что файла нет»
	stop := beginClientStreamR271(p, "node-current", placementTestModel)
	defer stop()

	if alt, _ := p.findPlacementOverReloadR293(placementTestModel, 16384, "node-current"); alt != "" {
		t.Fatalf("выбрали %q, хотя файла модели там нет (получили бы 503 model_not_found)", alt)
	}
}

// TestR293_NoPlacementWhenCandidateIsStreaming — чужой стрим на кандидате тоже стоп.
func TestR293_NoPlacementWhenCandidateIsStreaming(t *testing.T) {
	p := newPlacementTestProxy(t)
	stopCurrent := beginClientStreamR271(p, "node-current", placementTestModel)
	defer stopCurrent()
	stopAlt := beginClientStreamR271(p, "node-alt", placementTestModel)
	defer stopAlt()

	if alt, _ := p.findPlacementOverReloadR293(placementTestModel, 16384, "node-current"); alt != "" {
		t.Fatalf("выбрали %q, где уже идёт стрим — загрузка его и оборвёт", alt)
	}
}

// TestR293_NoPlacementWhenVRAMInsufficient — не хватает VRAM: не переезжаем.
func TestR293_NoPlacementWhenVRAMInsufficient(t *testing.T) {
	p := newPlacementTestProxy(t)
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["node-alt"].GPU = types.GPUMetrics{MemoryTotal: 4096, MemoryUsed: 3800, MemoryFree: 296}
	p.metricsMgr.mu.Unlock()
	stop := beginClientStreamR271(p, "node-current", placementTestModel)
	defer stop()

	if alt, _ := p.findPlacementOverReloadR293(placementTestModel, 16384, "node-current"); alt != "" {
		t.Fatalf("выбрали %q при свободных 296 МБ VRAM", alt)
	}
}

// TestR293_PlacementWhenCurrentCannotFitWindow — узел физически не вмещает окно.
func TestR293_PlacementWhenCurrentCannotFitWindow(t *testing.T) {
	p := newPlacementTestProxy(t)
	p.metricsMgr.mu.Lock()
	// Потолок текущего узла — 8192: окно 16384 он обслужить не сможет.
	p.metricsMgr.llamaMetrics["node-current"].GGUFMaxContext = 8192
	p.metricsMgr.mu.Unlock()

	alt, reason := p.findPlacementOverReloadR293(placementTestModel, 16384, "node-current")
	if alt != "node-alt" {
		t.Fatalf("размещение = %q (reason=%q), want node-alt: текущий узел окно не вмещает", alt, reason)
	}
}

// TestR293_FlagOffRestoresPreviousBehaviour — LB_PLACE_OVER_RELOAD=0.
func TestR293_FlagOffRestoresPreviousBehaviour(t *testing.T) {
	t.Setenv("LB_PLACE_OVER_RELOAD", "0")
	p := newPlacementTestProxy(t)
	stop := beginClientStreamR271(p, "node-current", placementTestModel)
	defer stop()

	if alt, _ := p.findPlacementOverReloadR293(placementTestModel, 16384, "node-current"); alt != "" {
		t.Fatalf("при выключенном флаге всё равно переехали на %q", alt)
	}
}

// TestR293_DeterministicChoice — одинаковые условия → один и тот же узел (без пинг-понга).
func TestR293_DeterministicChoice(t *testing.T) {
	p := newPlacementTestProxy(t)
	// Третий узел с такими же характеристиками: выбор должен быть стабильным.
	p.backends["node-alt2"] = &BackendState{Backend: &types.Backend{
		ID: "node-alt2", Host: "10.0.0.3", MaxConcurrentReqs: 1,
		Weight: 1, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp,
	}}
	p.metricsMgr.mu.Lock()
	p.metricsMgr.metrics["node-alt2"] = &types.BackendMetrics{
		ID: "node-alt2", GPU: types.GPUMetrics{MemoryTotal: 16384, MemoryUsed: 1000, MemoryFree: 15384},
	}
	p.metricsMgr.mu.Unlock()
	p.modelCatalog.store("node-alt2", []string{placementTestModel}, "", time.Now())

	stop := beginClientStreamR271(p, "node-current", placementTestModel)
	defer stop()

	first, _ := p.findPlacementOverReloadR293(placementTestModel, 16384, "node-current")
	for i := 0; i < 5; i++ {
		again, _ := p.findPlacementOverReloadR293(placementTestModel, 16384, "node-current")
		if again != first {
			t.Fatalf("выбор недетерминирован: %q vs %q", first, again)
		}
	}
}
