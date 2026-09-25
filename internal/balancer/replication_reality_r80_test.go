package balancer

// R80: replicated исполняется по-настоящему — группа видит реально загруженные
// копии, а VRAM-fit не объявляет деградацию, когда копии уже в VRAM.
//
// Найдено нагрузочным стендом P4 (R79): при двух загруженных копиях и почти
// заполненной VRAM запросы отклонялись 503-й («подходящих бэкендов 0»), а
// группа репликации оставалась пустой (трафик уходил в одну копию).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// replicatedLoadedProxyR80 — прокси с N бэкендами-заглушками, у которых в
// метриках отмечена загруженная модель и почти занятая VRAM.
func replicatedLoadedProxyR80(t *testing.T, backends int, freeVRAMMB uint64) (*Proxy, *atomic.Int32) {
	return replicatedStubProxyR80(t, backends, freeVRAMMB, true)
}

// replicatedStubProxyR80 — то же, но с управляемой отметкой «модель загружена».
func replicatedStubProxyR80(t *testing.T, backends int, freeVRAMMB uint64, modelLoaded bool) (*Proxy, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"r80-model","message":{"role":"assistant","content":"ok"},"done":true}`))
	}))
	t.Cleanup(stub.Close)
	_, port, _ := parseBackendHostPort(strings.TrimPrefix(stub.URL, "http://"))

	cfg := createTestConfig()
	cfg.Balancing.OperatingMode = string(types.OperatingModeStandard)
	cfg.Balancing.ModelReplication.Enabled = false
	cfg.Balancing.Placement = types.PlacementSettings{
		Enabled: true,
		Models: []types.PlacementModelRule{{
			Model:    "r80-model",
			Strategy: string(types.PlacementReplicated),
			Reason:   "R80: две копии уже загружены",
		}},
	}
	if backends < len(cfg.Backends) {
		cfg.Backends = cfg.Backends[:backends]
	}
	for i := range cfg.Backends {
		cfg.Backends[i].Host = "127.0.0.1"
		cfg.Backends[i].OllamaPort = port
		cfg.Backends[i].Status = types.StatusHealthy
		cfg.Backends[i].MaxConcurrentReqs = 1
	}
	proxy := newProxyWithCleanup(t, cfg)
	for _, b := range proxy.GetAllBackends() {
		metrics := &types.BackendMetrics{
			GPU: types.GPUMetrics{MemoryTotal: 8192, MemoryFree: freeVRAMMB},
		}
		if modelLoaded {
			metrics.LlamaCpp = types.LlamaCppMetrics{
				LoadedModels: []types.LlamaCppModel{{
					Name:          "r80-model",
					ContextLength: 4096,
					State:         string(types.ModelStateLoaded),
				}},
			}
		}
		proxy.UpdateMetrics(b.ID, metrics)
	}
	return proxy, &calls
}

func TestReplicationRealityR80_LoadedCopiesAreAdopted(t *testing.T) {
	proxy, calls := replicatedLoadedProxyR80(t, 2, 300)

	// Две реально загруженные копии → в группе два инстанса, догрузки нет.
	proxy.ensurePlacementInstances()
	group := proxy.modelReplication.GetGroup("r80-model")
	if group == nil {
		t.Fatal("группа репликации не создана политикой")
	}
	candidates := proxy.replicationSelector.GetGroupCandidates("r80-model")
	if len(candidates) != 2 {
		t.Fatalf("кандидатов в группе %d (%v), ожидалось 2 (усыновлённые копии)", len(candidates), candidates)
	}

	// replicate-стратегия без деградации: копии уже в VRAM, свободного мало.
	d := proxy.ResolvePlacement("r80-model", 8, "")
	if d.Degraded {
		t.Fatalf("решение деградировало при двух загруженных копиях: %s", d.Reason)
	}
	if _, reject := proxy.placementRejection(d); reject {
		t.Fatalf("запрос отклонён политикой, хотя обе копии загружены: %s", d.Reason)
	}

	// И запросы действительно идут (в юнит-тесте — в заглушку).
	rec, header := postChatPlacementR76(proxy, "r80-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200: %s", rec.Code, rec.Body.String())
	}
	if calls.Load() == 0 {
		t.Error("запрос не дошёл до бэкенда")
	}
	if got := header.Get("X-LB-Placement"); got != string(types.PlacementReplicated) {
		t.Errorf("X-LB-Placement = %q, ожидалось replicated", got)
	}
}

func TestReplicationRealityR80_LowVRAMWithoutLoadedModelStillDegrades(t *testing.T) {
	// Обратная сторона: копий нет и VRAM не хватает — деградация остаётся
	// (R79-поведение не сломано).
	proxy, _ := replicatedStubProxyR80(t, 2, 300, false)
	proxy.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"r80-model": {SizeBytes: 8 << 30},
	}

	d := proxy.ResolvePlacement("r80-model", 8, "")
	if !d.Degraded {
		t.Fatalf("ожидалась деградация (копий нет, VRAM мало): %s", d.Reason)
	}
	rec, _ := postChatPlacementR76(proxy, "r80-model")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d, ожидался 503 (fallback=error): %s", rec.Code, rec.Body.String())
	}
}

func TestReplicationRealityR80_BackendsWithModelLoaded(t *testing.T) {
	proxy, _ := replicatedLoadedProxyR80(t, 2, 300)
	ids := proxy.backendsWithModelLoaded("r80-model")
	if len(ids) != 2 {
		t.Fatalf("backendsWithModelLoaded = %v, ожидалось 2 бэкенда", ids)
	}
	if got := proxy.backendsWithModelLoaded("other-model"); len(got) != 0 {
		t.Errorf("для незагруженной модели получено %v", got)
	}
}

func TestReplicationRealityR80_DeadReplicaServesFromLiveOne(t *testing.T) {
	// §6, строка «один из бэкендов реплик unhealthy»: падение второй копии —
	// не повод отказывать клиенту, пока жива первая (иначе HA-смысл репликации
	// теряется: одна копия упала → 503 на всё).
	proxy, _ := replicatedLoadedProxyR80(t, 2, 300)

	backends := proxy.GetAllBackends()
	if len(backends) != 2 {
		t.Fatalf("бэкендов %d, ожидалось 2", len(backends))
	}
	proxy.mu.Lock()
	proxy.backends[backends[1].ID].Backend.Status = types.StatusUnhealthy
	proxy.mu.Unlock()

	d := proxy.ResolvePlacement("r80-model", 8, "")
	if d.Degraded {
		t.Fatalf("решение деградировало при живой копии: %s", d.Reason)
	}
	if _, reject := proxy.placementRejection(d); reject {
		t.Fatalf("запрос отклонён, хотя одна копия жива: %s", d.Reason)
	}
	if !strings.Contains(d.Reason, "готовых копий") {
		t.Errorf("в причине нет пометки о снижении избыточности: %s", d.Reason)
	}
	rec, _ := postChatPlacementR76(proxy, "r80-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200 с живой копии: %s", rec.Code, rec.Body.String())
	}
}
