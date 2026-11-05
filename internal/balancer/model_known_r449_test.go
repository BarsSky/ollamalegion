// model_known_r449_test.go — R449 (2026-11-05): «не загружена» ≠ «модели нет».
//
// ЖИВОЙ ДЕФЕКТ (стенд r261105-2238, порт 18079). Файл Qwen3-4B лежит на диске,
// но нагрузку держала другая модель (Qwen3.8-9B, шла загрузка 207 с). Ответ:
//
//	HTTP 404 {"code":"model_not_found","decision":"model_missing",
//	 "message":"... модели нет в каталоге бэкенда (available: Qwen3.8-9B-...)"}
//
// Причина: предпроверка R193 спрашивала /api/models, который перечисляет
// ЗАГРУЖЕННЫЕ модели, и «не среди загруженных» считала «нет модели». Для агента
// это терминальная ошибка, повторять которую он не станет — хотя воркер эту
// модель загрузил бы по первому запросу.
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newR449Proxy — прокси с одним llama.cpp-бэкендом, смотрящим на тестовый сервер.
func newR449Proxy(t *testing.T, srv *httptest.Server) *Proxy {
	t.Helper()
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, portStr, ok := strings.Cut(hostPort, ":")
	if !ok {
		t.Fatalf("не удалось разобрать URL тестового сервера: %s", srv.URL)
	}
	port := 0
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}
	p := &Proxy{
		backends: map[string]*BackendState{
			"bk-1": {Backend: &types.Backend{
				ID: "bk-1", Host: host, CppWorkerPort: port, Status: types.StatusHealthy,
				Type: types.BackendTypeLlamaCpp,
			}},
		},
		metricsMgr:   NewMetricsManager(),
		config:       &types.LoadBalancerConfig{},
		modelCatalog: newModelCatalog(),
	}
	return p
}

// mockWorkerR449 — воркер, у которого загружена ДРУГАЯ модель, а нужная лежит на диске.
func mockWorkerR449(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models":
			// Ключевое: перечислены ТОЛЬКО загруженные модели (так и отвечает cppworker).
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"count": 1,
				"models": []map[string]interface{}{{
					"name":  "other-model",
					"path":  "/app/models/other-model.gguf",
					"state": "loading",
				}},
			})
		case "/api/models/files":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"files": []map[string]interface{}{
					{"name": "wanted.gguf"},
					{"name": "other-model.gguf"},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestR449_OnDiskButNotLoadedIsAnswerable — файл на диске = модель есть.
func TestR449_OnDiskButNotLoadedIsAnswerable(t *testing.T) {
	srv := mockWorkerR449(t)
	defer srv.Close()
	p := newR449Proxy(t, srv)

	// Каталог файлов опрошен (это делает фоновый опрос балансера).
	p.modelCatalog.store("bk-1", []string{"wanted.gguf", "other-model.gguf"}, "", time.Now())

	if ok, available := p.modelAnswerable("bk-1", "wanted"); !ok {
		t.Fatalf("модель лежит на диске, но получен отказ (available=%v) — живой дефект вернулся", available)
	}
	// Имя с расширением и путь тоже должны опознаваться.
	if ok, _ := p.modelAnswerable("bk-1", "wanted.gguf"); !ok {
		t.Fatal("имя с расширением .gguf не опознано")
	}
	if ok, _ := p.modelAnswerable("bk-1", "/app/models/wanted.gguf"); !ok {
		t.Fatal("путь к файлу не опознан")
	}
}

// TestR449_AbsentEverywhereIsRefused — модели нет ни в загруженных, ни на диске.
func TestR449_AbsentEverywhereIsRefused(t *testing.T) {
	srv := mockWorkerR449(t)
	defer srv.Close()
	p := newR449Proxy(t, srv)
	p.modelCatalog.store("bk-1", []string{"wanted.gguf", "other-model.gguf"}, "", time.Now())

	ok, available := p.modelAnswerable("bk-1", "no-such-model")
	if ok {
		t.Fatal("модели нет нигде — отказ обязателен (R193: не отвечать чужой моделью)")
	}
	// Список в отказе должен быть про ДИСК, а не «что сейчас загружено»:
	// иначе оператор видит «доступна одна модель», хотя файлов два.
	joined := strings.Join(available, ",")
	if !strings.Contains(joined, "wanted.gguf") || !strings.Contains(joined, "other-model.gguf") {
		t.Fatalf("available = %v, want файлы с диска", available)
	}
}

// TestR449_UnknownCatalogDoesNotRefuse — каталог файлов не опрошен: не отказываем.
//
// Правило R193: предпроверка не должна становиться новым источником отказов.
func TestR449_UnknownCatalogDoesNotRefuse(t *testing.T) {
	srv := mockWorkerR449(t)
	defer srv.Close()
	p := newR449Proxy(t, srv) // каталог НЕ заполнен

	if ok, _ := p.modelAnswerable("bk-1", "wanted"); !ok {
		t.Fatal("каталог файлов не опрошен — отказывать нельзя (прежнее поведение)")
	}
}

// TestR449_LoadedModelStillAllowed — загруженная модель разрешена как раньше.
func TestR449_LoadedModelStillAllowed(t *testing.T) {
	srv := mockWorkerR449(t)
	defer srv.Close()
	p := newR449Proxy(t, srv)
	p.modelCatalog.store("bk-1", []string{"wanted.gguf"}, "", time.Now())

	if ok, _ := p.modelAnswerable("bk-1", "other-model"); !ok {
		t.Fatal("загруженная модель обязана быть разрешена")
	}
}
