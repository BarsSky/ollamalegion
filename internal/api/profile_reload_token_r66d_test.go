//go:build llama_stub

// profile_reload_token_r66d_test.go — R66d (2026-09-23).
//
// РЕГРЕСС: «применить профиль из WebUI» не работало — apply возвращал
//   200 {"backends":[{"status":"error",
//        "message":"cppworker reload returned 401: {\"error\":\"invalid or missing API token\"}"}]}
// потому что балансер слал POST /api/models/reload БЕЗ токена, если в записи
// бэкенда CppWorkerApiToken пуст. А в bundled-стеке он пуст: бэкенд регистрируют
// cppworker (с токеном), shell-скрипт и sidecar-агент (без токена), и после
// дедупликации по host:port токен в живой записи теряется.
//
// При этом cppworker защищает /api/models/reload через authMiddleware
// (cmd/cppworker/router.go:31), а остальные модели-эндпоинты — нет, поэтому
// load из балансера работал, а reload/apply — нет.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// boolPtr — указатель на bool (для полей профиля *bool).
func boolPtr(b bool) *bool { return &b }

// splitHostPortTest — разбирает URL httptest-сервера на host и port.
func splitHostPortTest(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port from %q: %v", rawURL, err)
	}
	return u.Hostname(), port
}

// mockReloadCppworker — мок cppworker, который принимает /api/models/reload
// только с правильным токеном (как authMiddleware) и отвергает неизвестные
// поля тела (как DisallowUnknownFields в cmd/cppworker).
func mockReloadCppworker(t *testing.T, wantToken string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	// Поля, которые реально понимает reloadModelRequest (cmd/cppworker/types.go:134).
	knownFields := map[string]bool{
		"name": true, "contextSize": true, "batchSize": true, "gpuLayers": true,
		"flashAttn": true, "numa": true, "useMmap": true, "force": true,
		"parallel": true, "kvCacheType": true,
		"overrideTensors": true, "overrideTensorBufts": true,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/reload" {
			http.NotFound(w, r)
			return
		}
		calls++
		got := r.Header.Get("X-API-Token")
		if got == "" {
			if ah := r.Header.Get("Authorization"); len(ah) > 7 && ah[:7] == "Bearer " {
				got = ah[7:]
			}
		}
		if wantToken != "" && got != wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid or missing API token"})
			return
		}
		// Проверяем имена полей: cppworker декодирует с DisallowUnknownFields,
		// поэтому "numGpuLayers" (вместо "gpuLayers") давал 400 и apply не работал.
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
		for field := range payload {
			if !knownFields[field] {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": `invalid JSON: json: unknown field "` + field + `"`,
				})
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestReloadModelOnCppWorker_FallsBackToBalancerToken — пустой
// CppWorkerApiToken в записи бэкенда не должен приводить к 401: берём токен
// балансера (LB_API_TOKEN), он же единый токен стека.
func TestReloadModelOnCppWorker_FallsBackToBalancerToken(t *testing.T) {
	const shared = "r66d-shared-token"
	t.Setenv("LB_API_TOKEN", shared)
	t.Setenv("CPPWORKER_API_TOKEN", "")

	srv, _ := mockReloadCppworker(t, shared)
	host, port := splitHostPortTest(t, srv.URL)

	s := &Server{}
	backend := types.Backend{
		ID:            "b1",
		Host:          host,
		CppWorkerPort: port,
		// CppWorkerApiToken намеренно пуст — как в живом bundled-стеке.
	}
	if _, err := s.reloadModelOnCppWorker(backend, "gemma-4-E4B-it-Q4_K_M", types.LlamaCppModelProfile{
		ContextLength: 8192,
		BatchSize:     512,
	}); err != nil {
		t.Fatalf("reload должен пройти с токеном балансера, got: %v", err)
	}
}

// TestReloadModelOnCppWorker_PrefersBackendToken — если токен в записи бэкенда
// есть, используем именно его (он пришёл от cppworker при регистрации).
func TestReloadModelOnCppWorker_PrefersBackendToken(t *testing.T) {
	const backendToken = "backend-specific-token"
	t.Setenv("LB_API_TOKEN", "balancer-token")
	t.Setenv("CPPWORKER_API_TOKEN", "")

	srv, _ := mockReloadCppworker(t, backendToken)
	host, port := splitHostPortTest(t, srv.URL)

	s := &Server{}
	backend := types.Backend{
		ID:                "b1",
		Host:              host,
		CppWorkerPort:     port,
		CppWorkerApiToken: backendToken,
	}
	if _, err := s.reloadModelOnCppWorker(backend, "m", types.LlamaCppModelProfile{ContextLength: 4096}); err != nil {
		t.Fatalf("reload с токеном бэкенда должен пройти, got: %v", err)
	}
}

// TestReloadModelOnCppWorker_BodyUsesCppworkerFieldNames — тело reload-запроса
// должно использовать имена полей cppworker (gpuLayers, а не numGpuLayers):
// cppworker декодирует с DisallowUnknownFields и отвечал
//   400 {"error":"invalid JSON: json: unknown field \"numGpuLayers\""}
// из-за чего «применить профиль» в WebUI не работало.
func TestReloadModelOnCppWorker_BodyUsesCppworkerFieldNames(t *testing.T) {
	t.Setenv("LB_API_TOKEN", "t")
	srv, _ := mockReloadCppworker(t, "t")
	host, port := splitHostPortTest(t, srv.URL)

	s := &Server{}
	if _, err := s.reloadModelOnCppWorker(types.Backend{ID: "b1", Host: host, CppWorkerPort: port},
		"gemma-4-E4B-it-Q4_K_M", types.LlamaCppModelProfile{
			ContextLength: 8192,
			BatchSize:     512,
			NumGPULayers:  -1,
			FlashAttn:     boolPtr(true),
			UseMmap:       boolPtr(true),
			KVCacheType:   "q4_0",
		}); err != nil {
		t.Fatalf("reload с полями профиля должен приниматься cppworker'ом, got: %v", err)
	}
}

// TestReloadModelOnCppWorker_NoTokenAnywhereStillFails — если токена нет нигде,
// cppworker отвечает 401 и это видно как понятная ошибка (не молчаливый успех).
func TestReloadModelOnCppWorker_NoTokenAnywhereStillFails(t *testing.T) {
	t.Setenv("LB_API_TOKEN", "")
	t.Setenv("CPPWORKER_API_TOKEN", "")

	srv, _ := mockReloadCppworker(t, "expected-token")
	host, port := splitHostPortTest(t, srv.URL)

	s := &Server{}
	if _, err := s.reloadModelOnCppWorker(types.Backend{ID: "b1", Host: host, CppWorkerPort: port},
		"m", types.LlamaCppModelProfile{ContextLength: 4096}); err == nil {
		t.Fatal("без токена cppworker отвечает 401 — ошибка должна вернуться наверх")
	}
}
