//go:build llama_stub

// autotune_apply_r66c_test.go — R66c (2026-09-22): авто-применение плана AutoTune.
//
// Дефект: executeAutoTuneReload собирал env-map и вызывал
// applyCppWorkerEnvReload, который ВСЕГДА возвращал ошибку
// «env-based reload not yet implemented; apply plan manually». То есть
// авто-перезагрузка AutoTune не работала никогда, а ошибка уходила в circuit
// breaker (RecordError) и в историю событий — оператор в UI видел
// «AutoTune reload failed» и растущий cool-down вместо применения плана,
// хотя ручной endpoint /api/v1/admin/autotune/{id}/apply работал.
//
// Тест фиксирует контракт: авто-путь делает unload + load и доносит параметры
// плана (n_ctx в первую очередь — именно его AutoTune и меняет).

package balancer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestExecuteAutoTuneReload_AppliesPlan(t *testing.T) {
	var (
		mu          sync.Mutex
		unloadCalls int
		loadBodies  []map[string]interface{}
	)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/unload":
			mu.Lock()
			unloadCalls++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"unloaded"}`))

		case "/api/models/load", "/api/models/load-with-params":
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]interface{}
			_ = json.Unmarshal(body, &parsed)
			mu.Lock()
			loadBodies = append(loadBodies, parsed)
			mu.Unlock()

			// Как настоящий cppworker: 202 + Location, готовность — в /api/models.
			w.Header().Set("Location", "/api/models/load/progress?model=m1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":              "loading",
				"name":                "m1",
				"estimatedLoadTimeMs": 1,
				"model":               map[string]interface{}{"name": "m1", "state": "loading"},
			})

		case "/api/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"count":  1,
				"models": []map[string]interface{}{{"name": "m1", "state": "loaded"}},
			})

		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	host, port := parseTestHostPort(t, mock.URL)
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 18080, APIPort: 18081},
		Backends: []types.Backend{{
			ID:                "b-autotune",
			Name:              "autotune mock",
			Host:              host,
			CppWorkerPort:     port,
			Type:              types.BackendTypeLlamaCpp,
			Status:            types.StatusHealthy,
			MaxConcurrentReqs: 4,
		}},
		Balancing: types.BalancingSettings{
			Algorithm:     types.AlgorithmResourceAware,
			OperatingMode: "llama_cpp",
		},
	}
	p := newProxyWithCleanup(t, cfg)

	circuit := NewAutoTuneTracker(DefaultAutoTuneCircuitConfig()).GetCircuit("b-autotune", "m1")
	plan := &AutoTuneReloadPlan{
		ModelName:    "m1",
		ContextSize:  8192,
		KVCacheType:  "q8_0",
		NumGPULayers: 20,
		Reason:       "R66c test: n_ctx over-allocation",
	}

	if err := p.executeAutoTuneReload("b-autotune", "m1", plan, circuit); err != nil {
		t.Fatalf("executeAutoTuneReload вернул ошибку — план снова не применяется (регресс к заглушке): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if unloadCalls == 0 {
		t.Error("cppworker не получил /api/models/unload — план применён не полностью")
	}
	if len(loadBodies) == 0 {
		t.Fatal("cppworker не получил /api/models/load — план не применён")
	}

	body := loadBodies[0]
	if got, ok := body["contextSize"].(float64); !ok || int(got) != 8192 {
		t.Errorf("в load-запросе нет contextSize=8192 (именно n_ctx и меняет AutoTune): %v", body)
	}
	if got, _ := body["kvCacheType"].(string); got != "q8_0" {
		t.Errorf("kvCacheType не доехал до cppworker: %v", body)
	}
	if got, ok := body["gpuLayers"].(float64); !ok || int(got) != 20 {
		t.Errorf("gpuLayers не доехал до cppworker: %v", body)
	}
}
