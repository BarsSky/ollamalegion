// rpc_peers_r125.go — R125 (2026-10-16, пункт 3 разбора стенда): какие узлы
// раскладки РЕАЛЬНО активны.
//
// ЗАЧЕМ. Оператор просил метрики: «нехватает метрик для того чтобы понимать
// какой донор активен и какая между ними скорость обмена по сети». Список узлов
// (CPPWORKER_RPC_SERVERS) показывался как текст, и по нему нельзя было понять,
// отвечает ли узел: мастер проверяет доступность ТОЛЬКО в момент загрузки модели
// (гейт CheckRPCServers), а в UI это состояние не доходило вовсе.
//
// Здесь тот же TCP-диалог, что у гейта (одна реализация правила «узел доступен»),
// но с ПООДНОЙ выдачей: адрес, доступность, задержка, причина. Это позволяет
// показать «какой донор активен» фактом, а не по наличию строки в списке.
//
// ЧЕСТНО О ГРАНИЦАХ. Проверяется TCP-доступность узла, а НЕ пропускная способность:
// у rpc-server нет ручки здоровья и нет эхо-протокола, поэтому «скорость обмена»
// этим пробником измерить нельзя. Доступность и задержка — то, что можно измерить
// честно; для скорости нужен отдельный измеритель (см. отчёт по пункту 3).
package cppbackend

import (
	"net"
	"strings"
	"time"
)

// RPCPeerState — состояние одного узла раскладки.
type RPCPeerState struct {
	Addr string `json:"addr"`
	// Reachable — TCP-соединение установлено (то же условие, что у гейта загрузки).
	Reachable bool `json:"reachable"`
	// LatencyMS — время установления TCP-соединения (не пропускная способность).
	LatencyMS int64 `json:"latencyMs,omitempty"`
	// Error — почему не отвечает (пусто у доступного узла).
	Error string `json:"error,omitempty"`
}

// ProbeRPCServers — проверить список узлов и вернуть состояние ПО КАЖДОМУ.
//
// timeout <= 0 → RPCHealthDialTimeout (тот же дефолт, что у гейта загрузки, чтобы
// UI и загрузка не расходились в вердикте).
func ProbeRPCServers(servers []string, timeout time.Duration) []RPCPeerState {
	if timeout <= 0 {
		timeout = RPCHealthDialTimeout
	}
	out := make([]RPCPeerState, 0, len(servers))
	for _, raw := range servers {
		addr := strings.TrimSpace(raw)
		if addr == "" {
			continue
		}
		st := RPCPeerState{Addr: addr}
		started := time.Now()
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			st.Reachable = false
			st.Error = err.Error()
			out = append(out, st)
			continue
		}
		st.LatencyMS = time.Since(started).Milliseconds()
		_ = conn.Close()
		st.Reachable = true
		out = append(out, st)
	}
	return out
}

// CountReachableRPC — сколько узлов из снимка отвечает (для сводки в UI/метриках).
func CountReachableRPC(peers []RPCPeerState) int {
	n := 0
	for _, p := range peers {
		if p.Reachable {
			n++
		}
	}
	return n
}
