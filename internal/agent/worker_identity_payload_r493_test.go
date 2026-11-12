// worker_identity_payload_r493_test.go — R493-12i (2026-11-12): что АГЕНТ реально
// отправляет балансеру в регистрации.
//
// Проверяем сквозной контракт, а не только функцию-резолвер: в теле POST
// /api/v1/backends поля worker* обязаны нести идентичность ВОРКЕРА (из его
// /api/info), поля agent* — идентичность агента, а workerIdentitySource —
// откуда взяты worker*.
package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

type payloadCaptureR493 struct {
	mu   sync.Mutex
	body map[string]interface{}
}

func (c *payloadCaptureR493) record(raw []byte) {
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return
	}
	c.mu.Lock()
	c.body = m
	c.mu.Unlock()
}

func (c *payloadCaptureR493) get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.body[key].(string); ok {
		return v
	}
	return ""
}

// TestR493_RegistrationCarriesWorkerIdentity — тело регистрации: worker* — от
// воркера, agent* — от агента, source назван.
func TestR493_RegistrationCarriesWorkerIdentity(t *testing.T) {
	capture := &payloadCaptureR493{}
	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Агент регистрируется на /api/v1/agents/register (не /api/v1/backends: это путь самой Go-стороны воркера).
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/register" {
			raw, _ := io.ReadAll(r.Body)
			capture.record(raw)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"action":"created"}`))
			return
		}
		// Остальные вызовы (heartbeat и т.п.) — пустой успех.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer balancer.Close()

	// «Воркер»: отдельная сборка, отличная от агента.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/info" {
			_, _ = w.Write([]byte(`{"build_version":"r-worker-1","build_commit":"aaaaaaaa",` +
				`"build_date":"2026-11-12T10:00:00Z","rpc_proto":"6.0.0"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer worker.Close()

	prevTimeout := cppWorkerProbeTimeout
	cppWorkerProbeTimeout = 2 * time.Second
	defer func() { cppWorkerProbeTimeout = prevTimeout }()

	cfg := &types.AgentConfig{
		AgentID:       "bundled-agent-test",
		BalancerURL:   balancer.URL,
		CppWorkerURL:  worker.URL,
		BackendType:   types.BackendTypeLlamaCpp,
		MetricsPort:   18034,
		CppWorkerPort: 18092,
	}
	a := NewAgent(cfg)
	if err := a.register(); err != nil {
		t.Fatalf("register() failed: %v", err)
	}

	if got := capture.get("workerVersion"); got != "r-worker-1" {
		t.Fatalf("workerVersion = %q, ожидалось значение ВОРКЕРА r-worker-1 (а не агента)", got)
	}
	if got := capture.get("workerCommit"); got != "aaaaaaaa" {
		t.Fatalf("workerCommit = %q, ожидалось aaaaaaaa (коммит воркера)", got)
	}
	if got := capture.get("workerRpcProto"); got != "6.0.0" {
		t.Fatalf("workerRpcProto = %q, ожидалось 6.0.0", got)
	}
	if got := capture.get("workerIdentitySource"); got != workerIdentityFromWorkerR493 {
		t.Fatalf("workerIdentitySource = %q, ожидался %q", got, workerIdentityFromWorkerR493)
	}
	// Поля агента присутствуют как отдельные ключи (могут быть пустыми в
	// локальной сборке без -ldflags) — главное, что они не подменяют worker*.
	if capture.get("workerVersion") == capture.get("agentVersion") && capture.get("agentVersion") != "" {
		t.Logf("agentVersion=%q совпал с workerVersion — в bundled-образе это возможно",
			capture.get("agentVersion"))
	}
}
