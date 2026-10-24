package balancer

// R92 (S3 плана 2026-10-07): исполнитель стратегии `sharded`.
//
// Acceptance criteria S3: «placement: sharded на 2 узлах отдаёт 200; при
// выключенном воркере — предсказуемый отказ/деградация, без зависаний».
//
// Здесь проверяется именно это: решение политики (готова ли группа и влезает ли
// модель В СУММУ), маршрутизация на мастера, исключение воркеров из общего
// выбора и поведение HTTP-пути при fallback=error.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

const (
	r92MasterID = "r92-master"
	r92WorkerID = "r92-worker"
)

// shardedProxyR92 — прокси с двумя узлами (мастер + воркер) и, опционально,
// логическим бэкендом из них. profileSizeGB задаёт размер модели в профиле.
func shardedProxyR92(t *testing.T, groups []types.BackendGroupConfig, freeMasterMB, freeWorkerMB int, registerWorker bool) *Proxy {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m-sharded","message":{"role":"assistant","content":"ok"},"done":true}`))
	}))
	t.Cleanup(stub.Close)
	_, port, _ := parseBackendHostPort(strings.TrimPrefix(stub.URL, "http://"))

	cfg := createTestConfig()
	cfg.Backends = []types.Backend{{
		ID: r92MasterID, Name: "master", Host: "127.0.0.1", OllamaPort: port,
		Type: types.BackendTypeOllama, Status: types.StatusHealthy,
		MaxConcurrentReqs: 1, Weight: 1, Labels: []string{"gpu:sm_86"},
	}}
	if registerWorker {
		cfg.Backends = append(cfg.Backends, types.Backend{
			ID: r92WorkerID, Name: "worker", Host: "127.0.0.1", OllamaPort: port,
			Type: types.BackendTypeOllama, Status: types.StatusHealthy,
			MaxConcurrentReqs: 1, Weight: 1, Labels: []string{"gpu:sm_86"},
		})
	}
	cfg.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"m-sharded": {SizeBytes: 8 << 30}, // 8 ГиБ весов → need = 10 ГиБ
	}
	cfg.Balancing.Placement = types.PlacementSettings{
		Enabled: true,
		Models: []types.PlacementModelRule{{
			Model:    "m-sharded",
			Strategy: string(types.PlacementSharded),
		}},
	}
	cfg.Balancing.BackendGroups = groups

	p := newProxyWithCleanup(t, cfg)
	p.UpdateMetrics(r92MasterID, &types.BackendMetrics{
		GPU: types.GPUMetrics{MemoryTotal: 8192, MemoryFree: uint64(freeMasterMB)},
	})
	if registerWorker {
		p.UpdateMetrics(r92WorkerID, &types.BackendMetrics{
			GPU: types.GPUMetrics{MemoryTotal: 8192, MemoryFree: uint64(freeWorkerMB)},
		})
	}
	return p
}

// shardedGroupR92 — логический бэкенд из двух узлов (layer split).
func shardedGroupR92() []types.BackendGroupConfig {
	return []types.BackendGroupConfig{{
		ID:       "g1",
		Model:    "m-sharded",
		Strategy: "layer",
		Members: []types.BackendGroupMemberConfig{
			{BackendID: r92MasterID, Role: "master"},
			{BackendID: r92WorkerID, Role: "worker", RPCEndpoint: "192.0.2.11:50052"},
		},
	}}
}

func TestPlacementShardedR92_ReadyGroupAnnotatesDecision(t *testing.T) {
	// Сумма свободного VRAM узлов = 12 ГиБ ≥ need 10 ГиБ → раскладка исполнима.
	proxy := shardedProxyR92(t, shardedGroupR92(), 6144, 6144, true)

	d := proxy.ResolvePlacement("m-sharded", 8, "")
	if d.Strategy != types.PlacementSharded {
		t.Fatalf("strategy = %q, ожидалось sharded", d.Strategy)
	}
	if !d.Executable {
		t.Fatalf("стратегия исполнима (R92): %+v", d)
	}
	if d.Degraded {
		t.Fatalf("группа готова и модель влезает в сумму — деградации быть не должно: %s", d.Reason)
	}
	if d.GroupID != "g1" || d.GroupMembers != 2 {
		t.Errorf("решение должно указывать группу: groupId=%q members=%d", d.GroupID, d.GroupMembers)
	}
	if !strings.Contains(d.Reason, "sharded → группа g1") {
		t.Errorf("причина должна описывать раскладку: %s", d.Reason)
	}
}

