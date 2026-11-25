package balancer

// model_delete_token_r758_test.go — R758 (2026-11-25): удаление модели с диска
// воркера обязано идти с токеном админской ручки.
//
// ЖИВОЙ ДЕФЕКТ (первый стенд): WebUI → «GGUF-модели» → «Удалить» всегда
// показывал ошибку
//
//	Delete failed: cppworker error (HTTP 401): invalid or missing API token
//
// Причина: `/api/models/delete` у воркера закрыт authMiddleware, а балансер
// вызывал его через `sendRawRequest` — БЕЗ заголовка X-API-Token. R307 закрыл
// токеном load (`applyWorkerAdminTokenR307`) и unload
// (`sendRawRequestWithTokenR307`), а delete остался в стороне: удаление файла
// с узла не работало вообще (только вручную через docker exec).
//
// Проверяется ПОВЕДЕНИЕ на настоящем HTTP-сервере: без заголовка он отвечает
// 401 ровно как cppworker, с заголовком — удаляет.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// newDeleteHarnessR758 — прокси с одним llama.cpp-бэкендом и фейковым воркером,
// который, как настоящий, требует X-API-Token на /api/models/delete.
func newDeleteHarnessR758(t *testing.T, wantToken string) (*ModelManager, string, int, *bool, *string) {
	t.Helper()
	gotToken := ""
	deleteCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/delete" {
			http.NotFound(w, r)
			return
		}
		gotToken = r.Header.Get("X-API-Token")
		if wantToken != "" && gotToken != wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid or missing API token"})
			return
		}
		deleteCalled = true
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "name": "m"})
	}))
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)

	mm := NewModelManager(nil)
	p := &Proxy{
		config:       &types.LoadBalancerConfig{},
		backends:     map[string]*BackendState{},
		metricsMgr:   NewMetricsManager(),
		modelManager: mm,
	}
	mm.proxy = p
	p.backends["b1"] = &BackendState{
		Backend: &types.Backend{
			ID:                "b1",
			Host:              host,
			Status:            types.StatusHealthy,
			Type:              types.BackendTypeLlamaCpp,
			CppWorkerPort:     port,
			CppWorkerApiToken: wantToken,
		},
	}
	return mm, host, port, &deleteCalled, &gotToken
}

// TestR758_DeleteSendsWorkerToken — токен из конфигурации бэкенда доезжает до
// воркера, и удаление считается успешным.
func TestR758_DeleteSendsWorkerToken(t *testing.T) {
	mm, host, port, called, gotToken := newDeleteHarnessR758(t, "secret-token")

	res := mm.executeLlamaCppDelete(host, port, "b1", ModelOpRequest{Operation: "delete", ModelName: "m"})
	if res == nil {
		t.Fatal("executeLlamaCppDelete вернул nil")
	}
	if !res.Success {
		t.Fatalf("удаление не удалось: %q (workerStatus=%d) — ровно это оператор видел как "+
			"«Delete failed: ... HTTP 401 invalid or missing API token»", res.Error, res.WorkerStatus)
	}
	if !*called {
		t.Error("воркер не получил запрос на удаление")
	}
	if *gotToken != "secret-token" {
		t.Errorf("воркер получил токен %q, want \"secret-token\": без заголовка он отвечает 401", *gotToken)
	}
}

// TestR758_DeleteWithoutTokenStillWorksOnOpenWorker — у бэкенда токена нет
// (воркер без авторизации): заголовка тоже нет, и это не ошибка.
//
// Та же семантика, что у unload: пустая строка = «токена нет», воркер без
// авторизации такой запрос пропускает.
func TestR758_DeleteWithoutTokenStillWorksOnOpenWorker(t *testing.T) {
	mm, host, port, called, gotToken := newDeleteHarnessR758(t, "")

	res := mm.executeLlamaCppDelete(host, port, "b1", ModelOpRequest{Operation: "delete", ModelName: "m"})
	if !res.Success {
		t.Fatalf("на воркере без авторизации удаление должно проходить: %q", res.Error)
	}
	if !*called {
		t.Error("воркер не получил запрос на удаление")
	}
	if *gotToken != "" {
		t.Errorf("воркер получил непустой токен %q, хотя у бэкенда его нет", *gotToken)
	}
}

// TestR758_DeleteReportsWorkerErrorAsIs — отказ воркера остаётся отказом с
// текстом причины, а не «успехом с пустым результатом».
func TestR758_DeleteReportsWorkerErrorAsIs(t *testing.T) {
	mm, _, _, _, _ := newDeleteHarnessR758(t, "secret-token")
	// Порт закрытого сервера: соединение не установится.
	res := mm.executeLlamaCppDelete("127.0.0.1", 1, "b1", ModelOpRequest{Operation: "delete", ModelName: "m"})
	if res == nil || res.Success {
		t.Fatal("недоступный воркер не должен считаться успешным удалением")
	}
	if res.Error == "" {
		t.Error("ошибка недоступного воркера не описана")
	}
}
