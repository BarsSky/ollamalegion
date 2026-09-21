// proxy_timeout_never_test.go — TDD test for LB_STREAMING_NEVER_TIMEOUT ENV override.
//
// R58.1 (2026-09-03): user pain — "балансер по жестким таймаутам рубил
// работу клиентам, хотя по идее это не требуется так как если будет в
// этом необходимость пользователь сам передаст ответ через клиент по
// прекращению". Добавляем ENV override для отключения streaming таймаутов.
//
// Семантика:
//   - LB_STREAMING_NEVER_TIMEOUT=1 → getGlobalStreamTimeout/Idle/FirstByte/Request
//     возвращают 0 (= "no timeout"). Callers пропускают context.WithTimeout
//     и SetReadDeadline.
//   - Default (unset / =0) → текущее поведение (600s/120s/900s/120s defaults).
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newTestProxyForTimeout создаёт Proxy с минимальным валидным config для
// тестирования timeout helpers (без state, metrics, session и т.д.).
func newTestProxyForTimeout() *Proxy {
	return &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{},
		},
	}
}

// TestProxy_LBStreamingNeverTimeout_Defaults — без ENV возвращаются defaults.
//
// R60.26 (2026-09-10): getGlobalRequestTimeout default изменён с 120s на 0
// (no timeout). Это убирает "magic number" поведение из config.json:
// timeout — opt-in через ENV, default disabled.
//
// R65d (2026-09-20) — ИСПРАВЛЕНИЕ ТЕСТА. Раньше тест ожидал 600s от
// getGlobalStreamTimeout при пустом config. Но R65c осознанно изменил контракт:
// `sec <= 0 → 0` (NO TIMEOUT), потому что на слабых машинах с partial offload
// (Qwen3-22B, 4-5 tok/s, 4096 токенов = 14 мин) жёсткий stream-timeout обрывал
// длинные ответы, и пользователь не получал полный текст. См. комментарий
// в getGlobalStreamTimeout и cmd/balancer/main.go (WriteTimeout убран по той же
// причине). Тест не был обновлён вместе с кодом и падал.
//
// Оставшиеся дефолты: idle=120s (hang detection), firstByte=900s, request=0.
func TestProxy_LBStreamingNeverTimeout_Defaults(t *testing.T) {
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "")
	t.Setenv("LB_REQUEST_TIMEOUT_SEC", "")
	t.Setenv("LB_STREAMING_IDLE_TIMEOUT_SEC", "")
	p := newTestProxyForTimeout()

	if got := p.getGlobalStreamTimeout(); got != 0 {
		t.Errorf("getGlobalStreamTimeout() = %v, want 0 (R65c: no total stream timeout by default)", got)
	}
	if got := p.getGlobalStreamingIdleTimeout(); got != 120*time.Second {
		t.Errorf("getGlobalStreamingIdleTimeout() = %v, want 120s (default)", got)
	}
	// R60.26: requestTimeout default = 0 (was 120s). Timeout — opt-in через ENV.
	if got := p.getGlobalRequestTimeout(); got != 0 {
		t.Errorf("getGlobalRequestTimeout() = %v, want 0 (R60.26 default disabled)", got)
	}
	if got := p.getGlobalFirstByteTimeout(); got != 900*time.Second {
		t.Errorf("getGlobalFirstByteTimeout() = %v, want 900s (default)", got)
	}
}

// TestProxy_LBStreamingNeverTimeout_ExplicitConfigHonored — если оператор
// ЯВНО задал Balancing.StreamTimeout, он honoured (R65c меняет только дефолт).
func TestProxy_LBStreamingNeverTimeout_ExplicitConfigHonored(t *testing.T) {
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "")
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{StreamTimeout: 600},
		},
	}
	if got := p.getGlobalStreamTimeout(); got != 600*time.Second {
		t.Errorf("getGlobalStreamTimeout() = %v, want 600s (explicit config honoured)", got)
	}
}

// TestProxy_LBStreamingNeverTimeout_Enabled — с ENV=1 все 4 функции
// возвращают 0 (= "no timeout"). Caller (proxy_request.go,
// streaming.go) интерпретирует 0 как "skip timeout".
func TestProxy_LBStreamingNeverTimeout_Enabled(t *testing.T) {
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "1")
	p := newTestProxyForTimeout()

	if got := p.getGlobalStreamTimeout(); got != 0 {
		t.Errorf("getGlobalStreamTimeout() = %v, want 0 (NEVER_TIMEOUT)", got)
	}
	if got := p.getGlobalStreamingIdleTimeout(); got != 0 {
		t.Errorf("getGlobalStreamingIdleTimeout() = %v, want 0 (NEVER_TIMEOUT)", got)
	}
	if got := p.getGlobalRequestTimeout(); got != 0 {
		t.Errorf("getGlobalRequestTimeout() = %v, want 0 (NEVER_TIMEOUT)", got)
	}
	if got := p.getGlobalFirstByteTimeout(); got != 0 {
		t.Errorf("getGlobalFirstByteTimeout() = %v, want 0 (NEVER_TIMEOUT)", got)
	}
}

// TestProxy_LBStreamingNeverTimeout_PerModel — per-model helpers тоже уважают ENV.
// Когда ENV=1, getModelStreamTimeout/getModelStreamingIdleTimeout
// (даже с per-model profile) возвращают 0.
func TestProxy_LBStreamingNeverTimeout_PerModel(t *testing.T) {
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "1")
	// Создаём proxy с per-model profile, которая БЕЗ ENV дала бы timeout.
	p := &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{
				StreamTimeout:        600, // 10m global default
				StreamingIdleTimeout: 120, // 2m
			},
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"qwen3-8b": {
					StreamingTimeoutSec:     3600, // 1h per-model override
					StreamingIdleTimeoutSec: 600,  // 10m per-model override
				},
			},
		},
	}
	if got := p.getModelStreamTimeout("qwen3-8b"); got != 0 {
		t.Errorf("getModelStreamTimeout(qwen3-8b) = %v, want 0 (NEVER_TIMEOUT overrides per-model)", got)
	}
	if got := p.getModelStreamingIdleTimeout("qwen3-8b"); got != 0 {
		t.Errorf("getModelStreamingIdleTimeout(qwen3-8b) = %v, want 0 (NEVER_TIMEOUT overrides per-model)", got)
	}
}

// TestProxy_LBStreamingNeverTimeout_DisabledExplicit —
// LB_STREAMING_NEVER_TIMEOUT=0 (или "false") = disable override,
// используем значения из config.
//
// R65d: ожидание исправлено — при пустом config R65c даёт 0 (no timeout), а не
// 600s. Здесь проверяем именно то, что явный "0" НЕ включает override
// (то есть idle-таймаут остаётся ненулевым), а не конкретную величину
// stream-timeout, которая зависит от config.
func TestProxy_LBStreamingNeverTimeout_DisabledExplicit(t *testing.T) {
	t.Setenv("LB_STREAMING_NEVER_TIMEOUT", "0")
	p := newTestProxyForTimeout()
	// При NEVER_TIMEOUT=1 idle был бы 0; при "0" — обычный дефолт 120s.
	if got := p.getGlobalStreamingIdleTimeout(); got != 120*time.Second {
		t.Errorf("getGlobalStreamingIdleTimeout() = %v, want 120s (explicit 0 disables NEVER_TIMEOUT)", got)
	}
	if got := p.getGlobalFirstByteTimeout(); got != 900*time.Second {
		t.Errorf("getGlobalFirstByteTimeout() = %v, want 900s (explicit 0 disables NEVER_TIMEOUT)", got)
	}
}
