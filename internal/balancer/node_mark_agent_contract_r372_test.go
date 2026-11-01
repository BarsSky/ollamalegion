// node_mark_agent_contract_r372_test.go — R372 (2026-11-01).
//
// Правило: метка «агент молчит» (AgentSilentSince) означает недоступность узла
// ТОЛЬКО у записи, где агент объявлен (HasAgent). Для вручную добавленного узла
// (агент к балансеру не ходит) решает TCP-проба — иначе раскладка залипает
// выключенной навсегда.
//
// Живой факт, из которого правило выросло (двухузловой стенд):
//
//	donor record: hasAgent=false, agentSilentSince=непусто, status=healthy
//	rpcNode:      running=true, listening=true, reachable=false
//	лог каждые 15 с: «узел донора недоступен с балансера addr=100.64.0.7:50052»
//	а curl ИЗ контейнера балансера: Established connection to 100.64.0.7:50052
//	мастер: rpc_enabled=false — двухузловая раскладка не включалась вообще.
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR372_ManualNodeSilentMarkDoesNotBlockProbe — вручную добавленный узел с
// меткой молчания обязан проверяться TCP-пробой.
func TestR372_ManualNodeSilentMarkDoesNotBlockProbe(t *testing.T) {
	p, _ := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	donor := p.backends["donor-1"].Backend
	donor.HasAgent = false
	donor.Status = types.StatusHealthy
	silent := time.Now().Add(-time.Minute)
	donor.AgentSilentSince = &silent

	if p.nodeMarkedUnavailable(testDonorAddr) {
		t.Fatal("вручную добавленный узел (hasAgent=false) признан мёртвым по метке агента:\n" +
			"    именно это залипание держало rpc_enabled=false при живом доноре")
	}

	// Проба должна ДОЙТИ до TCP и получить честный ответ.
	setReachability(p, testDonorAddr)
	if !p.rpcProbeFunc()(testDonorAddr, time.Second) {
		t.Error("живой узел обязан проходить TCP-пробу, даже если у записи осталась метка молчания")
	}
}

// TestR372_DeclaredAgentSilentStillBlocks — обратная сторона: если агент
// объявлен и молчит, узел по-прежнему считается недоступным (R118.21).
func TestR372_DeclaredAgentSilentStillBlocks(t *testing.T) {
	p, _ := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	donor := p.backends["donor-1"].Backend
	donor.HasAgent = true
	donor.Status = types.StatusHealthy
	silent := time.Now().Add(-time.Minute)
	donor.AgentSilentSince = &silent

	if !p.nodeMarkedUnavailable(testDonorAddr) {
		t.Error("молчащий ОБЪЯВЛЕННЫЙ агент обязан считаться недоступностью узла")
	}
}

// TestR372_UnhealthyStillBlocks — нездоровый статус блокирует независимо от агента.
func TestR372_UnhealthyStillBlocks(t *testing.T) {
	p, _ := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	donor := p.backends["donor-1"].Backend
	donor.HasAgent = false
	donor.Status = types.StatusUnhealthy

	if !p.nodeMarkedUnavailable(testDonorAddr) {
		t.Error("нездоровый узел обязан блокироваться")
	}
}