func TestPlacementShardedR92_InsufficientSumDegrades(t *testing.T) {
	// По отдельности узлы тоже малы, но главное — СУММА (4 ГиБ) меньше need.
	// Если бы fit считался по max (2 ГиБ), вывод был бы тот же — поэтому
	// проверим ещё и числа в причине.
	proxy := shardedProxyR92(t, shardedGroupR92(), 2048, 2048, true)

	d := proxy.ResolvePlacement("m-sharded", 8, "")
	if !d.Degraded {
		t.Fatalf("суммы узлов не хватает — решение обязано деградировать: %+v", d)
	}
	if d.GroupID != "" {
		t.Errorf("group id не должен выдаваться за исполнимую раскладку: %q", d.GroupID)
	}
	if !strings.Contains(d.Reason, "нужно") || !strings.Contains(d.Reason, "доступно") {
		t.Errorf("в причине должны быть ЧИСЛА (need/доступно), а не «будет медленно»: %s", d.Reason)
	}
	if !strings.Contains(d.Reason, "g1") {
		t.Errorf("причина должна называть группу: %s", d.Reason)
	}
}

func TestPlacementShardedR92_SumAcrossNodesIsEnough(t *testing.T) {
	// 6 ГиБ на узле мало для модели 8 ГиБ (need 10 ГиБ), но СУММА 12 ГиБ —
	// достаточно. Это и есть отличие раскладки от репликации, где каждая копия
	// обязана влезть целиком.
	proxy := shardedProxyR92(t, shardedGroupR92(), 6144, 6144, true)

	if d := proxy.ResolvePlacement("m-sharded", 8, ""); d.Degraded {
		t.Fatalf("раскладка должна собираться по сумме узлов: %s", d.Reason)
	}

	// Контроль: правило `replicated` при тех же метриках исполнить нельзя.
	proxy.SetPlacementSettings(types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackError,
		Models: []types.PlacementModelRule{{
			Model:    "m-sharded",
			Strategy: string(types.PlacementReplicated),
		}},
	})
	if d := proxy.ResolvePlacement("m-sharded", 8, ""); !d.Degraded {
		t.Fatalf("для репликации модель обязана влезать в КАЖДЫЙ узел целиком: %+v", d)
	}
}

func TestPlacementShardedR92_NoGroupsConfigured(t *testing.T) {
	proxy := shardedProxyR92(t, nil, 6144, 6144, true)

	d := proxy.ResolvePlacement("m-sharded", 8, "")
	if !d.Degraded {
		t.Fatalf("без backendGroups раскладку исполнять нечем: %+v", d)
	}
	if !strings.Contains(d.Reason, "логические бэкенды не настроены") {
		t.Errorf("причина должна указывать на отсутствие групп: %s", d.Reason)
	}
}

func TestPlacementShardedR92_ModelNotCoveredByAnyGroup(t *testing.T) {
	groups := shardedGroupR92()
	groups[0].Model = "другая-модель"
	proxy := shardedProxyR92(t, groups, 6144, 6144, true)

	d := proxy.ResolvePlacement("m-sharded", 8, "")
	if !d.Degraded {
		t.Fatalf("маска группы не совпала — раскладки нет: %+v", d)
	}
	if !strings.Contains(d.Reason, "не описывает") {
		t.Errorf("причина должна говорить, что модель не описана ни одной группой: %s", d.Reason)
	}
}

