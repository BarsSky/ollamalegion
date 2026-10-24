//go:build llama_stub

// rpc_server_log_handler_r172_test.go — R172 (2026-10-18).
//
// Проверяется КОНТРАКТ ручки лога узла (/api/v1/cppworker/rpc-server/log):
// структура ответа, «нет узла» ≠ «лог пуст», и метод.
//
// Глубокое поведение (перехват вывода, exit-статус, перезапуск) проверяется в
// internal/cppbackend/rpc_server_*_r172_test.go — здесь только то, что видит
// оператор и клиент по HTTP.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// withRPCServerChild — подменить глобальный супервизор на время теста.
func withRPCServerChild(t *testing.T, child *cppbackend.RPCServerChild) {
	t.Helper()
	prev := rpcServerChild
	rpcServerChild = child
	t.Cleanup(func() {
		if rpcServerChild != nil {
			_ = rpcServerChild.Stop()
		}
		rpcServerChild = prev
	})
}

// TestR172_Handler_NilNodeIsHonest — узел не настроен: ответ обязан ОТЛИЧАТЬ
// «узла нет» от «лог пуст», иначе оператор ищет аварию в пустом логе.
func TestR172_Handler_NilNodeIsHonest(t *testing.T) {
	withRPCServerChild(t, nil)

	rec := httptest.NewRecorder()
	handleRPCServerLog(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/rpc-server/log", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("код %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v (%q)", err, rec.Body.String())
	}
	if enabled, _ := body["enabled"].(bool); enabled {
		t.Error("enabled=true при отсутствии узла")
	}
	if note, _ := body["note"].(string); note == "" {
		t.Error("нет пояснения, почему лог пуст: пустой ответ читается как «ничего не было»")
	}
}

// TestR172_Handler_RejectsNonGet — ручка только для чтения.
func TestR172_Handler_RejectsNonGet(t *testing.T) {
	withRPCServerChild(t, nil)
	rec := httptest.NewRecorder()
	handleRPCServerLog(rec, httptest.NewRequest(http.MethodPost, "/api/v1/cppworker/rpc-server/log", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("код %d, want 405", rec.Code)
	}
}

// TestR172_Handler_ReportsNodeFacts — у настроенного, но не запущенного узла
// ответ обязан содержать ФАКТЫ (enabled/running/PID/адрес/политику перезапуска и
// массив строк), а не только пустой объект.
func TestR172_Handler_ReportsNodeFacts(t *testing.T) {
	child := cppbackend.NewRPCServerChild(cppbackend.Config{
		RPCServerEnabled: true,
		RPCServerBind:    "127.0.0.1",
		RPCServerPort:    50991,
		RPCServerBin:     "/nonexistent/rpc-server-r172",
		RPCServerRestart: false,
		RPCServerDebug:   true,
	})
	withRPCServerChild(t, child)

	rec := httptest.NewRecorder()
	handleRPCServerLog(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/rpc-server/log?lines=5", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v (%q)", err, rec.Body.String())
	}
	if enabled, _ := body["enabled"].(bool); !enabled {
		t.Error("enabled=false, хотя узел включён в конфиге")
	}
	if running, _ := body["running"].(bool); running {
		t.Error("running=true у узла, который не запускали")
	}
	if debug, _ := body["debug"].(bool); !debug {
		t.Error("debug=false, хотя отладочный режим включён")
	}
	if restart, _ := body["restartEnabled"].(bool); restart {
		t.Error("restartEnabled=true, хотя перезапуск выключен в конфиге")
	}
	if _, ok := body["lines"].([]interface{}); !ok {
		t.Errorf("lines не массив: %T (%v)", body["lines"], body["lines"])
	}
	if addr, _ := body["addr"].(string); addr != "127.0.0.1:50991" {
		t.Errorf("addr = %q, want 127.0.0.1:50991 (адрес нужен оператору и мастеру)", addr)
	}
	// Поля причины смерти присутствуют всегда (пустые значения при отсутствии
	// падений): клиент не должен угадывать, есть ли они в ответе.
	for _, key := range []string{"lastExit", "lastExitAt", "lastExitPid", "lastError", "logLines", "logDropped"} {
		if _, ok := body[key]; !ok {
			t.Errorf("в ответе нет поля %q", key)
		}
	}
}
