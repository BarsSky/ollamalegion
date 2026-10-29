// runtime_overrides_r119.go — R119 (2026-10-15): настройки, которые оператор
// меняет из WebUI, обязаны (а) применяться на ходу и (б) переживать пересоздание
// контейнера, имея ПРИОРИТЕТ над значением из окружения.
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ ФАЙЛ, А НЕ cppworker-defaults.json. Порядок загрузки
// конфигурации в cppbackend: JSON-дефолты → окружение (`CPPWORKER_*` ДОЛЖНЫ
// побеждать файл). Поэтому значение, записанное в defaults-файл, на рестарте
// ЗАТИРАЕТСЯ переменной окружения:
//
//	CPPWORKER_IDLE_UNLOAD_MINUTES=10 в compose + PUT idleUnloadMinutes=3 из WebUI
//	→ конфиг на ходу 3 (верно) → пересоздание контейнера → снова 10.
//
// Живой случай ровно такой: `.env`-документация обещала «0 = выключено», compose
// ставил 10, а правка из WebUI не доживала до перезапуска — оператор видел
// «настройка не сохраняется».
//
// Решение: файл явных операторских переопределений, который читается ПОСЛЕ
// окружения и потому имеет приоритет. Путь задаётся CPPWORKER_CONFIG_DIR (как у
// defaults-файла) и ДОЛЖЕН лежать на смонтированном томе, иначе пересоздание
// контейнера его сотрёт (в compose это делает CPPWORKER_CONFIG_DIR=/app/.cache).
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// runtimeOverridesFileName — имя файла операторских переопределений.
const runtimeOverridesFileName = "cppworker-runtime-overrides.json"

// runtimeOverridesPath — путь к файлу переопределений.
func runtimeOverridesPath() string {
	dir := strings.TrimSpace(os.Getenv("CPPWORKER_CONFIG_DIR"))
	if dir == "" {
		dir = "config"
	}
	return filepath.Join(dir, runtimeOverridesFileName)
}

// loadRuntimeOverrides читает файл переопределений (нет файла = пусто, не ошибка).
func loadRuntimeOverrides() map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	raw, err := os.ReadFile(runtimeOverridesPath())
	if err != nil {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		logger.Get().Warnw(i18n.T("cppworker.runtime_overrides_r119.file_unparsed"),
			"path", runtimeOverridesPath(), "error", err)
		return map[string]json.RawMessage{}
	}
	return out
}