func TestPlacementShardedR92_MissingWorkerDegrades(t *testing.T) {
	// Воркер описан в группе, но не зарегистрирован в балансере (типичная
	// картина, пока не собран S1: rpc-server не поднят).
	proxy := shardedProxyR92(t, shardedGroupR92(), 6144, 0, false)

	d := proxy.ResolvePlacement("m-sharded", 8, "")
	if !d.Degraded {
		t.Fatalf("без воркера раскладка неполная — нужна деградация: %+v", d)
	}
	if !strings.Contains(d.Reason, "не зарегистрирован") {
		t.Errorf("причина должна называть проблему узла: %s", d.Reason)
	}
	if got := proxy.selectBackend("m-sharded", types.BackendTypeOllama); got == r92WorkerID {
		t.Error("незарегистрированный воркер не может быть целью запроса")
	}
}

func TestPlacementShardedR92_RoutesToMasterAndHidesWorkers(t *testing.T) {
	proxy := shardedProxyR92(t, shardedGroupR92(), 6144, 6144, true)

	if got := proxy.selectBackend("m-sharded", types.BackendTypeOllama); got != r92MasterID {
		t.Fatalf("selectBackend = %q, ожидалось %q (мастер логического бэкенда)", got, r92MasterID)
	}

	// Воркер не должен быть кандидатом ни в одной группе приоритетов: он держит
	// только часть слоёв, и прямой запрос на него сломается.
	for _, group := range proxy.expandCandidates("любая-модель", nil) {
		for _, id := range group.BackendIDs {
			if id == r92WorkerID {
				t.Fatalf("воркер логического бэкенда попал в кандидаты (priority=%d)", group.Priority)
			}
		}
	}

	// Модель, не покрытая группой, не маршрутизируется через неё.
	if got := proxy.selectBackend("чужая-модель", types.BackendTypeOllama); got == r92WorkerID {
		t.Error("чужая модель не должна уходить на воркера")
	}
}

func TestPlacementShardedR92_RequestServedThroughGroup(t *testing.T) {
	proxy := shardedProxyR92(t, shardedGroupR92(), 6144, 6144, true)

	rec, header := postChatPlacementR76(proxy, "m-sharded")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200 (раскладка собрана): %s", rec.Code, rec.Body.String())
	}
	if got := header.Get("X-LB-Placement"); got != string(types.PlacementSharded) {
		t.Errorf("X-LB-Placement = %q, ожидалось sharded (фактически исполненная стратегия)", got)
	}
	if got := header.Get("X-LB-Placement-Fallback"); got != "" {
		t.Errorf("подмены стратегии не было, X-LB-Placement-Fallback должен быть пуст: %q", got)
	}
}

func TestPlacementShardedR92_RequestRejectedWithoutGroup(t *testing.T) {
	// fallback=error (default): без исполнителя клиент получает 503 с причиной,
	// а не молчаливый одиночный путь.
	proxy := shardedProxyR92(t, nil, 6144, 6144, true)

	rec, _ := postChatPlacementR76(proxy, "m-sharded")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d, ожидался 503: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "логические бэкенды не настроены") {
		t.Errorf("в отказе должна быть причина: %s", rec.Body.String())
	}
}

func TestPlacementShardedR92_GroupVisibleForAPI(t *testing.T) {
	// Наблюдаемость S2: группа видна как ОДИН бэкенд — со стратегией, слотом
	// мастера и составом. Это то, что показывают /api/v1/placement и монитор.
	proxy := shardedProxyR92(t, shardedGroupR92(), 6144, 6144, true)
	st, ok := proxy.BackendGroupStateForModel("m-sharded")
	if !ok {
		t.Fatal("состояние группы должно быть доступно для API/монитора")
	}
	if st.ID != "g1" || st.Strategy != string(types.BackendGroupStrategyLayer) {
		t.Errorf("состояние группы неверно: %+v", st)
	}
	if st.Slots != 1 {
		t.Errorf("слоты группы = лимит мастера (1), получено %d", st.Slots)
	}
	if got := proxy.BackendGroupStatuses(); len(got) != 1 {
		t.Errorf("BackendGroupStatuses = %d записей, ожидалось 1", len(got))
	}
}
