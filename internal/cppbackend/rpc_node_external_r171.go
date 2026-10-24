// rpc_node_external_r171.go — R171 (2026-10-17): режим ВНЕШНЕГО узла раскладки.
//
// ЗАЧЕМ. R170 локализовал отказ: встроенный узел (дочерний процесс воркера, R123)
// падает, когда клиент находится на ДРУГОЙ машине
// (`ggml-rpc.cpp:566 Remote RPC server crashed or returned malformed response`),
// а ОТДЕЛЬНЫЙ контейнер с тем же образом и тем же env работает (проверено 3
// конфигурациями, см. docs/architecture/layer-sharding.ru.md). Воркер не может запустить второй
// контейнер: у него нет доступа к docker-сокету, и давать его — плохая идея.
//
// ЧТО ДЕЛАЕМ ВМЕСТО ЭТОГО. Воркер поддерживает режим «узел живёт отдельным
// контейнером»: он НЕ порождает процесс, а проверяет внешний адрес и честно
// отчитывается о нём (enabled / running / listening / external). Тогда:
//   - балансер по-прежнему управляет ЖЕЛАНИЕМ (R133/R164: роль donor → приказ);
//   - оператор поднимает сам контейнер из compose (профиль `rpc-node`);
//   - статус не врёт: externally managed помечен как `external: true`.
//
// ГРАНИЦА ПОЛНОМОЧИЙ (как и с публикацией порта): запустить чужой контейнер
// воркер не может и не должен — это свойство хоста. Поэтому Stop() в этом режиме
// только снимает желание и пишет, что гасить может оператор.
package cppbackend

import (
	"net"
	"strconv"
	"strings"
	"time"
)

// rpcNodeExternalAddr — адрес внешнего узла из конфига (пусто = режим дочернего
// процесса, как раньше). Формат: host:port.
func rpcNodeExternalAddr(cfg Config) string {
	return strings.TrimSpace(cfg.RPCServerExternalAddr)
}

// rpcNodeProbeExternal — доступен ли внешний узел (TCP).
//
// Проверяем именно порт: «контейнер существует» и «узел принимает мастера» — разные
// утверждения, и второе и есть то, что нужно раскладке.
func rpcNodeProbeExternal(addr string, timeout time.Duration) bool {
	if strings.TrimSpace(addr) == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// externalNodeStatus — статус внешнего узла для /api/models и /api/v1/cppworker/config.
//
// Поля намеренно те же, что у дочернего процесса, плюс:
//   - External=true — «этим узлом управляет не воркер»;
//   - PID=0 — процесса в этом контейнере нет;
//   - в Notes — что именно проверили.
func externalNodeStatus(enabled bool, addr string, probeTimeout time.Duration) RPCServerStatus {
	host, portStr := "", 0
	if h, p, err := net.SplitHostPort(addr); err == nil {
		host = h
		if v, cerr := strconv.Atoi(p); cerr == nil {
			portStr = v
		}
	}
	st := RPCServerStatus{
		Enabled:  enabled,
		Bind:     host,
		Port:     portStr,
		Addr:     addr,
		External: true,
		Bin:      "external container (compose profile rpc-node)",
	}
	if !enabled {
		st.Notes = append(st.Notes, "внешний узел: желание выключено (сам контейнер гасит оператор)")
		return st
	}
	if rpcNodeProbeExternal(addr, probeTimeout) {
		st.Running = true
		st.Listening = true
		st.Notes = append(st.Notes, "внешний узел доступен ("+addr+")")
		return st
	}
	st.Notes = append(st.Notes, "внешний узел не отвечает на "+addr+
		": проверьте, что контейнер rpc-node поднят и порт опубликован")
	return st
}
