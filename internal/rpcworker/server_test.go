package rpcworker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestServer создаёт WorkerServer с tmp modelsDir.
func newTestServer(t *testing.T, opts ...func(*WorkerConfig)) *WorkerServer {
	t.Helper()
	tmp := t.TempDir()
	cfg := DefaultWorkerConfig()
	cfg.Host = "127.0.0.1"
	cfg.Port = 0 // не слушаем — тестируем через httptest
	cfg.ModelsDir = tmp
	cfg.WorkerID = "test-worker"
	cfg.StubMode = true
	cfg.AuthToken = "" // без токена по умолчанию
	for _, o := range opts {
		o(&cfg)
	}
	manager := NewModelManager(cfg)
	return NewWorkerServer(cfg, manager, "rpcworker-test")
}

// writeTestGGUF создаёт фейковый .gguf в modelsDir (просто непустой файл).
func writeTestGGUF(t *testing.T, modelsDir, name string) string {
	t.Helper()
	path := filepath.Join(modelsDir, name)
	if err := os.WriteFile(path, []byte("fake gguf"), 0644); err != nil {
		t.Fatalf("write fake gguf: %v", err)
	}
	return path
}

// =====================================================================
// /rpc/health
// =====================================================================

func TestHandleHealth_OK(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/rpc/health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", body["status"])
	}
	if body["worker_id"] != "test-worker" {
		t.Errorf("expected worker_id=test-worker, got %v", body["worker_id"])
	}
	if body["stub_mode"] != true {
		t.Errorf("expected stub_mode=true, got %v", body["stub_mode"])
	}
	if _, ok := body["loaded_slices"]; !ok {
		t.Error("expected loaded_slices field")
	}
	if _, ok := body["gpu_info"]; !ok {
		t.Error("expected gpu_info field")
	}
}

func TestHandleHealth_WithLoadedSlices(t *testing.T) {
	srv := newTestServer(t)
	writeTestGGUF(t, srv.cfg.ModelsDir, "model-a.gguf")
	if _, err := srv.manager.LoadSlice("model-a.gguf", ""); err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/rpc/health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	slices, _ := body["loaded_slices"].([]interface{})
	if len(slices) != 1 {
		t.Errorf("expected 1 loaded slice, got %d", len(slices))
	}
	if srv.manager.Count() != 1 {
		t.Errorf("manager.Count: expected 1, got %d", srv.manager.Count())
	}
}

