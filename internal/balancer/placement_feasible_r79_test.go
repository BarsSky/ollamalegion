package balancer

// R79 (хвост P3 §6 + подготовка P4): исполнимость ЯВНОЙ стратегии.
//
// До R79 `strategy: replicated` не проверялась вообще: при одном подходящем
// бэкенде группа репликации создавалась с minInstances=1, ответ отдавала одна
// копия, а X-LB-Placement рапортовал `replicated` — тихий уход в другую
// стратегию, который §6 плана запрещает. Здесь проверяем:
//   - fallback=error → 503 с числами (сколько подходящих бэкендов, сколько нужно);
//   - fallback=single → 200, X-LB-Placement: single, X-LB-Placement-Fallback: replicated;
//   - auto.allowDegraded=true → 200 и решение видно в /api/v1/placement как degraded;
//   - деградации нет, когда подходящих бэкендов хватает (2 копии влезают);
//   - глобальный дефолт (operatingMode=replication на одном бэкенде) не
//     затрагивается — обратная совместимость;
//   - группа репликации по явному replicated без auto.minBackends создаётся с
//     minInstances=2.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// replicatedPolicyProxyR79 — прокси с backendsCount бэкендами-заглушками
// (upstream 200), политикой replicated для модели и метриками VRAM.
func replicatedPolicyProxyR79(t *testing.T, backendsCount int, policy types.PlacementSettings, freeVRAMMB uint64) *Proxy {
	t.Helper()
	var upstreamCalls atomic.Int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"r79-model","message":{"role":"assistant","content":"ok"},"done":true}`))
	}))
	t.Cleanup(stub.Close)
	_, port, _ := parseBackendHostPort(strings.TrimPrefix(stub.URL, "http://"))

	cfg := createTestConfig()
	cfg.Balancing.OperatingMode = string(types.OperatingModeStandard)
	cfg.Balancing.ModelReplication.Enabled = false
	cfg.Balancing.Placement = policy

	// Оставляем ровно backendsCount бэкендов и направляем их в заглушку.
	if backendsCount < len(cfg.Backends) {
		cfg.Backends = cfg.Backends[:backendsCount]
	}
	for i := range cfg.Backends {
		cfg.Backends[i].Host = "127.0.0.1"
		cfg.Backends[i].OllamaPort = port
		cfg.Backends[i].Status = types.StatusHealthy
		cfg.Backends[i].MaxConcurrentReqs = 1
	}
	proxy := newProxyWithCleanup(t, cfg)
	for _, b := range proxy.GetAllBackends() {
		proxy.UpdateMetrics(b.ID, &types.BackendMetrics{
			GPU: types.GPUMetrics{MemoryTotal: 24 * 1024, MemoryFree: freeVRAMMB},
		})
	}
	return proxy
}

func replicatedRuleR79(model string, minBackends int, allowDegraded bool) types.PlacementSettings {
	rule := types.PlacementModelRule{
		Model:    model,
		Strategy: string(types.PlacementReplicated),
		Reason:   "R79: две копии модели",
	}
	if minBackends > 0 || allowDegraded {
		rule.Auto = &types.PlacementAutoRule{MinBackends: minBackends, AllowDegraded: allowDegraded}
	}
	return types.PlacementSettings{Enabled: true, Fallback: types.PlacementFallbackError, Models: []types.PlacementModelRule{rule}}
}

func TestPlacementFeasible_R79_ReplicatedOneBackendRejected(t *testing.T) {
	// Модель влезает (8 ГБ весов, свободно 20 ГБ), но бэкенд всего один:
	// replicated исполнить нельзя — при fallback=error это 503, не тихая
	// выдача одной копией.
	proxy := replicatedPolicyProxyR79(t, 1, replicatedRuleR79("r79-model", 0, false), 20*1024)

	d := proxy.ResolvePlacement("r79-model", 0, "")
	if d.Strategy != types.PlacementReplicated {
		t.Fatalf("strategy = %q, ожидалось replicated", d.Strategy)
	}
	if !d.Degraded {
		t.Fatalf("решение не помечено degraded: %s", d.Reason)
	}
	if !strings.Contains(d.Reason, "подходящих бэкендов 1") || !strings.Contains(d.Reason, "нужно 2") {
		t.Errorf("в причине нет чисел о бэкендах: %s", d.Reason)
	}

	rec, header := postChatPlacementR76(proxy, "r79-model")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d, ожидался 503 (replicated без второго бэкенда, fallback=error): %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "replicated") {
		t.Errorf("в ошибке нет стратегии: %s", rec.Body.String())
	}
	if header.Get("X-LB-Placement") != "" {
		t.Errorf("при отказе заголовок X-LB-Placement не нужен, получено %q", header.Get("X-LB-Placement"))
	}
}

func TestPlacementFeasible_R79_ReplicatedFallbackSingleServes(t *testing.T) {
	policy := replicatedRuleR79("r79-model", 0, false)
	policy.Fallback = types.PlacementFallbackSingle
	proxy := replicatedPolicyProxyR79(t, 1, policy, 20*1024)

	rec, header := postChatPlacementR76(proxy, "r79-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200 (fallback=single): %s", rec.Code, rec.Body.String())
	}
	// Фактически исполнила одна копия — заголовок не должен врать.
	if got := header.Get("X-LB-Placement"); got != string(types.PlacementSingle) {
		t.Errorf("X-LB-Placement = %q, ожидалось single (исполнено одной копией)", got)
	}
	if got := header.Get("X-LB-Placement-Fallback"); got != string(types.PlacementReplicated) {
		t.Errorf("X-LB-Placement-Fallback = %q, ожидалось replicated (заявлено политикой)", got)
	}
}

func TestPlacementFeasible_R79_AllowDegradedServes(t *testing.T) {
	proxy := replicatedPolicyProxyR79(t, 1, replicatedRuleR79("r79-model", 0, true), 20*1024)

	rec, header := postChatPlacementR76(proxy, "r79-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200 (auto.allowDegraded=true): %s", rec.Code, rec.Body.String())
	}
	if got := header.Get("X-LB-Placement"); got != string(types.PlacementSingle) {
		t.Errorf("X-LB-Placement = %q, ожидалось single", got)
	}
	// Деградация видна оператору в метриках, а не только в логе.
	if got := proxy.PlacementMetricsSummary()["degraded"]; got != 1 {
		t.Errorf("placement.degraded = %v, ожидалось 1", got)
	}
}

func TestPlacementFeasible_R79_ReplicatedTwoBackendsNoDegradation(t *testing.T) {
	proxy := replicatedPolicyProxyR79(t, 2, replicatedRuleR79("r79-model", 0, false), 20*1024)

	d := proxy.ResolvePlacement("r79-model", 0, "")
	if d.Strategy != types.PlacementReplicated || d.Degraded {
		t.Fatalf("strategy=%q degraded=%v, ожидалось replicated без деградации (reason=%s)",
			d.Strategy, d.Degraded, d.Reason)
	}
	rec, header := postChatPlacementR76(proxy, "r79-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200: %s", rec.Code, rec.Body.String())
	}
	if got := header.Get("X-LB-Placement"); got != string(types.PlacementReplicated) {
		t.Errorf("X-LB-Placement = %q, ожидалось replicated", got)
	}
	if got := header.Get("X-LB-Placement-Fallback"); got != "" {
		t.Errorf("деградации нет — заголовок fallback не нужен, получено %q", got)
	}
}

func TestPlacementFeasible_R79_VRAMFitCounts(t *testing.T) {
	// Два бэкенда, но свободно по 1 ГБ: 8 ГБ весов × 1.25 = 10 ГБ на копию —
	// не влезает ни на один, значит копий 0 < 2 → degraded с числами.
	proxy := replicatedPolicyProxyR79(t, 2, replicatedRuleR79("r79-model", 0, false), 1024)
	proxy.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"r79-model": {SizeBytes: 8 << 30},
	}

	d := proxy.ResolvePlacement("r79-model", 8, "")
	if !d.Degraded {
		t.Fatalf("решение не degraded при нехватке VRAM: %s", d.Reason)
	}
	if !strings.Contains(d.Reason, "подходящих бэкендов 0") {
		t.Errorf("в причине нет числа подходящих бэкендов: %s", d.Reason)
	}
	if !strings.Contains(d.Reason, "ГБ") {
		t.Errorf("в причине нет объёмов (need/свободно): %s", d.Reason)
	}
}

func TestPlacementFeasible_R79_GlobalOperatingModeUntouched(t *testing.T) {
	// Обратная совместимость: operatingMode=replication на одном бэкенде —
	// легальная конфигурация до политики, политика её не трогает.
	cfg := createTestConfig()
	cfg.Backends = cfg.Backends[:1]
	cfg.Backends[0].Status = types.StatusHealthy
	cfg.Balancing.OperatingMode = string(types.OperatingModeReplication)
	cfg.Balancing.Placement = types.PlacementSettings{} // политика выключена
	proxy := newProxyWithCleanup(t, cfg)

	d := proxy.ResolvePlacement("r79-model", 0, "")
	if d.Strategy != types.PlacementReplicated {
		t.Fatalf("глобальная стратегия = %q, ожидалось replicated (из operatingMode)", d.Strategy)
	}
	if d.Degraded {
		t.Errorf("глобальный дефолт не должен деградировать: %s", d.Reason)
	}
	if _, reject := proxy.placementRejection(d); reject {
		t.Error("глобальный дефолт не должен отклоняться политикой")
	}
}

func TestPlacementReplication_R79_ExplicitReplicatedGroupsTwoInstances(t *testing.T) {
	proxy := replicatedPolicyProxyR79(t, 2, replicatedRuleR79("r79-model", 0, false), 20*1024)

	group := proxy.modelReplication.GetGroup("r79-model")
	if group == nil {
		t.Fatal("группа репликации для явного replicated не создана")
	}
	if group.MinInstances != 2 {
		t.Errorf("minInstances = %d, ожидалось 2 (replicated = минимум две копии)", group.MinInstances)
	}
	if group.MaxInstances < group.MinInstances {
		t.Errorf("maxInstances = %d < minInstances = %d", group.MaxInstances, group.MinInstances)
	}
	// Явный auto.minBackends у replicated — операторский выбор, не переопределяем.
	policy := replicatedRuleR79("r79-model", 1, false)
	proxy2 := replicatedPolicyProxyR79(t, 2, policy, 20*1024)
	group2 := proxy2.modelReplication.GetGroup("r79-model")
	if group2 == nil {
		t.Fatal("группа репликации не создана (auto.minBackends=1)")
	}
	if group2.MinInstances != 1 {
		t.Errorf("minInstances = %d, ожидалось 1 (явный auto.minBackends)", group2.MinInstances)
	}
}
