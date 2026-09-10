// proxy_timeout_r6026_test.go — TDD tests for R60.26 timeout semantics.
//
// R60.26 (2026-09-10): убираем "magic number" поведение из config.json.
// Принцип: timeout — это opt-in через ENV, default = 0 (no timeout, не
// обрываем long-running генерацию). config.json: requestTimeout остаётся
// для backward compat, но НЕ применяется если не указан ENV явно.
//
// Семантика:
//   - LB_STREAMING_NEVER_TIMEOUT=1 (highest priority) → 0
//   - LB_REQUEST_TIMEOUT_SEC=N (explicit) → N seconds
//   - LB_REQUEST_TIMEOUT_SEC=0 / invalid / empty → 0 (disabled)
//   - config.json: requestTimeout — IGNORED в R60.26 (legacy, оставлен
//     для тех, кто хочет явно поставить через ENV)
package balancer

import (
	"os"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestGetRequestTimeout_R6026_DefaultDisabled — без ENV → 0.
// R60.26: убираем magic number. config.json: requestTimeout=600 НЕ применяется.
func TestGetRequestTimeout_R6026_DefaultDisabled(t *testing.T) {
	os.Unsetenv("LB_REQUEST_TIMEOUT_SEC")
	os.Unsetenv("LB_STREAMING_NEVER_TIMEOUT")
	if got := getRequestTimeout(); got != 0 {
		t.Errorf("getRequestTimeout() = %v, want 0 (R60.26 default disabled)", got)
	}
}

// TestGetRequestTimeout_R6026_NeverTimeoutWins — NEVER_TIMEOUT > 0
// даже если LB_REQUEST_TIMEOUT_SEC=600 явно. Highest priority.
func TestGetRequestTimeout_R6026_NeverTimeoutWins(t *testing.T) {
	os.Unsetenv("LB_REQUEST_TIMEOUT_SEC")
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "1")
	t.Setenv("LB_REQUEST_TIMEOUT_SEC", "600")
	if got := getRequestTimeout(); got != 0 {
		t.Errorf("getRequestTimeout() with NEVER_TIMEOUT=1 + REQ_TIMEOUT=600 = %v, want 0 (NEVER wins)", got)
	}
}

// TestGetRequestTimeout_R6026_ExplicitValue — ENV=N → N seconds.
func TestGetRequestTimeout_R6026_ExplicitValue(t *testing.T) {
	os.Unsetenv("LB_STREAMING_NEVER_TIMEOUT")
	t.Setenv("LB_REQUEST_TIMEOUT_SEC", "1800")
	if got := getRequestTimeout(); got != 1800*time.Second {
		t.Errorf("getRequestTimeout() = %v, want 1800s (explicit ENV)", got)
	}
}

// TestGetRequestTimeout_R6026_ExplicitZero — ENV=0 → 0 (explicit "no timeout").
func TestGetRequestTimeout_R6026_ExplicitZero(t *testing.T) {
	os.Unsetenv("LB_STREAMING_NEVER_TIMEOUT")
	t.Setenv("LB_REQUEST_TIMEOUT_SEC", "0")
	if got := getRequestTimeout(); got != 0 {
		t.Errorf("getRequestTimeout() = %v, want 0 (explicit ENV=0)", got)
	}
}

// TestGetRequestTimeout_R6026_InvalidIgnored — invalid value → 0 (default).
func TestGetRequestTimeout_R6026_InvalidIgnored(t *testing.T) {
	os.Unsetenv("LB_STREAMING_NEVER_TIMEOUT")
	t.Setenv("LB_REQUEST_TIMEOUT_SEC", "abc")
	if got := getRequestTimeout(); got != 0 {
		t.Errorf("getRequestTimeout() = %v, want 0 (invalid → default)", got)
	}
}

// TestGetRequestTimeout_R6026_NegativeIgnored — negative value → 0 (default).
func TestGetRequestTimeout_R6026_NegativeIgnored(t *testing.T) {
	os.Unsetenv("LB_STREAMING_NEVER_TIMEOUT")
	t.Setenv("LB_REQUEST_TIMEOUT_SEC", "-100")
	if got := getRequestTimeout(); got != 0 {
		t.Errorf("getRequestTimeout() = %v, want 0 (negative → default)", got)
	}
}

// TestGetGlobalRequestTimeout_R6026_ConfigIgnored —
// config.json: requestTimeout=600 НЕ применяется (R60.26).
// Default = 0 (disabled), даже если config говорит 600.
func TestGetGlobalRequestTimeout_R6026_ConfigIgnored(t *testing.T) {
	os.Unsetenv("LB_REQUEST_TIMEOUT_SEC")
	os.Unsetenv("LB_STREAMING_NEVER_TIMEOUT")
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{
				RequestTimeout: 600, // legacy config, должно быть IGNORED в R60.26
			},
		},
	}
	if got := p.getGlobalRequestTimeout(); got != 0 {
		t.Errorf("getGlobalRequestTimeout() = %v, want 0 (R60.26 config.json ignored)", got)
	}
}

// TestGetGlobalRequestTimeout_R6026_ExplicitEnv —
// LB_REQUEST_TIMEOUT_SEC=1800 → 1800s, config.json: 600 ignored.
func TestGetGlobalRequestTimeout_R6026_ExplicitEnv(t *testing.T) {
	os.Unsetenv("LB_STREAMING_NEVER_TIMEOUT")
	t.Setenv("LB_REQUEST_TIMEOUT_SEC", "1800")
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{
				RequestTimeout: 600, // legacy, ignored
			},
		},
	}
	if got := p.getGlobalRequestTimeout(); got != 1800*time.Second {
		t.Errorf("getGlobalRequestTimeout() = %v, want 1800s (ENV overrides config)", got)
	}
}