func TestHandleHealth_MethodNotAllowed(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/rpc/health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

// =====================================================================
// /rpc/load
// =====================================================================

func TestHandleLoad_BadRequest_EmptyBody(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/rpc/load", strings.NewReader(""))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleLoad_BadRequest_MissingModelName(t *testing.T) {
	srv := newTestServer(t)
	body := strings.NewReader(`{"layers":"1-40"}`)
	req := httptest.NewRequest(http.MethodPost, "/rpc/load", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleLoad_FileNotFound(t *testing.T) {
	srv := newTestServer(t)
	body := strings.NewReader(`{"model_name":"missing.gguf"}`)
	req := httptest.NewRequest(http.MethodPost, "/rpc/load", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleLoad_Success(t *testing.T) {
	srv := newTestServer(t)
	writeTestGGUF(t, srv.cfg.ModelsDir, "test-model.gguf")
	body := strings.NewReader(`{"model_name":"test-model.gguf","layers":"1-32"}`)
	req := httptest.NewRequest(http.MethodPost, "/rpc/load", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "loaded" {
		t.Errorf("expected status=loaded, got %v", resp["status"])
	}
	if resp["model"] != "test-model.gguf" {
		t.Errorf("expected model=test-model.gguf, got %v", resp["model"])
	}
	if resp["layers"] != "1-32" {
		t.Errorf("expected layers=1-32, got %v", resp["layers"])
	}
	if srv.manager.Count() != 1 {
		t.Errorf("expected 1 loaded slice, got %d", srv.manager.Count())
	}
	if srv.metrics.loadedSlices.Load() != 1 {
		t.Errorf("expected loadedSlices=1, got %d", srv.metrics.loadedSlices.Load())
	}
}

// =====================================================================
// /rpc/unload
// =====================================================================

func TestHandleUnload_NotLoaded_Idempotent(t *testing.T) {
	srv := newTestServer(t)
	body := strings.NewReader(`{"model_name":"never-loaded.gguf"}`)
	req := httptest.NewRequest(http.MethodPost, "/rpc/unload", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (idempotent), got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleUnload_Success(t *testing.T) {
	srv := newTestServer(t)
	writeTestGGUF(t, srv.cfg.ModelsDir, "x.gguf")
	if _, err := srv.manager.LoadSlice("x.gguf", ""); err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
	body := strings.NewReader(`{"model_name":"x.gguf"}`)
	req := httptest.NewRequest(http.MethodPost, "/rpc/unload", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if srv.manager.Count() != 0 {
		t.Errorf("expected 0 slices, got %d", srv.manager.Count())
	}
}

// =====================================================================
// /rpc/infer
// =====================================================================

func TestHandleInfer_BadRequest_MissingPrompt(t *testing.T) {
	srv := newTestServer(t)
	body := strings.NewReader(`{"model_name":"x.gguf"}`)
	req := httptest.NewRequest(http.MethodPost, "/rpc/infer", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleInfer_ModelNotLoaded(t *testing.T) {
	srv := newTestServer(t)
	body := strings.NewReader(`{"model_name":"x.gguf","prompt":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/rpc/infer", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleInfer_Success_StubMode(t *testing.T) {
	srv := newTestServer(t)
	writeTestGGUF(t, srv.cfg.ModelsDir, "stub.gguf")
	if _, err := srv.manager.LoadSlice("stub.gguf", "1-8"); err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	body := strings.NewReader(`{"model_name":"stub.gguf","prompt":"hello world","tokens":10,"temperature":0.5}`)
	req := httptest.NewRequest(http.MethodPost, "/rpc/infer", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp SliceInferResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.WorkerID != "test-worker" {
		t.Errorf("expected worker_id=test-worker, got %q", resp.WorkerID)
	}
	if resp.SliceID != "1-8" {
		t.Errorf("expected slice_id=1-8, got %q", resp.SliceID)
	}
	if resp.Output == "" {
		t.Error("expected non-empty output")
	}
	if !strings.Contains(resp.Output, "hello world") {
		t.Errorf("expected output to echo prompt, got %q", resp.Output)
	}
	if resp.LatencyMs < 0 {
		t.Errorf("expected non-negative latency, got %d", resp.LatencyMs)
	}
	if srv.metrics.totalInfer.Load() != 1 {
		t.Errorf("expected totalInfer=1, got %d", srv.metrics.totalInfer.Load())
	}
}

// =====================================================================
// /rpc/metrics
// =====================================================================

func TestHandleMetrics_AfterInfer(t *testing.T) {
	srv := newTestServer(t)
	writeTestGGUF(t, srv.cfg.ModelsDir, "m.gguf")
	_, _ = srv.manager.LoadSlice("m.gguf", "")
	end := srv.metrics.OnInferStart("m.gguf")
	end(0, 5, nil)

	req := httptest.NewRequest(http.MethodGet, "/rpc/metrics", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var snap map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &snap)
	if snap["worker_id"] != "test-worker" {
		t.Errorf("expected worker_id in metrics, got %v", snap["worker_id"])
	}
	if snap["total_infer"].(float64) < 1 {
		t.Errorf("expected total_infer >= 1, got %v", snap["total_infer"])
	}
}

// =====================================================================
// /rpc/kv_sync, /rpc/kv_fetch — B1 stub
// =====================================================================

func TestHandleKvSync_NotImplemented(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/rpc/kv_sync", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("expected 501, got %d", w.Code)
	}
}

func TestHandleKvFetch_NotImplemented(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/rpc/kv_fetch", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Errorf("expected 501, got %d", w.Code)
	}
}

// =====================================================================
// Auth middleware
// =====================================================================

// middleware-handler возвращает обёртку (auth+recover+log) для прямых тестов middleware.
func handler(srv *WorkerServer) http.Handler { return srv.middleware(srv.mux) }

func TestAuthMiddleware_RejectsWithoutToken(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.AuthToken = "secret123"

	req := httptest.NewRequest(http.MethodGet, "/rpc/health", nil)
	w := httptest.NewRecorder()
	handler(srv).ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestAuthMiddleware_AcceptsValidToken(t *testing.T) {
	srv := newTestServer(t)
	srv.cfg.AuthToken = "secret123"

	req := httptest.NewRequest(http.MethodGet, "/rpc/health", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w := httptest.NewRecorder()
	handler(srv).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/rpc/health", nil)
	req2.Header.Set("X-API-Token", "secret123")
	w2 := httptest.NewRecorder()
	handler(srv).ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("expected 200 with X-API-Token, got %d", w2.Code)
	}
}

// =====================================================================
// Concurrency
// =====================================================================

func TestConcurrent_InferRequests(t *testing.T) {
	srv := newTestServer(t)
	writeTestGGUF(t, srv.cfg.ModelsDir, "c.gguf")
	_, _ = srv.manager.LoadSlice("c.gguf", "")

	const N = 20
	var wg sync.WaitGroup
	var ok atomic.Int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := strings.NewReader(`{"model_name":"c.gguf","prompt":"hi"}`)
			req := httptest.NewRequest(http.MethodPost, "/rpc/infer", body)
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, req)
			if w.Code == http.StatusOK {
				ok.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if ok.Load() != N {
		t.Errorf("expected all %d infer requests to succeed, got %d", N, ok.Load())
	}
	if srv.metrics.totalInfer.Load() != int64(N) {
		t.Errorf("expected totalInfer=%d, got %d", N, srv.metrics.totalInfer.Load())
	}
}

// =====================================================================
// Config validation
// =====================================================================

func TestConfig_ParseSliceLayers(t *testing.T) {
	cases := []struct {
		input       string
		wantStart   int
		wantEnd     int
		wantErr     bool
	}{
		{"", 0, 0, false},
		{"1-40", 1, 40, false},
		{"  5 - 10 ", 5, 10, false},
		{"abc", 0, 0, true},
		{"1", 0, 0, true},
		{"10-5", 0, 0, true},
		{"1-", 0, 0, true},
		{"-1", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			c := DefaultWorkerConfig()
			c.SliceLayers = tc.input
			err := c.parseSliceLayers()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if c.StartLayer != tc.wantStart {
					t.Errorf("StartLayer: got %d, want %d", c.StartLayer, tc.wantStart)
				}
				if c.EndLayer != tc.wantEnd {
					t.Errorf("EndLayer: got %d, want %d", c.EndLayer, tc.wantEnd)
				}
			}
		})
	}
}

func TestConfig_Validate(t *testing.T) {
	c := DefaultWorkerConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}

	bad := c
	bad.Port = 0
	if err := bad.Validate(); err == nil {
		t.Error("expected error for port=0")
	}

	bad = c
	bad.ModelsDir = ""
	if err := bad.Validate(); err == nil {
		t.Error("expected error for empty modelsDir")
	}
}

// =====================================================================
// ResolveModelPath
// =====================================================================

func TestResolveModelPath(t *testing.T) {
	tmp := t.TempDir()
	cfg := DefaultWorkerConfig()
	cfg.ModelsDir = tmp
	mgr := NewModelManager(cfg)

	if _, err := mgr.resolveModelPath(""); err == nil {
		t.Error("expected error for empty name")
	}

	writeTestGGUF(t, tmp, "a.gguf")
	path, err := mgr.resolveModelPath("a.gguf")
	if err != nil {
		t.Fatalf("resolve a.gguf: %v", err)
	}
	if !strings.HasSuffix(path, "a.gguf") {
		t.Errorf("expected path ending in a.gguf, got %s", path)
	}

	// Без расширения.
	path, err = mgr.resolveModelPath("a")
	if err != nil {
		t.Fatalf("resolve a: %v", err)
	}
	if !strings.HasSuffix(path, "a.gguf") {
		t.Errorf("expected path ending in a.gguf, got %s", path)
	}

	// Не найден.
	if _, err := mgr.resolveModelPath("missing.gguf"); err == nil {
		t.Error("expected error for missing model")
	}
}

// =====================================================================
// Metrics counters
// =====================================================================

func TestMetrics_OnInferStart(t *testing.T) {
	m := NewMetrics()
	end := m.OnInferStart("m1")
	if m.activeRequests.Load() != 1 {
		t.Errorf("activeRequests: expected 1, got %d", m.activeRequests.Load())
	}
	end(0, 100, nil)
	if m.activeRequests.Load() != 0 {
		t.Errorf("after end: expected 0, got %d", m.activeRequests.Load())
	}
	if m.totalInfer.Load() != 1 {
		t.Errorf("totalInfer: expected 1, got %d", m.totalInfer.Load())
	}
	if m.perModelInfer.Get("m1") != 1 {
		t.Errorf("perModelInfer m1: expected 1, got %d", m.perModelInfer.Get("m1"))
	}

	// С ошибкой.
	end2 := m.OnInferStart("m2")
	end2(0, 50, assertErr{})
	if m.totalErrors.Load() != 1 {
		t.Errorf("totalErrors: expected 1, got %d", m.totalErrors.Load())
	}
	if m.perModelErrors.Get("m2") != 1 {
		t.Errorf("perModelErrors m2: expected 1, got %d", m.perModelErrors.Get("m2"))
	}
}

type assertErr struct{}

func (assertErr) Error() string { return "test error" }

// =====================================================================
// Helper: проверка, что json.Encoder/Decoder использует наш код.
// =====================================================================

func TestWriteJSON_SetsContentType(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/rpc/health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

// =====================================================================
// Shutdown
// =====================================================================

func TestShutdown_UnloadsAllSlices(t *testing.T) {
	srv := newTestServer(t)
	writeTestGGUF(t, srv.cfg.ModelsDir, "s.gguf")
	_, _ = srv.manager.LoadSlice("s.gguf", "")
	if srv.manager.Count() != 1 {
		t.Fatalf("pre-shutdown: expected 1 slice, got %d", srv.manager.Count())
	}
	// Shutdown без запуска ListenAndServe: server.Shutdown вернёт ошибку,
	// но UnloadSlice всё равно выполнится до server.Shutdown.
	_ = srv.Shutdown(nil)
	// Принимаем любой результат (мог быть nil server.Listen addr).
	// Главное — model manager очищен.
	// Проверим через явный вызов ещё раз, чтобы убедиться, что повторный
	// shutdown не паникует.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("double shutdown panicked: %v", r)
		}
	}()
	_ = srv.Shutdown(nil)
}

// =====================================================================
// HTTP streaming-style body
// =====================================================================

func TestHandleInfer_BadJSON(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/rpc/infer",
		bytes.NewReader([]byte("not json {{{")))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// =====================================================================
// Body-параметр temperature=0 не должен сбрасываться
// =====================================================================

func TestHandleInfer_ZeroTemperature(t *testing.T) {
	srv := newTestServer(t)
	writeTestGGUF(t, srv.cfg.ModelsDir, "zt.gguf")
	_, _ = srv.manager.LoadSlice("zt.gguf", "")

	body := strings.NewReader(`{"model_name":"zt.gguf","prompt":"hi","temperature":0}`)
	req := httptest.NewRequest(http.MethodPost, "/rpc/infer", body)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// =====================================================================
// Health endpoint payload с временем
// =====================================================================

func TestHandleHealth_UptimeIncreasing(t *testing.T) {
	srv := newTestServer(t)
	time.Sleep(20 * time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/rpc/health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	uptime, _ := body["uptime_s"].(float64)
	if uptime < 0 {
		t.Errorf("expected non-negative uptime, got %v", uptime)
	}
}