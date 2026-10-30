//go:build llama_stub

// layout_skip_offline_r315_test.go — R315 (2026-10-30): недоступному бэкенду
// приказ о раскладке не отправляется.
//
// ЖИВОЙ ФАКТ. Донор выключен (запись это знает: ollama_unavailable), а гард
// раскладки каждые 5 минут слал ему PUT и ждал сетевой таймаут. Строка в логе
// ничего не добавляла («узел мёртв» уже видно), а ожидание задерживало тик.
package balancer

import (
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func r315Proxy(t *testing.T, status types.BackendStatus, role string) (*Proxy, *int32) {
	t.Helper()
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["node-1"] = &BackendState{Backend: &types.Backend{
		ID: "node-1", Host: "192.0.2.10", Type: types.BackendTypeLlamaCpp,
		Status: status, Role: role, CppWorkerPort: 18092,
	}}
	if role == types.BackendRoleMain {
		// Главному нужен хотя бы один донор: без него probRPCNodes нечего
		// опрашивать (len(servers)==0 → проба не вызывается), и контроль ничего
		// не докажет.
		p.backends["node-donor"] = &BackendState{Backend: &types.Backend{
			ID: "node-donor", Host: "192.0.2.11", Type: types.BackendTypeLlamaCpp,
			Status: types.StatusHealthy, Role: types.BackendRoleDonor, CppWorkerPort: 18092,
		}}
	}
	var probes int32
	// Поле rpcProbe, а не аксессор rpcProbeFunc(): аксессор возвращает замыкание,
	// которое само зовёт базу через p.mu — присвоить его нельзя.
	p.rpcProbe = func(addr string, d time.Duration) bool {
		atomic.AddInt32(&probes, 1)
		return false
	}
	return p, &probes
}

func TestR315_OfflineBackendGetsNoOrder(t *testing.T) {
	// R315.1: unhealthy добавлен по живому прогону (донор висит именно в нём).
	// degraded НЕ входит: узел работает частично и приказ ему нужен.
	for _, status := range []types.BackendStatus{types.StatusOffline, types.StatusOllamaUnavailable, types.StatusUnhealthy} {
		p, probes := r315Proxy(t, status, types.BackendRoleDonor)
		degraded, reason := p.EnsureRPCLayout("node-1")
		if !degraded {
			t.Errorf("status=%s: degraded=false — состав раскладки при мёртвом узле обязан быть объявлен", status)
		}
		if reason == "" {
			t.Errorf("status=%s: причина не названа (оператору нужно знать, почему приказа нет)", status)
		}
		if n := atomic.LoadInt32(probes); n != 0 {
			t.Errorf("status=%s: сделано %d TCP-проб — приказу некуда идти, проба не нужна", status, n)
		}
	}
}

func TestR315_ReachableBackendStillGetsOrder(t *testing.T) {
	// Контроль: healthy-узел обязан дойти до пробы (и, если она не прошла, —
	// получить честный отказ). Иначе правка выключила бы раскладку целиком.
	p, probes := r315Proxy(t, types.StatusHealthy, types.BackendRoleMain)
	_, reason := p.EnsureRPCLayout("node-1")
	if n := atomic.LoadInt32(probes); n == 0 {
		t.Fatal("healthy-бэкенд не опрошен: правка заблокировала раскладку для живых узлов")
	}
	if reason == "" {
		t.Error("донор недоступен (проба false), а причина не названа")
	}
}
