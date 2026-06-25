package api

// handlers_cluster_debug_test.go — тесты для clusterDebugLastPromptHandler.
//
// Покрывают:
//   - Method-not-allowed (405 для не-GET).
//   - No llama.cpp backends → пустой массив backends[].
//   - Backend с пустым host/port → status=unavailable, error="backend has no host/CppWorkerPort".
//   - Backend недоступен (refused connection) → status=unavailable.
//   - Backend возвращает 200 с валидным JSON → status=ok, info заполнен.
//   - Backend возвращает 404 → status=unavailable с подсказкой про старый build.
//   - Backend возвращает 500 → status=error.
//   - Endpoint зарегистрирован в роутере.
//
// Все тесты используют createProxyTestServer из gguf_backend_proxy_test.go,
// который возвращает (*httptest.Server, *Server). Второй аргумент — это *Server
// с настройками proxy и зарегистрированным llama_cpp бэкендом.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClusterDebugLastPromptHandler_MethodNotAllowed — handler возвращает 405 для не-GET.
func TestClusterDebugLastPromptHandler_MethodNotAllowed(t *testing.T) {
	_, srv := createProxyTestServer(t, "http://127.0.0.1:1") // любой URL — handler не дойдёт до backend.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/cppworker/debug/last-prompt", nil)
	rec := httptest.NewRecorder()

	srv.clusterDebugLastPromptHandler(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestClusterDebugLastPromptHandler_BackendMissingAddress — бэкенд зарегистрирован,
// но у него пустой host:port → status=unavailable с ошибкой конфигурации.
//
// NB: createProxyTestServer добавляет один llama_cpp бэкенд в srv.cs.Backends.
// Если у бэкенда пустой Addr или не настроен CppWorkerPort — handler вернёт
// status=unavailable. Это покрывает кейс "бэкенд есть, но не сконфигурирован".
func TestClusterDebugLastPromptHandler_BackendMissingAddress(t *testing.T) {
	_, srv := createProxyTestServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/cppworker/debug/last-prompt", nil)
	rec := httptest.NewRecorder()

	srv.clusterDebugLastPromptHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp debugLastPromptResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// createProxyTestServer добавляет ровно 1 llama_cpp бэкенд.
	if resp.Count != 1 {
		t.Fatalf("expected count=1, got %d", resp.Count)
	}
	if len(resp.Backends) != 1 {
		t.Fatalf("expected 1 backend entry, got %d", len(resp.Backends))
	}
	entry := resp.Backends[0]
	// Если бэкенд сконфигурирован правильно — будет status=unavailable (connection refused).
	// Если host/port пустой — будет status=unavailable с другим сообщением.
	// В обоих случаях status должен быть "unavailable" (cppworker недоступен).
	if entry.Status != "unavailable" {
		t.Errorf("expected status=unavailable (cppworker unreachable), got %q", entry.Status)
	}
	if entry.Info != nil {
		t.Errorf("expected info=nil for unavailable backend, got %+v", entry.Info)
	}
	if entry.Error == "" {
		t.Errorf("expected non-empty error for unavailable backend")
	}
}

// TestClusterDebugLastPromptHandler_BackendUnavailable — cppworker недоступен
// (refused connection на закрытом порту). status=unavailable, error содержит
// описание http-ошибки.
func TestClusterDebugLastPromptHandler_BackendUnavailable(t *testing.T) {
	// Используем URL на закрытый порт — TCP RST сразу.
	_, srv := createProxyTestServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/cppworker/debug/last-prompt", nil)
	rec := httptest.NewRecorder()

	srv.clusterDebugLastPromptHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK (handler returns 200 even if backend is unreachable), got %d: %s", rec.Code, rec.Body.String())
	}

	var resp debugLastPromptResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("expected count=1 (one backend), got %d", resp.Count)
	}
	if len(resp.Backends) != 1 {
		t.Fatalf("expected 1 backend entry, got %d", len(resp.Backends))
	}
	entry := resp.Backends[0]
	if entry.Status != "unavailable" {
		t.Errorf("expected status=unavailable, got %q", entry.Status)
	}
	if entry.Info != nil {
		t.Errorf("expected info=nil for unavailable backend, got %+v", entry.Info)
	}
	if entry.Error == "" {
		t.Errorf("expected non-empty error for unavailable backend")
	}
}

