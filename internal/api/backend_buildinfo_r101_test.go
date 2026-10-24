package api

// R101 (S1 плана 2026-10-07): версия сборки llama.cpp доезжает от узла до
// оператора и НЕ стирается heartbeat-ом.
//
// Зачем это вообще: обе стороны RPC обязаны быть собраны из одного коммита —
// протокол версионно-чувствителен, и расхождение даёт таймаут загрузки вместо
// понятной ошибки (§0.6 плана). Проверка живёт в валидаторе композиции, а он
// читает `types.Backend.BuildInfo`; значит версия обязана: (1) приниматься при
// регистрации, (2) отдаваться в /api/v1/backends, (3) переживать обновления
// статуса, которые пересобирают структуру бэкенда целиком.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func backendsListR101(t *testing.T, s *Server) []map[string]interface{} {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends?includeUnhealthy=true", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/backends: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Backends []map[string]interface{} `json:"backends"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, rec.Body.String())
	}
	return resp.Backends
}

func backendEntryR101(t *testing.T, s *Server, id string) map[string]interface{} {
	t.Helper()
	for _, entry := range backendsListR101(t, s) {
		if entry["id"] == id {
			return entry
		}
	}
	t.Fatalf("бэкенд %s не найден в /api/v1/backends", id)
	return nil
}

func TestBackendBuildInfo_AcceptedAndExposed_R101(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, nil)
	defer cleanup()

	body := []byte(`{
		"id": "bi-1", "name": "node 2", "host": "192.0.2.11",
		"cppWorkerPort": 18092, "backendType": "llama_cpp",
		"buildInfo": "gguf-v0.19.0-1369-g1d2869c6e"
	}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/backends: HTTP %d: %s", rec.Code, rec.Body.String())
	}

	entry := backendEntryR101(t, s, "bi-1")
	if entry["buildInfo"] != "gguf-v0.19.0-1369-g1d2869c6e" {
		t.Fatalf("buildInfo = %v, ожидалась версия узла", entry["buildInfo"])
	}
}

func TestBackendBuildInfo_SurvivesHeartbeat_R101(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "bi-2", Name: "node", Host: "192.0.2.11", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
		BuildInfo: "gguf-v0.19.0-1369-g1d2869c6e",
	}})
	defer cleanup()

	// Обновление статуса БЕЗ версии: путь PUT пересобирает Backend целиком, и
	// раньше любое такое обновление стирало бы версию — а с ней и проверку
	// совместимости сторон RPC.
	body := []byte(`{"weight": 50}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/backends/bi-2", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: HTTP %d: %s", rec.Code, rec.Body.String())
	}

	entry := backendEntryR101(t, s, "bi-2")
	if entry["buildInfo"] != "gguf-v0.19.0-1369-g1d2869c6e" {
		t.Errorf("версия потеряна при обновлении без поля: %v", entry["buildInfo"])
	}
	if entry["weight"] != float64(50) {
		t.Errorf("обновление не применилось: weight = %v", entry["weight"])
	}
}

func TestBackendBuildInfo_UpdatedWhenReported_R101(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "bi-3", Name: "node", Host: "192.0.2.11", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
		BuildInfo: "old-build",
	}})
	defer cleanup()

	// Пересобрали узел — он сообщает новую версию, и она обязана замениться:
	// иначе валидатор сравнивал бы устаревшие данные и пропускал расхождение.
	body := []byte(`{"buildInfo": "gguf-v0.19.0-1500-gdeadbeef"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/backends/bi-3", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: HTTP %d: %s", rec.Code, rec.Body.String())
	}

	if entry := backendEntryR101(t, s, "bi-3"); entry["buildInfo"] != "gguf-v0.19.0-1500-gdeadbeef" {
		t.Errorf("версия не обновилась: %v", entry["buildInfo"])
	}
}

func TestBuildInfoOrKeepR101(t *testing.T) {
	cases := []struct{ from, existing, want string }{
		{"new", "old", "new"},
		{"", "old", "old"},
		{"   ", "old", "old"},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := buildInfoOrKeepR101(tc.from, tc.existing); got != tc.want {
			t.Errorf("buildInfoOrKeepR101(%q, %q) = %q, ожидалось %q", tc.from, tc.existing, got, tc.want)
		}
	}
}

// TestAgentsRegister_BuildInfoReachesBackend_R101 — вторая половина находки 6
// живой приёмки 2026-10-11.
//
// В бандл-стенде бэкенд регистрирует ВСТРОЕННЫЙ АГЕНТ: Go-сторона cppworker
// выключена (CPPWORKER_REGISTER_DISABLE=true, иначе на один endpoint появлялись
// бы две записи). А поле buildInfo читал только PUT /api/v1/backends, то есть
// путь, которым в этом стеке никто не ходит. Следствие на стенде: buildInfo у
// бэкендов оставался пустым даже при живом узле, и валидатор композиции не мог
// сверить версии сторон RPC — выдавал version_unknown («совместимость не
// подтверждена»), хотя узел версию знал и сообщал.
func TestAgentsRegister_BuildInfoReachesBackend_R101(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, nil)
	defer cleanup()

	register := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("регистрация агента: HTTP %d: %s", rec.Code, rec.Body.String())
		}
	}

	register(`{"agentId": "agent-bi", "name": "node 2", "host": "192.0.2.12",
		"backendType": "llama_cpp", "cppWorkerPort": 18092,
		"buildInfo": "gguf-v0.19.0-1369-g1d2869c6e"}`)

	if entry := backendEntryR101(t, s, "agent-bi"); entry["buildInfo"] != "gguf-v0.19.0-1369-g1d2869c6e" {
		t.Fatalf("buildInfo = %v, ожидалось значение из регистрации агента", entry["buildInfo"])
	}

	// Повторная регистрация БЕЗ поля не имеет права стирать известную версию:
	// узел, который ещё не обновился, не должен обнулять то, что уже знаем.
	register(`{"agentId": "agent-bi", "name": "node 2", "host": "192.0.2.12",
		"backendType": "llama_cpp", "cppWorkerPort": 18092}`)

	if entry := backendEntryR101(t, s, "agent-bi"); entry["buildInfo"] != "gguf-v0.19.0-1369-g1d2869c6e" {
		t.Fatalf("buildInfo после повторной регистрации без поля = %v, ожидалось прежнее", entry["buildInfo"])
	}
}
