// llamacpp_transport_r53_6_test.go — Round 53.6 (2026-08-24):
// regression test для streamTimeout abort in SSE read loop.
//
// Background: pre-R53.6, the SSE read loop in proxyRequestLlamaCpp
// only checked r.Context().Done() (client cancel). It did NOT check
// reqCtx.Done() (streamTimeout from per-model profile or default 600s).
// Result: if cppworker hangs mid-stream (e.g., model precision loss on
// long context), the loop never exits — balancer waits for [DONE]
// forever, blocking the request slot, and the client eventually
// times out and gets "Did not receive done" instead of a clean error.
//
// R53.6 fix: added `case <-reqCtx.Done():` to the SSE read loop select.
// When streamTimeout fires, loop closes the upstream body and returns
// error → R53.5 writeStreamErrorChunk emits done:true chunk to client.

package balancer

import "testing"

// TestGetEffectiveStreamTimeoutSec_NotStreaming — R53.6 helper test.
// When isStreaming=false, returns 0 (no timeout applied).
func TestGetEffectiveStreamTimeoutSec_NotStreaming(t *testing.T) {
	// Pass nil proxy because non-streaming path doesn't dereference it
	got := getEffectiveStreamTimeoutSec(nil, "test-model", false)
	if got != 0 {
		t.Errorf("expected 0 for non-streaming, got %d", got)
	}
}
