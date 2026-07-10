package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// Round 13 (2026-07-10): tests for /api/v1/backends/{id}/agent/heartbeat split.

func TestAgentBackendHeartbeat_HappyPath(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{
			ID:              "cppworker-gpu-bundled",
			Host:            "cppworker-gpu",
			CppWorkerPort:   18092,
			Type:            types.BackendTypeLlamaCpp,
			Status:          types.StatusHealthy,
			MaxConcurrentReqs: 4,
		},
	})
	defer cleanup()

	// Pre-attach agent (simulating prior /agents/register with action=attached)
	s.proxy.AttachAgentToBackend("cppworker-gpu-bundled", "cppworker-gpu-bundled-agent", 18032)

	body := map[string]interface{}{
		"type":                  "heartbeat",
		"agentId":               "cppworker-gpu-bundled-agent",
		"weight":                80,
		"maxConcurrentRequests": 8,
		"maxModels":             6,
		"status":                "healthy",
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/cppworker-gpu-bundled/agent/heartbeat", bytes.NewReader(b))
	req.Header.Set("X-Agent-ID", "cppworker-gpu-bundled-agent")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify: LastAgentContact updated
	backend := s.proxy.GetBackend("cppworker-gpu-bundled")
	if backend == nil {
		t.Fatal("backend not found")
	}
	if backend.LastAgentContact.IsZero() {
		t.Error("LastAgentContact should be set after heartbeat")
	}
	// Verify: runtime limits applied
	if backend.Weight != 80 {
		t.Errorf("Weight = %d, want 80", backend.Weight)
	}
}

func TestAgentBackendHeartbeat_AutoAttach(t *testing.T) {
	// Если бэкенд создан cppworker'ом без агента, agent может прислать heartbeat
	// и автоматически attach'нуться (первый пришедший становится владельцем).
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{
			ID:              "cppworker-gpu-bundled",
			Host:            "cppworker-gpu",
			CppWorkerPort:   18092,
			Type:            types.BackendTypeLlamaCpp,
			Status:          types.StatusHealthy,
			MaxConcurrentReqs: 4,
		},
	})
	defer cleanup()

	body := `{"type":"heartbeat","agentId":"new-agent","status":"healthy"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/cppworker-gpu-bundled/agent/heartbeat", strings.NewReader(body))
	req.Header.Set("X-Agent-ID", "new-agent")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify: agent auto-attached
	backend := s.proxy.GetBackend("cppworker-gpu-bundled")
	if !backend.HasAgent {
		t.Error("HasAgent should be true after auto-attach")
	}
	if backend.AgentID != "new-agent" {
		t.Errorf("AgentID = %q, want new-agent", backend.AgentID)
	}
}

func TestAgentBackendHeartbeat_AgentIDMismatch(t *testing.T) {
	// Защита от подмены: если бэкенд уже прикреплён к агенту A,
	// агент B не может обновить heartbeat.
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{
			ID:              "cppworker-gpu-bundled",
			Host:            "cppworker-gpu",
			CppWorkerPort:   18092,
			Type:            types.BackendTypeLlamaCpp,
			Status:          types.StatusHealthy,
			MaxConcurrentReqs: 4,
		},
	})
	defer cleanup()

	// Pre-attach agent A
	s.proxy.AttachAgentToBackend("cppworker-gpu-bundled", "agent-A", 18032)

	// Agent B пытается отправить heartbeat
	body := `{"type":"heartbeat","agentId":"agent-B","status":"healthy"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/cppworker-gpu-bundled/agent/heartbeat", strings.NewReader(body))
	req.Header.Set("X-Agent-ID", "agent-B")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify: agent A всё ещё прикреплён, B не подменил
	backend := s.proxy.GetBackend("cppworker-gpu-bundled")
	if backend.AgentID != "agent-A" {
		t.Errorf("AgentID = %q, want agent-A (не должен быть подменён)", backend.AgentID)
	}
}

