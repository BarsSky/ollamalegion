// llamacpp_handlers_readonly_round19_test.go — Round 19 hotfix tests
// (2026-08-03). User reported bug: balancer /api/tags returns ONLY loaded
// models when ≥1 is loaded, hiding on-disk .gguf files from clients.
//
// These tests verify the fix: handleTags / handleOpenAIModels ALWAYS
// aggregate loaded + on-disk models, regardless of how many are loaded.
//
// Uses an in-process cppworker stub (no real network) that serves
// /api/models/files with the on-disk .gguf list. We test:
//   T1: 1 loaded + 2 on disk → /api/tags returns 2 (dedup: model-A in both → 1 entry)
//   T2: 0 loaded + 1 on disk → /api/tags returns 1
//   T3: 1 loaded + 2 on disk → /v1/models returns 2 (dedup)
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// fileStub — минимальный cppworker stub который отдаёт /api/models/files.
// Реальная логика listModels/backend не нужна для этого теста — мы только
// проверяем что balancer запрашивает /api/models/files ВСЕГДА, не только
// когда loaded=0.
type fileStub struct {
	mu       sync.Mutex
	files    []stubFileEntry
	failNext bool
}

type stubFileEntry struct {
	Name       string `json:"name"`
	SizeBytes  int64  `json:"sizeBytes"`
	ModifiedAt string `json:"modifiedAt"`
}

func (f *fileStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/models/files":
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failNext {
			f.failNext = false
			http.Error(w, "stub forced failure", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"files": f.files,
			"count": len(f.files),
		})
	case "/health":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	default:
		http.NotFound(w, r)
	}
}

// buildProxyWithStub создаёт Proxy + LlamaCppRouter + metrics с 1 stub-бэкендом
// на /api/models/files. Поддерживает опциональный список preloaded моделей.
func buildProxyWithStub(t *testing.T, files []stubFileEntry, loadedModels []types.LlamaCppModel) (*Proxy, *httptest.Server) {
	t.Helper()
	stub := &fileStub{files: files}
	ts := httptest.NewServer(stub)
	t.Cleanup(ts.Close)

	hostPort := strings.TrimPrefix(ts.URL, "http://")
	idx := strings.LastIndex(hostPort, ":")
	if idx < 0 {
		t.Fatalf("bad hostPort: %q", hostPort)
	}
	host := hostPort[:idx]
	port := 0
	for i := idx + 1; i < len(hostPort); i++ {
		port = port*10 + int(hostPort[i]-'0')
	}

	cfg := &types.LoadBalancerConfig{}
	cfg.Backends = []types.Backend{
		{
			ID:            "stub-1",
			Type:          types.BackendTypeLlamaCpp,
			Host:          host,
			CppWorkerPort: port,
			Status:        types.StatusHealthy,
		},
	}
	proxy := newProxyWithCleanup(t, cfg)
	proxy.llamaCppRouter = &LlamaCppRouter{proxy: proxy}
	proxy.backends["stub-1"] = &BackendState{Backend: &cfg.Backends[0]}

	if loadedModels != nil {
		// Round 19 fix: пишем напрямую в llamaMetrics (где хранит cppworker-poller),
		// а не в metrics[id].LlamaCpp (где хранит Ollama-agent).
		proxy.metricsMgr.mu.Lock()
		proxy.metricsMgr.llamaMetrics["stub-1"] = &types.LlamaCppMetrics{
			LoadedModels: loadedModels,
		}
		proxy.metricsMgr.mu.Unlock()
	}

	return proxy, ts
}

// TestRound19_HandleTags_AlwaysIncludesOnDisk — главный тест бага.
//
// Сценарий: backend имеет 1 loaded (model-A) + 2 on-disk (model-A, model-B).
// /api/tags должен вернуть 2 (dedup: model-A в обоих источниках → 1 entry).
// До Round 19 фикса возвращал 1 (только loaded = model-A).
func TestRound19_HandleTags_AlwaysIncludesOnDisk(t *testing.T) {
	proxy, _ := buildProxyWithStub(t,
		[]stubFileEntry{
			{Name: "model-A.gguf", SizeBytes: 1_000_000_000, ModifiedAt: "2026-08-01T10:00:00Z"},
			{Name: "model-B.gguf", SizeBytes: 2_000_000_000, ModifiedAt: "2026-08-02T10:00:00Z"},
		},
		[]types.LlamaCppModel{
			{Name: "model-A", Path: "/app/models/model-A.gguf", State: "loaded"},
		},
	)

	// Test /api/tags — должно вернуть 2 (1 loaded + 2 on-disk, dedup → 2).
	tagsReq := httptest.NewRequest("GET", "/api/tags", nil)
	tagsW := httptest.NewRecorder()
	proxy.llamaCppRouter.handleTags(tagsW, tagsReq)
	if tagsW.Code != http.StatusOK {
		t.Fatalf("handleTags: expected 200, got %d (body: %s)", tagsW.Code, tagsW.Body.String())
	}
	var tagsResp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(tagsW.Body.Bytes(), &tagsResp); err != nil {
		t.Fatalf("handleTags: bad JSON: %v", err)
	}
	if len(tagsResp.Models) != 2 {
		t.Errorf("handleTags: expected 2 models (1 loaded + 2 on-disk, dedup), got %d: %+v",
			len(tagsResp.Models), tagsResp.Models)
	}

	// Test /v1/models — должно вернуть 2.
	openaiReq := httptest.NewRequest("GET", "/v1/models", nil)
	openaiW := httptest.NewRecorder()
	proxy.llamaCppRouter.handleOpenAIModels(openaiW, openaiReq)
	if openaiW.Code != http.StatusOK {
		t.Fatalf("handleOpenAIModels: expected 200, got %d", openaiW.Code)
	}
	var openaiResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(openaiW.Body.Bytes(), &openaiResp); err != nil {
		t.Fatalf("handleOpenAIModels: bad JSON: %v", err)
	}
	if len(openaiResp.Data) != 2 {
		t.Errorf("handleOpenAIModels: expected 2 models, got %d: %+v",
			len(openaiResp.Data), openaiResp.Data)
	}
}

// TestRound19_HandleTags_NoLoaded_StillReturnsOnDisk — если loaded=0, всё равно
// отдаём on-disk список (regression: новый /api/models/files source покрывает
// этот случай; раньше работало через fallback на /api/tags, но мы хотим
// убедиться что primary source корректно работает).
func TestRound19_HandleTags_NoLoaded_StillReturnsOnDisk(t *testing.T) {
	proxy, _ := buildProxyWithStub(t,
		[]stubFileEntry{
			{Name: "model-X.gguf", SizeBytes: 3_000_000_000, ModifiedAt: "2026-08-01T10:00:00Z"},
		},
		nil, // 0 loaded
	)

	tagsReq := httptest.NewRequest("GET", "/api/tags", nil)
	tagsW := httptest.NewRecorder()
	proxy.llamaCppRouter.handleTags(tagsW, tagsReq)
	var tagsResp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	_ = json.Unmarshal(tagsW.Body.Bytes(), &tagsResp)
	if len(tagsResp.Models) != 1 {
		t.Errorf("handleTags: expected 1 on-disk model, got %d: %+v",
			len(tagsResp.Models), tagsResp.Models)
	}
	if len(tagsResp.Models) > 0 && tagsResp.Models[0].Name != "model-X" {
		t.Errorf("handleTags: expected name=model-X (without .gguf), got %q", tagsResp.Models[0].Name)
	}
}
