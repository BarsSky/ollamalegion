// loading_retry_test.go — unit-тесты для loading_retry.go.
//
// Покрывают:
//   - loadingSignalFromBody: парсинг разных форматов 503-ответа от cppworker/ollama.
//   - checkLlamaCppModelLoaded: опрос /api/models/load/progress с состояниями loaded/loading/error.
//   - checkOllamaModelLoaded: опрос /api/ps с загруженными/незагруженными моделями.
//   - waitForBackendModelLoaded: успешный polling, timeout, context cancel.
//
// Использует httptest.NewServer для имитации бэкенда.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// =====================================================================
// loadingSignalFromBody tests
// =====================================================================

func TestLoadingSignalFromBody_CppWorkerFormat(t *testing.T) {
	body := []byte(`{"error":"model is loading: qwen3.6-72b","loading":true,"model":"qwen3.6-72b","elapsedMs":1500,"retryAfterMs":3000}`)
	isLoading, modelName, retryMs := loadingSignalFromBody(http.StatusServiceUnavailable, body)
	if !isLoading {
		t.Fatal("expected isLoading=true for cppworker format")
	}
	if modelName != "qwen3.6-72b" {
		t.Errorf("expected modelName=qwen3.6-72b, got %q", modelName)
	}
	if retryMs != 3000 {
		t.Errorf("expected retryMs=3000, got %d", retryMs)
	}
}

func TestLoadingSignalFromBody_LoadingTrueWithoutError(t *testing.T) {
	body := []byte(`{"loading":true,"model":"test-model"}`)
	isLoading, modelName, _ := loadingSignalFromBody(http.StatusServiceUnavailable, body)
	if !isLoading {
		t.Fatal("expected isLoading=true when loading:true field present")
	}
	if modelName != "test-model" {
		t.Errorf("expected modelName=test-model, got %q", modelName)
	}
}

func TestLoadingSignalFromBody_ErrorOnly(t *testing.T) {
	// Plain error string without loading:true — должен распознать по "model is loading".
	body := []byte(`{"error":"model is loading: foo"}`)
	isLoading, modelName, _ := loadingSignalFromBody(http.StatusServiceUnavailable, body)
	if !isLoading {
		t.Fatal("expected isLoading=true for error containing 'model is loading'")
	}
	if modelName != "foo" {
		t.Errorf("expected modelName=foo (extracted from error), got %q", modelName)
	}
}

func TestLoadingSignalFromBody_NotLoading(t *testing.T) {
	body := []byte(`{"error":"upstream connection refused"}`)
	isLoading, _, _ := loadingSignalFromBody(http.StatusServiceUnavailable, body)
	if isLoading {
		t.Fatal("expected isLoading=false for generic 503 error")
	}
}

func TestLoadingSignalFromBody_WrongStatus(t *testing.T) {
	// 200 с loading: true — НЕ считается loading-сигналом (это другой код).
	body := []byte(`{"loading":true,"model":"x"}`)
	isLoading, _, _ := loadingSignalFromBody(http.StatusOK, body)
	if isLoading {
		t.Fatal("expected isLoading=false when status is not 503")
	}
}

func TestLoadingSignalFromBody_DefaultRetryMs(t *testing.T) {
	body := []byte(`{"loading":true,"model":"x"}`)
	_, _, retryMs := loadingSignalFromBody(http.StatusServiceUnavailable, body)
	if retryMs != 3000 {
		t.Errorf("expected default retryMs=3000, got %d", retryMs)
	}
}

// =====================================================================
// checkLlamaCppModelLoaded tests
// =====================================================================

func TestCheckLlamaCppModelLoaded_Loaded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/load/progress" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("model") != "qwen" {
			t.Errorf("expected model=qwen query, got %q", r.URL.Query().Get("model"))
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"name": "qwen", "state": "loaded",
		})
	}))
	defer server.Close()

	p := &Proxy{}
	client := &http.Client{Timeout: 2 * time.Second}
	baseURL := server.URL

	loaded, err := p.checkLlamaCppModelLoaded(context.Background(), client, baseURL, "qwen")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loaded {
		t.Fatal("expected loaded=true for state=loaded")
	}
}

func TestCheckLlamaCppModelLoaded_Loading(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"name": "qwen", "state": "loading",
		})
	}))
	defer server.Close()

	p := &Proxy{}
	client := &http.Client{Timeout: 2 * time.Second}
	loaded, err := p.checkLlamaCppModelLoaded(context.Background(), client, server.URL, "qwen")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded {
		t.Fatal("expected loaded=false for state=loading")
	}
}

