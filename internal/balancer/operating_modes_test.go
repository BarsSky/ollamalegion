// operating_modes_test.go — tests для Phase 8.1 OperatingMode helpers.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestOperatingModeCanonical_Standard — canonical form для standard mode.
func TestOperatingModeCanonical_Standard(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "standard"},         // empty → standard (default)
		{"standard", "standard"}, // canonical
		{"Standard", "standard"}, // mixed case
		{"STANDARD", "standard"}, // upper case
	}
	for _, tc := range tests {
		got := OperatingModeCanonical(tc.in)
		if got != tc.want {
			t.Errorf("OperatingModeCanonical(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestOperatingModeCanonical_RpcCoordinator — legacy alias normalization.
func TestOperatingModeCanonical_RpcCoordinator(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"rpc_coordinator", "rpc_coordinator"},   // canonical (Phase 8)
		{"rpc-coordinator", "rpc_coordinator"},   // legacy hyphenated
		{"RpcCoordinator", "rpc_coordinator"},   // PascalCase
		{"RPC_COORDINATOR", "rpc_coordinator"}, // SCREAMING
	}
	for _, tc := range tests {
		got := OperatingModeCanonical(tc.in)
		if got != tc.want {
			t.Errorf("OperatingModeCanonical(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestOperatingModeCanonical_OtherModes — остальные modes (replication,
// virtual_router, distributed_inference) не имеют aliases в Phase 8 —
// проверяем что они проходят без изменений.
func TestOperatingModeCanonical_OtherModes(t *testing.T) {
	modes := []string{"replication", "virtual_router", "distributed_inference"}
	for _, m := range modes {
		got := OperatingModeCanonical(m)
		if got != m {
			t.Errorf("OperatingModeCanonical(%q) = %q, want %q", m, got, m)
		}
	}
}

// TestOperatingModeCanonical_UnknownPassthrough — unknown modes возвращаются
// as-is (не ломаем legacy config).
func TestOperatingModeCanonical_UnknownPassthrough(t *testing.T) {
	// "invalid_mode_xyz" используется в tests/config_export_import_test.go:173
	// для проверки что balancer не падает на unknown modes.
	got := OperatingModeCanonical("invalid_mode_xyz")
	if got != "invalid_mode_xyz" {
		t.Errorf("OperatingModeCanonical(unknown) should passthrough, got %q", got)
	}
}

// TestIsRpcCoordinatorMode — главный helper Phase 8.1.
func TestIsRpcCoordinatorMode(t *testing.T) {
	trueCases := []string{
		"rpc_coordinator",
		"rpc-coordinator", // legacy alias
		"RpcCoordinator",
		"RPC_COORDINATOR",
	}
	for _, m := range trueCases {
		if !IsRpcCoordinatorMode(m) {
			t.Errorf("IsRpcCoordinatorMode(%q) = false, want true", m)
		}
	}
	falseCases := []string{
		"standard",
		"replication",
		"virtual_router",
		"distributed_inference",
		"",
		"invalid_mode_xyz",
		"Standard",  // mixed-case — NOT a recognized alias
		"rpc coord", // not a valid form
	}
	for _, m := range falseCases {
		if IsRpcCoordinatorMode(m) {
			t.Errorf("IsRpcCoordinatorMode(%q) = true, want false", m)
		}
	}
}

// TestIsStandardMode — стандарт + empty = true.
func TestIsStandardMode(t *testing.T) {
	trueCases := []string{"", "standard", "Standard", "STANDARD"}
	for _, m := range trueCases {
		if !IsStandardMode(m) {
			t.Errorf("IsStandardMode(%q) = false, want true", m)
		}
	}
	falseCases := []string{
		"replication", "rpc_coordinator", "virtual_router", "distributed_inference",
		"invalid_mode_xyz",
	}
	for _, m := range falseCases {
		if IsStandardMode(m) {
			t.Errorf("IsStandardMode(%q) = true, want false", m)
		}
	}
}

// TestOperatingModeTypeConstants — types.OperatingMode* constants существуют и
// имеют правильные string values (regression guard если кто-то переименует).
func TestOperatingModeTypeConstants(t *testing.T) {
	cases := []struct {
		constant types.OperatingMode
		want     string
	}{
		{types.OperatingModeStandard, "standard"},
		{types.OperatingModeReplication, "replication"},
		{types.OperatingModeRpcCoordinator, "rpc_coordinator"},
		{types.OperatingModeVirtualRouter, "virtual_router"},
		{types.OperatingModeDistributedInference, "distributed_inference"},
	}
	for _, tc := range cases {
		if string(tc.constant) != tc.want {
			t.Errorf("OperatingMode constant = %q, want %q", string(tc.constant), tc.want)
		}
	}
}
