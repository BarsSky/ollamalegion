//go:build llama_stub

// cluster_engine_auto_r112_test.go — R112 (2026-10-11).
//
// ЗАЧЕМ. С R112 значение backendEngine=auto стало дефолтом (config.Load) и
// основным режимом смешанного стенда: «тип бэкенда не навязан, движок — по
// составу кластера». Проверка живого стенда сразу после раскатки показала, что
// PUT /api/v1/cluster/config такое значение ОТВЕРГАЛ (400 «Invalid backendEngine.
// Valid: ollama_api, llama_cpp») — карточка «Авто» в настройках WebUI не
// сохранялась, а сервер отвечал ошибкой.
//
// Тест фиксирует контракт: auto принимается и сохраняется; неизвестное значение
// по-прежнему 400 (не «молча игнорируем»).
package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR112_ClusterConfigPut_AcceptsAutoBackendEngine — auto сохраняется.
func TestR112_ClusterConfigPut_AcceptsAutoBackendEngine(t *testing.T) {
	s := authEnabledServer(t)

	putClusterConfig(t, s, map[string]interface{}{"backendEngine": "auto"})

	if s.config.BackendEngine != types.EngineAuto {
		t.Fatalf("backendEngine = %q, want %q: значение auto не сохраняется",
			s.config.BackendEngine, types.EngineAuto)
	}
}

// TestR112_ClusterConfigPut_AcceptsExplicitEngines — явные значения не сломаны.
func TestR112_ClusterConfigPut_AcceptsExplicitEngines(t *testing.T) {
	cases := map[string]types.BackendEngine{
		"llama_cpp":  types.EngineLlamaCPP,
		"ollama_api": types.EngineOllamaAPI,
		"auto":       types.EngineAuto,
	}
	for req, want := range cases {
		s := authEnabledServer(t)
		putClusterConfig(t, s, map[string]interface{}{"backendEngine": req})
		if s.config.BackendEngine != want {
			t.Errorf("backendEngine %q → %q, want %q", req, s.config.BackendEngine, want)
		}
	}
}

// TestR112_ClusterConfigPut_RejectsUnknownBackendEngine — мусор = 400.
func TestR112_ClusterConfigPut_RejectsUnknownBackendEngine(t *testing.T) {
	s := authEnabledServer(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/cluster/config",
		bytes.NewReader([]byte(`{"backendEngine":"bogus"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Token", "r65d-secret-token")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("неизвестный backendEngine: код %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}
