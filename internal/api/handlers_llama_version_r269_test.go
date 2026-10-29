//go:build llama_stub

// handlers_llama_version_r269_test.go — R269 (2026-10-29): эндпоинт
// /api/v1/llama-version отдаёт состояние проверки и свежесть сборок узлов.
//
// Проверяем контракт, а не «умеет ли сервер отвечать 200»: WebUI строит
// уведомление ИМЕННО по этому ответу, поэтому обязательны поля enabled/relation
// и отсутствие паники, когда трекер вообще не запущен.

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestR269_LlamaVersionHandler_Contract(t *testing.T) {
	s := newAgentCapacityServer(t)
	if err := s.proxy.AddBackend(types.Backend{
		ID: "n1", Host: "10.0.0.1", CppWorkerPort: 18092, BuildInfo: "b4321",
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}

	rec := httptest.NewRecorder()
	s.llamaVersionHandler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/llama-version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d (%s)", rec.Code, rec.Body.String())
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if _, ok := body["enabled"]; !ok {
		t.Error("нет поля enabled — WebUI не отличит «проверка выключена» от «нет обновлений»")
	}
	rel, _ := body["relation"].(string)
	if rel == "" {
		t.Error("нет поля relation — уведомление строить не из чего")
	}
	backends, ok := body["backends"].([]interface{})
	if !ok {
		t.Fatalf("нет массива backends: %v", body["backends"])
	}
	if len(backends) != 1 {
		t.Fatalf("узлов в агрегате %d, want 1", len(backends))
	}
	first, _ := backends[0].(map[string]interface{})
	if first["id"] != "n1" {
		t.Errorf("id узла = %v, want n1", first["id"])
	}
	if first["current"] != "b4321" {
		t.Errorf("current = %v, want b4321 (строка сборки узла)", first["current"])
	}
}

func TestR269_LlamaVersionHandler_MethodNotAllowed(t *testing.T) {
	s := newAgentCapacityServer(t)
	rec := httptest.NewRecorder()
	s.llamaVersionHandler(rec, httptest.NewRequest(http.MethodPost, "/api/v1/llama-version", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: HTTP %d, want 405", rec.Code)
	}
}
