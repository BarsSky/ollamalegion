// rpc_mode_r115.go — R115: разбор режима раскладки без побочных эффектов.
//
// ВЫНЕСЕНО ИЗ main.go, ЧТОБЫ БЫЛО ПРОВЕРЯЕМО. Прежняя логика жила прямо в
// main(): список узлов безусловно ставил `RPCEnabled = true`, поэтому явное
// `CPPWORKER_RPC_ENABLE=false` не имело силы — режим нельзя было выключить, не
// стерев список узлов. Ошибка была невидимой: обе переменные читались, лог
// писался, а результат не соответствовал сказанному.
//
// ПРАВИЛО (порядок важен):
//  1. непустой и корректный CPPWORKER_RPC_SERVERS задаёт список узлов;
//  2. ЯВНЫЙ CPPWORKER_RPC_ENABLE — решающий: false выключает раскладку и
//     отбрасывает список узлов (иначе «выключено» продолжало бы дозваниваться);
//  3. если CPPWORKER_RPC_ENABLE не задан, режим определяется списком узлов:
//     есть узлы — раскладка включена (прежнее поведение, совместимость);
//  4. включено без единого узла — предупреждение, а не тихая видимость настройки.
package main

import (
	"strconv"
	"strings"
)

// rpcMode — итог разбора переменных окружения раскладки.
type rpcMode struct {
	Enabled bool
	// Servers — узлы, которые реально пойдут в раскладку. nil при выключенном
	// режиме: конфиг не должен хранить адреса, по которым никто не ходит.
	Servers []string
	// Explicit — CPPWORKER_RPC_ENABLE был задан (нужно для честного лога:
	// «выключено флагом» и «выключено по умолчанию» — разные состояния).
	Explicit bool
	// Notes — что решено; Warnings — что оператор должен знать.
	Notes    []string
	Warnings []string
}

// resolveRPCMode разбирает значения переменных. Чистая функция: ни логов, ни
// чтения окружения — только правила, поэтому её проверяют тестом, а не стендом.
func resolveRPCMode(enableRaw, serversRaw string) rpcMode {
	var m rpcMode

	serversRaw = strings.TrimSpace(serversRaw)
	if serversRaw != "" {
		servers, err := parseRPCServersR101(serversRaw)
		if err != nil {
			// Отказ, а не частичный приём: половина списка означала бы модель,
			// разрезанную не так, как думал оператор.
			m.Warnings = append(m.Warnings,
				"CPPWORKER_RPC_SERVERS: invalid list, ignoring (expected host:port,host:port)")
		} else {
			m.Servers = servers
		}
	}

	enableRaw = strings.TrimSpace(enableRaw)
	if enableRaw != "" {
		if v, err := strconv.ParseBool(enableRaw); err == nil {
			m.Explicit = true
			m.Enabled = v
		} else {
			m.Warnings = append(m.Warnings,
				"CPPWORKER_RPC_ENABLE: invalid bool, ignoring")
		}
	}

	if m.Explicit && !m.Enabled {
		// Явное «выключено» сильнее списка узлов. Список отбрасываем, а не просто
		// игнорируем: иначе он остался бы в конфиге и всплыл при следующем
		// включении флага, что выглядело бы как самовольная раскладка.
		if len(m.Servers) > 0 {
			m.Notes = append(m.Notes,
				"RPC disabled by flag: node list ignored and cleared")
			m.Servers = nil
		} else {
			m.Notes = append(m.Notes, "RPC disabled by flag")
		}
		return m
	}

	if !m.Explicit {
		// Совместимость: непустой список узлов включал раскладку и раньше.
		m.Enabled = len(m.Servers) > 0
		if m.Enabled {
			m.Notes = append(m.Notes,
				"RPC enabled implicitly by non-empty node list")
		}
		return m
	}

	// Явное «включено».
	if len(m.Servers) == 0 {
		m.Warnings = append(m.Warnings,
			"CPPWORKER_RPC_ENABLE=true, but no nodes configured — layout stays local")
	}
	return m
}
