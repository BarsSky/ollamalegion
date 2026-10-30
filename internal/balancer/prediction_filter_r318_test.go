//go:build llama_stub

// prediction_filter_r318_test.go — R318 (2026-10-30): фильтр по прогнозу — настройка.
//
// ЖИВОЙ ДЕФЕКТ. Тумблер «Prediction filtering» в панели сервер принимал и молча не
// применял, GET всегда отдавал true. При этом фильтр — жёсткое исключение: узел с
// Prediction.SecondsToCritical < 300 выпадает из кандидатов, и при шумном прогнозе
// клиент получает «нет доступных бэкендов» на здоровом стенде.
package balancer

import (
	"testing"

	"ollama-loadbalancer/internal/settings"
)

func TestR318_PredictionFilterIsSetting(t *testing.T) {
	reg := withSettingsRegistry(t)

	def, ok := reg.Definition("LB_PREDICTION_FILTER")
	if !ok {
		t.Fatal("LB_PREDICTION_FILTER не зарегистрирована")
	}
	if !def.HotReload {
		t.Error("HotReload = false, а обязана меняться без рестарта")
	}
	if def.Default != 1 {
		t.Errorf("Default = %d, want 1 (поведение по умолчанию не меняем)", def.Default)
	}
	if def.Validate != nil {
		if err := def.Validate(def.Default); err != nil {
			t.Errorf("дефолт не проходит собственную проверку: %v", err)
		}
	}

	// По умолчанию фильтр включён — прежнее поведение.
	if !predictedCriticalR318(120) {
		t.Error("по умолчанию узел с 120 c до критического ресурса обязан исключаться")
	}
	if predictedCriticalR318(900) {
		t.Error("900 c до критического ресурса — не повод исключать узел")
	}
	if predictedCriticalR318(0) {
		t.Error("нулевой прогноз (нет данных) не должен исключать узел")
	}

	// Выключили — фильтр не исключает никого.
	if _, err := reg.Set("LB_PREDICTION_FILTER", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if predictedCriticalR318(120) {
		t.Error("фильтр выключен, а узел всё равно исключён")
	}
	if PredictionFilterEnabled() {
		t.Error("PredictionFilterEnabled() = true после выключения")
	}

	// Включили обратно — действует сразу (hot).
	if _, err := reg.Set("LB_PREDICTION_FILTER", 1); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !predictedCriticalR318(120) {
		t.Error("фильтр включён, а узел не исключён")
	}
	_ = settings.Default()
}
