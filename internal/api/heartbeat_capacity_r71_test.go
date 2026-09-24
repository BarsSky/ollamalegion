package api

// R71 (2026-09-24): heartbeat агента больше не создаёт и не перекрывает
// вместимость бэкенда.
//
// Живой дефект: балансер отдавал агенту в ответе на heartbeat
// config.maxConcurrentRequests = runtimeMaxConcurrentRequests, агент применял
// значение локально и в следующем heartbeat возвращал его же. Балансер писал
// это «эхо» как новую вместимость, поэтому узел с n_parallel=1 (cppworker
// присылает maxConcurrentRequests=1 при саморегистрации) жил с порогом
// admission-очереди 4. PUT /limits откатывался за ~3 с (heartbeat раз в 3 с).
//
// Проверяем обе стороны:
//  1. heartbeat не пишет RuntimeMaxConcurrentRequests;
//  2. в ответе агенту отдаётся эффективная вместимость: для
//     саморегистрировавшейся ноды — её n_parallel, а не runtime-значение.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func nodeOwnedBackend() types.Backend {
	return types.Backend{
		ID:                "cppworker-gpu-bundled-agent",
		Host:              "cppworker-gpu",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		Status:            types.StatusHealthy,
		MaxConcurrentReqs: 1, // реальный n_parallel=1
		Labels:            []string{"cppworker", "auto-registered"},
		// «Эхо» устаревшей константы агента: не должно побеждать n_parallel.
		RuntimeMaxConcurrentRequests: 4,
		RuntimeCapacityFromNode:      true,
	}
}

// seedCapacityState — воспроизводим состояние живой стойки: у записи
// MaxConcurrentReqs = n_parallel, но runtime-значение осталось «эхом» (4).
// AddBackend сам проставляет runtime = MaxConcurrentReqs для auto-registered
// бэкендов (R71), поэтому «испорченное» состояние задаём явным UpdateBackend.
func seedCapacityState(t *testing.T, s *Server, maxConcurrentReqs, runtime int, fromNode bool) {
	t.Helper()
	b := *s.proxy.GetBackend("cppworker-gpu-bundled-agent")
	b.MaxConcurrentReqs = maxConcurrentReqs
	b.RuntimeMaxConcurrentRequests = runtime
	b.RuntimeCapacityFromNode = fromNode
	if err := s.proxy.UpdateBackend("cppworker-gpu-bundled-agent", b); err != nil {
		t.Fatalf("seed capacity state: %v", err)
	}
}

func postHeartbeat(t *testing.T, s *Server, path string, payload map[string]interface{}) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal heartbeat: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("X-Agent-ID", "cppworker-gpu-bundled-agent")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat %s: expected 200, got %d: %s", path, rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("heartbeat %s: invalid JSON: %v", path, err)
	}
	return out
}

func capacityFromResponse(t *testing.T, resp map[string]interface{}) float64 {
	t.Helper()
	cfg, ok := resp["config"].(map[string]interface{})
	if !ok {
		t.Fatalf("response has no config object: %v", resp)
	}
	raw, ok := cfg["maxConcurrentRequests"]
	if !ok {
		t.Fatalf("config has no maxConcurrentRequests: %v", cfg)
	}
	v, ok := raw.(float64)
	if !ok {
		t.Fatalf("maxConcurrentRequests is not a number: %T", raw)
	}
	return v
}

// Хендлер /api/v1/agents/heartbeat (legacy, используется живым агентом).
func TestHeartbeatR71_DoesNotWriteCapacityAndServesNodeCapacity_Legacy(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{nodeOwnedBackend()})
	defer cleanup()
	s.proxy.AttachAgentToBackend("cppworker-gpu-bundled-agent", "cppworker-gpu-bundled-agent", 18032)
	seedCapacityState(t, s, 1, 4, true) // n_parallel=1, runtime испорчен «эхом»

	resp := postHeartbeat(t, s, "/api/v1/agents/heartbeat", map[string]interface{}{
		"type":                  "heartbeat",
		"agentId":               "cppworker-gpu-bundled-agent",
		"status":                "healthy",
		"maxConcurrentRequests": 4, // локальная константа агента (эхо)
		"maxModels":             1,
	})

	got := s.proxy.GetBackend("cppworker-gpu-bundled-agent")
	if got.RuntimeMaxConcurrentRequests != 4 {
		t.Errorf("RuntimeMaxConcurrentRequests = %d, ожидалось 4 без изменений: heartbeat не должен писать вместимость",
			got.RuntimeMaxConcurrentRequests)
	}
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 1 {
		t.Errorf("EffectiveMaxConcurrentRequests = %d, ожидалось 1 (n_parallel ноды)", eff)
	}
	if cap := capacityFromResponse(t, resp); cap != 1 {
		t.Errorf("в ответе агенту maxConcurrentRequests = %v, ожидалось 1 (n_parallel ноды)", cap)
	}
}

