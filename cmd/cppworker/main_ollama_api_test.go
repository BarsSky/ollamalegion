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