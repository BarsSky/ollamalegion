package config

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// ValidateBackendTypeConsistency проверяет, что все активные бэкенды одного типа.
// Возвращает ошибку, если обнаружены бэкенды разных типов.
func ValidateBackendTypeConsistency(backends []types.Backend) error {
	if len(backends) == 0 {
		return nil
	}

	activeBackends := make([]types.Backend, 0, len(backends))
	for _, b := range backends {
		if b.Status != types.StatusOffline {
			activeBackends = append(activeBackends, b)
		}
	}

	if len(activeBackends) <= 1 {
		return nil
	}

	// Все активные бэкенды должны быть одного типа
	firstType := normalizeBackendType(activeBackends[0].Type)
	for _, b := range activeBackends[1:] {
		bt := normalizeBackendType(b.Type)
		if bt != firstType {
			return fmt.Errorf("backend type mismatch: backend %q is %s, but backend %q is %s — all backends must be of the same type (ollama or llama_cpp)",
				activeBackends[0].ID, firstType.Label(), b.ID, bt.Label())
		}
	}
	return nil
}

// ValidateModeBackendCompatibility проверяет, что operatingMode совместим с типом бэкендов.
// Возвращает ошибку, если режим не поддерживает текущие бэкенды.
func ValidateModeBackendCompatibility(mode string, backends []types.Backend) error {
	if mode == "" {
		return nil // режим не задан — считаем совместимым
	}

	for _, b := range backends {
		if b.Status == types.StatusOffline {
			continue
		}
		bt := normalizeBackendType(b.Type)
		if !types.IsModeCompatibleWithBackendType(mode, bt) {
			return fmt.Errorf("operating mode %q requires %s backends, but backend %q is of type %s",
				mode, modeRequiredTypes(mode), b.ID, bt.Label())
		}
	}
	return nil
}

// SanitizeConfigOnModeSwitch очищает нерелевантные настройки при смене режима.
// Например, при переключении на llama.cpp-режим удаляет OllamaConfig у бэкендов.
func SanitizeConfigOnModeSwitch(config *types.LoadBalancerConfig, newMode string) {
	isOllama := types.IsModeOllama(newMode)
	isLlamaCpp := types.IsModeLlamaCpp(newMode)

	// Очищаем нерелевантные RPC-конфиги
	if isOllama {
		config.Balancing.DistInference.Enabled = false
		config.Balancing.VirtualModels.Enabled = false
	} else if isLlamaCpp {
		config.Balancing.ModelReplication.Enabled = false
		config.Balancing.RpcCoordinator.Enabled = false
	}

	// Очищаем настройки бэкендов
	for i := range config.Backends {
		b := &config.Backends[i]
		if isOllama {
			// Для Ollama удаляем llama.cpp-конфиг
			b.CppWorkerConfig = nil
			b.CppWorkerPort = 0
			b.Type = types.BackendTypeOllama
		} else if isLlamaCpp {
			// Для llama.cpp удаляем Ollama-конфиг
			b.OllamaConfig = nil
			b.OllamaPort = 0
			b.AgentPort = 0
			b.HasAgent = false
			b.Type = types.BackendTypeLlamaCpp
		}
	}

	// Устанавливаем движок
	if isOllama {
		config.BackendEngine = types.EngineOllamaAPI
	} else if isLlamaCpp {
		config.BackendEngine = types.EngineLlamaCPP
	}
}

// DeactivateIncompatibleBackends помечает бэкенды несовместимого типа как offline.
// Вызывается при смене режима на несовместимый с текущими бэкендами.
func DeactivateIncompatibleBackends(config *types.LoadBalancerConfig, mode string) int {
	deactivated := 0
	allowedTypes, ok := types.ModeBackendTypes[mode]
	if !ok {
		return 0 // неизвестный режим — ничего не трогаем
	}

	allowed := make(map[types.BackendType]bool)
	for _, t := range allowedTypes {
		allowed[t] = true
	}

	for i := range config.Backends {
		b := &config.Backends[i]
		if b.Status == types.StatusOffline {
			continue
		}
		if !allowed[normalizeBackendType(b.Type)] {
			b.Status = types.StatusOffline
			deactivated++
		}
	}
	return deactivated
}