// Хендлер /api/v1/backends/{id}/agent/heartbeat (новый endpoint).
func TestHeartbeatR71_DoesNotWriteCapacityAndServesNodeCapacity_BackendScoped(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{nodeOwnedBackend()})
	defer cleanup()
	s.proxy.AttachAgentToBackend("cppworker-gpu-bundled-agent", "cppworker-gpu-bundled-agent", 18032)
	seedCapacityState(t, s, 1, 4, true)

	resp := postHeartbeat(t, s, "/api/v1/backends/cppworker-gpu-bundled-agent/agent/heartbeat", map[string]interface{}{
		"type":                  "heartbeat",
		"agentId":               "cppworker-gpu-bundled-agent",
		"status":                "healthy",
		"maxConcurrentRequests": 4,
	})

	got := s.proxy.GetBackend("cppworker-gpu-bundled-agent")
	if got.RuntimeMaxConcurrentRequests != 4 {
		t.Errorf("RuntimeMaxConcurrentRequests = %d, ожидалось 4 без изменений", got.RuntimeMaxConcurrentRequests)
	}
	if cap := capacityFromResponse(t, resp); cap != 1 {
		t.Errorf("в ответе агенту maxConcurrentRequests = %v, ожидалось 1 (n_parallel ноды)", cap)
	}
}

// Раньше heartbeat создавал runtime-лимит с нуля (у бэкенда без лимитов).
func TestHeartbeatR71_DoesNotCreateRuntimeLimit(t *testing.T) {
	backend := nodeOwnedBackend()
	backend.RuntimeMaxConcurrentRequests = 0
	backend.RuntimeCapacityFromNode = false
	backend.MaxConcurrentReqs = 2

	s, cleanup := setupTestServerWithBackends(t, []types.Backend{backend})
	defer cleanup()
	s.proxy.AttachAgentToBackend("cppworker-gpu-bundled-agent", "cppworker-gpu-bundled-agent", 18032)
	seedCapacityState(t, s, 2, 0, false) // у бэкенда вообще нет runtime-лимита

	postHeartbeat(t, s, "/api/v1/agents/heartbeat", map[string]interface{}{
		"type":                  "heartbeat",
		"agentId":               "cppworker-gpu-bundled-agent",
		"status":                "healthy",
		"maxConcurrentRequests": 8,
	})

	got := s.proxy.GetBackend("cppworker-gpu-bundled-agent")
	if got.RuntimeMaxConcurrentRequests != 0 {
		t.Errorf("RuntimeMaxConcurrentRequests = %d, ожидалось 0: heartbeat не источник вместимости",
			got.RuntimeMaxConcurrentRequests)
	}
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 2 {
		t.Errorf("EffectiveMaxConcurrentRequests = %d, ожидалось 2 (статический max)", eff)
	}
}

// Операторский лимит (RuntimeCapacityFromNode=false) остаётся приоритетным и
// именно он отдаётся агенту.
func TestHeartbeatR71_OperatorLimitWins(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{nodeOwnedBackend()})
	defer cleanup()
	s.proxy.AttachAgentToBackend("cppworker-gpu-bundled-agent", "cppworker-gpu-bundled-agent", 18032)

	if err := s.proxy.UpdateBackendLimits("cppworker-gpu-bundled-agent", 1, 3); err != nil {
		t.Fatalf("UpdateBackendLimits: %v", err)
	}

	resp := postHeartbeat(t, s, "/api/v1/agents/heartbeat", map[string]interface{}{
		"type":                  "heartbeat",
		"agentId":               "cppworker-gpu-bundled-agent",
		"status":                "healthy",
		"maxConcurrentRequests": 9, // эхо не должно перебить лимит оператора
	})

	got := s.proxy.GetBackend("cppworker-gpu-bundled-agent")
	if got.RuntimeMaxConcurrentRequests != 3 {
		t.Errorf("RuntimeMaxConcurrentRequests = %d, ожидалось 3 (лимит оператора)", got.RuntimeMaxConcurrentRequests)
	}
	if eff := got.EffectiveMaxConcurrentRequests(); eff != 3 {
		t.Errorf("EffectiveMaxConcurrentRequests = %d, ожидалось 3 (лимит оператора)", eff)
	}
	if cap := capacityFromResponse(t, resp); cap != 3 {
		t.Errorf("в ответе агенту maxConcurrentRequests = %v, ожидалось 3 (лимит оператора)", cap)
	}
}
