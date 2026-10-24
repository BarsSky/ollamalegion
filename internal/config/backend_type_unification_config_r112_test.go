// backend_type_unification_config_r112_test.go — R112 (2026-10-11), слой конфига.
//
// Проверяем ровно те три места, из-за которых llama.cpp и Ollama не работали в
// одном кластере:
//  1. ValidateBackendTypeConsistency больше не запрещает смесь типов;
//  2. SanitizeConfigOnModeSwitch не стирает конфиги «чужого» типа и не навязывает
//     backendEngine при смене режима;
//  3. MigrateLegacyBackendTypes не выводит движок кластера из типа ПЕРВОГО
//     бэкенда (иначе смешанный стенд получал один доминирующий движок).
package config

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func mixedBackendsR112() []types.Backend {
	return []types.Backend{
		{ID: "cppworker-gpu", Type: types.BackendTypeLlamaCpp, CppWorkerPort: 18091, Status: types.StatusHealthy},
		{ID: "ollama-node", Type: types.BackendTypeOllama, OllamaPort: 11434, Status: types.StatusHealthy},
	}
}

func TestR112_ValidateBackendTypeConsistency_AllowsMixed(t *testing.T) {
	if err := ValidateBackendTypeConsistency(mixedBackendsR112()); err != nil {
		t.Fatalf("смесь llama.cpp + Ollama объявлена ошибкой: %v", err)
	}
}

func TestR112_ModeCompatibility_AllowsMixedForEveryMode(t *testing.T) {
	for mode := range types.ModeBackendTypes {
		for _, b := range mixedBackendsR112() {
			if err := ValidateModeBackendCompatibility(mode, []types.Backend{b}); err != nil {
				t.Errorf("режим %q несовместим с типом %q: %v", mode, b.Type, err)
			}
		}
		if n := DeactivateIncompatibleBackends(&types.LoadBalancerConfig{Backends: mixedBackendsR112()}, mode); n != 0 {
			t.Errorf("режим %q деактивировал %d бэкенд(ов) — стенд разваливается при смене режима", mode, n)
		}
	}
}

func TestR112_SanitizeConfigOnModeSwitch_KeepsBackendTypes(t *testing.T) {
	for _, mode := range []string{"virtual_router", "distributed_inference", "replication", "rpc_coordinator"} {
		cfg := &types.LoadBalancerConfig{
			BackendEngine: types.EngineAuto,
			Backends:      mixedBackendsR112(),
		}
		SanitizeConfigOnModeSwitch(cfg, mode)

		if cfg.BackendEngine != types.EngineAuto {
			t.Errorf("режим %q навязал backendEngine=%q, want auto", mode, cfg.BackendEngine)
		}
		if cfg.Backends[0].CppWorkerPort == 0 {
			// CppWorkerPort не должен обнуляться ни при каком режиме.
			t.Errorf("режим %q обнулил CppWorkerPort llama.cpp-бэкенда", mode)
		}
		if cfg.Backends[0].Type != types.BackendTypeLlamaCpp {
			t.Errorf("режим %q переписал тип llama.cpp-бэкенда на %q", mode, cfg.Backends[0].Type)
		}
		if cfg.Backends[1].OllamaPort == 0 || cfg.Backends[1].Type != types.BackendTypeOllama {
			t.Errorf("режим %q разобрал Ollama-бэкенд (port=%d type=%q)",
				mode, cfg.Backends[1].OllamaPort, cfg.Backends[1].Type)
		}
	}
}

func TestR112_MigrateLegacyBackendTypes_EngineIsAuto(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "ollama-1", OllamaPort: 11434, Status: types.StatusHealthy},
		},
	}
	MigrateLegacyBackendTypes(cfg)
	if cfg.BackendEngine != types.EngineAuto {
		t.Fatalf("движок выведен из первого бэкенда (%q), want auto: тип одного бэкенда не описывает кластер",
			cfg.BackendEngine)
	}
}
