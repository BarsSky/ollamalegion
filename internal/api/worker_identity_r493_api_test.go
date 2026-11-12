//go:build llama_stub

// worker_identity_r493_api_test.go — R493-12i (2026-11-12): балансер хранит и
// показывает ДВЕ идентичности — воркера и агента — и называет источник.
//
// ЖИВОЙ ДЕФЕКТ. В bundled-стеке бэкенд регистрирует встроенный агент, и поля
// workerVersion/workerCommit он заполнял своей версией. На стенде это дало
// workerCommit=9d42a329 при образе gpu-261110-1925 с /app/identity.json →
// 24b3a151: при проверке выкладки поле читалось как «раскатали не тот образ».
// Теперь агент присылает идентичность ВОРКЕРА (из его /api/info) отдельными
// полями, свою — полями agent*, а workerIdentitySource говорит, откуда взято.
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func registerAgentR493(t *testing.T, s *Server, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", "node-r493")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/v1/agents/register: HTTP %d: %s", rec.Code, rec.Body.String())
	}
}

// TestR493_WorkerAndAgentIdentityAreSeparate — поля не путаются, источник назван.
func TestR493_WorkerAndAgentIdentityAreSeparate(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, nil)
	defer cleanup()

	registerAgentR493(t, s, `{
		"agentId": "node-r493", "host": "192.0.2.31", "backendType": "llama_cpp",
		"cppWorkerPort": 18092, "agentPort": 18034,
		"workerVersion": "r261112-2146", "workerCommit": "32153c14",
		"workerBuildDate": "2026-11-12T18:46:37Z", "workerRpcProto": "6.0.0",
		"workerIdentitySource": "worker-http",
		"agentVersion": "a6157066", "agentCommit": "9d42a329"
	}`)

	entry := backendEntryR101(t, s, "node-r493")
	if entry["workerVersion"] != "r261112-2146" || entry["workerCommit"] != "32153c14" {
		t.Fatalf("identичность ВОРКЕРА не сохранена: version=%v commit=%v",
			entry["workerVersion"], entry["workerCommit"])
	}
	if entry["agentVersion"] != "a6157066" || entry["agentCommit"] != "9d42a329" {
		t.Fatalf("идентичность АГЕНТА не сохранена: version=%v commit=%v",
			entry["agentVersion"], entry["agentCommit"])
	}
	if entry["workerIdentitySource"] != "worker-http" {
		t.Fatalf("workerIdentitySource = %v, ожидался worker-http: без него «откуда взято» неизвестно",
			entry["workerIdentitySource"])
	}
}

// TestR493_IdentitySurvivesHeartbeat — повторная регистрация без полей не стирает
// известное (иначе проверка релиза мигала бы каждые 30 с).
func TestR493_IdentitySurvivesHeartbeat(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "node-r493b", Name: "node", Host: "192.0.2.32", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
		WorkerVersion: "r261112-2146", WorkerCommit: "32153c14",
		WorkerIdentitySource: "worker-http",
		AgentVersion:         "a6157066", AgentCommit: "9d42a329",
	}})
	defer cleanup()

	registerAgentR493(t, s, `{"agentId": "node-r493b", "host": "192.0.2.32",
		"backendType": "llama_cpp", "cppWorkerPort": 18092, "agentPort": 18034}`)

	entry := backendEntryR101(t, s, "node-r493b")
	if entry["workerCommit"] != "32153c14" {
		t.Fatalf("workerCommit затёрт heartbeat-ом: %v", entry["workerCommit"])
	}
	if entry["agentCommit"] != "9d42a329" {
		t.Fatalf("agentCommit затёрт heartbeat-ом: %v", entry["agentCommit"])
	}
	if entry["workerIdentitySource"] != "worker-http" {
		t.Fatalf("workerIdentitySource затёрт: %v", entry["workerIdentitySource"])
	}
}

// TestR493_ApplyAgentIdentityIgnoresEmpty — отдельная функция подчиняется тому же
// правилу «пустое не стирает известное».
func TestR493_ApplyAgentIdentityIgnoresEmpty(t *testing.T) {
	b := &types.Backend{ID: "w"}
	applyAgentIdentityR493(b, "a6157066", "9d42a329", "agent-fallback")
	applyAgentIdentityR493(b, "", "", "")
	if b.AgentVersion != "a6157066" || b.AgentCommit != "9d42a329" || b.WorkerIdentitySource != "agent-fallback" {
		t.Fatalf("пустые значения затёрли известное: %+v", b)
	}
	// source перезаписывается: он описывает ТЕКУЩИЙ способ получения.
	applyAgentIdentityR493(b, "", "", "worker-http")
	if b.WorkerIdentitySource != "worker-http" {
		t.Fatalf("source не обновился: %q", b.WorkerIdentitySource)
	}
	applyAgentIdentityR493(nil, "x", "y", "z")
}

// TestR493_BackendsJSONHasIdentityKeys — ключи обязаны быть в ответе всегда (даже
// пустыми): UI и разбор архива читают их по имени.
func TestR493_BackendsJSONHasIdentityKeys(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "node-r493c", Name: "node", Host: "192.0.2.33", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backends?includeUnhealthy=true", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	var resp struct {
		Backends []map[string]interface{} `json:"backends"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	for _, b := range resp.Backends {
		if b["id"] != "node-r493c" {
			continue
		}
		for _, key := range []string{"workerVersion", "workerCommit", "agentVersion", "agentCommit", "workerIdentitySource"} {
			if _, ok := b[key]; !ok {
				t.Fatalf("в /api/v1/backends нет ключа %q: %v", key, b)
			}
		}
		return
	}
	t.Fatal("бэкенд node-r493c не найден")
}