func TestCheckLlamaCppModelLoaded_NotFound(t *testing.T) {
	// 404 → best-effort loaded=true (модель не в loading-списке, значит, основной
	// запрос либо пройдёт, либо снова вернёт 503).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer server.Close()

	p := &Proxy{}
	client := &http.Client{Timeout: 2 * time.Second}
	loaded, err := p.checkLlamaCppModelLoaded(context.Background(), client, server.URL, "qwen")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loaded {
		t.Fatal("expected loaded=true for 404 (best-effort)")
	}
}

func TestCheckLlamaCppModelLoaded_ErrorState(t *testing.T) {
	// state="error" → loaded=true (best-effort, основной запрос покажет ошибку).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"name": "qwen", "state": "error", "error": "load failed",
		})
	}))
	defer server.Close()

	p := &Proxy{}
	client := &http.Client{Timeout: 2 * time.Second}
	loaded, err := p.checkLlamaCppModelLoaded(context.Background(), client, server.URL, "qwen")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loaded {
		t.Fatal("expected loaded=true for state=error (best-effort)")
	}
}

// =====================================================================
// checkOllamaModelLoaded tests
// =====================================================================

func TestCheckOllamaModelLoaded_Present(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]string{
				{"name": "llama3:latest"},
				{"name": "qwen3:7b"},
			},
		})
	}))
	defer server.Close()

	p := &Proxy{}
	client := &http.Client{Timeout: 2 * time.Second}
	loaded, err := p.checkOllamaModelLoaded(context.Background(), client, server.URL, "qwen3:7b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loaded {
		t.Fatal("expected loaded=true when model in /api/ps")
	}
}

func TestCheckOllamaModelLoaded_PresentNoTag(t *testing.T) {
	// Ollama возвращает "qwen3:7b", клиент ищет "qwen3" → должно совпасть по prefix.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]string{{"name": "qwen3:7b"}},
		})
	}))
	defer server.Close()

	p := &Proxy{}
	client := &http.Client{Timeout: 2 * time.Second}
	loaded, err := p.checkOllamaModelLoaded(context.Background(), client, server.URL, "qwen3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loaded {
		t.Fatal("expected loaded=true (prefix match)")
	}
}

func TestCheckOllamaModelLoaded_NotPresent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []map[string]string{{"name": "other-model:latest"}},
		})
	}))
	defer server.Close()

	p := &Proxy{}
	client := &http.Client{Timeout: 2 * time.Second}
	loaded, err := p.checkOllamaModelLoaded(context.Background(), client, server.URL, "qwen3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded {
		t.Fatal("expected loaded=false when model not in /api/ps")
	}
}

func TestCheckOllamaModelLoaded_EmptyList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"models": []interface{}{},
		})
	}))
	defer server.Close()

	p := &Proxy{}
	client := &http.Client{Timeout: 2 * time.Second}
	loaded, err := p.checkOllamaModelLoaded(context.Background(), client, server.URL, "qwen3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loaded {
		t.Fatal("expected loaded=false for empty list")
	}
}

// =====================================================================
// waitForBackendModelLoaded tests
// =====================================================================

func TestWaitForBackendModelLoaded_ImmediateSuccess(t *testing.T) {
	// Бэкенд сразу отдаёт state=loaded → polling делает 1 запрос, возвращает true.
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "m", "state": "loaded"})
	}))
	defer server.Close()

	p := &Proxy{}
	backend := &types.Backend{
		ID:           "test",
		Host:         "127.0.0.1",
		CppWorkerPort: parsePortFromURL(server.URL),
		Engine:       types.EngineLlamaCPP,
		Type:         types.BackendTypeLlamaCpp,
	}

	loaded, err := p.waitForBackendModelLoaded(context.Background(), backend, "m", "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loaded {
		t.Fatal("expected loaded=true")
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Errorf("expected 1 poll call, got %d", c)
	}
}

func TestWaitForBackendModelLoaded_RetryUntilLoaded(t *testing.T) {
	// Первые 2 опроса — loading, третий — loaded. Проверяем, что polling работает.
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&callCount, 1)
		w.WriteHeader(http.StatusOK)
		if c < 3 {
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "m", "state": "loading"})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "m", "state": "loaded"})
		}
	}))
	defer server.Close()

	p := &Proxy{}
	backend := &types.Backend{
		ID:           "test",
		Host:         "127.0.0.1",
		CppWorkerPort: parsePortFromURL(server.URL),
		Engine:       types.EngineLlamaCPP,
		Type:         types.BackendTypeLlamaCpp,
	}

	// Используем короткий interval для ускорения теста.
	originalInterval := loadingRetryInterval
	// (для теста используем оригинальный, чтобы не ломать константу)
	_ = originalInterval

	loaded, err := p.waitForBackendModelLoaded(context.Background(), backend, "m", "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loaded {
		t.Fatal("expected loaded=true after retries")
	}
	if c := atomic.LoadInt32(&callCount); c != 3 {
		t.Errorf("expected 3 poll calls, got %d", c)
	}
}

