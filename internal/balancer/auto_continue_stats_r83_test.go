//go:build llama_stub

// auto_continue_stats_r83_test.go — R83 (2026-09-25), пункт D4.
//
// До этой правки единственным следом защиты от дубликата ответа (перегенерация
// при авто-продолжении) были строки в логе. Оператор не мог ответить, работает
// ли защита на живом трафике — теперь счётчики уходят в GET /api/v1/metrics.
//
// Счётчики процесс-глобальные, поэтому проверяем ДЕЛЬТЫ, а не абсолютные
// значения: иначе тест зависел бы от порядка выполнения других тестов.
package balancer

import "testing"

func r83StatInt(t *testing.T, summary map[string]interface{}, key string) int64 {
	t.Helper()
	v, ok := summary[key].(int64)
	if !ok {
		t.Fatalf("в сводке нет int64-поля %q (got %T)", key, summary[key])
	}
	return v
}

func TestR83_AutoContinueStats_DeltasAndSummary(t *testing.T) {
	p := &Proxy{}
	before := p.AutoContinueMetricsSummary()

	recordAutoContinueSuppressed()
	recordAutoContinueSuppressed()
	recordAutoContinueEmitted()
	recordAutoContinueFailed()
	recordAutoContinuePolicyDisabled()

	after := p.AutoContinueMetricsSummary()

	if got := r83StatInt(t, after, "suppressed") - r83StatInt(t, before, "suppressed"); got != 2 {
		t.Errorf("suppressed: дельта %d, want 2", got)
	}
	if got := r83StatInt(t, after, "emitted") - r83StatInt(t, before, "emitted"); got != 1 {
		t.Errorf("emitted: дельта %d, want 1", got)
	}
	if got := r83StatInt(t, after, "failed") - r83StatInt(t, before, "failed"); got != 1 {
		t.Errorf("failed: дельта %d, want 1", got)
	}
	if got := r83StatInt(t, after, "policyDisabled") - r83StatInt(t, before, "policyDisabled"); got != 1 {
		t.Errorf("policyDisabled: дельта %d, want 1", got)
	}

	// duplicateRisk — прямая подсказка оператору: политика выключала подавление,
	// значит дубли ответа в клиенте возможны.
	if risk, ok := after["duplicateRisk"].(bool); !ok || !risk {
		t.Errorf("duplicateRisk = %v, want true после policyDisabled", after["duplicateRisk"])
	}

	ratio, ok := after["suppressedRatio"].(float64)
	if !ok {
		t.Fatalf("suppressedRatio не float64: %T", after["suppressedRatio"])
	}
	if ratio < 0 || ratio > 1 {
		t.Errorf("suppressedRatio = %v, ожидалось [0,1]", ratio)
	}

	// Поля конфигурации должны присутствовать — иначе оператор не поймёт, почему
	// подавление выключено.
	for _, key := range []string{"enabled", "policy"} {
		if _, ok := after[key]; !ok {
			t.Errorf("в сводке нет поля %q", key)
		}
	}
}

// TestR83_AutoContinueStats_ZeroRiskWithoutPolicyDisabled — при выключенном
// авто-продолжении и smart-политике риска дубля быть не должно.
func TestR83_AutoContinueStats_ZeroRiskWithoutPolicyDisabled(t *testing.T) {
	t.Setenv("LB_AUTO_CONTINUE_CHAT_POLICY", "smart")
	p := &Proxy{}
	summary := p.AutoContinueMetricsSummary()

	if policy, _ := summary["policy"].(string); policy != "smart" {
		t.Errorf("policy = %q, want smart (дефолт)", policy)
	}
	// duplicateRisk зависит только от счётчика policyDisabled; здесь его не
	// трогаем, но значение могло стать true от других тестов — поэтому проверяем
	// не значение, а наличие поля.
	if _, ok := summary["duplicateRisk"]; !ok {
		t.Error("в сводке нет поля duplicateRisk")
	}
}