// saveRuntimeOverrides пишет файл переопределений (создаёт каталог при нужде).
func saveRuntimeOverrides(overrides map[string]json.RawMessage) error {
	path := runtimeOverridesPath()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(overrides, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// recordRuntimeOverride — запомнить одно переопределение (ключ = имя поля в
// PUT /config/update, значение = JSON-значение).
func recordRuntimeOverride(key string, value interface{}) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	overrides := loadRuntimeOverrides()
	encoded, err := json.Marshal(value)
	if err != nil {
		logger.Get().Warnw(i18n.T("cppworker.runtime_overrides_r119.value_not_serializable"),
			"key", key, "error", err)
		return
	}
	overrides[key] = encoded
	if err := saveRuntimeOverrides(overrides); err != nil {
		logger.Get().Warnw(i18n.T("cppworker.runtime_overrides_r119.save_failed"),
			"path", runtimeOverridesPath(), "key", key, "error", err)
		return
	}
	logger.Get().Infow(i18n.T("cppworker.runtime_overrides_r119.saved"),
		"path", runtimeOverridesPath(), "key", key, "value", string(encoded))
}

// appliedListContains — есть ли ключ в списке применённых обновлений.
func appliedListContains(applied []string, key string) bool {
	for _, a := range applied {
		if a == key {
			return true
		}
	}
	return false
}

// applyRuntimeOverrides — наложить операторские переопределения ПОВЕРХ
// окружения. Поддерживаются: idleUnloadMinutes, rpcServerEnabled/rpcServerPort
// (узел раскладки этой машины) и rpcEnabled/rpcServers (состав раскладки,
// который назначает балансер) — ровно те настройки, для которых приоритет над
// env прямо требуется. Остальные ключи сохраняются в файле, но не применяются,
// чтобы не менять молча поведение параметров, где окружение обязано побеждать.
//
// Возвращает список применённых ключей (для лога).
func applyRuntimeOverrides(cfg *cppbackend.Config) []string {
	if cfg == nil {
		return nil
	}
	overrides := loadRuntimeOverrides()
	if len(overrides) == 0 {
		return nil
	}
	var applied []string

	if raw, ok := overrides["idleUnloadMinutes"]; ok {
		var minutes int
		if err := json.Unmarshal(raw, &minutes); err == nil && minutes >= 0 {
			if cfg.IdleUnloadMinutes != minutes {
				logger.Get().Infow(i18n.T("cppworker.runtime_overrides_r119.idle_unload_overridden"),
					"env_or_default", cfg.IdleUnloadMinutes, "operator_value", minutes)
			}
			cfg.IdleUnloadMinutes = minutes
			applied = append(applied, "idleUnloadMinutes")
		} else {
			logger.Get().Warnw(i18n.T("cppworker.runtime_overrides_r119.idle_unload_unparsed"),
				"value", string(raw))
		}
	}

	// R123 (2026-10-16, пункт 2): узел раскладки (донор) — тот же контракт, что у
	// idleUnloadMinutes: включение из WebUI действует на ходу И переживает
	// пересоздание контейнера, побеждая CPPWORKER_RPC_SERVER_ENABLED из окружения.
	//
	// ЗАЧЕМ ПРИОРИТЕТ НАД ENV. Оператор включает узел галочкой в настройках
	// бэкенда; если env-значение побеждало, после пересоздания контейнера галочка
	// тихо снималась бы — ровно класс дефектов «настройка не сохраняется»,
	// который уже разбирали на idleUnloadMinutes.
	if raw, ok := overrides["rpcServerEnabled"]; ok {
		var enabled bool
		if err := json.Unmarshal(raw, &enabled); err == nil {
			if cfg.RPCServerEnabled != enabled {
				logger.Get().Infow(i18n.T("cppworker.runtime_overrides_r119.rpc_server_enabled_overridden"),
					"env_or_default", cfg.RPCServerEnabled, "operator_value", enabled)
			}
			cfg.RPCServerEnabled = enabled
			applied = append(applied, "rpcServerEnabled")
		} else {
			logger.Get().Warnw(i18n.T("cppworker.runtime_overrides_r119.rpc_server_enabled_unparsed"), "value", string(raw))
		}
	}
	if raw, ok := overrides["rpcServerPort"]; ok {
		var port int
		if err := json.Unmarshal(raw, &port); err == nil && port > 0 && port <= 65535 {
			cfg.RPCServerPort = port
			applied = append(applied, "rpcServerPort")
		} else {
			logger.Get().Warnw(i18n.T("cppworker.runtime_overrides_r119.rpc_server_port_unparsed"), "value", string(raw))
		}
	}

	// R304 (2026-10-30): РОЛЬ БЭКЕНДА — тот же контракт, что у узла выше.
	//
	// ЗАЧЕМ. Роль объявляет балансер, но контейнер пересоздаётся независимо от
	// него: без файла переопределений машина после recreate снова считала бы себя
	// «ролью без объявления» и начала бы поднимать свои копии моделей — при том
	// что мастер уже спланировал раскладку по её памяти. Источник значения —
	// только приказ (backendRole в PUT /config/update), env здесь принципиально
	// не читается.
	if raw, ok := overrides["backendRole"]; ok {
		var role string
		if err := json.Unmarshal(raw, &role); err != nil {
			logger.Get().Warnw(i18n.T("cppworker.donor.role_override_unparsed"), "value", string(raw))
		} else if norm, known := cppbackend.NormalizeBackendRole(role); !known {
			// Неизвестное значение НЕ применяем: «donor» с опечаткой не должен
			// ни запрещать загрузку, ни тихо превращаться в другую роль.
			logger.Get().Warnw(i18n.T("cppworker.donor.role_unknown_value"), "value", role)
		} else {
			if cfg.BackendRole != norm {
				logger.Get().Infow(i18n.T("cppworker.donor.role_applied"),
					"config_value", cfg.BackendRole, "applied_value", norm)
			}
			cfg.BackendRole = norm
			applied = append(applied, "backendRole")
		}
	}

	// R133 (2026-10-16): САМА раскладка — кто донор — тоже приоритет над env.
	//
	// ЖИВОЙ ДЕФЕКТ. CPPWORKER_RPC_ENABLE=true и CPPWORKER_RPC_SERVERS=<донор> в
	// deployments/.env возвращали раскладку на СТАРЫЙ (уже выключенный) узел при
	// каждом пересоздании контейнера — независимо от того, что решил балансер.
	// Теперь состояние, применённое балансером (роли main/donor) или оператором
	// из WebUI, читается из файла переопределений и побеждает окружение.
	//
	// Разбор списка узлов идёт через parseRPCServersR101 — ту же функцию, что и
	// PUT /config/update: второй реализации правил «host:port» быть не должно.
	if raw, ok := overrides["rpcEnabled"]; ok {
		var enabled bool
		if err := json.Unmarshal(raw, &enabled); err == nil {
			if cfg.RPCEnabled != enabled {
				logger.Get().Infow(i18n.T("cppworker.runtime_overrides_r119.rpc_enabled_overridden"),
					"env_or_default", cfg.RPCEnabled, "applied_value", enabled)
			}
			cfg.RPCEnabled = enabled
			applied = append(applied, "rpcEnabled")
		} else {
			logger.Get().Warnw(i18n.T("cppworker.runtime_overrides_r119.rpc_enabled_unparsed"), "value", string(raw))
		}
	}
	if raw, ok := overrides["rpcServers"]; ok {
		var servers []string
		if err := json.Unmarshal(raw, &servers); err != nil {
			logger.Get().Warnw(i18n.T("cppworker.runtime_overrides_r119.rpc_servers_unparsed"), "value", string(raw))
		} else if len(servers) == 0 {
			// Пустой список — это ЗНАЧИМОЕ состояние («доноров нет»), а не мусор:
			// именно его записывает балансер, когда снимает раскладку. Через
			// parseRPCServersR101 его не пропустить (там пустой вход = ошибка),
			// поэтому обрабатываем отдельно — иначе env-список доноров остался бы
			// в конфиге и вернулся бы при следующем включении раскладки.
			if len(cfg.RPCServers) > 0 {
				logger.Get().Infow(i18n.T("cppworker.runtime_overrides_r119.rpc_servers_cleared"),
					"env_or_default", len(cfg.RPCServers))
			}
			cfg.RPCServers = nil
			applied = append(applied, "rpcServers")
		} else if parsed, perr := parseRPCServersR101(strings.Join(servers, ",")); perr != nil {
			logger.Get().Warnw(i18n.T("cppworker.runtime_overrides_r119.rpc_servers_invalid"),
				"value", string(raw), "error", perr)
		} else {
			if len(cfg.RPCServers) != len(parsed) {
				logger.Get().Infow(i18n.T("cppworker.runtime_overrides_r119.rpc_servers_overridden"),
					"env_or_default", len(cfg.RPCServers), "applied_count", len(parsed))
			}
			cfg.RPCServers = parsed
			applied = append(applied, "rpcServers")
		}
	}
	return applied
}