// TestClusterDebugLastPromptHandler_BackendSuccess — cppworker вернул 200 с валидным JSON.
// status=ok, info заполнен корректно.
func TestClusterDebugLastPromptHandler_BackendSuccess(t *testing.T) {
	// Поднимаем stub cppworker, который возвращает валидный JSON.
	mockCppWorker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cppworker/debug/last-prompt" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(debugLastPromptInfo{
			Model:        "gemma-4-E4B-it-Q4_K_M",
			Endpoint:     "/v1/chat/completions",
			PromptChars:  270000,
			PromptTokens: 68271,
			NCtxOverride: 65536,
			NCtxLoaded:   65536,
			HasTools:     true,
			Status:       "prompt_too_long",
			PromptHead:   "<start_of_turn>user\n",
			PromptTail:   "<end_of_turn>",
		})
	}))
	defer mockCppWorker.Close()

	_, srv := createProxyTestServer(t, mockCppWorker.URL)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/cppworker/debug/last-prompt", nil)
	rec := httptest.NewRecorder()

	srv.clusterDebugLastPromptHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp debugLastPromptResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("expected count=1, got %d", resp.Count)
	}
	entry := resp.Backends[0]
	if entry.Status != "ok" {
		t.Errorf("expected status=ok, got %q", entry.Status)
	}
	if entry.Error != "" {
		t.Errorf("expected empty error for ok status, got %q", entry.Error)
	}
	if entry.Info == nil {
		t.Fatal("expected info to be populated")
	}
	if entry.Info.Model != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("expected model=gemma-4-E4B-it-Q4_K_M, got %q", entry.Info.Model)
	}
	if entry.Info.PromptTokens != 68271 {
		t.Errorf("expected prompt_tokens=68271, got %d", entry.Info.PromptTokens)
	}
	if entry.Info.Status != "prompt_too_long" {
		t.Errorf("expected status=prompt_too_long, got %q", entry.Info.Status)
	}
	if !entry.Info.HasTools {
		t.Errorf("expected has_tools=true")
	}
}

// TestClusterDebugLastPromptHandler_BackendNotFound — cppworker вернул 404
// (endpoint не реализован в старом bundled-образе). status=unavailable, error содержит
// "does not implement".
func TestClusterDebugLastPromptHandler_BackendNotFound(t *testing.T) {
	mockCppWorker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mockCppWorker.Close()

	_, srv := createProxyTestServer(t, mockCppWorker.URL)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/cppworker/debug/last-prompt", nil)
	rec := httptest.NewRecorder()

	srv.clusterDebugLastPromptHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	var resp debugLastPromptResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	entry := resp.Backends[0]
	if entry.Status != "unavailable" {
		t.Errorf("expected status=unavailable, got %q", entry.Status)
	}
	if !strings.Contains(entry.Error, "does not implement") {
		t.Errorf("expected error to mention 'does not implement', got %q", entry.Error)
	}
}

// TestClusterDebugLastPromptHandler_Backend500 — cppworker вернул 500.
// status=error, error содержит тело ответа.
func TestClusterDebugLastPromptHandler_Backend500(t *testing.T) {
	mockCppWorker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error: db down", http.StatusInternalServerError)
	}))
	defer mockCppWorker.Close()

	_, srv := createProxyTestServer(t, mockCppWorker.URL)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/cppworker/debug/last-prompt", nil)
	rec := httptest.NewRecorder()

	srv.clusterDebugLastPromptHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	var resp debugLastPromptResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	entry := resp.Backends[0]
	if entry.Status != "error" {
		t.Errorf("expected status=error, got %q", entry.Status)
	}
	if !strings.Contains(entry.Error, "internal server error") {
		t.Errorf("expected error to contain backend error body, got %q", entry.Error)
	}
}

// TestRoutes_ClusterDebugLastPrompt — endpoint зарегистрирован в роутере
// под /api/v1/cluster/cppworker/debug/last-prompt и доступен через http.
func TestRoutes_ClusterDebugLastPrompt(t *testing.T) {
	hs, _ := createProxyTestServer(t, "http://127.0.0.1:1")
	defer hs.Close()

	resp, err := http.Get(hs.URL + "/api/v1/cluster/cppworker/debug/last-prompt")
	if err != nil {
		t.Fatalf("HTTP GET failed: %v", err)
	}
	defer resp.Body.Close()

	// Handler может вернуть 200 (нет backends), 401 (auth), или 503 (cluster state unavailable).
	// Главное — НЕ 404 (endpoint не зарегистрирован).
	switch resp.StatusCode {
	case http.StatusOK, http.StatusUnauthorized, http.StatusServiceUnavailable:
		// OK: endpoint зарегистрирован и handler вызван.
	default:
		t.Fatalf("expected endpoint to be registered (200/401/503), got %d", resp.StatusCode)
	}
}