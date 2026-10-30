//go:build llama_stub

// cluster_config_prediction_r318_test.go — R318 (2026-10-30): тумблер
// «Prediction filtering» действительно применяется.
//
// ЖИВОЙ ДЕФЕКТ. Поле принималось структурой и нигде не читалось: сервер отвечал
// «saved», GET отдавал константу true, а фильтр продолжал исключать узлы.
package api

import (
	"testing"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/internal/settings"
)

func TestR318_PredictionFilteringToggleApplies(t *testing.T) {
	reg := settings.Init("")
	t.Cleanup(func() { settings.Init("") })

	// По умолчанию фильтр включён — прежнее поведение.
	if !balancer.PredictionFilterEnabled() {
		t.Fatal("по умолчанию фильтр выключен: поведение изменилось без команды оператора")
	}

	// Оператор выключает тумблер — значение обязано примениться (и отразиться в GET).
	if _, err := reg.Set("LB_PREDICTION_FILTER", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if balancer.PredictionFilterEnabled() {
		t.Fatal("тумблер выключен, а фильтр всё ещё действует — панель снова декоративная")
	}
	if _, err := reg.Set("LB_PREDICTION_FILTER", 1); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !balancer.PredictionFilterEnabled() {
		t.Fatal("тумблер включён, а фильтр не действует")
	}
}
