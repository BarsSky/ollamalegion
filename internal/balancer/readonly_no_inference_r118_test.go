// readonly_no_inference_r118_test.go — R118 (2026-10-14): READ-ONLY/MGMT пути
// Ollama-поверхности НЕ идут в inference-транспорт.
//
// ЖИВОЙ ДЕФЕКТ (OpenWebUI → POST /api/show на :18080). Запрос уходил в
// proxyRequest → proxyRequestLlamaCppNonStream, где живёт preflight n_ctx. Тот
// видел `loaded_n_ctx=0` у незагруженной модели, решал «надо перезагрузить» и
// РЕАЛЬНО запускал загрузку:
//
//	proxyRequest: checking backend type for llamacpp routing  path=/api/show
//	preflightNCtxReload: detected n_ctx mismatch, loaded_n_ctx=0 requested=32768
//	preflightNCtxReload(async): starting reload ... target_n_ctx=32768
//	cppworker: loading model ctxSize=32768 gpuLayers=-2 (partial_offload/cpu_only)
//	proxyRequestLlamaCppNonStream: preflight n_ctx reload, returning 503
//
// Последствия на стенде: чтение метаданных грузило модель почти целиком на CPU,
// CPU уходил в 96%, сторож checkResourceLimits начинал блокировать бэкенд, а
// клиент (OpenWebUI 503 не повторяет) видел «Server Connection Error».
//
// Контракт Ollama, который держат эти тесты: read-only путь отвечает СРАЗУ и
// НИКОГДА не запускает загрузку/перезагрузку модели.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// r118ReadOnlyEnvs — пути, которые по контракту Ollama читают состояние.
var r118ReadOnlyEnvs = []string{
	"/api/show", "/api/tags", "/api/ps", "/api/version", "/v1/models",
	"/api/models", "/api/models/files", "/api/pull", "/api/copy",
	"/api/delete", "/api/create", "/api/blobs/sha256:abc",
}

// TestR118_ReadOnlyPathListIsPinned — список read-only путей защищён тестом.
//
// Если путь выпадет из isReadOnlyOrMgmtEndpoint, он снова поедет в
// inference-транспорт (preflight + авто-загрузка + 503) — это и есть дефект R118.
func TestR118_ReadOnlyPathListIsPinned(t *testing.T) {
	for _, path := range r118ReadOnlyEnvs {
		if !isReadOnlyOrMgmtEndpoint(path) {
			t.Errorf("isReadOnlyOrMgmtEndpoint(%q) = false, want true: путь снова попадёт "+
				"в inference-транспорт (preflight n_ctx + авто-загрузка + 503)", path)
		}
	}
	// Граница: inference-пути read-only НЕ являются, их поведение (ожидание слота)
	// менять этим правилом нельзя.
	for _, path := range []string{"/api/chat", "/api/generate", "/v1/chat/completions", "/api/embed"} {
		if isReadOnlyOrMgmtEndpoint(path) {
			t.Errorf("isReadOnlyOrMgmtEndpoint(%q) = true, want false: inference-путь "+
				"потерял бы admission-очередь и n_ctx preflight", path)
		}
	}
}

// r118ProxyWithFakeBackend — Proxy с одним llama.cpp-бэкендом, который смотрит на
// httptest-сервер. Возвращает также журнал путей, которые бэкенд реально получил.
func r118ProxyWithFakeBackend(t *testing.T, srv *httptest.Server) (*Proxy, *pathRecorder) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse httptest url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("httptest port: %v", err)
	}

	rec := &pathRecorder{}
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["bk"] = &BackendState{
		Backend: &types.Backend{
			ID:            "bk",
			Type:          types.BackendTypeLlamaCpp,
			Host:          u.Hostname(),
			CppWorkerPort: port,
			Status:        types.StatusHealthy,
		},
	}
	return p, rec
}

// pathRecorder — потокобезопасный журнал путей, полученных фейковым бэкендом.
type pathRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *pathRecorder) add(p string) {
	r.mu.Lock()
	r.paths = append(r.paths, p)
	r.mu.Unlock()
}

func (r *pathRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

// TestR118_ShowIsDirectProxyWithoutLoad — POST /api/show уходит на бэкенд напрямую:
// ответ бэкенда доезжает до клиента, а загрузка/перезагрузка модели НЕ запускается.
func TestR118_ShowIsDirectProxyWithoutLoad(t *testing.T) {
	rec := &pathRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		if r.URL.Path == "/api/show" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"family":"qwen"},"model_info":{"n_ctx":8192}}`))
			return
		}
		// Любой путь загрузки/перезагрузки — это и есть дефект R118.
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"loading"}`))
	}))
	defer srv.Close()

	p, _ := r118ProxyWithFakeBackend(t, srv)
	_ = rec

	req := httptest.NewRequest(http.MethodPost, "/api/show",
		strings.NewReader(`{"name":"Qwen3.8-27B-UD-Q4_K_M"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	if err := p.proxyRequest(w, req, "bk"); err != nil {
		t.Fatalf("proxyRequest(/api/show): %v", err)
	}

	if w.Code != http.StatusOK {
		t.Errorf("POST /api/show → HTTP %d, want 200 (read-only путь не имеет права "+
			"отвечать 503 «идёт перезагрузка»); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "qwen") {
		t.Errorf("ответ бэкенда не доехал до клиента: %s", w.Body.String())
	}

	for _, got := range rec.all() {
		switch {
		case got == "/api/models/load" || got == "/api/models/reload":
			t.Errorf("read-only путь запустил загрузку/перезагрузку модели (%s): "+
				"POST /api/show обязан только читать метаданные", got)
		case strings.HasPrefix(got, "/api/models/load"):
			t.Errorf("read-only путь обратился к загрузке модели (%s)", got)
		}
	}
}

// TestR118_ReadOnlyPathsNeverTouchInferenceTransport — все read-only пути идут
// прямым прокси, ни один из них не попадает в preflight n_ctx (а значит не может
// запустить загрузку и вернуть 503).
func TestR118_ReadOnlyPathsNeverTouchInferenceTransport(t *testing.T) {
	for _, path := range r118ReadOnlyEnvs {
		t.Run(path, func(t *testing.T) {
			rec := &pathRecorder{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.add(r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer srv.Close()

			p, _ := r118ProxyWithFakeBackend(t, srv)
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()

			if err := p.proxyRequest(w, req, "bk"); err != nil {
				t.Fatalf("proxyRequest(%s): %v", path, err)
			}
			if w.Code != http.StatusOK {
				t.Errorf("%s → HTTP %d, want 200 (прямой прокси)", path, w.Code)
			}
			paths := rec.all()
			if len(paths) != 1 || paths[0] != path {
				t.Errorf("%s: бэкенд получил %v, want ровно один запрос к %s", path, paths, path)
			}
		})
	}
}
