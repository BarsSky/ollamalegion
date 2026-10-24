// rpc_layout_preflight_r118_21_test.go — R118.21 (2026-10-15): раскладка
// выключается ДО попытки загрузки, если балансер ЗНАЕТ, что узел мёртв.
//
// ЖИВОЙ ДЕФЕКТ (тест DSH как клиента, стенд, мастер):
//
//	1) ensureModelLoadedOnBackend: async auto-load → загрузка ПОШЛА с мёртвым
//	   узлом 192.0.2.10:50052 → клиент получил 503
//	2) …+9 c: EnsureRPCLayout: «узлы раскладки недоступны … загрузка идёт локально»
//	3) …+9 c: executeLlamaCppLoad: «раскладка деградировала — грузим без мёртвых узлов»
//
// То есть первый запрос клиента после потери узла падал, а повторный проходил.
// Причина: проба узла — TCP, а на хосте с docker-proxy TCP-соединение к
// опубликованному порту принимается даже когда контейнер мёртв. При этом у
// балансера уже была правда об узле: его запись была помечена unhealthy и без
// агента (agent timeout).
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR118_21_NodeMarkedUnhealthyDegradesBeforeLoad — TCP «отвечает», но запись
// узла мёртвая → раскладка выключается сразу, загрузка идёт локально.
func TestR118_21_NodeMarkedUnhealthyDegradesBeforeLoad(t *testing.T) {
	p, rec := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)

	// TCP-проба даёт ЛОЖНОПОЛОЖИТЕЛЬНЫЙ результат (как docker-proxy на Windows).
	setReachability(p, testDonorAddr)

	donor := p.backends["donor-1"].Backend
	donor.Status = types.StatusUnhealthy
	silent := time.Now().Add(-2 * time.Minute); donor.AgentSilentSince = &silent

	degraded, reason := p.EnsureRPCLayout("main-1")
	if !degraded {
		t.Fatalf("раскладка обязана деградировать: узел помечен недоступным (reason=%q)", reason)
	}
	if rec.enabled() {
		t.Error("в cppworker ушла ВКЛЮЧЁННАЯ раскладка с мёртвым узлом — клиент получит 503")
	}

	// Узел вернулся: раскладка включается обратно.
	donor.Status = types.StatusHealthy
	donor.AgentSilentSince = nil
	if degraded2, _ := p.EnsureRPCLayout("main-1"); degraded2 {
		t.Error("здоровый узел не должен считаться деградацией")
	}
	if !rec.enabled() {
		t.Error("раскладка обязана вернуться на живой узел")
	}
}

// TestR118_21_UnknownNodeFallsBackToTCP — если записи об узле нет, решает
// TCP-проба: узел может быть зарегистрирован вручную, без агента.
func TestR118_21_UnknownNodeFallsBackToTCP(t *testing.T) {
	p, _ := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	setReachability(p, testDonorAddr)

	if p.nodeMarkedUnavailable(testDonorAddr) {
		t.Error("здоровый известный узел не должен считаться мёртвым")
	}
	if p.nodeMarkedUnavailable("198.51.100.7:50052") {
		t.Error("неизвестный узел не должен блокироваться — решает TCP-проба")
	}
	if !p.rpcProbeFunc()(testDonorAddr, time.Second) {
		t.Error("живой узел обязан проходить пробу")
	}
	if p.rpcProbeFunc()("198.51.100.7:50052", time.Second) {
		t.Error("недоступный узел не должен проходить пробу")
	}
}

// TestR118_21_AgentSilentAloneIsEnough — «агент молчит» (AgentSilentSince) сам по
// себе признак недоступности, даже если статус ещё healthy.
func TestR118_21_AgentSilentAloneIsEnough(t *testing.T) {
	p, _ := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	donor := p.backends["donor-1"].Backend
	silent := time.Now().Add(-time.Minute); donor.AgentSilentSince = &silent

	if !p.nodeMarkedUnavailable(testDonorAddr) {
		t.Error("молчащий агент обязан считаться недоступностью узла")
	}
}
