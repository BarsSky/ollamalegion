// Package balancer — OperatingMode helpers.
//
// Phase 8 (2026-07-10): P.1 — rpc_coordinator production mode.
//
// OperatingMode определяет, как balancer обрабатывает inference-запросы:
//   - "standard" (default)         → обычный прокси через Proxy.ServeHTTP
//   - "replication"                 → через model replication manager
//   - "rpc_coordinator"             → через RpcCoordinatorDispatcher (Phase 8+)
//   - "virtual_router"              → через VirtualModelRouter
//   - "distributed_inference"       → через distributed inference engine
//
// В Phase 8.1 мы только добавляем type-safe helper. Полная интеграция
// rpc_coordinator (dispatcher wiring + streaming) — Phase 9.
package balancer

import "ollama-loadbalancer/pkg/types"

// OperatingModeCanonical returns the canonical mode name. Если mode == "" или
// legacy-alias (например, "rpc-coordinator" с дефисом), возвращает
// canonical form ("standard" / "rpc_coordinator"). Используется для
// нормализации в cluster_state и метриках.
//
// TODO (Phase 8.5+): пока оставлено без strict validation — unknown modes
// (e.g. "invalid_mode_xyz" в test) возвращаются as-is для backward compat
// с existing dispatch tests (tests/operating_mode_dispatch_test.go:174).
func OperatingModeCanonical(mode string) string {
	switch mode {
	case "", "standard", "Standard", "STANDARD":
		return "standard"
	case "replication", "Replication", "REPLICATION":
		return "replication"
	case "rpc_coordinator", "rpc-coordinator", "RpcCoordinator", "RPC_COORDINATOR":
		return "rpc_coordinator"
	case "virtual_router", "virtual-router", "VirtualRouter", "VIRTUAL_ROUTER":
		return "virtual_router"
	case "distributed_inference", "distributed-inference", "DistributedInference", "DISTRIBUTED_INFERENCE":
		return "distributed_inference"
	default:
		// Unknown — return as-is (don't break legacy configs).
		return mode
	}
}

// IsRpcCoordinatorMode returns true если balancer в rpc_coordinator mode.
// Включает legacy aliases ("rpc-coordinator" с дефисом).
//
// Phase 8: используется в Proxy.ServeHTTP для intercept при наличии
// RpcCoordinatorDispatcher. В Phase 8.5 — scaffold-блок, в Phase 9 — full.
func IsRpcCoordinatorMode(mode string) bool {
	canonical := OperatingModeCanonical(mode)
	return canonical == "rpc_coordinator"
}

// IsStandardMode — true если standard ИЛИ empty (default).
// Используется для fallback в Proxy.ServeHTTP и dispatcher.ShouldRoute.
func IsStandardMode(mode string) bool {
	canonical := OperatingModeCanonical(mode)
	return canonical == "standard"
}

// Ensure types.OperatingMode constants are accessible from balancer package
// (compile-time check — если types.OperatingMode* переименуют, увидим).
var (
	_ = types.OperatingModeStandard
	_ = types.OperatingModeRpcCoordinator
)
