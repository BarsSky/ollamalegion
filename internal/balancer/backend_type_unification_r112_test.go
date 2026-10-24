// backend_type_unification_r112_test.go — R112 (2026-10-11).
//
// ЗАЧЕМ. До R112 тип бэкенда был связан с operatingMode и с сохранённым
// backendEngine:
//   - ModeBackendTypes разрешал virtual_router/distributed_inference только для
//     llama_cpp, а остальные режимы — для оба текстовых типов;
//   - ModeEngines жёстко назначал llama.cpp этим двум режимам;
//   - config.Load выводил backendEngine из режима и СОХРАНЯЛ его;
//   - determineRequestBackendType и getEffectiveBackendType считали явный
//     backendEngine фильтром и разрешали только один тип.
//
// Живое следствие: cppworker (llama.cpp) и Ollama-нода не уживались в одном
// кластере — второй тип выпадал из маршрутизации и из выдачи WebUI, хотя оба
// обслуживают одну и ту же Ollama-поверхность /api/*. Требование оператора:
// «Ollama API должно работать одинаково для всех клиентов» и «llama и ollama
// должны работать вместе».
//
// ЧТО ФИКСИРУЕМ:
//  1. любой режим совместим с любым текстовым типом и не назначает движок;
//  2. «залипший» backendEngine в конфиге НЕ сужает /api/* до одного типа;
//  3. устаревший backendEngine (типа нет в кластере) не прячет второй тип;
//  4. осознанный backendEngine уважается, пока бэкенды этого типа есть;
//  5. смешанный кластер → движок auto, effective type пуст (показывать оба).
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR112_AllModesAllowBothTextTypes — режим больше не ограничивает тип бэкенда.
func TestR112_AllModesAllowBothTextTypes(t *testing.T) {
	if len(types.ModeBackendTypes) == 0 {
		t.Fatal("ModeBackendTypes пуст — тест потерял смысл")
	}
	for mode := range types.ModeBackendTypes {
		for _, bt := range []types.BackendType{types.BackendTypeOllama, types.BackendTypeLlamaCpp} {
			if !types.IsModeCompatibleWithBackendType(mode, bt) {
				t.Errorf("режим %q не допускает тип %q — llama.cpp и Ollama не смогут работать вместе",
					mode, bt)
			}
		}
		if engine := types.ModeEngines[mode]; engine != types.EngineAuto {
			t.Errorf("режим %q назначает движок %q, want auto (режим не владеет движком)",
				mode, engine)
		}
	}
}

// TestR112_OllamaSurfaceIgnoresConfiguredEngine — «залипший» backendEngine не сужает /api/*.
//
// Сценарий с живой машины: в config.json лежит backendEngine=llama_cpp (он был
// выведен из operatingMode при инициализации старой версией и сохранён).
// Раньше /api/chat в смешанном кластере уезжал ТОЛЬКО на llama.cpp-бэкенды, и
// модель, загруженная на Ollama-ноде, была «не найдена».
func TestR112_OllamaSurfaceIgnoresConfiguredEngine(t *testing.T) {
	p := proxyWithTextBackends(t, types.BackendTypeOllama, types.BackendTypeLlamaCpp)
	p.config.BackendEngine = types.EngineLlamaCPP

	for _, path := range []string{"/api/chat", "/api/generate", "/api/tags", "/api/show"} {
		got := p.determineRequestBackendType(requestForPath(t, path, ""))
		if got != "" {
			t.Errorf("путь %s разрешён только для %q — второй текстовый тип отрезан конфигом", path, got)
		}
	}

	allowed := p.getDefaultAllowedTypes()
	if len(allowed) != 2 {
		t.Fatalf("getDefaultAllowedTypes() = %v, want оба текстовых типа", allowed)
	}
}

// TestR112_StaleEngineDoesNotHideOtherType — устаревший backendEngine не прячет второй тип.
func TestR112_StaleEngineDoesNotHideOtherType(t *testing.T) {
	p := proxyWithTextBackends(t, types.BackendTypeOllama)
	p.config.BackendEngine = types.EngineLlamaCPP // в кластере llama.cpp нет

	if got := p.getBackendEngine(); got != types.EngineOllamaAPI {
		t.Errorf("движок = %q, want ollama_api: конфиг ссылается на отсутствующий тип", got)
	}
	if got := p.getEffectiveBackendType(); got != types.BackendTypeOllama {
		t.Errorf("effectiveBackendType = %q, want ollama: иначе Ollama-бэкенд исчезнет из WebUI", got)
	}
}

// TestR112_ExplicitEngineHonouredWhilePresent — осознанный выбор уважается, пока тип есть.
func TestR112_ExplicitEngineHonouredWhilePresent(t *testing.T) {
	mixed := proxyWithTextBackends(t, types.BackendTypeOllama, types.BackendTypeLlamaCpp)
	mixed.config.BackendEngine = types.EngineLlamaCPP
	if got := mixed.getBackendEngine(); got != types.EngineLlamaCPP {
		t.Errorf("явный выбор llama_cpp при живом llama.cpp-бэкенде: движок = %q", got)
	}

	// Но доминирующий ТИП по-прежнему считается по составу: смесь → «показывать оба».
	if got := mixed.getEffectiveBackendType(); got != "" {
		t.Errorf("смешанный кластер: effectiveBackendType = %q, want \"\"", got)
	}
}

// TestR112_EngineAutoForMixedCluster — оба типа → auto.
func TestR112_EngineAutoForMixedCluster(t *testing.T) {
	p := proxyWithTextBackends(t, types.BackendTypeOllama, types.BackendTypeLlamaCpp)
	if got := p.getBackendEngine(); got != types.EngineAuto {
		t.Errorf("смешанный кластер: движок = %q, want auto", got)
	}
}

// TestR112_ModeSwitchDoesNotNarrowTypes — смена режима не сужает допустимые типы.
//
// Раньше переключение на virtual_router/distributed_inference означало
// «только llama.cpp»: DeactivateIncompatibleBackends гасил Ollama-ноды, а
// SanitizeConfigOnModeSwitch стирал их конфиги.
func TestR112_ModeSwitchDoesNotNarrowTypes(t *testing.T) {
	for _, mode := range []string{"standard", "replication", "rpc_coordinator", "virtual_router", "distributed_inference"} {
		p := proxyWithTextBackends(t, types.BackendTypeOllama, types.BackendTypeLlamaCpp)
		p.config.Balancing.OperatingMode = mode

		allowed := p.getDefaultAllowedTypes()
		if len(allowed) != 2 {
			t.Errorf("режим %q: allowedTypes = %v, want оба текстовых типа", mode, allowed)
		}
		if got := p.getEffectiveBackendType(); got != "" {
			t.Errorf("режим %q: effectiveBackendType = %q, want \"\" (смешанный кластер)", mode, got)
		}
	}
}
