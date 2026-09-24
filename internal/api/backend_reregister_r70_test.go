// backend_reregister_r70_test.go — R70 (2026-09-24): повторная саморегистрация
// бэкенда обновляет его параметры, а не отбивается 409.
//
// ПРОБЛЕМА (живой стенд): cppworker при старте шлёт POST /api/v1/backends с
// реальной вместимостью, но для уже существующего ID балансер отвечал
// 409 Conflict — параметры оставались старыми. После того как cppworker начал
// сообщать настоящий n_parallel (1 вместо старой константы 4), балансер всё ещё
// держал maxConcurrentRequests=4: admission-очередь включалась на неверном
// пороге, «лишние» запросы молча блокировались внутри cppworker.
//
// ФИКС: если повторная регистрация пришла от ТОГО ЖЕ бэкенда (совпадают host и
// порт), параметры обновляются (200 + updated=true). Чужой бэкенд с тем же ID —
// по-прежнему 409.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

func newBackendReregisterServer(t *testing.T) *Server {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{
				ID:                "cppworker-gpu-bundled-agent",
				Name:              "CppWorker GPU",
				Host:              "cppworker-gpu",
				CppWorkerPort:     18092,
				OllamaPort:        11434,
				Type:              types.BackendTypeLlamaCpp,
				Status:            types.StatusHealthy,
				Weight:            100,
				MaxConcurrentReqs: 4, // старое значение (было константой в регистрации)
			},
		},
	}
	p := balancer.NewProxy(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return NewServer(p, cfg, nil)
}

func postBackendR70(t *testing.T, s *Server, body map[string]interface{}) (int, map[string]interface{}) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backends", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.addBackend(rec, req)
	var parsed map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	return rec.Code, parsed
}

// TestReRegisterSameBackend_R70_UpdatesCapacity — повторная регистрация той же
// ноды обновляет вместимость (200), admission-очередь получает реальный порог.
func TestReRegisterSameBackend_R70_UpdatesCapacity(t *testing.T) {
	s := newBackendReregisterServer(t)

	code, resp := postBackendR70(t, s, map[string]interface{}{
		"id":                    "cppworker-gpu-bundled-agent",
		"name":                  "cppworker-gpu-bundled-agent",
		"host":                  "cppworker-gpu",
		"cppWorkerPort":         18092,
		"backendType":           "llama_cpp",
		"weight":                100,
		"maxConcurrentRequests": 1, // реальный n_parallel
		"maxModels":             4,
		"labels":                []string{"cppworker", "auto-registered"},
		"cppWorkerApiToken":     "tok",
	})

	if code != http.StatusOK {
		t.Fatalf("повторная регистрация: ожидался 200, получено %d (%v)", code, resp)
	}
	if updated, _ := resp["updated"].(bool); !updated {
		t.Errorf("ожидался флаг updated=true, получено %v", resp)
	}
	backend := s.proxy.GetBackend("cppworker-gpu-bundled-agent")
	if backend == nil {
		t.Fatal("бэкенд пропал после обновления")
	}
	if backend.MaxConcurrentReqs != 1 {
		t.Errorf("MaxConcurrentReqs = %d, ожидалось 1 (реальный n_parallel)", backend.MaxConcurrentReqs)
	}
	if backend.RuntimeMaxConcurrentRequests != 1 {
		t.Errorf("RuntimeMaxConcurrentRequests = %d, ожидалось 1 (иначе runtime-override перекрывает реальную вместимость)",
			backend.RuntimeMaxConcurrentRequests)
	}
	if backend.CppWorkerApiToken != "tok" {
		t.Errorf("токен не обновлён: %q", backend.CppWorkerApiToken)
	}
}

// TestReRegisterDifferentHost_R70_StillConflicts — чужой бэкенд с тем же ID
// (другой host/порт) по-прежнему получает 409: молча перезаписывать нельзя.
func TestReRegisterDifferentHost_R70_StillConflicts(t *testing.T) {
	s := newBackendReregisterServer(t)

	code, resp := postBackendR70(t, s, map[string]interface{}{
		"id":                    "cppworker-gpu-bundled-agent",
		"host":                  "another-host",
		"cppWorkerPort":         19999,
		"backendType":           "llama_cpp",
		"maxConcurrentRequests": 8,
		"labels":                []string{"auto-registered"},
	})

	if code != http.StatusConflict {
		t.Fatalf("ожидался 409 для чужого host, получено %d (%v)", code, resp)
	}
	if backend := s.proxy.GetBackend("cppworker-gpu-bundled-agent"); backend.MaxConcurrentReqs != 4 {
		t.Errorf("параметры чужого бэкенда изменились: MaxConcurrentReqs=%d", backend.MaxConcurrentReqs)
	}
}
