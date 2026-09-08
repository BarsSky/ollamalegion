// proxy_timeout_llamacpp_env_test.go — REGRESSION TEST: LB_LLAMACPP_STREAM_TIMEOUT_SEC
// was REMOVED in R60.18 F3.
//
// R60.17 (2026-09-08): root cause OpenWebUI multi-turn cutoff — env override
// LB_LLAMACPP_STREAM_TIMEOUT_SEC=90 перекрывал per-model profile и убивал
// длинные ответы на 2+ вопросах сессии.
//
// R60.17 fix: закомментировали env var в .env.bundled-with-agent.
//
// R60.18 F3 fix: УДАЛЕН код который читал этот env var. Total stream timeout —
// wrong abstraction для LLM streaming (см. docs/R60.18-env-flags-audit.md).
//
// Этот тест ловит регрессию: если кто-то опять добавит LB_LLAMACPP_STREAM_TIMEOUT_SEC
// env override, тест сломается (показывая что env var вернулся = баг OpenWebUI
// вернулся).
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestProxy_LB_LLAMACPP_STREAM_TIMEOUT_SEC_Removed —
// R60.18 F3 regression: setting LB_LLAMACPP_STREAM_TIMEOUT_SEC env var
// НЕ должно влиять на stream timeout. Должен использоваться per-model profile.
func TestProxy_LB_LLAMACPP_STREAM_TIMEOUT_SEC_Removed(t *testing.T) {
	t.Setenv("LB_LLAMACPP_STREAM_TIMEOUT_SEC", "90")
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "") // убрать из возможных победителей

	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{
				StreamTimeout:        600,
				StreamingIdleTimeout: 120,
			},
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"qwen3-8b": {
					StreamingTimeoutSec:     3600, // 1h per-model
					StreamingIdleTimeoutSec: 600,
				},
			},
		},
		modelLatencyTracker: NewModelLatencyTracker(),
	}

	// Если env var ВСЁ ЕЩЁ читается (regression!), getModelStreamTimeout вернёт 90s
	// и per-model profile (1h) будет проигнорирован — OpenWebUI cutoff возвращается.
	got := p.getModelStreamTimeout("qwen3-8b")
	want := 3600 * time.Second // per-model profile wins
	if got != want {
		t.Errorf("getModelStreamTimeout(qwen3-8b) с LB_LLAMACPP_STREAM_TIMEOUT_SEC=90 "+
			"вернул %v, want %v. ENV var НЕ ДОЛЖЕН читаться (R60.18 F3 removal). "+
			"Если видишь 90s — env var вернулся, OpenWebUI multi-turn cutoff тоже.",
			got, want)
	}
}

// TestProxy_LB_LLAMACPP_STREAM_TIMEOUT_SEC_RemovedFromGlobal —
// global stream timeout тоже не реагирует на LB_LLAMACPP_STREAM_TIMEOUT_SEC.
func TestProxy_LB_LLAMACPP_STREAM_TIMEOUT_SEC_RemovedFromGlobal(t *testing.T) {
	t.Setenv("LB_LLAMACPP_STREAM_TIMEOUT_SEC", "30")
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "")

	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{
				StreamTimeout:        600, // 10min
				StreamingIdleTimeout: 120,
			},
		},
	}

	got := p.getGlobalStreamTimeout()
	want := 600 * time.Second
	if got != want {
		t.Errorf("getGlobalStreamTimeout() с LB_LLAMACPP_STREAM_TIMEOUT_SEC=30 = %v, want %v "+
			"(env var НЕ ДОЛЖЕН читаться — R60.18 F3 removal)",
			got, want)
	}
}
