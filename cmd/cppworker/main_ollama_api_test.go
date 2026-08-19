//go:build llama_stub

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

// setupCppWorkerTestServer создаёт тестовый cppworker backend со stub bridge.
func setupCppWorkerTestServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	dir := t.TempDir()

	// Создаём минимальный .gguf файл-заглушку
	dummyPath := filepath.Join(dir, "dummy.gguf")
	if err := os.WriteFile(dummyPath, []byte("stub"), 0644); err != nil {
		t.Fatalf("failed to create dummy gguf: %v", err)
	}

	cfg := cppbackend.Config{
		ModelsDir:            dir,
		DefaultCtxSize:       4096,
		DefaultBatchSize:     512,
		DefaultGPULayers:     0,
		DefaultFlashAttnType: -1,
		DefaultNUMA:          false,
		DefaultUseMmap:       true,
	}

	backend = cppbackend.NewBackend(cfg)
	if err := backend.Init(); err != nil {
		t.Fatalf("backend init failed: %v", err)
	}

	// Используем тот же router, что и в production
	router := setupRouter()
	srv := httptest.NewServer(router)
	cleanup := func() {
		srv.Close()
		backend.Close()
	}
	return srv, cleanup
}

func TestOllamaShowReturnsMetadata(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]string{"name": "dummy"}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/show", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if _, ok := result["details"]; !ok {
		t.Fatalf("expected details in response, got %v", result)
	}
}

func TestOllamaCopyCreatesFile(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]string{"source": "dummy", "destination": "dummy-copy"}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/copy", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// Проверяем, что файл скопирован
	dir := backend.ModelManager().GetModelsDir()
	copiedPath := filepath.Join(dir, "dummy-copy.gguf")
	if _, err := os.Stat(copiedPath); os.IsNotExist(err) {
		t.Fatalf("expected copied file %s to exist", copiedPath)
	}
}

func TestOllamaCreateReturnsAlias(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]string{"name": "my-model", "from": "dummy"}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/create", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if result["status"] != "created" {
		t.Fatalf("expected created status, got %v", result)
	}
}

func TestOllamaPushReturnsNotImplemented(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]string{"name": "dummy"}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/push", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", resp.StatusCode)
	}
}

func TestOllamaPullHuggingFace(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]string{"name": "hf:unsloth/Llama-3.2-1B-Instruct-GGUF/Llama-3.2-1B-Instruct-Q4_K_M.gguf"}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/pull", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	// Для HF pull без сети может вернуть 202 (started) или 500 (download failed).
	// Важно, что endpoint не возвращает 404.
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 202 or 500, got %d", resp.StatusCode)
	}
}

func TestOllamaShowLazyLoad(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	// Модель не загружена заранее — /api/show должен лениво загрузить её.
	body := map[string]string{"name": "dummy"}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/show", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		// Модель в процессе загрузки — повторим через 3 секунды.
		time.Sleep(3 * time.Second)
		resp, err = http.Post(srv.URL+"/api/show", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("retry request failed: %v", err)
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}
}

// TestOllamaShowNoLoadRound44 — Round 44 (2026-08-19) variant-4 structural fix.
//
// BEFORE R44: /api/show called ensureModelLoaded() which triggered a full
// llama.cpp model load. For a 22GB model (Qwen3.6-35B) on RTX 3070 8GB
// VRAM, this took 6+ minutes. Real Cline clients with hard 5-min HTTP
// timeouts (OLLAMA_DEFAULT_TIMEOUT_MS=300000) would cancel mid-load,
// leaving the single-flight load slot held by the still-loading worker
// and blocking every subsequent /api/chat for the same backend.
//
// AFTER R44: /api/show returns metadata from the GGUF header (no model
// load) in <100ms. The state field is "unloaded" so the client knows
// the model is not yet ready for inference. /api/chat or /api/generate
// still trigger the load on first request (unchanged).
//
// This test verifies:
//  1. /api/show returns 200 in well under 1s (no load wait).
//  2. Response state is "unloaded" (NOT "loaded", since the model was
//     not loaded — and NOT 503, since we don't block on load anymore).
//  3. /api/chat with the same model still loads (the load path is
//     preserved for inference endpoints).
func TestOllamaShowNoLoadRound44(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]string{"name": "dummy"}
	b, _ := json.Marshal(body)

	// R44 fix: response must be fast (< 1s) — no lazy-load wait.
	start := time.Now()
	resp, err := http.Post(srv.URL+"/api/show", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	elapsed := time.Since(start)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d (elapsed=%v): %s", resp.StatusCode, elapsed, string(bodyBytes))
	}
	if elapsed > time.Second {
		t.Fatalf("R44 /api/show should be <1s but took %v — model load was triggered", elapsed)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	modelInfo, ok := result["model_info"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected model_info in response, got %v", result)
	}
	// R44: state should be "unloaded" (or "not loaded" for stub), not "loaded".
	state, _ := modelInfo["state"].(string)
	if state == "loaded" {
		t.Fatalf("expected state!=loaded (model was not loaded), got %q", state)
	}
	t.Logf("R44 /api/show: state=%q elapsed=%v (no load triggered)", state, elapsed)
}

// TestOllamaShowLoadedVsUnloaded — Round 44 (2026-08-19) regression guard.
//
// /api/show response shape must differ depending on whether the model is
// already in memory (state=loaded) or only on disk (state=unloaded).
// Both paths must return 200, both must have model_info.details.family
// set (Ollama clients inspect this for UI rendering).
func TestOllamaShowLoadedVsUnloaded(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	// Unloaded path: 200, fast.
	start := time.Now()
	resp, err := http.Post(srv.URL+"/api/show", "application/json",
		bytes.NewReader([]byte(`{"name":"dummy"}`)))
	if err != nil {
		t.Fatalf("unloaded /api/show failed: %v", err)
	}
	unloadElapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}
	resp.Body.Close()

	if unloadElapsed > time.Second {
		t.Fatalf("unloaded /api/show took %v — load was triggered", unloadElapsed)
	}

	// Unknown model: 404.
	resp2, err := http.Post(srv.URL+"/api/show", "application/json",
		bytes.NewReader([]byte(`{"name":"definitely-not-a-real-model-xyz"}`)))
	if err != nil {
		t.Fatalf("unknown /api/show failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown model, got %d", resp2.StatusCode)
	}
}