// metrics_autocontinue_r83_test.go — R83 (2026-09-25), пункт D4.
//
// Сводка авто-продолжения в GET /api/v1/metrics: оператор видит, работает ли
// защита от дубликата ответа, не читая логи.
package api

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestMetricsAutoContinueSummary_R83(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "b1", Host: "localhost", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()

	metrics := getMetricsJSONR78(t, s)

	raw, ok := metrics["autoContinue"]
	if !ok {
		t.Fatal("в GET /api/v1/metrics нет блока autoContinue — оператор не увидит работу защиты от дубликата")
	}
	ac, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("autoContinue не объект: %T", raw)
	}

	// Все ключи, по которым принимается решение «работает ли защита».
	for _, key := range []string{
		"enabled", "policy", "suppressed", "emitted", "failed",
		"policyDisabled", "suppressedRatio", "duplicateRisk",
	} {
		if _, ok := ac[key]; !ok {
			t.Errorf("в блоке autoContinue нет поля %q (got %v)", key, ac)
		}
	}

	// Числовые счётчики — JSON-числа, а не строки (иначе парсеры/UI споткнутся).
	for _, key := range []string{"suppressed", "emitted", "failed", "policyDisabled"} {
		if _, ok := ac[key].(float64); !ok {
			t.Errorf("autoContinue.%s = %T, want число", key, ac[key])
		}
	}
	if _, ok := ac["suppressedRatio"].(float64); !ok {
		t.Errorf("autoContinue.suppressedRatio = %T, want число", ac["suppressedRatio"])
	}
	if _, ok := ac["duplicateRisk"].(bool); !ok {
		t.Errorf("autoContinue.duplicateRisk = %T, want bool", ac["duplicateRisk"])
	}
	if policy, _ := ac["policy"].(string); policy == "" {
		t.Error("autoContinue.policy пуст — не видно, какая политика подавления активна")
	}
}
