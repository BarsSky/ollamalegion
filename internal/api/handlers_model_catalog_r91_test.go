// handlers_model_catalog_r91_test.go — R91 (2026-10-09): read-only каталог
// моделей на диске (GET /api/v1/models/catalog).
//
// Проверяем то, ради чего ручка существует: оператор видит, что балансер знает
// про файлы на узлах, насколько свеж снимок, и может принудительно перечитать
// листинги (?refresh=true) — вместо лазания по логам при 503 model_not_found.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// catalogFileStub — воркер, отдающий /api/models/files.
//
// Порядок полей — по требованию govet fieldalignment (указателесодержащие первыми).
type catalogFileStub struct {
	srv   *httptest.Server
	files []string
	mu    sync.Mutex
	hits  int
}

func newCatalogFileStub(t *testing.T, files ...string) *catalogFileStub {
	t.Helper()
	s := &catalogFileStub{files: files}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/files" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.mu.Lock()
		s.hits++
		files := append([]string{}, s.files...)
		s.mu.Unlock()
		items := make([]map[string]any, 0, len(files))
		for _, name := range files {
			items = append(items, map[string]any{"name": name})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"files": items, "aliases": []any{}})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *catalogFileStub) port(t *testing.T) int {
	t.Helper()
	port, err := strconv.Atoi(strings.TrimPrefix(s.srv.URL, "http://127.0.0.1:"))
	if err != nil {
		t.Fatalf("parse stub port: %v", err)
	}
	return port
}

func newCatalogAPIServer(t *testing.T, stub *catalogFileStub) *Server {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{{
			ID: "cppworker-1", Name: "w", Host: "127.0.0.1", CppWorkerPort: stub.port(t),
			Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1,
		}},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, HealthCheckInterval: 10,
			MetricsInterval: 5, RequestTimeout: 30, QueueTimeout: 60, QueueMaxSize: 100, QueueWorkers: 4,
		},
		API:  types.APISettings{RateLimit: 500, RateBurst: 1000},
		Auth: types.AuthConfig{Enabled: false},
	}
	proxy := balancer.NewProxy(cfg)
	t.Cleanup(func() { _ = proxy.Shutdown(context.Background()) })
	return NewServer(proxy, cfg, balancer.NewHealthChecker(proxy, 10*time.Second, 3))
}

func getModelsCatalog(t *testing.T, s *Server, query string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleModelsCatalog(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models/catalog"+query, nil))
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec.Code, doc
}

// TestModelsCatalogR91_RefreshShowsOnDiskFiles — ?refresh=true перечитывает
// листинги и отдаёт их в снимке.
func TestModelsCatalogR91_RefreshShowsOnDiskFiles(t *testing.T) {
	stub := newCatalogFileStub(t, "gemma-4-E4B-it-Q4_K_M.gguf", "qwen3.8-27b.gguf")
	s := newCatalogAPIServer(t, stub)

	code, doc := getModelsCatalog(t, s, "?refresh=true")
	if code != http.StatusOK {
		t.Fatalf("status = %d, ожидался 200 (%v)", code, doc)
	}
	if doc["refreshed"] != true {
		t.Errorf("refreshed = %v, ожидалось true", doc["refreshed"])
	}
	backends, _ := doc["backends"].([]any)
	if len(backends) != 1 {
		t.Fatalf("backends = %v, ожидалась одна запись", doc["backends"])
	}
	row, _ := backends[0].(map[string]any)
	if row["backendId"] != "cppworker-1" || row["known"] != true {
		t.Fatalf("строка бэкенда не описывает снимок: %v", row)
	}
	files, _ := row["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("files = %v, ожидались два файла", row["files"])
	}
	if !strings.Contains(files[0].(string), "gemma-4") {
		t.Errorf("в файлах нет gemma-4: %v", files)
	}
	if int(row["ageSec"].(float64)) > 60 {
		t.Errorf("снимок только что снят, ageSec=%v", row["ageSec"])
	}
}

// TestModelsCatalogR91_UnknownIsNotAnEmptyDisk — «не опрошен» и «пусто на диске»
// различимы: без снимка known=false и есть пояснение.
func TestModelsCatalogR91_UnknownIsNotAnEmptyDisk(t *testing.T) {
	// Фоновое обновление выключаем ДО создания прокси: иначе немедленный первый
	// проход poller'а успел бы снять снимок и тест проверял бы не то.
	t.Setenv(balancer.EnvModelCatalogRefreshSec, "0")
	stub := newCatalogFileStub(t, "gemma-4-E4B-it-Q4_K_M.gguf")
	s := newCatalogAPIServer(t, stub)

	code, doc := getModelsCatalog(t, s, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if doc["refreshed"] != false {
		t.Errorf("refreshed = %v, без ?refresh=true обновлять не должны", doc["refreshed"])
	}
	row, _ := doc["backends"].([]any)[0].(map[string]any)
	if row["known"] != false {
		t.Fatalf("без опроса known должен быть false: %v", row)
	}
	note, _ := row["note"].(string)
	if !strings.Contains(note, "не опрошен") {
		t.Errorf("нет пояснения про неопрошенный каталог: %q", note)
	}
	if stub.hits != 0 {
		t.Errorf("при LB_MODEL_CATALOG_REFRESH_SEC=0 опросов быть не должно (hits=%d)", stub.hits)
	}
}

// TestModelsCatalogR91_MethodNotAllowed — ручка read-only.
func TestModelsCatalogR91_MethodNotAllowed(t *testing.T) {
	stub := newCatalogFileStub(t)
	s := newCatalogAPIServer(t, stub)
	rec := httptest.NewRecorder()
	s.handleModelsCatalog(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/catalog", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: status = %d, ожидался 405", rec.Code)
	}
}
