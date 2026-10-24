//go:build llama_stub

// rpc_peers_handler_r125_test.go — R125 (пункт 3): ручка отдаёт ФАКТ доступности
// узлов, а не пересказ списка из конфига.
package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// livePeer — слушающий сокет (роль доступного донора).
func livePeer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func rpcPeersRequest(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/rpc-peers", nil)
	w := httptest.NewRecorder()
	handleCppWorkerRPCPeers(w, req)
	return w
}

// TestR125_RPCPeers_ReportsReachabilityPerNode — доступный узел виден как
// активный, недоступный — как неактивный с причиной.
func TestR125_RPCPeers_ReportsReachabilityPerNode(t *testing.T) {
	prev := currentConfig
	t.Cleanup(func() { currentConfig = prev })
	live := livePeer(t)
	currentConfig = &cppbackend.Config{
		RPCEnabled: true,
		RPCServers: []string{live, "127.0.0.1:1"},
	}

	w := rpcPeersRequest(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Servers   []cppbackend.RPCPeerState `json:"servers"`
		Reachable int                       `json:"reachable"`
		Total     int                       `json:"total"`
		Note      string                    `json:"note"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if resp.Total != 2 || len(resp.Servers) != 2 {
		t.Fatalf("узлов: total=%d servers=%d, want 2", resp.Total, len(resp.Servers))
	}
	if resp.Reachable != 1 {
		t.Errorf("reachable = %d, want 1 (один узел слушает, второй порт закрыт)", resp.Reachable)
	}
	if !resp.Servers[0].Reachable {
		t.Errorf("живой узел %s помечен недоступным: %+v", resp.Servers[0].Addr, resp.Servers[0])
	}
	if resp.Servers[1].Reachable || resp.Servers[1].Error == "" {
		t.Errorf("недоступный узел: %+v — нужен reachable=false и причина", resp.Servers[1])
	}
	// Честность формулировки: UI не должен принять задержку за пропускную способность.
	if !strings.Contains(resp.Note, "пропускная способность") {
		t.Errorf("в ответе нет оговорки про пропускную способность: %q", resp.Note)
	}
}

// TestR125_RPCPeers_DisabledLayoutReportsNoNodes — при выключенной раскладке
// список пуст: это «не настроено», а не «узлы недоступны».
func TestR125_RPCPeers_DisabledLayoutReportsNoNodes(t *testing.T) {
	prev := currentConfig
	t.Cleanup(func() { currentConfig = prev })
	currentConfig = &cppbackend.Config{
		RPCEnabled: false,
		RPCServers: []string{livePeer(t)}, // список есть, но режим выключен
	}

	var resp struct {
		Servers    []cppbackend.RPCPeerState `json:"servers"`
		RPCEnabled bool                      `json:"rpcEnabled"`
		Total      int                       `json:"total"`
	}
	if err := json.Unmarshal(rpcPeersRequest(t).Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if resp.RPCEnabled {
		t.Error("rpcEnabled = true при выключенном режиме")
	}
	if resp.Total != 0 || len(resp.Servers) != 0 {
		t.Errorf("при выключенной раскладке узлов быть не должно: total=%d", resp.Total)
	}
}

// TestR125_RPCPeers_RequiresGET — ручка только на чтение.
func TestR125_RPCPeers_RequiresGET(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cppworker/rpc-peers", nil)
	w := httptest.NewRecorder()
	handleCppWorkerRPCPeers(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}
