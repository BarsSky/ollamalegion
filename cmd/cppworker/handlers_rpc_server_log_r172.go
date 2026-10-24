// handlers_rpc_server_log_r172.go — R172 (2026-10-18, разбор падения узла).
//
//	GET /api/v1/cppworker/rpc-server/log?lines=N
//	  → {"enabled":true,"running":false,"external":false,"pid":0,
//	     "addr":"0.0.0.0:50052","lastExit":"signal: segmentation fault (core dumped)",
//	     "lastExitAt":"...","lastExitPid":1234,"restartEnabled":true,
//	     "restarting":true,"restartInMs":1500,"debug":false,
//	     "logLines":812,"logDropped":0,
//	     "lines":[{"at":"...","text":"..."}, ...]}
//
// ЗАЧЕМ. Клиент раскладки на любой неуспешный ответ сервера пишет одно и то же:
// «Remote RPC server crashed or returned malformed response» (ggml-rpc.cpp:35), а
// настоящую причину печатает только сервер — в stderr, через GGML_LOG_ERROR
// (регион тензора вне буфера, неизвестный тип, невозможность аллокации). До R172
// этот вывод жил лишь в `docker logs` контейнера-донора и исчезал при его
// перезаписи, поэтому разбор аварии упирался в догадки. Теперь последние строки
// узла лежат в памяти воркера, доступны по этой ручке и, главное, попадают в
// лог самого воркера вместе с exit-статусом.
//
// ПОЧЕМУ ОТДЕЛЬНАЯ РУЧКА, А НЕ ПОЛЕ В /config. Статус читают балансер и WebUI
// постоянно; лог объёмный и нужен по требованию. В статусе остаётся короткий
// хвост (LogTail) — ровно на случай «узел лежит, что случилось».
package main

import (
	"net/http"
	"strconv"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
)

// rpcServerLogDefaultLines / rpcServerLogMaxRequestLines — сколько строк отдавать
// по умолчанию и максимум. Верхняя граница нужна, чтобы один запрос не вытянул
// весь буфер вместе с ответом на десятки мегабайт.
const (
	rpcServerLogDefaultLines    = 200
	rpcServerLogMaxRequestLines = 2000
)

func handleRPCServerLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	if rpcServerChild == nil {
		// Честно: узел не настроен в этом процессе. Пустой список строк вместо
		// 200 с нулями — чтобы «лога нет» не выглядело как «лог пуст».
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled": false,
			"lines":   []interface{}{},
			"note":    i18n.T("cppworker.handlers_rpc_server_log_r172.not_configured"),
		})
		return
	}

	lines := rpcServerLogDefaultLines
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			lines = n
		}
	}
	if lines > rpcServerLogMaxRequestLines {
		lines = rpcServerLogMaxRequestLines
	}

	st := rpcServerChild.Status()
	tail := rpcServerChild.LogTail(lines)
	if tail == nil {
		tail = []cppbackend.RPCServerLogLine{}
	}
	captured, dropped := rpcServerChild.LogStats()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":  st.Enabled,
		"external": st.External,
		"running":  st.Running,
		"pid":      st.PID,
		"addr":     st.Addr,
		"bin":      st.Bin,
		"uptimeS":  st.UptimeSec,
		"restarts": st.Restarts,
		// Как именно умер ПРЕДЫДУЩИЙ процесс и когда: первое, что отличает
		// «сервер разбился» от «сервер ответил false».
		"lastExit":    st.LastExit,
		"lastExitAt":  st.LastExitAt,
		"lastExitPid": st.LastExitPID,
		"lastError":   st.LastError,
		// Перезапуск: «лежит» и «вернётся через 1.5 с» — разные состояния.
		"restartEnabled": st.RestartEnabled,
		"restarting":     st.Restarting,
		"restartInMs":    st.RestartInMS,
		"debug":          st.Debug,
		"logLines":       captured,
		"logDropped":     dropped,
		"lines":          tail,
	})
}
