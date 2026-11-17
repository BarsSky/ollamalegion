package balancer

// models_aggregate_r493_test.go — дефект 2 из разбора 12c: агрегат /api/models
// балансера терял окно НА СЛОТ и число слотов.
//
// ЖИВОЙ ФАКТ (архив второго стенда 2026-11-16): воркер в /api/models отдаёт
// context_per_seq, max_slots, parallel, kv_cache_type, а агрегат
// `handleModels` их выбрасывал (в ответе балансера были только суммарное
// contextSize и фактический offload). На стенде с parallel=1 это невидимо, но
// при parallel>1 клиент видит суммарное окно и принимает его за своё — это класс
// дефекта R83 на другой ручке.
//
// Тест проверяет контракт ответа: поля доезжают из воркера до клиента, а ноль
// (старая сборка воркера, поле не сообщено) не выдаётся за факт.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestR493_ModelsAggregateKeepsPerSlotWindow(t *testing.T) {
	// Фейковый воркер: отдаёт ровно то, что печатает cppworker в /api/models.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"models":[{
			"name":"Qwen3.8-27B","path":"/models/q.gguf","state":"loaded",
			"size_bytes":16464440224,"n_layers":64,"n_embd":5120,"n_vocab":151936,
			"context_size":188416,"gpu_layers_actual":62,"active_queries":1,
			"loaded_at":"2026-11-16T14:47:30Z",
			"context_per_seq":94208,"max_slots":2,"parallel":2,"kv_cache_type":"q4_0"
		}]}`))
	}))
	defer worker.Close()

	host, port := splitHostPortR493(t, worker.URL)

	conf := &types.LoadBalancerConfig{
		Backends: []types.Backend{{
			ID: "w1", Name: "w1", Host: host, CppWorkerPort: port,
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
		}},
	}
	p := newProxyWithCleanup(t, conf)
	lr := p.llamaCppRouter
	if lr == nil {
		t.Fatal("llamaCppRouter не создан")
	}

	rec := httptest.NewRecorder()
	lr.handleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
	}

	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, rec.Body.String())
	}
	if len(out.Models) != 1 {
		t.Fatalf("моделей %d, ожидалась 1: %s", len(out.Models), rec.Body.String())
	}
	m := out.Models[0]
	for field, want := range map[string]float64{
		"contextSize":   188416, // суммарное окно остаётся
		"contextPerSeq": 94208,  // и окно НА СЛОТ теперь тоже
		"maxSlots":      2,
		"parallel":      2,
	} {
		got, ok := m[field]
		if !ok {
			t.Errorf("в ответе нет поля %q (клиент примет суммарное окно за своё)", field)
			continue
		}
		if got.(float64) != want {
			t.Errorf("%s = %v, ожидалось %v", field, got, want)
		}
	}
	if m["kvCacheType"] != "q4_0" {
		t.Errorf("kvCacheType = %v, ожидалось q4_0", m["kvCacheType"])
	}
}

// splitHostPortR493 — разобрать URL httptest-сервера на host и порт.
func splitHostPortR493(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u := rawURL[len("http://"):]
	for i := len(u) - 1; i >= 0; i-- {
		if u[i] == ':' {
			port, err := strconv.Atoi(u[i+1:])
			if err != nil {
				t.Fatalf("порт в %q: %v", rawURL, err)
			}
			return u[:i], port
		}
	}
	t.Fatalf("в URL %q нет порта", rawURL)
	return "", 0
}
