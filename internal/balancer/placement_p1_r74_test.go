package balancer

// R74 (P1, placement policy): исполнение стратегий pool/replicated/auto.
//
// План: plans/2026-09-23-multi-backend-placement-policy.md, этап P1.
// P0 (R72) только резолвил стратегию и показывал её оператору; R74 подключает
// исполнение:
//   - pool       → тот же VirtualRouter, что и operatingMode=virtual_router,
//                  но включается политикой для конкретной модели;
//   - replicated → группа репликации создаётся по политике, дальше работает
//                  штатный replicationSelector (без operatingMode=replication);
//   - auto       → детерминированное подмножество §4 (алиас → pool,
//                  prefer=replicated + minBackends → replicated, иначе single).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/virtualmodel"
	"ollama-loadbalancer/pkg/types"
)

// poolStubR74 — заглушка бэкенда, запоминающая имена моделей, которые доехали.
// Порядок полей — под fieldalignment (указатель, слайс, счётчик).
type poolStubR74 struct {
	server *httptest.Server
	models []string
	calls  atomic.Int64
}

func newPoolStubR74(t *testing.T) *poolStubR74 {
	t.Helper()
	stub := &poolStubR74{}
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &env)
		<-mu
		stub.models = append(stub.models, env.Model)
		mu <- struct{}{}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"model":  env.Model,
			"answer": "stub",
			"done":   true,
		})
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *poolStubR74) hostPort() string {
	return strings.TrimPrefix(s.server.URL, "http://")
}

// setupPoolPolicyR74 — прокси с двумя заглушками в пуле виртуальной модели,
// включённой placement-политикой (operatingMode остаётся standard).
func setupPoolPolicyR74(t *testing.T, policyEnabled bool) (*Proxy, *poolStubR74, *poolStubR74) {
	t.Helper()
	stub1, stub2 := newPoolStubR74(t), newPoolStubR74(t)

	cfg := createE2EConfig()
	cfg.Balancing.OperatingMode = string(types.OperatingModeStandard)
	if policyEnabled {
		cfg.Balancing.Placement = types.PlacementSettings{
			Enabled: true,
			Models: []types.PlacementModelRule{{
				Model:    "virtual:r74",
				Strategy: string(types.PlacementPool),
				Reason:   "R74: алиас обслуживается пулом из двух бэкендов",
				Pool:     []string{stub1.hostPort(), stub2.hostPort()},
			}},
		}
	}
	proxy := newProxyWithCleanup(t, cfg)

	registry := proxy.GetVirtualModelRegistry()
	if registry == nil {
		t.Fatal("registry виртуальных моделей не создан (virtualModels.enabled)")
	}
	registry.SetEnabled(true)
	if err := registry.Register(types.VirtualModelConfig{
		Name:        "virtual:r74",
		Description: "R74 pool test",
		Selection:   virtualmodel.SelectionRoundRobin,
		BackendPool: []string{stub1.hostPort(), stub2.hostPort()},
		ModelName:   "r74-physical",
	}); err != nil {
		t.Fatalf("register virtual model: %v", err)
	}
	proxy.SetVirtualRouter(NewVirtualRouter(registry, proxy))
	return proxy, stub1, stub2
}

func postChatR74(proxy *Proxy, model string) (*httptest.ResponseRecorder, http.Header) {
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":false}`, model)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	return rec, rec.Result().Header
}

func TestPlacementP1_PoolFromPolicy_R74(t *testing.T) {
	proxy, stub1, stub2 := setupPoolPolicyR74(t, true)

	rec, header := postChatR74(proxy, "virtual:r74")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200 (pool по политике): %s", rec.Code, rec.Body.String())
	}
	if got := header.Get("X-LB-Placement"); got != string(types.PlacementPool) {
		t.Errorf("X-LB-Placement = %q, ожидалось %q (источник)", got, types.PlacementPool)
	}
	if got := header.Get("X-LB-Placement-Source"); got != string(PlacementSourceModel) {
		t.Errorf("X-LB-Placement-Source = %q, ожидалось %q", got, PlacementSourceModel)
	}
	// Алиас должен быть переписан на физическую модель пула.
	total := stub1.calls.Load() + stub2.calls.Load()
	if total == 0 {
		t.Fatal("ни один бэкенд пула не получил запрос")
	}
	for _, stub := range []*poolStubR74{stub1, stub2} {
		for _, m := range stub.models {
			if m != "r74-physical" {
				t.Errorf("на бэкенд пула пришла модель %q, ожидалась физическая %q", m, "r74-physical")
			}
		}
	}
}

