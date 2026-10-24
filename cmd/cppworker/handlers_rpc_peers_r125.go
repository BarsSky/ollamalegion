// handlers_rpc_peers_r125.go — R125 (2026-10-16, пункт 3 разбора стенда).
//
//	GET /api/v1/cppworker/rpc-peers
//	  → {"servers":[{addr,reachable,latencyMs,error}],"reachable":N,"total":M,"checkedAt":...}
//
// ЗАЧЕМ ОТДЕЛЬНАЯ РУЧКА, А НЕ ПОЛЕ В /config. Проверка узлов — это СЕТЕВОЙ ввод-вывод
// с таймаутом (узлы могут не отвечать): подмешивать её в каждое чтение конфига
// (его опрашивают балансер и WebUI, в том числе в фоне) значило бы платить
// задержкой за каждое открытие формы. Здесь проверка выполняется по запросу —
// когда оператор открыл секцию «Донор / приёмник» или нажал «проверить».
//
// Проверяется ДОСТУПНОСТЬ (TCP), а не пропускная способность: у rpc-server нет
// ни ручки здоровья, ни эхо-протокола, поэтому честно измеряются доступность и
// задержка соединения. Скорость обмена этим пробником не измеряется и в ответе
// не выдумывается.
package main

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
)

// linkBenchTimeout — сколько максимум длится замер пропускной способности.
const linkBenchTimeout = 30 * time.Second

// rpcPeersProbeTimeout — таймаут на узел. Тот же порядок, что у гейта загрузки
// (cppbackend.RPCHealthDialTimeout), чтобы вердикт UI и загрузки совпадал.
const rpcPeersProbeTimeout = 1500 * time.Millisecond

func handleCppWorkerRPCPeers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	if currentConfig == nil {
		writeError(w, http.StatusServiceUnavailable, "backend not initialized yet")
		return
	}
	// Узлы берём у действующего конфига: при выключенной раскладке список пуст —
	// это и есть честный ответ «раскладка не настроена», а не «узлы недоступны».
	servers := []string{}
	if currentConfig.RPCEnabled {
		servers = append(servers, currentConfig.RPCServers...)
	}
	peers := cppbackend.ProbeRPCServers(servers, rpcPeersProbeTimeout)

	// R129 (2026-10-16, пункт 3): по запросу (?bench=1) дополнительно измеряем
	// ПРОПУСКНУЮ СПОСОБНОСТЬ до каждого доступного узла.
	//
	// ЧТО ИЗМЕРЯЕТСЯ И ПОЧЕМУ ТАК. Замер идёт по HTTP-поверхности воркера на узле
	// (/api/v1/cppworker/link-bench), а не по порту rpc-server: у того свой
	// бинарный протокол без эхо, поэтому «залить в него мегабайты» ничего не
	// измеряет. HTTP воркера идёт по ТОМУ ЖЕ пути между машинами, а значит
	// ограничивает и обмен раскладки.
	//
	// Дорого (десятки МБ трафика) → только по явному запросу, не в фоне.
	var speed []cppbackend.LinkSpeedResult
	if benchEnabled(r) {
		mb := benchMegabytes(r)
		port := benchPort(r)
		for i := range peers {
			if !peers[i].Reachable {
				continue
			}
			host := peers[i].Addr
			if c := strings.LastIndex(host, ":"); c > 0 {
				host = host[:c] // у узла берём только хост: порт HTTP задаётся отдельно
			}
			url := "http://" + net.JoinHostPort(host, port) + "/api/v1/cppworker/link-bench"
			speed = append(speed, cppbackend.MeasureLinkSpeed(r.Context(), url, mb, linkBenchTimeout))
		}
	}

	resp := map[string]interface{}{
		"servers":    peers,
		"reachable":  cppbackend.CountReachableRPC(peers),
		"total":      len(peers),
		"rpcEnabled": currentConfig.RPCEnabled,
		"checkedAt":  time.Now().UTC().Format(time.RFC3339),
		"probe":      "tcp",
		"note":       i18n.T("cppworker.handlers_rpc_peers_r125.note"),
	}
	if speed != nil {
		resp["linkSpeed"] = speed
		resp["linkSpeedPort"] = benchPort(r)
		resp["linkSpeedMb"] = benchMegabytes(r)
	}
	writeJSON(w, http.StatusOK, resp)
}

// benchEnabled — запрошен ли замер пропускной способности (?bench=1).
func benchEnabled(r *http.Request) bool {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("bench")))
	return v == "1" || v == "true" || v == "yes"
}

// benchMegabytes — объём замера (по умолчанию 8 МБ, ограничен linkBenchMaxMB).
func benchMegabytes(r *http.Request) int {
	mb := 8
	if v := strings.TrimSpace(r.URL.Query().Get("mb")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mb = n
		}
	}
	if mb > linkBenchMaxMB {
		mb = linkBenchMaxMB
	}
	return mb
}

// benchPort — порт HTTP-поверхности воркера на узле (по умолчанию 18092: он же в
// compose стенда; узел может слушать другой порт, поэтому параметр).
func benchPort(r *http.Request) string {
	if v := strings.TrimSpace(r.URL.Query().Get("benchPort")); v != "" {
		return v
	}
	return "18092"
}