// normalizeBackendType приводит тип бэкенда к стандартному значению.
// Пустой тип считается Ollama (для совместимости со старыми конфигами).
func normalizeBackendType(bt types.BackendType) types.BackendType {
	return normalizeBackendTypeWithHeuristic(bt, 0)
}

// normalizeBackendTypeWithHeuristic — нормализация с эвристикой по CppWorkerPort.
// Если тип не задан, но CppWorkerPort > 0 → llama.cpp.
func normalizeBackendTypeWithHeuristic(bt types.BackendType, cppWorkerPort int) types.BackendType {
	if bt == "" {
		if cppWorkerPort > 0 {
			return types.BackendTypeLlamaCpp
		}
		return types.BackendTypeOllama
	}
	switch bt {
	case types.BackendTypeOllama, types.BackendTypeLlamaCpp:
		return bt
	default:
		return types.BackendTypeOllama // неизвестный → Ollama
	}
}

// modeRequiredTypes возвращает строку с перечислением требуемых типов для режима
func modeRequiredTypes(mode string) string {
	allowed, ok := types.ModeBackendTypes[mode]
	if !ok {
		return "any"
	}
	labels := make([]string, len(allowed))
	for i, t := range allowed {
		labels[i] = t.Label()
	}
	return strings.Join(labels, " or ")
}

// MigrateLegacyBackendTypes выполняет миграцию старых конфигов без поля Type.
// Использует эвристику: если CppWorkerPort > 0 → llama.cpp, иначе ollama.
func MigrateLegacyBackendTypes(config *types.LoadBalancerConfig) {
	for i := range config.Backends {
		if config.Backends[i].Type == "" {
			if config.Backends[i].CppWorkerPort > 0 {
				config.Backends[i].Type = types.BackendTypeLlamaCpp
			} else {
				config.Backends[i].Type = types.BackendTypeOllama
			}
		}
	}
	// Если BackendEngine не задан и есть бэкенды, определяем из типа
	if config.BackendEngine == "" && len(config.Backends) > 0 {
		firstType := normalizeBackendType(config.Backends[0].Type)
		switch firstType {
		case types.BackendTypeOllama:
			config.BackendEngine = types.EngineOllamaAPI
		case types.BackendTypeLlamaCpp:
			config.BackendEngine = types.EngineLlamaCPP
		}
	}
}

// ValidateConfigOnLoad выполняет полную валидацию конфигурации при загрузке.
// Выполняет миграцию, проверку консистентности и возвращает предупреждения.
func ValidateConfigOnLoad(config *types.LoadBalancerConfig) []string {
	var warnings []string

	// Миграция legacy-конфигов
	MigrateLegacyBackendTypes(config)

	// Проверка консистентности типов бэкендов
	if err := ValidateBackendTypeConsistency(config.Backends); err != nil {
		warnings = append(warnings, err.Error())
	}

	// Проверка совместимости режима с бэкендами
	if config.Balancing.OperatingMode != "" {
		if err := ValidateModeBackendCompatibility(config.Balancing.OperatingMode, config.Backends); err != nil {
			warnings = append(warnings, err.Error())
		}
	}

	// Проверка соответствия BackendEngine и OperatingMode
	if config.BackendEngine != "" && config.Balancing.OperatingMode != "" {
		expectedEngine, ok := types.ModeEngines[config.Balancing.OperatingMode]
		if ok && expectedEngine != config.BackendEngine {
			warnings = append(warnings,
				fmt.Sprintf("BackendEngine %q does not match OperatingMode %q (expected %q)",
					config.BackendEngine, config.Balancing.OperatingMode, expectedEngine))
		}
	}

	return warnings
}