func TestPlacementP1_PoolNotEnabledWithoutPolicy_R74(t *testing.T) {
	// operatingMode=standard и политики нет → pool не включается: алиас уходит
	// стандартным путём (поведение до R74 сохраняется).
	proxy, stub1, stub2 := setupPoolPolicyR74(t, false)

	rec, header := postChatR74(proxy, "virtual:r74")
	if header.Get("X-LB-Placement") == string(types.PlacementPool) {
		t.Error("pool включился без политики и без operatingMode=virtual_router")
	}
	if rec.Code == http.StatusOK {
		t.Errorf("алиас обслужился стандартным путём (HTTP %d) — ожидалось, что пул не задействован", rec.Code)
	}
	if stub1.calls.Load()+stub2.calls.Load() != 0 {
		t.Error("бэкенды пула получили запрос, хотя pool не разрешён")
	}
}

func TestPlacementP1_ReplicationGroupFromPolicy_R74(t *testing.T) {
	cfg := createTestConfig()
	cfg.Balancing.ModelReplication.Enabled = false // поднимаем по требованию политики
	cfg.Balancing.Placement = types.PlacementSettings{
		Enabled: true,
		Models: []types.PlacementModelRule{{
			Model:    "repl-model",
			Strategy: string(types.PlacementReplicated),
			Reason:   "R74: держим реплики на двух бэкендах",
			Auto:     &types.PlacementAutoRule{MinBackends: 2, MaxShardCount: 2},
		}},
	}
	proxy := newProxyWithCleanup(t, cfg)

	if proxy.modelReplication == nil || proxy.replicationSelector == nil {
		t.Fatal("менеджер репликации не поднят по требованию политики")
	}
	group := proxy.modelReplication.GetGroup("repl-model")
	if group == nil {
		t.Fatal("группа репликации для repl-model не создана политикой")
	}

	// Идемпотентность: повторная синхронизация не создаёт вторую группу.
	before := len(proxy.modelReplication.GetGroups())
	if actions := proxy.syncPlacementReplicationGroups(); len(actions) != 0 {
		t.Errorf("повторная синхронизация вернула действия: %v", actions)
	}
	if after := len(proxy.modelReplication.GetGroups()); after != before {
		t.Errorf("число групп изменилось: %d → %d", before, after)
	}

	// Штатный селектор узнаёт модель как групповую (реплицируемую).
	if !proxy.replicationSelector.IsGroupModel("repl-model") {
		t.Fatal("replicationSelector не считает repl-model групповой моделью")
	}

	// Выбор бэкенда для групповой модели должен остаться валидным (либо
	// реплика группы, либо обычный путь, если инстансы ещё грузятся: в
	// юнит-тесте backend-заглушки warmup не обслуживают).
	selected := proxy.selectBackend("repl-model", "", true)
	if selected == "" {
		t.Fatal("selectBackend не выбрал бэкенд для групповой модели")
	}
	known := map[string]bool{}
	for _, b := range proxy.GetAllBackends() {
		known[b.ID] = true
	}
	if !known[selected] {
		t.Errorf("selectBackend вернул неизвестный бэкенд %q", selected)
	}

	// Наблюдаемость для GET /api/v1/placement.
	status := proxy.PlacementReplicationStatus()
	if status["managerReady"] != true {
		t.Errorf("managerReady = %v, ожидалось true", status["managerReady"])
	}
	groups, ok := status["groups"].(map[string]interface{})
	if !ok {
		t.Fatalf("groups отсутствует: %T", status["groups"])
	}
	entry, ok := groups["repl-model"].(map[string]interface{})
	if !ok {
		t.Fatalf("нет записи о repl-model: %v", groups)
	}
	if entry["hasGroup"] != true {
		t.Errorf("hasGroup = %v, ожидалось true", entry["hasGroup"])
	}
}

