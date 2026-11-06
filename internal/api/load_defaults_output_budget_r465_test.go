//go:build llama_stub

// load_defaults_output_budget_r465_test.go — R465 (2026-11-06): бюджет вывода
// виден в WebUI-контракте, а не только в коде.
//
// ЖИВАЯ ЖАЛОБА: «num_predict в env выставлен, но в WebUI его значения не видеть:
// поле в настройках бэкенда всегда 0, как и большинство других настроек…».
// Причина: значение живёт в ENV (`CPPWORKER_DEFAULT_N_PREDICT_REASONING`) и в
// константах политики, а не в Config, поэтому load-defaults про это поле молчал.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// newLoadDefaultsServerWithCppWorker — сервер с фейковым воркером, который
// отдаёт outputBudget (R465).
func newLoadDefaultsServerWithCppWorker(t *testing.T) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cppworkerConfigPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"config": map[string]interface{}{
				"defaultCtxSize":   32768,
				"defaultBatchSize": 512,
				"defaultGpuLayers": -1,
			},
			"outputBudget": map[string]interface{}{
				"reasoningDefault":        8192,
				"reasoningDefaultSource":  "default",
				"reasoningDefaultEnv":     "",
				"ceiling":                 16384,
				"ceilingSource":           "env",
				"ceilingEnv":              "16384",
				"upperBoundRaisesDefault": true,
			},
		})
	}))
	t.Cleanup(srv.Close)

	hostPort := srv.URL[len("http://"):]
	host, port := "", 0
	for i := 0; i < len(hostPort); i++ {
		if hostPort[i] == ':' {
			host = hostPort[:i]
			for _, ch := range hostPort[i+1:] {
				port = port*10 + int(ch-'0')
			}
			break
		}
	}
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{{
			ID: "cppworker-1", Host: host, CppWorkerPort: port,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
		}},
	}
	p := balancer.NewProxy(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return NewServer(p, cfg, nil)
}

// TestR465_LoadDefaultsExposesOutputBudget — оператор видит число и источник.
func TestR465_LoadDefaultsExposesOutputBudget(t *testing.T) {
	s := newLoadDefaultsServerWithCppWorker(t)
	_, body := doLoadDefaults(t, s, http.MethodGet, "")

	effective, _ := body["effective"].(map[string]interface{})
	source, _ := body["source"].(map[string]interface{})
	if effective == nil || source == nil {
		t.Fatalf("ответ без effective/source: %v", body)
	}

	if got, _ := effective["numPredictReasoning"].(float64); got != 8192 {
		t.Fatalf("numPredictReasoning = %v, want 8192 (дефолт reasoning из воркера)", effective["numPredictReasoning"])
	}
	if src, _ := source["numPredictReasoning"].(string); src != "cppworker-default" {
		t.Fatalf("source numPredictReasoning = %q, want cppworker-default", src)
	}
	if got, _ := effective["numPredictCeiling"].(float64); got != 16384 {
		t.Fatalf("numPredictCeiling = %v, want 16384", effective["numPredictCeiling"])
	}
	if src, _ := source["numPredictCeiling"].(string); src != "cppworker-env" {
		t.Fatalf("source numPredictCeiling = %q, want cppworker-env", src)
	}
	if raises, ok := effective["numPredictUpperBoundRaises"].(bool); !ok || !raises {
		t.Fatalf("numPredictUpperBoundRaises = %v, want true (R463: граница клиента поднимает дефолт)",
			effective["numPredictUpperBoundRaises"])
	}
	// Раздел целиком тоже должен доходить до клиента — иначе нельзя показать
	// «почему такое число» без второго запроса.
	cw, _ := body["cppworker"].(map[string]interface{})
	if cw == nil || cw["outputBudget"] == nil {
		t.Fatalf("в ответе нет cppworker.outputBudget: %v", body["cppworker"])
	}
}

// TestR465_NoWorkerMeansNoneNotZero — без воркера источник «none», а не
// выдуманный ноль (иначе оператор читает 0 как «разрешено 0 токенов»).
func TestR465_NoWorkerMeansNoneNotZero(t *testing.T) {
	s, _ := newLoadDefaultsServer(t)
	_, body := doLoadDefaults(t, s, http.MethodGet, "")
	source, _ := body["source"].(map[string]interface{})
	effective, _ := body["effective"].(map[string]interface{})
	if _, ok := effective["numPredictReasoning"]; ok {
		t.Fatalf("без воркера число взяться не может: %v", effective["numPredictReasoning"])
	}
	if src, _ := source["numPredictReasoning"].(string); src != "none" {
		t.Fatalf("source = %q, want none", src)
	}
}
