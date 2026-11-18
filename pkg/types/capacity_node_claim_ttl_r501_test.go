//go:build llama_stub

// capacity_node_claim_ttl_r501_test.go — R501 (2026-11-19), аудит S3, C-1.
//
// ЖИВОЙ ДЕФЕКТ. `NodeClaimedCapacity` считается ОДИН раз при старте агента и
// «замерзает» до его рестарта. Если узел после смены модели держит 4 слота, а
// заявка говорила 2, балансер продолжал пускать 2 — и проверить это было нечем
// до перезапуска агента.
//
// Правило: заявка участвует, только пока подтверждалась не позже TTL
// (LB_CAPACITY_NODE_CLAIM_TTL_SEC, дефолт 900 c). TTL=0 — не проверять (прежнее
// поведение). Работает в режиме LB_CAPACITY_SINGLE_SOURCE=on: тень и off решений
// не меняют (§5 аудита: новые правила не включаются по умолчанию).
package types

import (
	"testing"
	"time"
)

// resetCapacityCachesR501 — сброс sync.Once-кэшей (иначе t.Setenv «залипнет»
// на весь пакет).
func resetCapacityCachesR501() {
	ResetCapacitySingleSourceModeR501R501()
	ResetCapacityNodeClaimTTLR501()
}

// TestR501_StaleNodeClaimIgnored — просроченная заявка не участвует в решении.
func TestR501_StaleNodeClaimIgnored(t *testing.T) {
	resetCapacityCachesR501()
	t.Cleanup(resetCapacityCachesR501)
	t.Setenv(capacityModeEnvR501, "on")
	t.Setenv(capacityNodeClaimTTLEnvR501, "900")

	staleAt := time.Now().Add(-2 * time.Hour)
	stale := &Backend{
		ID: "b", Type: BackendTypeLlamaCpp,
		MaxConcurrentReqs:   4,
		NodeClaimedCapacity: 2,
		NodeClaimedAt:       &staleAt,
	}
	eff, src := stale.ResolveCapacity()
	if eff != 4 {
		t.Errorf("вместимость = %d, ожидалось 4: заявка узла старше TTL (2 часа) "+
			"не должна участвовать в решении (аудит C-1)", eff)
	}
	if src != CapacitySourceStatic {
		t.Errorf("источник = %q, ожидалось %q: участвовала статика конфига",
			src, CapacitySourceStatic)
	}

	// Свежая заявка (минуту назад) — участвует, как раньше.
	freshAt := time.Now().Add(-time.Minute)
	fresh := &Backend{
		ID: "b", Type: BackendTypeLlamaCpp,
		MaxConcurrentReqs:   4,
		NodeClaimedCapacity: 2,
		NodeClaimedAt:       &freshAt,
	}
	eff, src = fresh.ResolveCapacity()
	if eff != 2 || src != CapacitySourceNode {
		t.Errorf("свежая заявка: (%d, %q), ожидалось (2, %q)", eff, src, CapacitySourceNode)
	}

	// TTL=0 — возраст не проверяется (явный выход для стендов без heartbeat).
	ResetCapacityNodeClaimTTLR501()
	t.Setenv(capacityNodeClaimTTLEnvR501, "0")
	eff, src = stale.ResolveCapacity()
	if eff != 2 || src != CapacitySourceNode {
		t.Errorf("TTL=0: (%d, %q), ожидалось (2, %q): проверка возраста выключена",
			eff, src, CapacitySourceNode)
	}
}

// TestR501_NodeClaimFreshNilIsTrusted — заявка без отметки (запись старой
// сборки) считается свежей: LoadState проставит ей время записи state.json, а
// «неизвестен возраст» не повод отбрасывать физику узла на старте.
func TestR501_NodeClaimFreshNilIsTrusted(t *testing.T) {
	resetCapacityCachesR501()
	t.Cleanup(resetCapacityCachesR501)
	t.Setenv(capacityNodeClaimTTLEnvR501, "900")

	if !NodeClaimFreshR501(nil, time.Now()) {
		t.Error("заявка без отметки времени не должна считаться просроченной")
	}
	old := time.Now().Add(-time.Hour)
	if NodeClaimFreshR501(&old, time.Now()) {
		t.Error("заявка часовой давности при TTL 900 c должна считаться просроченной")
	}
}

// TestR501_NodeClaimTTLEnvParsing — флаг читается, мусор не ломает дефолт.
func TestR501_NodeClaimTTLEnvParsing(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", capacityNodeClaimTTLDefaultR501},
		{"900", 900 * time.Second},
		{"0", 0},
		{"banana", capacityNodeClaimTTLDefaultR501},
		{"-5", capacityNodeClaimTTLDefaultR501},
	}
	for _, c := range cases {
		resetCapacityCachesR501()
		t.Setenv(capacityNodeClaimTTLEnvR501, c.env)
		if got := CapacityNodeClaimTTLR501(); got != c.want {
			t.Errorf("LB_CAPACITY_NODE_CLAIM_TTL_SEC=%q → %v, ожидалось %v",
				c.env, got, c.want)
		}
	}
	resetCapacityCachesR501()
}
