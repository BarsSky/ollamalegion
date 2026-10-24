package api

// R92 (S2 плана 2026-10-07): логический бэкенд виден оператору.
//
// Acceptance criteria S2 требует, чтобы группа была видна как ОДИН бэкенд с
// узлами и их VRAM — здесь проверяются обе поверхности:
//   - GET /api/v1/placement → groups + backendGroupWarnings;
//   - GET /api/v1/backends  → groups (физические узлы остаются в backends).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func backendGroupsR92() types.BalancingSettings {
	return types.BalancingSettings{
		BackendGroups: []types.BackendGroupConfig{{
			ID:       "g1",
			Model:    "qwen3-14b*",
			Strategy: "layer",
			Members: []types.BackendGroupMemberConfig{
				{BackendID: "r92-master", Role: "master"},
				{BackendID: "r92-worker", Role: "worker", RPCEndpoint: "192.0.2.11:50052"},
			},
		}},
	}
}

func r92Server(t *testing.T) *Server {
	t.Helper()
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "r92-master", Host: "localhost", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	t.Cleanup(cleanup)
	s.proxy.SetBackendGroupsR92(backendGroupsR92())
	return s
}

func TestPlacementEndpoint_Groups_R92(t *testing.T) {
	s := r92Server(t)

	resp := getPlacementJSON(t, s, "")
	groups, ok := resp["groups"].([]interface{})
	if !ok {
		t.Fatalf("groups отсутствует или не массив: %T (%v)", resp["groups"], resp["groups"])
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, ожидалось 1", len(groups))
	}
	g, ok := groups[0].(map[string]interface{})
	if !ok {
		t.Fatalf("элемент groups не объект: %T", groups[0])
	}
	if g["id"] != "g1" || g["strategy"] != "layer" {
		t.Errorf("группа описана неверно: %v", g)
	}
	if g["masterBackendId"] != "r92-master" {
		t.Errorf("masterBackendId = %v, ожидалось r92-master", g["masterBackendId"])
	}
	// Воркер в балансере не зарегистрирован (S1 не собран) — группа обязана быть
	// degraded и НЕ usable, с причиной: иначе WebUI покажет готовую раскладку,
	// которой нет.
	if g["usable"] != false || g["degraded"] != true {
		t.Errorf("группа без воркера должна быть degraded и не usable: %v", g)
	}
	if reason, _ := g["degradeReason"].(string); reason == "" {
		t.Error("degradeReason должен объяснять, почему раскладка не собрана")
	}
	members, _ := g["members"].([]interface{})
	if len(members) != 2 {
		t.Errorf("members = %d, ожидалось 2", len(members))
	}
	if _, ok := resp["backendGroupWarnings"]; !ok {
		t.Error("backendGroupWarnings должен присутствовать (даже пустым)")
	}
}

func TestPlacementEndpoint_GroupWarningsForBrokenConfig_R92(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "r92-master", Host: "localhost", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()
	// Группу сломали: у воркера нет rpcEndpoint, то есть мастер физически не
	// сможет подключить его GPU.
	s.proxy.SetBackendGroupsR92(types.BalancingSettings{
		BackendGroups: []types.BackendGroupConfig{{
			ID: "broken", Model: "m", Strategy: "layer",
			Members: []types.BackendGroupMemberConfig{
				{BackendID: "r92-master", Role: "master"},
				{BackendID: "r92-worker", Role: "worker"},
			},
		}},
	})

	resp := getPlacementJSON(t, s, "")
	warnings, ok := resp["backendGroupWarnings"].([]interface{})
	if !ok || len(warnings) == 0 {
		t.Fatalf("ожидались предупреждения конфига групп, получено %v", resp["backendGroupWarnings"])
	}
}

func TestBackendsEndpoint_IncludesGroups_R92(t *testing.T) {
	s := r92Server(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/backends: ожидался 200, получен %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("некорректный JSON: %v", err)
	}
	// Физические узлы остаются на месте — формат backends[] не меняется.
	if _, ok := resp["backends"].([]interface{}); !ok {
		t.Fatalf("backends отсутствует: %T", resp["backends"])
	}
	groups, ok := resp["groups"].([]interface{})
	if !ok || len(groups) != 1 {
		t.Fatalf("groups = %v, ожидалась одна группа", resp["groups"])
	}
}

func TestBackendsEndpoint_NoGroupsKeyWithoutConfig_R92(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "plain", Host: "localhost", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("некорректный JSON: %v", err)
	}
	// Обратная совместимость: без backendGroups ответ не меняется вовсе.
	if _, ok := resp["groups"]; ok {
		t.Errorf("без логических бэкендов ключ groups появляться не должен: %v", resp["groups"])
	}
}
