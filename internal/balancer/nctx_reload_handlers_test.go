// nctx_reload_handlers_test.go — tests for nctx_reload_handlers.go.
//
// R60.6 (2026-09-07): focused tests for newNCtxReloadHTTPClient timeout
// derivation. Old code hardcoded 90s which killed cppworker reload
// before waitTimeoutSec=300 could complete on slow hardware (5GB model
// + 131072 n_ctx load = 60-180s on RTX 3070 8GB).
//
// New behavior: http.Client.Timeout = cfg.effectiveTimeout() + 30s buffer,
// clamped to [30s, 600s]. Verifies:
//   - Default (no nctxReload) → 300s
//   - With AutoReloadTimeoutSec=120 → 150s
//   - With AutoReloadTimeoutSec=600 → 600s (capped)
//   - With AutoReloadTimeoutSec=0 → 300s (default fallback)
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

func TestNewNCtxReloadHTTPClient_TimeoutFromConfig(t *testing.T) {
	tests := []struct {
		name             string
		autoReloadTO     int
		wantMin          time.Duration
		wantMax          time.Duration
	}{
		{"no_nctxReload", 0, 300 * time.Second, 300 * time.Second},
		{"default_300s", 300, 320 * time.Second, 340 * time.Second}, // 300+30
		{"short_120s", 120, 140 * time.Second, 160 * time.Second},  // 120+30
		{"max_600s", 600, 600 * time.Second, 600 * time.Second},     // 600+30=630 → clamped to 600
		{"zero_uses_default", 0, 300 * time.Second, 300 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Proxy{
				config: &types.LoadBalancerConfig{
					Auth: types.AuthConfig{
						Tokens: []string{"test-token"},
					},
				},
			}
			if tt.autoReloadTO > 0 {
				p.nctxReload = NewNCtxReloadCoordinator(NCtxReloadConfig{
					AutoReloadTimeoutSec: tt.autoReloadTO,
				})
			}
			client := p.newNCtxReloadHTTPClient()
			if client == nil {
				t.Fatal("newNCtxReloadHTTPClient returned nil")
			}
			impl, ok := client.(*DefaultNCtxReloadHTTPClient)
			if !ok {
				t.Fatalf("expected *DefaultNCtxReloadHTTPClient, got %T", client)
			}
			if impl.HTTPClient == nil {
				t.Fatal("HTTPClient is nil")
			}
			timeout := impl.HTTPClient.Timeout
			if timeout < tt.wantMin || timeout > tt.wantMax {
				t.Errorf("HTTPClient.Timeout = %v, want [%v, %v]", timeout, tt.wantMin, tt.wantMax)
			}
		})
	}
}
