// proxy_timeout_llamacpp_env_test.go — TDD test for LB_LLAMACPP_STREAM_TIMEOUT_SEC
// ENV override behavior.
//
// R60.17 (2026-09-08): user pain — "OpenWebUI multi-turn chat на 2+ вопросе
// обрывается без ошибок, ответ неполный".
//
// Root cause: ENV override LB_LLAMACPP_STREAM_TIMEOUT_SEC=90 в .env
// балансера перекрывал per-model profile И глобальный default 600s.
// 90s timeout — это жёсткий cap, который убивал "развёрнутые ответы"
// на длинных prompt с conversation history (2+ вопросов).
//
// REGRESSION TEST:
//
// Test 1: с LB_LLAMACPP_STREAM_TIMEOUT_SEC=90 → per-model profile ИГНОРИРУЕТСЯ,
//
//	возвращается 90s (это и есть bug).
//
// Test 2: без ENV → per-model profile используется (1h в тесте).
// Test 3: без ENV, без per-model profile → fallback на default 600s.
//
// Этот test ловит регрессию, если кто-то опять поставит жёсткий ENV
// override для fail-fast, не учитывая OpenWebUI multi-turn сценарий.
package balancer

import (
	"os"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestProxy_LB_LLAMACPP_STREAM_TIMEOUT_SEC_OverridesPerModelProfile —
// R60.17 regression: ENV=90 перекрывает per-model profile (1h).
// Это и есть root cause OpenWebUI multi-turn cutoff.
func TestProxy_LB_LLAMACPP_STREAM_TIMEOUT_SEC_OverridesPerModelProfile(t *testing.T) {
	t.Setenv("LB_LLAMACPP_STREAM_TIMEOUT_SEC", "90")
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{
				StreamTimeout:        600,
				StreamingIdleTimeout: 120,
			},
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"qwen3-8b": {
					StreamingTimeoutSec:     3600, // 1h per-model override
					StreamingIdleTimeoutSec: 600,
				},
			},
		},
	}

	got := p.getModelStreamTimeout("qwen3-8b")
	want := 90 * time.Second
	if got != want {
		t.Errorf("getModelStreamTimeout(qwen3-8b) with ENV=90 = %v, want %v "+
			"(ENV override beats per-model profile — R60.17 root cause)",
			got, want)
	}
}

// TestProxy_LB_LLAMACPP_STREAM_TIMEOUT_SEC_UnsetFallsToProfile —
// без ENV override — per-model profile используется.
func TestProxy_LB_LLAMACPP_STREAM_TIMEOUT_SEC_UnsetFallsToProfile(t *testing.T) {
	os.Unsetenv("LB_LLAMACPP_STREAM_TIMEOUT_SEC")
	os.Unsetenv("LB_STREAMING_NEVER_TIMEOUT")
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{
				StreamTimeout:        600,
				StreamingIdleTimeout: 120,
			},
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"qwen3-8b": {
					StreamingTimeoutSec:     3600, // 1h per-model override
					StreamingIdleTimeoutSec: 600,
				},
			},
		},
		modelLatencyTracker: NewModelLatencyTracker(), // нужен для доступа к Tier 1 (per-model)
	}

	got := p.getModelStreamTimeout("qwen3-8b")
	want := 3600 * time.Second
	if got != want {
		t.Errorf("getModelStreamTimeout(qwen3-8b) без ENV = %v, want %v "+
			"(per-model profile должен использоваться)",
			got, want)
	}
}

// TestProxy_LB_LLAMACPP_STREAM_TIMEOUT_SEC_UnsetNoProfile_GlobalDefault —
// без ENV и без per-model profile — используется глобальный default 600s.
func TestProxy_LB_LLAMACPP_STREAM_TIMEOUT_SEC_UnsetNoProfile_GlobalDefault(t *testing.T) {
	os.Unsetenv("LB_LLAMACPP_STREAM_TIMEOUT_SEC")
	os.Unsetenv("LB_STREAMING_NEVER_TIMEOUT")
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{
				StreamTimeout:        600, // 10m default
				StreamingIdleTimeout: 120,
			},
		},
	}

	// Без per-model profile и modelLatencyTracker (nil) → p.getGlobalStreamTimeout()
	got := p.getModelStreamTimeout("nonexistent-model")
	want := 600 * time.Second
	if got != want {
		t.Errorf("getModelStreamTimeout(nonexistent-model) = %v, want %v "+
			"(без ENV/profile — глобальный default 600s)",
			got, want)
	}
}

// TestProxy_LB_LLAMACPP_STREAM_TIMEOUT_SEC_IgnoredWhenInvalid —
// невалидный ENV (parse error) — fallback на default path.
func TestProxy_LB_LLAMACPP_STREAM_TIMEOUT_SEC_IgnoredWhenInvalid(t *testing.T) {
	t.Setenv("LB_LLAMACPP_STREAM_TIMEOUT_SEC", "not-a-number")
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{
				StreamTimeout:        600,
				StreamingIdleTimeout: 120,
			},
		},
		modelLatencyTracker: NewModelLatencyTracker(),
	}

	// Невалидный ENV парсится, но strconv.Atoi != nil → fall through к
	// global default. Проверяем, что НЕ возвращается 0 (= "no timeout").
	got := p.getModelStreamTimeout("nonexistent-model")
	if got == 0 || got == 90*time.Second {
		t.Errorf("getModelStreamTimeout() с невалидным ENV = %v, ожидался fallback на 600s default",
			got)
	}
}
