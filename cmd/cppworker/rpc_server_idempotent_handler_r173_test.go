//go:build llama_stub

// rpc_server_idempotent_handler_r173_test.go — R173 (2026-10-18).
//
// Проводка исправления через HTTP: повторный PUT /api/v1/cppworker/config/update
// с ТЕМИ ЖЕ значениями узла раскладки не должен пересоздавать супервизор (а до
// R173 пересоздавал — и гасил живой процесс посреди сессии раскладки).
//
// Идентичность супервизора проверяется указателем: это прямое доказательство
// «процесс не пересоздавали», не зависящее от того, удалось ли его поднять.
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestR173_Handler_RepeatOrderDoesNotRecreateNode — главный контрфакт на уровне
// ручки: два одинаковых приказа — один и тот же супервизор.
func TestR173_Handler_RepeatOrderDoesNotRecreateNode(t *testing.T) {
	port := freeRPCPort(t)
	cfg := rpcTestSetup(t, "/nonexistent/rpc-server-r173")
	cfg.RPCServerPort = port
	rpcServerChild = cppbackend.NewRPCServerChild(*cfg)

	body := `{"rpcServerEnabled":true,"rpcServerPort":` + strconv.Itoa(port) + `}`

	if w := putConfigUpdate(t, body); w.Code != http.StatusOK {
		t.Fatalf("первый приказ: status = %d, body=%s", w.Code, w.Body.String())
	}
	first := rpcServerChild
	if first == nil {
		t.Fatal("после приказа супервизор не создан")
	}

	if w := putConfigUpdate(t, body); w.Code != http.StatusOK {
		t.Fatalf("повторный приказ: status = %d, body=%s", w.Code, w.Body.String())
	}
	second := rpcServerChild
	if second != first {
		t.Error("повторный приказ с ТЕМИ ЖЕ значениями пересоздал супервизор — " +
			"в бою это гасит живой rpc-server посреди передачи весов мастером (авария R173)")
	}

	// Смена порта обязана пересоздать: порт зашит в argv дочернего процесса.
	changed := `{"rpcServerEnabled":true,"rpcServerPort":` + strconv.Itoa(altRPCPort(port)) + `}`
	if w := putConfigUpdate(t, changed); w.Code != http.StatusOK {
		t.Fatalf("смена порта: status = %d, body=%s", w.Code, w.Body.String())
	}
	if rpcServerChild == second {
		t.Error("смена порта НЕ пересоздала супервизор — узел остался на старом порту")
	}
}

// TestR173_Handler_RepeatedOrderIsReportedHonestly — в ответе должно быть видно
// ФАКТ состояния (rpcServer), а не только «applied»: оператор и балансер читают
// именно его.
func TestR173_Handler_RepeatedOrderIsReportedHonestly(t *testing.T) {
	port := freeRPCPort(t)
	cfg := rpcTestSetup(t, "/nonexistent/rpc-server-r173")
	cfg.RPCServerPort = port
	rpcServerChild = cppbackend.NewRPCServerChild(*cfg)

	body := `{"rpcServerEnabled":true,"rpcServerPort":` + strconv.Itoa(port) + `}`
	w := putConfigUpdate(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		RPC *cppbackend.RPCServerStatus `json:"rpcServer"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if resp.RPC == nil {
		t.Fatal("в ответе нет rpcServer")
	}
	if !resp.RPC.Enabled {
		t.Error("rpcServer.enabled = false после приказа включить")
	}
	if resp.RPC.Port != port {
		t.Errorf("rpcServer.port = %d, want %d", resp.RPC.Port, port)
	}
	// Бинаря нет — процесс подняться не может, и статус обязан это сказать.
	if resp.RPC.Running && resp.RPC.LastError == "" {
		t.Error("running=true без причины — статус врёт")
	}
	if !strings.Contains(w.Body.String(), "\"applied\"") {
		t.Errorf("в ответе нет applied: %s", w.Body.String())
	}
}
