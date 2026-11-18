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

// model_catalog_backend_r516_test.go — R516 (2026-11-19): каталог моделей обязан
// называть узел, у которого прочитан список файлов.
//
// ЗАЧЕМ. `handleModelCatalog` читает `/api/models/files` у ОДНОГО воркера
// (`firstCppWorkerBackend`), а ответ об этом не говорил. Страница «Справка и
// шаблоны» (§2.7 разбора) из-за этого не могла предложить узел, где файл модели
// ЛЕЖИТ НА ДИСКЕ, но модель ещё не загружена: `loadedTargetsFor` брал только
// `/api/v1/cluster/models/loaded`, а сервер для применения требует лишь
// существования бэкенда. Живая жалоба: «где поднять» ждёт загруженную модель.
//
// Тест держит две вещи: (1) узел назван явно; (2) если воркера нет вовсе, поле
// пустое — страница не должна придумывать узел, которого не спрашивала.

// r516CatalogServer — сервер с одним llama.cpp-бэкендом и заглушкой воркера.
func r516CatalogServer(t *testing.T) *Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/files":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[{"name":"on-disk-only.gguf","sizeBytes":123456}]}`))
		case "/api/v1/cppworker/config":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"config":{"defaultCtxSize":32768}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("не разобрал URL upstream %q: %v", upstream.URL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("не разобрал порт upstream %q: %v", upstream.URL, err)
	}

	s, _ := newLoadDefaultsServer(t)
	if err := s.proxy.AddBackend(types.Backend{
		ID:            "cppworker-test",
		Host:          "127.0.0.1",
		CppWorkerPort: port,
		Type:          types.BackendTypeLlamaCpp,
		Status:        types.StatusHealthy,
	}); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	return s
}

func doCatalogR516(t *testing.T, s *Server) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/model-catalog", nil)
	w := httptest.NewRecorder()
	s.handleModelCatalog(w, req)
	var parsed map[string]interface{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("ответ не JSON: %v (%s)", err, w.Body.String())
		}
	}
	return w.Code, parsed
}

// TestR516_CatalogNamesBackend — узел, у которого прочитан список файлов, назван.
func TestR516_CatalogNamesBackend(t *testing.T) {
	s := r516CatalogServer(t)
	code, resp := doCatalogR516(t, s)
	if code != http.StatusOK {
		t.Fatalf("model-catalog: %d", code)
	}
	got, _ := resp["backendId"].(string)
	if got != "cppworker-test" {
		t.Fatalf("backendId = %q, want %q — страница не сможет привязать файлы к узлу и снова "+
			"будет предлагать только уже загруженные модели", got, "cppworker-test")
	}
	files, _ := resp["files"].([]interface{})
	if len(files) != 1 {
		t.Fatalf("файлов в каталоге: %d, want 1", len(files))
	}
}

// TestR516_CatalogWithoutWorkerHasNoBackend — узла нет, и это видно честно:
// пустое имя, а не выдуманное. Страница обязана показать только «нет данных».
func TestR516_CatalogWithoutWorkerHasNoBackend(t *testing.T) {
	s, _ := newLoadDefaultsServer(t)
	code, resp := doCatalogR516(t, s)
	if code != http.StatusOK {
		t.Fatalf("model-catalog без воркера: %d", code)
	}
	got, exists := resp["backendId"]
	if !exists {
		t.Fatalf("поля backendId нет вовсе — страница не отличит «узла нет» от «узел не назван»")
	}
	if name, _ := got.(string); name != "" {
		t.Fatalf("без воркера backendId = %q, want пусто", name)
	}
	if files, _ := resp["files"].([]interface{}); len(files) != 0 {
		t.Fatalf("без воркера файлов %d, want 0", len(files))
	}
}
