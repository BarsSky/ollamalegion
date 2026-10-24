// rpc_config_api_r115_test.go — R115: раскладка настраивается через API/WebUI.
//
// До R115 режим раскладки задавался ТОЛЬКО переменными окружения при старте:
// ни WebUI, ни API не могли ни увидеть его, ни изменить. Эти тесты фиксируют
// правила новой ручки `PUT /api/v1/cppworker/config/update`:
//
//   - `rpcEnabled:false` — решающий и очищает список узлов (та же семантика, что
//     у `CPPWORKER_RPC_ENABLE` при старте);
//   - непустой `rpcServers` включает раскладку;
//   - список узлов разбирается тем же парсером, что и переменная окружения;
//   - изменение раскладки НЕ перезагружает модель молча: ответ несёт
//     `rpc_reload_required`, а модель получит новую схему при следующей загрузке.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func putConfigRPC(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	setupConfigForTest(t)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cppworker/config/update",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handleCppWorkerUpdateConfig(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("config/update: HTTP %d (%s)", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не разбирается: %v (%s)", err, w.Body.String())
	}
	return resp
}

func appliedContains(resp map[string]interface{}, key string) bool {
	arr, _ := resp["applied"].([]interface{})
	for _, v := range arr {
		if s, _ := v.(string); s == key {
			return true
		}
	}
	return false
}

func TestR115_APISetsNodesAndEnablesLayout(t *testing.T) {
	cfg := setupConfigForTest(t)
	cfg.RPCEnabled = false
	cfg.RPCServers = nil

	resp := putConfigRPC(t, `{"rpcEnabled":true,"rpcServers":["192.0.2.10:50052"]}`)

	if !cfg.RPCEnabled {
		t.Errorf("R115: раскладка обязана включиться")
	}
	if len(cfg.RPCServers) != 1 || cfg.RPCServers[0] != "192.0.2.10:50052" {
		t.Errorf("R115: узлы = %v, ожидался 192.0.2.10:50052", cfg.RPCServers)
	}
	if !appliedContains(resp, "rpcEnabled") || !appliedContains(resp, "rpcServers") {
		t.Errorf("R115: применённые поля не отражены в ответе: %v", resp["applied"])
	}
	if resp["rpc_reload_required"] != true {
		t.Errorf("R115: смена раскладки обязана помечаться как «применится при следующей загрузке»")
	}
	// Модель перезагружать молча нельзя: смена раскладки не входит в load-affecting.
	if arr, _ := resp["reload_started"].([]interface{}); len(arr) != 0 {
		t.Errorf("R115: смена раскладки не должна запускать auto-reload, получено %v", arr)
	}
}

func TestR115_APIDisableClearsNodeList(t *testing.T) {
	cfg := setupConfigForTest(t)
	cfg.RPCEnabled = true
	cfg.RPCServers = []string{"192.0.2.10:50052"}

	resp := putConfigRPC(t, `{"rpcEnabled":false}`)

	if cfg.RPCEnabled {
		t.Errorf("R115: явное false обязано выключать раскладку")
	}
	if len(cfg.RPCServers) != 0 {
		t.Errorf("R115: при выключении список узлов обязан быть очищен, осталось %v", cfg.RPCServers)
	}
	if !appliedContains(resp, "rpcServers(cleared)") {
		t.Errorf("R115: очистка списка обязана быть видна в applied: %v", resp["applied"])
	}
}

func TestR115_APIServerListAloneEnablesLayout(t *testing.T) {
	cfg := setupConfigForTest(t)
	cfg.RPCEnabled = false
	cfg.RPCServers = nil

	putConfigRPC(t, `{"rpcServers":["192.0.2.10:50052","192.0.2.11:50052"]}`)

	if !cfg.RPCEnabled {
		t.Errorf("R115: непустой список узлов включает раскладку (совместимость с env-путём)")
	}
	if len(cfg.RPCServers) != 2 {
		t.Errorf("R115: ожидались два узла, получено %v", cfg.RPCServers)
	}
}

func TestR115_APIEmptyListClearsNodes(t *testing.T) {
	cfg := setupConfigForTest(t)
	cfg.RPCEnabled = true
	cfg.RPCServers = []string{"192.0.2.10:50052"}

	putConfigRPC(t, `{"rpcServers":[]}`)

	if len(cfg.RPCServers) != 0 {
		t.Errorf("R115: пустой массив — законный способ убрать узлы, получено %v", cfg.RPCServers)
	}
}

func TestR115_APIInvalidNodeIsRejectedNotSwallowed(t *testing.T) {
	cfg := setupConfigForTest(t)
	cfg.RPCEnabled = true
	cfg.RPCServers = []string{"192.0.2.10:50052"}

	resp := putConfigRPC(t, `{"rpcServers":["not-a-host-port"]}`)

	errs, _ := resp["validation_errors"].([]interface{})
	if len(errs) == 0 {
		t.Fatalf("R115: битый узел обязан попасть в validation_errors, ответ: %v", resp)
	}
	if len(cfg.RPCServers) != 1 || cfg.RPCServers[0] != "192.0.2.10:50052" {
		t.Errorf("R115: при ошибке валидации список узлов не меняется, получено %v", cfg.RPCServers)
	}
}

func TestR115_APIWrongTypeReported(t *testing.T) {
	setupConfigForTest(t)
	resp := putConfigRPC(t, `{"rpcEnabled":"yes","rpcServers":"192.0.2.10:50052"}`)

	errs, _ := resp["validation_errors"].([]interface{})
	if len(errs) < 2 {
		t.Errorf("R115: оба поля с неверным типом обязаны быть в validation_errors: %v", errs)
	}
}