func TestPlacementP1_AutoRefinement_R74(t *testing.T) {
	// 1) auto для виртуального алиаса → pool.
	proxy, _, _ := setupPoolPolicyR74(t, false)
	proxy.SetPlacementSettings(types.PlacementSettings{
		Enabled: true,
		Models: []types.PlacementModelRule{{
			Model:    "virtual:r74",
			Strategy: string(types.PlacementAuto),
			Reason:   "R74: auto для алиаса",
		}},
	})
	d := proxy.ResolvePlacement("virtual:r74", 1, "")
	if d.Strategy != types.PlacementPool {
		t.Errorf("auto для алиаса → %q, ожидалось pool (reason=%s)", d.Strategy, d.Reason)
	}
	if !d.Refined {
		t.Error("Refined должен быть true для auto")
	}

	// 2) auto с prefer=replicated и достаточным VRAM на двух бэкендах →
	// replicated (R75: выбор теперь по VRAM-fit, поэтому задаём размер модели
	// профилем и свободный VRAM метриками).
	cfg := createTestConfig() // два healthy бэкенда
	cfg.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"big-model": {SizeBytes: 8 << 30}, // 8 ГБ весов → need ≈ 10 ГБ
	}
	cfg.Balancing.Placement = types.PlacementSettings{
		Enabled: true,
		Models: []types.PlacementModelRule{{
			Model:    "big-model",
			Strategy: string(types.PlacementAuto),
			Auto:     &types.PlacementAutoRule{Prefer: []string{"replicated", "single"}, MinBackends: 2},
		}},
	}
	proxy2 := newProxyWithCleanup(t, cfg)
	for _, b := range proxy2.GetAllBackends() {
		proxy2.UpdateMetrics(b.ID, &types.BackendMetrics{
			GPU: types.GPUMetrics{MemoryTotal: 24 << 30, MemoryFree: 20 << 30},
		})
	}
	d = proxy2.ResolvePlacement("big-model", 30, "")
	if d.Strategy != types.PlacementReplicated {
		t.Errorf("auto с prefer=replicated → %q, ожидалось replicated (reason=%s)", d.Strategy, d.Reason)
	}
	if d.Degraded {
		t.Errorf("решение не должно быть degraded: %s", d.Reason)
	}

	// 3) auto без условий и без данных о размере модели → single с пометкой в
	// причине (R75: VRAM-fit проверить нельзя, деградацией это не считаем).
	proxy2.SetPlacementSettings(types.PlacementSettings{
		Enabled: true,
		Models: []types.PlacementModelRule{{
			Model:    "small-model",
			Strategy: string(types.PlacementAuto),
		}},
	})
	d = proxy2.ResolvePlacement("small-model", 2, "")
	if d.Strategy != types.PlacementSingle {
		t.Errorf("auto без условий → %q, ожидалось single", d.Strategy)
	}
	if !strings.Contains(d.Reason, "размер модели неизвестен") {
		t.Errorf("в reason должно быть указано, что размер модели неизвестен: %s", d.Reason)
	}
	if !d.Executable {
		t.Error("single исполним — Executable должен быть true")
	}
}

// Проверяем, что маски имён не создают группы (нужно точное имя) и попадают в
// отчёт как пропущенные.
func TestPlacementP1_ReplicationMaskSkipped_R74(t *testing.T) {
	cfg := createTestConfig()
	cfg.Balancing.ModelReplication.Enabled = false
	cfg.Balancing.Placement = types.PlacementSettings{
		Enabled: true,
		Models: []types.PlacementModelRule{{
			Model:    "Qwen3*",
			Strategy: string(types.PlacementReplicated),
		}},
	}
	proxy := newProxyWithCleanup(t, cfg)
	actions := proxy.syncPlacementReplicationGroups()
	found := false
	for _, a := range actions {
		if strings.Contains(a, "маска") {
			found = true
		}
	}
	if !found {
		t.Errorf("ожидалось сообщение о пропуске маски, получено: %v", actions)
	}
	if proxy.modelReplication != nil && proxy.modelReplication.GetGroup("Qwen3*") != nil {
		t.Error("группа не должна создаваться для маски")
	}
	_ = time.Second
}
