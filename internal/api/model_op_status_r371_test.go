// model_op_status_r371_test.go — R371 (2026-11-01).
//
// Правило: загрузка, запрещённая РОЛЬЮ узла (донор раскладки не поднимает своих
// копий), отвечает 409 Conflict и несёт roleRefused:true; любая другая
// неуспешная операция остаётся 500.
//
// Живой факт, из которого правило выросло (двухузловой стенд):
// POST /api/v1/cluster/models/<m>/reload {"backendId":"cppworker-donor"} дал
// правильный отказ R304 («донор раскладки... не поднимает своих копий»), но с
// кодом httpStatus:500 — то есть «сервер сломался», хотя запрос просто
// противоречит роли узла.
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/balancer"
)

// TestR371_RoleRefusedIsConflict — отказ по роли → 409.
func TestR371_RoleRefusedIsConflict(t *testing.T) {
	res := &balancer.ModelOpResult{
		Success:     false,
		RoleRefused: true,
		Error:       "узел cppworker-donor — донор раскладки: он отдаёт VRAM мастеру и не поднимает своих копий моделей",
	}
	if got := opFailureStatusR371(res); got != http.StatusConflict {
		t.Errorf("код = %d, want 409 (отказ по роли — конфликт политики, а не сбой сервера)", got)
	}
}

// TestR371_OtherFailuresStay500 — обычные сбои не превращаются в 4xx.
func TestR371_OtherFailuresStay500(t *testing.T) {
	cases := []*balancer.ModelOpResult{
		nil,
		{Success: false, Error: "connection refused"},
		{Success: false, Error: "model failed to load: out of memory"},
	}
	for _, res := range cases {
		if got := opFailureStatusR371(res); got != http.StatusInternalServerError {
			t.Errorf("код = %d, want 500 для %+v: настоящие сбои обязаны оставаться сбоями", got, res)
		}
	}
}

// TestR371STEWARD_HandlerUsesStatusHelper — стюард: обработчик кластерных
// операций обязан решать код через opFailureStatusR371, а не писать 500 руками.
// Иначе следующая правка вернёт «409 живёт только в тесте».
func TestR371STEWARD_HandlerUsesStatusHelper(t *testing.T) {
	const path = "handlers_cluster_models.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(src), "opFailureStatusR371(opResult)") {
		t.Errorf("%s: нет вызова opFailureStatusR371(opResult) — код отказа снова зашит цифрой", path)
	}
}

func TestR371_RoleRefusedIsVisibleInJSON(t *testing.T) {
	res := clusterReloadModelBackendResult{
		BackendID:   "cppworker-donor",
		Status:      "error",
		Error:       "донор раскладки",
		RoleRefused: true,
		HTTPStatus:  http.StatusConflict,
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]interface{}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, ok := back["roleRefused"]; !ok || v != true {
		t.Errorf("в ответе нет roleRefused:true: %s", string(raw))
	}
	if v, ok := back["httpStatus"]; !ok || int(v.(float64)) != http.StatusConflict {
		t.Errorf("httpStatus = %v, want 409: %s", back["httpStatus"], string(raw))
	}
}
