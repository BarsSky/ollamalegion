package api

// R78 (P3): сводка placement policy в GET /api/v1/metrics.
//
// Оператор видит в общих метриках, что политика активна и не деградирует молча:
// configured/enabled/fallback/operatingMode, счётчики по стратегиям,
// degraded/notExecutable, предупреждения и готовность репликации.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func getMetricsJSONR78(t *testing.T, s *Server) map[string]interface{} {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/metrics: ожидался 200, получен %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("некорректный JSON метрик: %v", err)
	}
	return out
}

func TestMetricsPlacementSummary_R78(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "b1", Host: "localhost", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()

	// Политика не настроена — блок есть, но помечен как ненастроенный.
	metrics := getMetricsJSONR78(t, s)
	placement, ok := metrics["placement"].(map[string]interface{})
	if !ok {
		t.Fatalf("в метриках нет блока placement: %T", metrics["placement"])
	}
	if placement["configured"] != false {
		t.Errorf("configured = %v, ожидалось false", placement["configured"])
	}

	// Включаем политику: auto (degraded — размер неизвестен/не влезает) + sharded (P2).
	s.proxy.SetPlacementSettings(types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackError,
		Models: []types.PlacementModelRule{
			{
				Model:    "m-auto",
				Strategy: string(types.PlacementAuto),
				Auto:     &types.PlacementAutoRule{Prefer: []string{"replicated", "single"}, MinBackends: 2},
			},
			{Model: "m-sharded", Strategy: string(types.PlacementSharded)},
		},
	})

	metrics = getMetricsJSONR78(t, s)
	placement, ok = metrics["placement"].(map[string]interface{})
	if !ok {
		t.Fatalf("в метриках нет блока placement: %T", metrics["placement"])
	}
	if placement["configured"] != true || placement["enabled"] != true {
		t.Errorf("configured/enabled = %v/%v, ожидалось true/true",
			placement["configured"], placement["enabled"])
	}
	if placement["fallback"] != "error" {
		t.Errorf("fallback = %v, ожидалось error", placement["fallback"])
	}
	if placement["operatingMode"] != "standard" {
		t.Errorf("operatingMode = %v, ожидалось standard", placement["operatingMode"])
	}
	if placement["rules"] != float64(2) {
		t.Errorf("rules = %v, ожидалось 2", placement["rules"])
	}
	if placement["notExecutable"] != float64(1) {
		t.Errorf("notExecutable = %v, ожидалось 1 (m-sharded — этап P2)", placement["notExecutable"])
	}
	if placement["warnings"] == nil {
		t.Error("warnings должен присутствовать (даже 0)")
	}
	if _, ok := placement["byStrategy"].(map[string]interface{}); !ok {
		t.Errorf("byStrategy отсутствует или не объект: %T", placement["byStrategy"])
	}
}
