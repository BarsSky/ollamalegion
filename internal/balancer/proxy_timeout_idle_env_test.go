// proxy_timeout_idle_env_test.go — TDD test for LB_STREAMING_IDLE_TIMEOUT_SEC
// ENV override (F1 из R60.18 env-flags audit).
//
// R60.18 (2026-09-08): user feedback — "нужно ли вообще ставить total stream
// timeout? Нет же возможности подгадать сколько времени хватит на ответ".
//
// Right design: keep `streamingIdleTimeout` (no chunks for N sec) as the
// ONLY hang detection mechanism. Drop total stream timeout.
//
// Audit found `LB_STREAMING_IDLE_TIMEOUT_SEC` is referenced in
// `config/config.bundled.json:32` and `internal/balancer/streaming.go:322, 325`
// error message as an env override, but the env var is NEVER actually read.
// Operators who set it per docs get silent no-op.
//
// This test catches the regression: env var must now actually be read
// and apply to per-model + global idle timeout resolution.
package balancer

import (
	"os"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestProxy_LB_STREAMING_IDLE_TIMEOUT_SEC_OverridesGlobal —
// R60.18 F1: phantom env var теперь читается. Global idle timeout
// overridden через env, без правки config.json.
func TestProxy_LB_STREAMING_IDLE_TIMEOUT_SEC_OverridesGlobal(t *testing.T) {
	t.Setenv("LB_STREAMING_IDLE_TIMEOUT_SEC", "42")
	p := newTestProxyForTimeout()

	got := p.getGlobalStreamingIdleTimeout()
	want := 42 * time.Second
	if got != want {
		t.Errorf("getGlobalStreamingIdleTimeout() = %v, want %v (env override)", got, want)
	}
}

// TestProxy_LB_STREAMING_IDLE_TIMEOUT_SEC_OverridesPerModel —
// R60.18 F1: per-model profile уступает env override (break-glass).
// То же поведение, что у LB_LLAMACPP_STREAM_TIMEOUT_SEC (R60.17).
// Per-model profile по-прежнему работает когда env не задан.
func TestProxy_LB_STREAMING_IDLE_TIMEOUT_SEC_OverridesPerModel(t *testing.T) {
	os.Unsetenv("LB_STREAMING_NEVER_TIMEOUT")
	t.Setenv("LB_STREAMING_IDLE_TIMEOUT_SEC", "30")
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{
				StreamingIdleTimeout: 600, // config = 10min
			},
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"qwen3-8b": {
					StreamingIdleTimeoutSec: 300, // per-model = 5min
				},
			},
		},
		modelLatencyTracker: NewModelLatencyTracker(),
	}

	got := p.getModelStreamingIdleTimeout("qwen3-8b")
	want := 30 * time.Second
	if got != want {
		t.Errorf("getModelStreamingIdleTimeout(qwen3-8b) с ENV=30 = %v, want %v "+
			"(env override beats per-model profile, как и R60.17 LB_LLAMACPP_STREAM_TIMEOUT_SEC)",
			got, want)
	}
}

// TestProxy_LB_STREAMING_IDLE_TIMEOUT_SEC_UnsetFallsToPerModel —
// без env — per-model profile используется.
func TestProxy_LB_STREAMING_IDLE_TIMEOUT_SEC_UnsetFallsToPerModel(t *testing.T) {
	os.Unsetenv("LB_STREAMING_IDLE_TIMEOUT_SEC")
	os.Unsetenv("LB_STREAMING_NEVER_TIMEOUT")
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{
				StreamingIdleTimeout: 600,
			},
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"qwen3-8b": {
					StreamingIdleTimeoutSec: 300, // 5min per-model
				},
			},
		},
		modelLatencyTracker: NewModelLatencyTracker(),
	}

	got := p.getModelStreamingIdleTimeout("qwen3-8b")
	want := 300 * time.Second
	if got != want {
		t.Errorf("getModelStreamingIdleTimeout(qwen3-8b) без ENV = %v, want %v "+
			"(per-model profile используется когда env не задан)",
			got, want)
	}
}

// TestProxy_LB_STREAMING_IDLE_TIMEOUT_SEC_InvalidIgnored —
// невалидный env → fallback на config (НЕ паника, НЕ zero).
func TestProxy_LB_STREAMING_IDLE_TIMEOUT_SEC_InvalidIgnored(t *testing.T) {
	t.Setenv("LB_STREAMING_IDLE_TIMEOUT_SEC", "not-a-number")
	p := newTestProxyForTimeout()

	got := p.getGlobalStreamingIdleTimeout()
	// config по умолчанию 0 → fallback на дефолт 120s
	if got == 0 {
		t.Errorf("getGlobalStreamingIdleTimeout() с невалидным ENV = 0 — должен быть 120s default")
	}
	if got == 30*time.Second || got == 42*time.Second {
		t.Errorf("getGlobalStreamingIdleTimeout() = %v — невалидный ENV должен игнорироваться", got)
	}
}

// TestProxy_LB_STREAMING_IDLE_TIMEOUT_SEC_NeverTimeoutStillWins —
// LB_STREAMING_NEVER_TIMEOUT=1 имеет ВЫСШИЙ приоритет, чем
// LB_STREAMING_IDLE_TIMEOUT_SEC. NEVER_TIMEOUT отключает всё.
func TestProxy_LB_STREAMING_IDLE_TIMEOUT_SEC_NeverTimeoutStillWins(t *testing.T) {
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "1")
	t.Setenv("LB_STREAMING_IDLE_TIMEOUT_SEC", "42")
	p := newTestProxyForTimeout()

	got := p.getGlobalStreamingIdleTimeout()
	if got != 0 {
		t.Errorf("getGlobalStreamingIdleTimeout() = %v, want 0 (NEVER_TIMEOUT > IDLE_TIMEOUT env)", got)
	}
}

// TestProxy_LB_STREAMING_IDLE_TIMEOUT_SEC_ZeroIgnored —
// LB_STREAMING_IDLE_TIMEOUT_SEC=0 → игнорируется (как пустая строка),
// НЕ "no timeout". 0 = invalid, fallback на config.
func TestProxy_LB_STREAMING_IDLE_TIMEOUT_SEC_ZeroIgnored(t *testing.T) {
	t.Setenv("LB_STREAMING_IDLE_TIMEOUT_SEC", "0")
	p := newTestProxyForTimeout()

	got := p.getGlobalStreamingIdleTimeout()
	if got == 0 {
		t.Errorf("getGlobalStreamingIdleTimeout() = 0, want 120s default (LB_STREAMING_IDLE_TIMEOUT_SEC=0 → ignore, NOT disable)")
	}
}
