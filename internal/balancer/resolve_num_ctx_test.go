package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestResolveNumCtx_BodyOnly(t *testing.T) {
	p := NewProxy(&types.LoadBalancerConfig{})
	body := []byte(`{"model":"test-model","messages":[{"role":"user","content":"test"}],"options":{"num_ctx":16384}}`)
	resolved := p.ResolveNumCtx("test-model", body, "test-backend")
	t.Logf("resolved: value=%d source=%s", resolved.Value, resolved.Source)
	if resolved.Value != 16384 {
		t.Errorf("expected 16384, got %d", resolved.Value)
	}
}