func TestWaitForBackendModelLoaded_ContextCancel(t *testing.T) {
	// Бэкенд всегда отдаёт loading; ctx отменяется через 100ms → polling выходит.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "m", "state": "loading"})
	}))
	defer server.Close()

	p := &Proxy{}
	backend := &types.Backend{
		ID:           "test",
		Host:         "127.0.0.1",
		CppWorkerPort: parsePortFromURL(server.URL),
		Engine:       types.EngineLlamaCPP,
		Type:         types.BackendTypeLlamaCpp,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	loaded, err := p.waitForBackendModelLoaded(ctx, backend, "m", "test")
	// При context cancel: либо loaded=false без err, либо err != nil.
	// Главное — функция вышла в пределах таймаута, а не зависла на 30 сек.
	if loaded {
		t.Fatal("expected loaded=false (context cancelled)")
	}
	if err == nil {
		t.Log("waitForBackendModelLoaded returned (false, nil) on context cancel — acceptable")
	} else if err != context.DeadlineExceeded && err != context.Canceled {
		t.Errorf("expected context error, got: %v", err)
	}
}

func TestWaitForBackendModelLoaded_NilBackend(t *testing.T) {
	p := &Proxy{}
	loaded, err := p.waitForBackendModelLoaded(context.Background(), nil, "m", "test")
	if loaded || err != nil {
		t.Errorf("expected (false, nil) for nil backend, got (%v, %v)", loaded, err)
	}
}

func TestWaitForBackendModelLoaded_EmptyModelName(t *testing.T) {
	p := &Proxy{}
	backend := &types.Backend{ID: "test", Host: "127.0.0.1", CppWorkerPort: 18092}
	loaded, err := p.waitForBackendModelLoaded(context.Background(), backend, "", "test")
	if loaded || err != nil {
		t.Errorf("expected (false, nil) for empty model, got (%v, %v)", loaded, err)
	}
}

func TestWaitForBackendModelLoaded_BackendUnreachable(t *testing.T) {
	// Бэкенд закрыт → первая же попытка poll даст network error.
	p := &Proxy{}
	backend := &types.Backend{
		ID:           "test",
		Host:         "127.0.0.1",
		CppWorkerPort: 1, // порт 1 обычно закрыт
		Engine:       types.EngineLlamaCPP,
		Type:         types.BackendTypeLlamaCpp,
	}

	loaded, err := p.waitForBackendModelLoaded(context.Background(), backend, "m", "test")
	if loaded {
		t.Fatal("expected loaded=false when backend unreachable")
	}
	if err == nil {
		t.Error("expected error when backend unreachable")
	}
}

// =====================================================================
// checkModelLoadedOnBackend — engine dispatch tests
// =====================================================================

func TestCheckModelLoadedOnBackend_DispatchLlamaCpp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Проверяем, что идём на /api/models/load/progress (llama.cpp),
		// а не /api/ps (ollama).
		if r.URL.Path != "/api/models/load/progress" {
			t.Errorf("expected llama.cpp path, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "m", "state": "loaded"})
	}))
	defer server.Close()

	p := &Proxy{}
	client := &http.Client{Timeout: 2 * time.Second}
	loaded, err := p.checkModelLoadedOnBackend(context.Background(), client, types.EngineLlamaCPP, server.URL, "m")
	if err != nil || !loaded {
		t.Errorf("expected (true, nil), got (%v, %v)", loaded, err)
	}
}

func TestCheckModelLoadedOnBackend_DispatchOllama(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Проверяем, что идём на /api/ps (ollama), а не /api/models/load/progress.
		if r.URL.Path != "/api/ps" {
			t.Errorf("expected ollama path, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"models": []map[string]string{{"name": "m"}}})
	}))
	defer server.Close()

	p := &Proxy{}
	client := &http.Client{Timeout: 2 * time.Second}
	loaded, err := p.checkModelLoadedOnBackend(context.Background(), client, types.EngineOllamaAPI, server.URL, "m")
	if err != nil || !loaded {
		t.Errorf("expected (true, nil), got (%v, %v)", loaded, err)
	}
}

// =====================================================================
// parsePortFromURL — вспомогательный helper для тестов
// =====================================================================

func parsePortFromURL(u string) int {
	// httptest URL имеет формат http://127.0.0.1:NNNN
	idx := strings.LastIndex(u, ":")
	if idx < 0 {
		return 0
	}
	portStr := u[idx+1:]
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)
	return port
}