func TestAgentBackendHeartbeat_BackendNotFound(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{})
	defer cleanup()

	body := `{"type":"heartbeat"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/nonexistent/agent/heartbeat", strings.NewReader(body))
	req.Header.Set("X-Agent-ID", "any-agent")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 Not Found, got %d", rec.Code)
	}
}

func TestAgentBackendHeartbeat_MissingXAgentID(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "b1", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/b1/agent/heartbeat", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	// X-Agent-ID отсутствует
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request, got %d", rec.Code)
	}
}

func TestAgentBackendHeartbeat_MethodNotAllowed(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "b1", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	defer cleanup()

	// GET вместо POST
	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends/b1/agent/heartbeat", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

func TestAgentBackendHeartbeat_WrongPath(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "b1", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy},
	})
	defer cleanup()

	// /api/v1/backends/{id}/agent/foo (не heartbeat) → попадает в default switch
	// backendHandler, который возвращает 405 Method Not Allowed.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/b1/agent/foo", strings.NewReader("{}"))
	req.Header.Set("X-Agent-ID", "x")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	// 405 (не 404) — backendHandler для не-распознанных подпутей возвращает 405
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 (Method Not Allowed), got %d", rec.Code)
	}
}

// Round 15 (2026-07-10): tests for /api/v1/backends/{id}/agent/metrics split.
// Без этого агент получал 404 "Backend not found" и метрики не доходили до WebUI.

func TestAgentBackendMetrics_HappyPath(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "cppworker-gpu-bundled", Host: "cppworker-gpu", CppWorkerPort: 18092,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 4},
	})
	defer cleanup()

	// Pre-attach agent
	s.proxy.AttachAgentToBackend("cppworker-gpu-bundled", "cppworker-gpu-bundled-agent", 18032)

	// Метрики (минимально валидные — JSON парсится в types.BackendMetrics)
	metricsBody := `{
		"timestamp": "2026-07-10T08:00:00Z",
		"gpu": {"usagePercent": 25.5, "memoryTotal": 8192, "memoryUsed": 1024, "memoryFree": 7168, "temperature": 65, "powerUsage": 150},
		"system": {"cpuUsagePercent": 30, "memoryTotal": 25042, "memoryUsed": 5000, "memoryFree": 20042}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/cppworker-gpu-bundled/agent/metrics",
		strings.NewReader(metricsBody))
	req.Header.Set("X-Agent-ID", "cppworker-gpu-bundled-agent")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAgentBackendMetrics_BackendNotFound(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{})
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/nonexistent/agent/metrics",
		strings.NewReader(`{}`))
	req.Header.Set("X-Agent-ID", "any-agent")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestAgentBackendMetrics_AgentIDMismatch(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "b1", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 4},
	})
	defer cleanup()
	s.proxy.AttachAgentToBackend("b1", "agent-A", 18032)

	// Agent-B пытается слать метрики на бэкенд agent-A
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/b1/agent/metrics",
		strings.NewReader(`{}`))
	req.Header.Set("X-Agent-ID", "agent-B")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden, got %d", rec.Code)
	}
}

func TestAgentBackendMetrics_AutoAttach(t *testing.T) {
	// cppworker-бэкенд без прикреплённого агента → agent шлёт метрики → auto-attach
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{
		{ID: "b1", Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 4},
	})
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends/b1/agent/metrics",
		strings.NewReader(`{}`))
	req.Header.Set("X-Agent-ID", "new-agent")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK (auto-attach), got %d", rec.Code)
	}

	backend := s.proxy.GetBackend("b1")
	if !backend.HasAgent {
		t.Error("HasAgent should be true after auto-attach via metrics")
	}
	if backend.AgentID != "new-agent" {
		t.Errorf("AgentID = %q, want new-agent", backend.AgentID)
	}
}

// setupTestServerWithBackends — helper для создания тестового сервера с предзагруженными бэкендами
func setupTestServerWithBackends(t *testing.T, backends []types.Backend) (*Server, func()) {
	t.Helper()
	// Используем createTestServer из handlers_test.go
	testServer, s, _ := createTestServer(t)
	for _, b := range backends {
		if err := s.proxy.AddBackend(b); err != nil {
			testServer.Close()
			t.Fatalf("failed to add backend %s: %v", b.ID, err)
		}
	}
	return s, testServer.Close
}