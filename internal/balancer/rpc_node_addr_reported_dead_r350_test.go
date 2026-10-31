// rpc_node_addr_reported_dead_r350_test.go — R350 (2026-10-31).
//
// ЖИВОЙ ДЕФЕКТ (двухузловой стенд, docs/plans/2026-10-31-two-node-donor-check.md).
// Донор переехал: запись бэкенда исправлена (`host: 100.64.0.7`), но сам воркер
// донора продолжал сообщать `rpc_server.addr = 192.168.13.34:50052` (у него внешний
// адрес задан CPPWORKER_RPC_EXTERNAL_ADDR и не обновлялся).
//
// По R171 отчёт воркера ГЛАВНЕЕ догадки балансера — и это правильно для внешних
// узлов с нестандартным портом. Но когда сообщённый адрес мёртв, а host записи
// отвечает, доверие к отчёту превращается в дефект: мастер получал мёртвый адрес,
// guard писал «раскладка работает не в полном составе», и загрузка шла ЛОКАЛЬНО
// (42/42 на CUDA0) при rpc_enabled=true. Снаружи: «донор доступен, а раскладка не
// поднимается».
//
// R350: сообщённый адрес проверяется; если он не отвечает, а host:port отвечает —
// используется host:port с предупреждением в лог. Молча подменять нельзя: оператор
// обязан узнать, что воркер донора врёт про свой адрес.
//
// Проверяется:
//  1. сообщённый ЖИВОЙ адрес остаётся приоритетным (R171 не сломан);
//  2. сообщённый МЁРТВЫЙ + живой host → берётся host;
//  3. оба мертвы → остаётся сообщённый (не теряем внешний порт);
//  4. воркер не сообщил адрес → host записи;
//  5. layoutDonorServers отдаёт тот же выбор (один источник правды).
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// r350Proxy — прокси с одним донором и подменяемой пробой узлов.
func r350Proxy(t *testing.T, reportedAddr string, alive map[string]bool) *Proxy {
	t.Helper()
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["donor-1"] = &BackendState{Backend: &types.Backend{
		ID:     "donor-1",
		Host:   "100.64.0.7",
		Status: types.StatusHealthy,
		Type:   types.BackendTypeLlamaCpp,
		Role:   types.BackendRoleDonor,
	}}
	if reportedAddr != "" {
		p.metricsMgr.mu.Lock()
		p.metricsMgr.llamaMetrics["donor-1"] = &types.LlamaCppMetrics{
			RPCNode: &types.RPCNodeStatus{Enabled: true, Running: true, Listening: true, Port: 50052, Addr: reportedAddr},
		}
		p.metricsMgr.mu.Unlock()
	}
	p.rpcProbe = func(addr string, _ time.Duration) bool { return alive[addr] }
	return p
}

// TestR350_ReportedLiveAddressWins — R171 не сломан: живой сообщённый адрес главнее.
func TestR350_ReportedLiveAddressWins(t *testing.T) {
	p := r350Proxy(t, "192.0.2.55:50052", map[string]bool{"192.0.2.55:50052": true})

	b := p.GetBackend("donor-1")
	addr, source := p.rpcNodeAddrResolved(b, time.Second)
	if addr != "192.0.2.55:50052" {
		t.Fatalf("addr = %q, want сообщённый воркером 192.0.2.55:50052 (R171)", addr)
	}
	if source != "reported" {
		t.Errorf("source = %q, want reported", source)
	}
}

// TestR350_ReportedDeadFallsBackToHost — главный сценарий: отчёт врёт, host живой.
func TestR350_ReportedDeadFallsBackToHost(t *testing.T) {
	p := r350Proxy(t, "192.168.13.34:50052", map[string]bool{"100.64.0.7:50052": true})

	b := p.GetBackend("donor-1")
	addr, source := p.rpcNodeAddrResolved(b, time.Second)
	if addr != "100.64.0.7:50052" {
		t.Fatalf("addr = %q, want host записи 100.64.0.7:50052 — иначе мастер получит "+
			"мёртвый адрес и загрузит модель локально при включённой раскладке", addr)
	}
	if source != "reported_dead_host_ok" {
		t.Errorf("source = %q, want reported_dead_host_ok (расхождение должно быть видно в логе)", source)
	}
}

// TestR350_BothDeadKeepsReported — оба мёртвы: не теряем внешний порт из отчёта.
func TestR350_BothDeadKeepsReported(t *testing.T) {
	p := r350Proxy(t, "192.0.2.55:50052", map[string]bool{})

	b := p.GetBackend("donor-1")
	addr, source := p.rpcNodeAddrResolved(b, time.Second)
	if addr != "192.0.2.55:50052" {
		t.Fatalf("addr = %q, want сообщённый (при обоих мёртвых нельзя молча менять порт)", addr)
	}
	if source != "reported" {
		t.Errorf("source = %q, want reported", source)
	}
}

// TestR350_NoReportUsesHost — воркер молчит про адрес: берём host записи.
func TestR350_NoReportUsesHost(t *testing.T) {
	p := r350Proxy(t, "", map[string]bool{})

	b := p.GetBackend("donor-1")
	addr, source := p.rpcNodeAddrResolved(b, time.Second)
	if addr != "100.64.0.7:50052" {
		t.Fatalf("addr = %q, want 100.64.0.7:50052", addr)
	}
	if source != "host" {
		t.Errorf("source = %q, want host", source)
	}
}

// TestR350_LayoutDonorsUseResolvedAddress — список узлов для мастера строится по
// тому же правилу (один источник правды: не может быть, чтобы guard проверял host,
// а мастер получал мёртвый отчёт).
func TestR350_LayoutDonorsUseResolvedAddress(t *testing.T) {
	p := r350Proxy(t, "192.168.13.34:50052", map[string]bool{"100.64.0.7:50052": true})

	servers := p.layoutDonorServers()
	if len(servers) != 1 {
		t.Fatalf("layoutDonorServers = %v, want один узел", servers)
	}
	if servers[0] != "100.64.0.7:50052" {
		t.Fatalf("layoutDonorServers[0] = %q, want 100.64.0.7:50052: мастер получил бы "+
			"мёртвый адрес из отчёта воркера", servers[0])
	}
}
