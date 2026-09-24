package balancer

// R76 (placement policy, этап P3): §6 плана — «никогда не уходить в другую
// стратегию молча» + requireHomogeneous.
//
//   - при `fallback=error` (default) запрос, который нельзя обслужить заявленной
//     стратегией (sharded/rpc — этап P2; auto degraded без allowDegraded),
//     получает явную 503 с причиной и блоком placement;
//   - при `fallback=single` запрос обслуживается обычным путём, а деградация
//     видна в заголовке X-LB-Placement-Fallback;
//   - `auto.allowDegraded=true` разрешает обслужить деградировавшее решение;
//   - `auto.requireHomogeneous` сужает набор бэкендов до одной группы с
//     одинаковой «личностью» GPU (метка gpu:*/sm_*, иначе объём VRAM).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// placementRejectProxyR76 — прокси с 2 бэкендами-заглушками (upstream отвечает
// 200) и заданной политикой; возвращает и счётчик обращений к upstream.
func placementRejectProxyR76(t *testing.T, placement types.PlacementSettings) (*Proxy, *int32) {
	t.Helper()
	var upstreamCalls int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"ok"},"done":true}`))
	}))
	t.Cleanup(stub.Close)

	_, port, _ := parseBackendHostPort(strings.TrimPrefix(stub.URL, "http://"))
	cfg := createTestConfig()
	cfg.Backends = []types.Backend{{
		ID: "r76-1", Name: "stub", Host: "127.0.0.1", OllamaPort: port,
		Type: types.BackendTypeOllama, Status: types.StatusHealthy,
		MaxConcurrentReqs: 1, Weight: 1, Labels: []string{"gpu:sm_86"},
	}}
	cfg.Balancing.Placement = placement
	proxy := newProxyWithCleanup(t, cfg)
	proxy.UpdateMetrics("r76-1", &types.BackendMetrics{
		GPU: types.GPUMetrics{MemoryTotal: 24 * 1024, MemoryFree: 20 * 1024},
	})
	return proxy, &upstreamCalls
}

func postChatPlacementR76(proxy *Proxy, model string) (*httptest.ResponseRecorder, http.Header) {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	return rec, rec.Result().Header
}

func TestPlacementP3_RejectsShardedWithFallbackError_R76(t *testing.T) {
	proxy, upstream := placementRejectProxyR76(t, types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackError,
		Models: []types.PlacementModelRule{{
			Model:    "m-sharded",
			Strategy: string(types.PlacementSharded),
			Reason:   "R76: раскладка ещё не исполняется",
		}},
	})

	rec, header := postChatPlacementR76(proxy, "m-sharded")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d, ожидался 503 (fallback=error, sharded не исполняется): %s",
			rec.Code, rec.Body.String())
	}
	if *upstream != 0 {
		t.Errorf("upstream получил %d запрос(ов): отказ по политике должен происходить ДО маршрутизации", *upstream)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, rec.Body.String())
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "не исполняется") || !strings.Contains(msg, "P2") {
		t.Errorf("в ошибке должно быть объяснение про этап P2: %s", msg)
	}
	placement, ok := body["placement"].(map[string]interface{})
	if !ok {
		t.Fatalf("в ответе нет блока placement: %v", body)
	}
	if placement["strategy"] != string(types.PlacementSharded) {
		t.Errorf("placement.strategy = %v, ожидалось sharded", placement["strategy"])
	}
	if placement["executable"] != false {
		t.Errorf("placement.executable = %v, ожидалось false", placement["executable"])
	}
	if header.Get("X-LB-Placement") != "" {
		t.Errorf("при отказе заголовок X-LB-Placement не нужен, получено %q", header.Get("X-LB-Placement"))
	}
}

func TestPlacementP3_FallbackSingleServesWithHeader_R76(t *testing.T) {
	proxy, upstream := placementRejectProxyR76(t, types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackSingle,
		Models: []types.PlacementModelRule{{
			Model:    "m-sharded",
			Strategy: string(types.PlacementSharded),
		}},
	})

	rec, header := postChatPlacementR76(proxy, "m-sharded")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200 (fallback=single): %s", rec.Code, rec.Body.String())
	}
	if *upstream == 0 {
		t.Error("запрос не дошёл до upstream — fallback=single должен обслужить обычным путём")
	}
	if got := header.Get("X-LB-Placement"); got != string(types.PlacementSingle) {
		t.Errorf("X-LB-Placement = %q, ожидалось single (фактически исполненная стратегия)", got)
	}
	if got := header.Get("X-LB-Placement-Fallback"); got != string(types.PlacementSharded) {
		t.Errorf("X-LB-Placement-Fallback = %q, ожидалось sharded (заявленная)", got)
	}
}

func TestPlacementP3_AllowDegradedServes_R76(t *testing.T) {
	// auto не может выбрать (модель не влезает) и правило разрешает деградацию.
	proxy, upstream := placementRejectProxyR76(t, types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackError,
		Models: []types.PlacementModelRule{{
			Model:    "m-big",
			Strategy: string(types.PlacementAuto),
			Auto: &types.PlacementAutoRule{
				Prefer:        []string{"replicated", "single"},
				MinBackends:   2,
				AllowDegraded: true,
			},
		}},
	})
	// Профиль: 40 ГБ весов → need ≈ 50 ГБ, свободно 20 ГБ.
	proxy.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"m-big": {SizeBytes: 40 << 30},
	}

	rec, header := postChatPlacementR76(proxy, "m-big")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200 (allowDegraded=true): %s", rec.Code, rec.Body.String())
	}
	if *upstream == 0 {
		t.Error("запрос не дошёл до upstream")
	}
	if got := header.Get("X-LB-Placement"); got != string(types.PlacementSingle) {
		t.Errorf("X-LB-Placement = %q, ожидалось single (деградация разрешена)", got)
	}
}

func TestPlacementP3_RejectsDegradedWithoutAllowDegraded_R76(t *testing.T) {
	proxy, _ := placementRejectProxyR76(t, types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackError,
		Models: []types.PlacementModelRule{{
			Model:    "m-big",
			Strategy: string(types.PlacementAuto),
			Auto:     &types.PlacementAutoRule{Prefer: []string{"replicated", "single"}, MinBackends: 2},
		}},
	})
	proxy.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"m-big": {SizeBytes: 40 << 30},
	}

	rec, _ := postChatPlacementR76(proxy, "m-big")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d, ожидался 503 (degraded без allowDegraded, fallback=error)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "auto не смогла выбрать") {
		t.Errorf("в ошибке должна быть причина деградации: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ГБ") {
		t.Errorf("в ошибке должны быть числа (need/свободно): %s", rec.Body.String())
	}
}

func TestPlacementP3_RequireHomogeneous_R76(t *testing.T) {
	// Два бэкенда с разными «личностями» GPU (sm_86 и sm_90), модель влезает на
	// оба, правило требует однородность и minBackends=2 → репликация невозможна.
	var upstream int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream++
		_, _ = w.Write([]byte(`{"done":true}`))
	}))
	defer stub.Close()
	_, port, _ := parseBackendHostPort(strings.TrimPrefix(stub.URL, "http://"))

	cfg := createTestConfig()
	cfg.Backends = []types.Backend{
		{ID: "h1", Host: "127.0.0.1", OllamaPort: port, Type: types.BackendTypeOllama,
			Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1, Labels: []string{"gpu:sm_86"}},
		{ID: "h2", Host: "127.0.0.1", OllamaPort: port, Type: types.BackendTypeOllama,
			Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1, Labels: []string{"gpu:sm_90"}},
	}
	cfg.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{"m-hom": {SizeBytes: 8 << 30}}
	cfg.Balancing.Placement = types.PlacementSettings{
		Enabled: true,
		Models: []types.PlacementModelRule{{
			Model:    "m-hom",
			Strategy: string(types.PlacementAuto),
			Auto: &types.PlacementAutoRule{
				Prefer:             []string{"replicated", "single"},
				MinBackends:        2,
				RequireHomogeneous: true,
			},
		}},
	}
	proxy := newProxyWithCleanup(t, cfg)
	for _, id := range []string{"h1", "h2"} {
		proxy.UpdateMetrics(id, &types.BackendMetrics{
			GPU: types.GPUMetrics{MemoryTotal: 24 * 1024, MemoryFree: 20 * 1024},
		})
	}

	d := proxy.ResolvePlacement("m-hom", 8, "")
	if d.Strategy != types.PlacementSingle {
		t.Fatalf("strategy = %q, ожидалось single: одинаковых GPU всего 1 на группу (reason=%s)",
			d.Strategy, d.Reason)
	}
	if !strings.Contains(d.Reason, "однородность") {
		t.Errorf("reason должен объяснять ограничение однородности: %s", d.Reason)
	}

	// Тот же набор, но требование однородности выключено → replicated.
	proxy.SetPlacementSettings(types.PlacementSettings{
		Enabled: true,
		Models: []types.PlacementModelRule{{
			Model:    "m-hom",
			Strategy: string(types.PlacementAuto),
			Auto:     &types.PlacementAutoRule{Prefer: []string{"replicated", "single"}, MinBackends: 2},
		}},
	})
	d = proxy.ResolvePlacement("m-hom", 8, "")
	if d.Strategy != types.PlacementReplicated {
		t.Errorf("без requireHomogeneous strategy = %q, ожидалось replicated (reason=%s)",
			d.Strategy, d.Reason)
	}
}

func TestPlacementP3_RejectionDisabledWhenPolicyOff_R76(t *testing.T) {
	// Политика выключена (default) — никаких отказов, обычный путь.
	proxy, upstream := placementRejectProxyR76(t, types.PlacementSettings{})

	rec, header := postChatPlacementR76(proxy, "any-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200 при выключенной политике", rec.Code)
	}
	if *upstream == 0 {
		t.Error("запрос не дошёл до upstream при выключенной политике")
	}
	if got := header.Get("X-LB-Placement-Source"); got != string(PlacementSourceDisabled) {
		t.Errorf("X-LB-Placement-Source = %q, ожидалось disabled", got)
	}
}
