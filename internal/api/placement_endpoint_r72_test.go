package api

// R72 (P0): GET /api/v1/placement — наблюдаемость placement-политики.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func placementTestConfigR72() types.PlacementSettings {
	return types.PlacementSettings{
		Enabled:              true,
		AllowRequestOverride: true,
		Models: []types.PlacementModelRule{
			{Model: "gemma-4*", Strategy: "single", Reason: "влезает в 8 ГБ VRAM"},
			{Model: "Qwen3*", Strategy: "sharded", Reason: "не влезает в один бэкенд"},
		},
	}
}

func getPlacementJSON(t *testing.T, s *Server, query string) map[string]interface{} {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/placement"+query, nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/placement%s: ожидался 200, получен %d: %s", query, rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("некорректный JSON: %v", err)
	}
	return out
}

func TestPlacementEndpoint_Summary_R72(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "b1", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()
	s.proxy.SetPlacementSettings(placementTestConfigR72())

	resp := getPlacementJSON(t, s, "")

	if resp["success"] != true {
		t.Errorf("success = %v", resp["success"])
	}
	if resp["enabled"] != true {
		t.Errorf("enabled = %v, ожидалось true", resp["enabled"])
	}
	if resp["operatingMode"] == "" {
		t.Error("operatingMode не должен быть пустым")
	}
	decisions, ok := resp["decisions"].([]interface{})
	if !ok {
		t.Fatalf("decisions отсутствует или не массив: %T", resp["decisions"])
	}
	// Глобальный дефолт "*" + две описанные модели.
	if len(decisions) != 3 {
		t.Errorf("decisions = %d, ожидалось 3", len(decisions))
	}
	first := decisions[0].(map[string]interface{})
	if first["model"] != "*" {
		t.Errorf("первое решение должно быть глобальным дефолтом, получено %v", first["model"])
	}
	if first["source"] != "global" {
		t.Errorf("source дефолта = %v, ожидалось global", first["source"])
	}

	// Неисполнимые стратегии перечислены отдельно — оператор видит границу P2.
	exec, ok := resp["executable"].([]interface{})
	if !ok {
		t.Fatalf("executable отсутствует: %T", resp["executable"])
	}
	found := map[string]bool{}
	for _, v := range exec {
		found[v.(string)] = true
	}
	if !found["single"] || !found["replicated"] {
		t.Errorf("executable = %v, ожидались single/replicated", exec)
	}
	if found["sharded"] {
		t.Error("sharded пока не исполняется (P2) — не должен попадать в executable")
	}
}

func TestPlacementEndpoint_SingleModelDecision_R72(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "b1", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()
	s.proxy.SetPlacementSettings(placementTestConfigR72())

	resp := getPlacementJSON(t, s, "?model=Qwen3.8-27B-UD-Q4_K_M&sizeGB=16")
	decision, ok := resp["decision"].(map[string]interface{})
	if !ok {
		t.Fatalf("decision отсутствует: %T", resp["decision"])
	}
	if decision["strategy"] != "sharded" {
		t.Errorf("strategy = %v, ожидалось sharded", decision["strategy"])
	}
	if decision["source"] != "model" {
		t.Errorf("source = %v, ожидалось model", decision["source"])
	}
	if decision["executable"] != false {
		t.Errorf("executable = %v, ожидалось false (sharded — этап P2)", decision["executable"])
	}
	if decision["reason"] == "" {
		t.Error("reason не должен быть пустым")
	}

	// Override из query (тот же путь, что X-LB-Placement) учитывается,
	// потому что allowRequestOverride=true.
	resp = getPlacementJSON(t, s, "?model=gemma-4-E4B-it-Q4_K_M&strategy=replicated")
	decision = resp["decision"].(map[string]interface{})
	if decision["strategy"] != "replicated" || decision["source"] != "request" {
		t.Errorf("override не применился: strategy=%v source=%v", decision["strategy"], decision["source"])
	}
}

func TestPlacementEndpoint_DisabledPolicy_R72(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "b1", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()
	// Политика по умолчанию выключена (обратная совместимость).
	resp := getPlacementJSON(t, s, "?model=любая-модель")

	if resp["enabled"] != false {
		t.Errorf("enabled = %v, ожидалось false по умолчанию", resp["enabled"])
	}
	decision := resp["decision"].(map[string]interface{})
	if decision["source"] != "disabled" {
		t.Errorf("source = %v, ожидалось disabled", decision["source"])
	}
	if decision["strategy"] != "single" {
		t.Errorf("strategy = %v, ожидалось single (operatingMode=standard)", decision["strategy"])
	}
	if decision["executable"] != true {
		t.Errorf("executable = %v, ожидалось true", decision["executable"])
	}
}

func TestPlacementEndpoint_WarningsAndMethod_R72(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "b1", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()
	invalid := types.PlacementSettings{
		Enabled: true,
		Models:  []types.PlacementModelRule{{Model: "a", Strategy: "spread"}},
	}
	s.proxy.SetPlacementSettings(invalid)

	resp := getPlacementJSON(t, s, "")
	warnings, ok := resp["warnings"].([]interface{})
	if !ok || len(warnings) == 0 {
		t.Fatalf("ожидались предупреждения валидации, получено %v", resp["warnings"])
	}

	// POST не поддерживается.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/placement", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/v1/placement: ожидался 405, получен %d", rec.Code)
	}
}
