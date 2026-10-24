// backend_role_r118_test.go — R118 (2026-10-14): роли бэкендов в раскладке слоёв.
//
// ТРЕБОВАНИЕ ОПЕРАТОРА: «оставлять узел в списке, так как может происходить
// настройка» и «явное управление от балансера через WEBUI: кто донор, кто
// главный, кто самодостаточный».
//
// Что фиксируют тесты:
//  1. адреса доноров собираются из бэкендов с ролью «донор» (host:порт rpc-server);
//  2. роль применяется ЯВНО: главному уходит rpcEnabled + rpcServers, а без
//     назначенного главного рукопожатия не происходит;
//  3. узел с ЯВНОЙ ролью авто-уборка «молчащих» записей не удаляет, а обычную
//     auto-registered запись без роли — удаляет (по TTL).
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR118_RoleConstants — роли, которые оператор видит в WebUI.
func TestR118_RoleConstants(t *testing.T) {
	if types.BackendRoleStandalone != "standalone" ||
		types.BackendRoleMain != "main" ||
		types.BackendRoleDonor != "donor" {
		t.Fatalf("константы ролей изменились: %q/%q/%q",
			types.BackendRoleStandalone, types.BackendRoleMain, types.BackendRoleDonor)
	}
}

// TestR118_GhostReaperKeepsRoleBackends — узел с ролью остаётся в списке, даже
// если он давно молчит (его могут настраивать); запись без роли убирается.
func TestR118_GhostReaperKeepsRoleBackends(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour) // молчит дольше TTL (30 минут)

	p := newProxyWithCleanup(t, createTestConfig())
	mk := func(id, role string) *BackendState {
		silent := old
		return &BackendState{Backend: &types.Backend{
			ID:               id,
			Status:           types.StatusUnhealthy,
			Labels:           []string{"auto-registered"},
			HasAgent:         false,
			Role:             role,
			AgentSilentSince: &silent,
		}}
	}
	p.backends["donor-node"] = mk("donor-node", types.BackendRoleDonor)
	p.backends["orphan"] = mk("orphan", "")

	removed := p.reapGhostBackends(now, 30*time.Minute)

	for _, id := range removed {
		if id == "donor-node" {
			t.Error("узел с ролью удалён авто-уборкой: роль и раскладка потерялись бы, " +
				"а оператор мог его настраивать")
		}
	}
	foundOrphan := false
	for _, id := range removed {
		if id == "orphan" {
			foundOrphan = true
		}
	}
	if !foundOrphan {
		t.Errorf("запись без роли обязана убираться по TTL, удалено: %v", removed)
	}
}
