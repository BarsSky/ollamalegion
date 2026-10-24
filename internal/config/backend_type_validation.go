package config

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// ValidateBackendTypeConsistency — ИСТОРИЧЕСКАЯ проверка «все активные текстовые
// бэкенды одного типа». С R112 (2026-10-11) она ВСЕГДА возвращает nil.
//
// Почему проверка снята. Требование «all text backends must be of the same type»
// прямо запрещало то, ради чего балансер существует: держать в одном кластере
// cppworker (llama.cpp, говорит на Ollama API по /api/*) рядом с Ollama-нодой и
// маршрутизировать запрос туда, где реально загружена модель. На практике запрет
// проявлялся как отказ регистрации второго типа, деактивация бэкендов при смене
// режима и «пропавшие» бэкенды в WebUI.
//
// Функция сохранена (её зовёт ValidateConfigOnLoad и внешние вызовы), чтобы не
// ломать контракт; жёсткие проверки типов убраны.
func ValidateBackendTypeConsistency(backends []types.Backend) error {
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

// SanitizeConfigOnModeSwitch управляет функциональными блоками режима при его
// смене.
//
// R112 (2026-10-11): функция больше НЕ трогает типы бэкендов, их конфиги и
// backendEngine. Раньше переключение на llama.cpp-режим (virtual_router /
// distributed_inference) обнуляло Ollama-поля у ВСЕХ бэкендов и навязывало
// backendEngine=llama_cpp — то есть один щелчок по режиму разбирал смешанный
// стенд. Режим теперь управляет только своими функциональными блоками; тип
// бэкенда — свойство бэкенда.
func SanitizeConfigOnModeSwitch(config *types.LoadBalancerConfig, newMode string) {
	if config == nil {
		return
	}
	// standard и неизвестный режим ничего не включают — как и раньше, не трогаем
	// уже настроенные блоки (оператор мог держать их включёнными осознанно).
	switch newMode {
	case "replication":
		config.Balancing.RpcCoordinator.Enabled = false
		config.Balancing.VirtualModels.Enabled = false
		config.Balancing.DistInference.Enabled = false
	case "rpc_coordinator":
		config.Balancing.ModelReplication.Enabled = false
		config.Balancing.VirtualModels.Enabled = false
		config.Balancing.DistInference.Enabled = false
	case "virtual_router":
		config.Balancing.ModelReplication.Enabled = false
		config.Balancing.RpcCoordinator.Enabled = false
		config.Balancing.DistInference.Enabled = false
	case "distributed_inference":
		config.Balancing.ModelReplication.Enabled = false
		config.Balancing.RpcCoordinator.Enabled = false
		config.Balancing.VirtualModels.Enabled = false
	}
	// backendEngine намеренно не выставляется: с R112 его значение по умолчанию —
	// auto, а фактический движок выводится из состава кластера.
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
	case types.BackendTypeOllama, types.BackendTypeLlamaCpp, types.BackendTypeImage:
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
	// Если BackendEngine не задан — auto.
	//
	// R112 (2026-10-11): раньше здесь движок выводился из типа ПЕРВОГО бэкенда,
	// из-за чего смешанный кластер получал движок одного типа и второй пул
	// отфильтровывался. Тип отдельного бэкенда не описывает кластер целиком.
	if config.BackendEngine == "" {
		config.BackendEngine = types.EngineAuto
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

	// R112 (2026-10-11): проверка «BackendEngine соответствует OperatingMode»
	// убрана. Режим больше не владеет движком (ModeEngines везде auto), поэтому
	// предупреждение срабатывало бы на КАЖДОМ конфиге с осознанно выбранным
	// backendEngine и не сообщало ничего полезного. Фактический движок выводится
	// из состава кластера: balancer.getBackendEngine().

	// R72 (P0): валидация placement-политики. Ошибки не блокируют запуск:
	// они видны в предупреждениях при загрузке и в GET /api/v1/placement.
	warnings = append(warnings, config.Balancing.Placement.Validate()...)

	// R92 (S2): валидация логических бэкендов (balancing.backendGroups[]).
	// Проверяются состав (ровно один master, ≥1 worker с rpcEndpoint),
	// уникальность id и допустимость strategy. Семантику «влезает ли модель в
	// группу» проверяет не конфиг, а резолвер: она зависит от живых метрик.
	warnings = append(warnings, types.ValidateBackendGroups(config.Balancing.BackendGroups)...)

	return warnings
}
