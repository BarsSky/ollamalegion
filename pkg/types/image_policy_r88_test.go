package types

import "testing"

// ============================================================
// R88 (2026-10-08): предохранитель GPU-лока снят по умолчанию.
//
// Доктрина таймаутов (internal/balancer/timeout_policy.go) запрещает
// duration-кап на работу. Живой случай: при капе 600 с лок снимался ПОСРЕДИ
// легитимной генерации — 2048x2048/40 шагов на RTX 3070 идёт 22m30s — и
// текстовый трафик пускался на карту, которая ещё считает. Лок освобождается по
// жизненному циклу запроса; кап — только явный opt-in оператора.
// ============================================================

// TestR88_DefaultLockFuseDisarmed — дефолт = «предохранитель не взведён».
func TestR88_DefaultLockFuseDisarmed(t *testing.T) {
	if DefaultImageExclusiveLockTimeoutSec != 0 {
		t.Fatalf("DefaultImageExclusiveLockTimeoutSec = %d, want 0 (кап на работу — только opt-in)",
			DefaultImageExclusiveLockTimeoutSec)
	}
	var s ImageResourceSettings
	if got := s.EffectiveExclusiveLockTimeout(); got != 0 {
		t.Errorf("EffectiveExclusiveLockTimeout() без настройки = %d, want 0 (без будильника)", got)
	}
}

// TestR88_ExplicitLockFuseStillHonoured — явно взведённый оператором кап
// работает как раньше: на нашем стенде он выставлен в 6000 с.
func TestR88_ExplicitLockFuseStillHonoured(t *testing.T) {
	s := ImageResourceSettings{ExclusiveLockTimeoutSec: 6000}
	if got := s.EffectiveExclusiveLockTimeout(); got != 6000 {
		t.Errorf("явный предохранитель не применён: %d, want 6000", got)
	}
}
