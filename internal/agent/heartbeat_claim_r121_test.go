//go:build llama_stub

// heartbeat_claim_r121_test.go — R121 (2026-10-15), пункт 1.
//
// ЖИВОЙ ДЕФЕКТ. После перезапуска балансера запись бэкенда теряла заявку узла о
// вместимости и откатывалась к статике конфига (2 вместо 1). Агент об этом не
// узнавал: он регистрируется только при СВОЁМ старте, а heartbeat слал лишь
// `maxConcurrentRequests` — то самое значение, которое балансер ему и навязал
// («эхо»). Принять такое поле за заявку узла нельзя, поэтому нужна ОТДЕЛЬНАЯ
// величина, посчитанная самим агентом.
//
// Тесты фиксируют: в heartbeat уезжает собственная заявка узла, и она не зависит
// от значения, применённого из ответа балансера.
package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR121_HeartbeatCarriesNodeCapacityClaim — heartbeat обязан нести
// `nodeMaxConcurrentRequests`, посчитанный узлом.
func TestR121_HeartbeatCarriesNodeCapacityClaim(t *testing.T) {
	var (
		mu       sync.Mutex
		received []byte
	)
	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(body)
		}
		mu.Lock()
		received = append(received, body...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Балансер навязывает узлу СВОЁ значение (2) — ровно тот случай, когда
		// «эхо» способно отравить заявку.
		_, _ = w.Write([]byte(`{"status":"ok","config":{"maxConcurrentRequests":2}}`))
	}))
	defer balancer.Close()

	a := &Agent{
		config:      &types.AgentConfig{AgentID: "a1", BackendID: "a1", Weight: 1},
		httpClient:  balancer.Client(),
		balancerURL: balancer.URL,
	}
	a.setNodeCapacityClaim(1)

	a.sendHeartbeat()

	mu.Lock()
	raw := string(received)
	mu.Unlock()
	if raw == "" {
		t.Fatal("heartbeat не отправлен")
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("heartbeat не JSON: %v (%s)", err, raw)
	}
	got, ok := payload["nodeMaxConcurrentRequests"]
	if !ok {
		t.Fatalf("в heartbeat нет поля nodeMaxConcurrentRequests: %s — балансер не сможет "+
			"восстановить вместимость после своего перезапуска", raw)
	}
	if n, _ := got.(float64); int(n) != 1 {
		t.Errorf("nodeMaxConcurrentRequests = %v, want 1 (собственная заявка узла)", got)
	}
}

// TestR121_HeartbeatClaimIgnoresBalancerEcho — заявка узла не должна меняться от
// того, что балансер прислал в ответе: иначе это ровно та петля «эхо», из-за
// которой узел с n_parallel=1 жил с чужим лимитом.
func TestR121_HeartbeatClaimIgnoresBalancerEcho(t *testing.T) {
	balancer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","config":{"maxConcurrentRequests":7}}`))
	}))
	defer balancer.Close()

	a := &Agent{
		config:      &types.AgentConfig{AgentID: "a2", BackendID: "a2", Weight: 1},
		httpClient:  balancer.Client(),
		balancerURL: balancer.URL,
	}
	a.setNodeCapacityClaim(1)

	a.sendHeartbeat()

	if got := a.NodeCapacityClaim(); got != 1 {
		t.Errorf("NodeCapacityClaim() = %d после ответа балансера с 7, want 1: "+
			"заявка узла подменилась «эхом» балансера", got)
	}
	// Локальное значение агент применяет (так и задумано — оно сходится к
	// вместимости узла), но заявку это не меняет.
	if a.config.MaxConcurrentRequests != 7 {
		t.Errorf("config.MaxConcurrentRequests = %d, want 7 (значение балансера применяется "+
			"локально)", a.config.MaxConcurrentRequests)
	}
}
