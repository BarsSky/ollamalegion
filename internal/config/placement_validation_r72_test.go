package config

// R72 (P0): валидация placement-политики на загрузке конфига.
// План: plans/2026-09-23-multi-backend-placement-policy.md

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestValidateConfigOnLoad_PlacementWarnings_R72(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		Backends: []types.Backend{{
			ID: "b1", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp,
		}},
		Balancing: types.BalancingSettings{
			OperatingMode: "standard",
			Placement: types.PlacementSettings{
				Enabled: true,
				Models: []types.PlacementModelRule{
					{Model: "a", Strategy: "spread"},                     // неизвестная стратегия
					{Model: "b", Strategy: "pool"},                       // pool без списка
					{Model: "c", Strategy: "sharded"},                    // не исполняется (P2)
					{Model: "", Strategy: "single"},                      // пустое имя
					{Model: "d", Strategy: "single", Selection: "magic"}, // неизвестный selection
				},
				Classes: []types.PlacementClassRule{
					{Match: types.PlacementMatch{SizeGB: ">abc"}, Strategy: "single"},
					{Match: types.PlacementMatch{}, Strategy: "single"},
				},
			},
		},
	}

	warnings := ValidateConfigOnLoad(cfg)
	if len(warnings) < 7 {
		t.Fatalf("ожидались предупреждения о placement-политике, получено %d: %v", len(warnings), warnings)
	}
	joined := strings.ToLower(strings.Join(warnings, " | "))
	for _, want := range []string{"placement.models[0]", "placement.models[1]", "placement.models[2]", "placement.classes[0]", "sizegb"} {
		if !strings.Contains(joined, strings.ToLower(want)) {
			t.Errorf("в предупреждениях нет %q: %v", want, warnings)
		}
	}
}

func TestValidateConfigOnLoad_CorrectPlacementNoWarnings_R72(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		Backends: []types.Backend{{
			ID: "b1", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp,
		}},
		Balancing: types.BalancingSettings{
			OperatingMode: "standard",
			Placement: types.PlacementSettings{
				Enabled:  true,
				Fallback: types.PlacementFallbackSingle,
				Models: []types.PlacementModelRule{
					{Model: "gemma-4*", Strategy: "single", Reason: "влезает в 8 ГБ"},
					{Model: "virtual:prod", Strategy: "pool", Pool: []string{"a:18092"}, Selection: "least_loaded"},
				},
				Classes: []types.PlacementClassRule{
					{Match: types.PlacementMatch{SizeGB: ">12"}, Strategy: "auto"},
				},
			},
		},
	}

	for _, w := range ValidateConfigOnLoad(cfg) {
		if strings.Contains(strings.ToLower(w), "placement") {
			t.Errorf("корректная политика дала предупреждение: %s", w)
		}
	}
}

// Обратная совместимость: конфиг без секции placement не даёт предупреждений
// и не меняет поведение (enabled=false по умолчанию).
func TestValidateConfigOnLoad_NoPlacementSection_R72(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		Backends: []types.Backend{{
			ID: "b1", Host: "localhost", CppWorkerPort: 18092, Type: types.BackendTypeLlamaCpp,
		}},
		Balancing: types.BalancingSettings{OperatingMode: "standard"},
	}
	for _, w := range ValidateConfigOnLoad(cfg) {
		if strings.Contains(strings.ToLower(w), "placement") {
			t.Errorf("конфиг без placement не должен давать предупреждений: %s", w)
		}
	}
}
