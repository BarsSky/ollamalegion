//go:build llama_stub

// autotune_unload_token_r412_test.go — R412 (2026-11-03): AutoTune unload
// обязан предъявлять токен ЭТОГО бэкенда.
//
// ЖИВОЙ ДЕФЕКТ. AutoTune apply передавал в ModelManager.executeLlamaCppUnload
// имя модели вместо backendID, workerAdminToken() не находил бэкенд, заголовок
// X-API-Token не ставился — воркер отвечал 401 «invalid or missing API token»,
// reload kv_cache q4_0 → f16 не применялся и повторялся на каждом запросе.
//
// Тест ловит именно класс «аргумент-не-тот»: он смотрит на реальный HTTP-запрос
// к воркеру (path + query + header), а не на внутреннюю переменную.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestR412_AutoTuneUnloadSendsBackendToken(t *testing.T) {
	var gotToken, gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-API-Token")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"unloaded"}`))
	}))
	defer server.Close()
	host, port := parseTestHostPort(t, server.URL)

	p := &Proxy{config: &types.LoadBalancerConfig{}}
	p.backends = map[string]*BackendState{
		"bk-r412": {
			Backend: &types.Backend{
				ID:                "bk-r412",
				Host:              host,
				CppWorkerPort:     port,
				CppWorkerApiToken: "token-of-this-backend-r412",
				Status:            types.StatusHealthy,
			},
		},
	}
	p.modelManager = NewModelManager(p)

	res := p.executeLlamaCppUnload("bk-r412", "gemma-4-E4B-it-Q4_K_M")
	if res == nil || !res.Success {
		t.Fatalf("unload не удался: %+v", res)
	}
	if gotPath != "/api/models/unload" {
		t.Fatalf("путь = %q, ожидался /api/models/unload", gotPath)
	}
	if gotQuery != "name=gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("query = %q: имя модели обязано доехать до воркера (backendID на его место "+
			"попадать не должен)", gotQuery)
	}
	if gotToken != "token-of-this-backend-r412" {
		t.Fatalf("X-API-Token = %q: unload обязан нести токен бэкенда, иначе воркер "+
			"отвечает 401 (живой дефект R412)", gotToken)
	}
}

func TestR412_AutoTuneUnloadUnknownBackend(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	p.backends = map[string]*BackendState{}
	p.modelManager = NewModelManager(p)

	res := p.executeLlamaCppUnload("missing", "m1")
	if res == nil || res.Success {
		t.Fatalf("неизвестный бэкенд должен давать ошибку, got %+v", res)
	}
	if res.Error == "" {
		t.Fatalf("ошибка обязана называть причину (backend not found), got пусто")
	}
}
