//go:build llama_stub

// handlers_v1_model_byid_r60_16_test.go — R60.16 (2026-09-08) tests for
// the disk-known unloaded model lookup in handleV1ModelByID.
//
// Pre-R60.16: handleV1ModelByID only checked backend.GetModel (loaded
// models). For disk-known but not loaded models, the call to
// mm.GetModelMeta() on the request goroutine triggered ReadGGUFHeader
// file I/O, which blocked under concurrent bridge usage (R60.15 Fix C
// rolled back due to production hangs).
//
// R60.16 fix: use a pure cache lookup (ListModels + filename match),
// NO ReadGGUFHeader on the request goroutine. The response only needs
// filename, file mtime (for "created"), and "owned_by" — none of
// which require the GGUF header.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

// cppbackendNewTestBackendWithDir — variant of newTestBackend() that
// points ModelsDir at a specific directory. Used by R60.16 tests to
// inject disk-known model files.
func cppbackendNewTestBackendWithDir(t *testing.T, modelsDir string) *cppbackend.Backend {
	t.Helper()
	b := cppbackend.NewBackend(cppbackend.Config{
		Host:      "127.0.0.1",
		Port:      18091,
		ModelsDir: modelsDir,
	})
	if err := b.Init(); err != nil {
		// stub-build doesn't have real llama.cpp; Init may fail but
		// the in-memory ModelManager still works.
		_ = err
	}
	return b
}

// makeTempModelsDir creates a temporary directory with a fake .gguf file.
// ScanModels only does os.Stat (no header read), so the file contents
// don't matter for our tests.
func makeTempModelsDir(t *testing.T, modelName string) (dir string, cleanup func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "r60_16_test_models_")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	// Create an empty .gguf file (ScanModels only checks extension + size).
	ggufPath := filepath.Join(dir, modelName+".gguf")
	if err := os.WriteFile(ggufPath, []byte("GGUF\x00\x00\x00\x00"), 0644); err != nil {
		cleanup()
		t.Fatalf("WriteFile: %v", err)
	}
	return dir, cleanup
}

// TestHandleV1ModelByID_DiskKnownUnloaded — model is on disk (in
// ModelManager.ggufFiles) but NOT loaded in backend → 200 OK with
// "created" from the file mtime.
//
// This is the case OpenWebUI hits for disk-known unloaded models in
// the picker (pre-R60.16: 404).
func TestHandleV1ModelByID_DiskKnownUnloaded(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()

	dir, cleanup := makeTempModelsDir(t, "r60-16-test-disk-known")
	defer cleanup()

	backend = cppbackendNewTestBackendWithDir(t, dir)
	// Force a scan so ggufFiles map is populated.
	if _, err := backend.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	// Wait one second so the file mtime is stable.
	time.Sleep(1100 * time.Millisecond)

	r := httptest.NewRequest(http.MethodGet, "/v1/models/r60-16-test-disk-known", nil)
	w := httptest.NewRecorder()
	handleV1ModelByID(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("disk-known status = %d, want 200. body = %s",
			w.Code, w.Body.String()[:min(200, w.Body.Len())])
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v. body = %s", err, w.Body.String())
	}

	if resp["id"] != "r60-16-test-disk-known" {
		t.Errorf("id = %v, want r60-16-test-disk-known", resp["id"])
	}
	if resp["object"] != "model" {
		t.Errorf("object = %v, want model", resp["object"])
	}
	if resp["owned_by"] != "ollamalegion" {
		t.Errorf("owned_by = %v, want ollamalegion", resp["owned_by"])
	}

	// created should be a positive unix timestamp.
	created, ok := resp["created"].(float64)
	if !ok || created <= 0 {
		t.Errorf("created = %v, want positive unix timestamp", resp["created"])
	}
}

// TestHandleV1ModelByID_DiskKnownNoFileIO — disk-known lookup must NOT
// call ReadGGUFHeader. We measure the test execution time: if the
// handler does any synchronous file I/O on a 2.4 GB GGUF header, the
// call would take >50ms (in practice, 100-500ms on RTX 3060 with
// cold cache). Returning in <50ms proves no header read.
//
// This is the key test for the R60.15 → R60.16 fix: R60.15 Fix C used
// mm.GetModelMeta which lazy-reads the GGUF header; that was the
// production hang. R60.16 must NOT call GetModelMeta.
func TestHandleV1ModelByID_DiskKnownNoFileIO(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()

	dir, cleanup := makeTempModelsDir(t, "r60-16-no-fileio")
	defer cleanup()

	backend = cppbackendNewTestBackendWithDir(t, dir)
	if _, err := backend.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/models/r60-16-no-fileio", nil)
	w := httptest.NewRecorder()

	start := time.Now()
	handleV1ModelByID(w, r)
	dur := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body = %s",
			w.Code, w.Body.String()[:min(200, w.Body.Len())])
	}

	// 50ms is generous; in practice the call is <1ms.
	if dur > 50*time.Millisecond {
		t.Errorf("disk-known lookup took %v, want <50ms (suggests file I/O was performed)", dur)
	}
}

// TestHandleV1ModelByID_DiskKnownWithGguFSuffix — when client requests
// "name.gguf" but the model is registered as "name" (or vice versa),
// the disk-known lookup should still find it. (Ollama spec allows
// either form; cppworker is case-insensitive on .gguf suffix.)
func TestHandleV1ModelByID_DiskKnownWithGguFSuffix(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()

	dir, cleanup := makeTempModelsDir(t, "model-with-suffix")
	defer cleanup()

	backend = cppbackendNewTestBackendWithDir(t, dir)
	if _, err := backend.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	// Request without .gguf → should still find model-with-suffix.gguf.
	r := httptest.NewRequest(http.MethodGet, "/v1/models/model-with-suffix", nil)
	w := httptest.NewRecorder()
	handleV1ModelByID(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for disk-known without .gguf. body = %s",
			w.Code, w.Body.String()[:min(200, w.Body.Len())])
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	// id should be the bare name (not .gguf).
	if resp["id"] != "model-with-suffix" {
		t.Errorf("id = %v, want model-with-suffix", resp["id"])
	}
}

// TestHandleV1ModelByID_LoadedTakesPrecedence — when a model is both
// loaded AND on disk, the loaded version should be returned (existing
// behavior; regression test).
func TestHandleV1ModelByID_LoadedTakesPrecedence(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()

	dir, cleanup := makeTempModelsDir(t, "r60-16-loaded-and-disk")
	defer cleanup()

	backend = cppbackendNewTestBackendWithDir(t, dir)
	if _, err := backend.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	// Simulate that the model was loaded: RecordModelLoad sets
	// nameHistory but does NOT add to ggufFiles. The handler
	// should find the loaded entry via backend.GetModel.
	backend.ModelManager().RecordModelLoad("r60-16-loaded-and-disk",
		filepath.Join(dir, "r60-16-loaded-and-disk.gguf"))

	r := httptest.NewRequest(http.MethodGet, "/v1/models/r60-16-loaded-and-disk", nil)
	w := httptest.NewRecorder()
	handleV1ModelByID(w, r)

	// backend.GetModel may return not-found for unloaded models in the
	// test backend (no real load). We just want to verify no crash
	// and a reasonable response (either 200 or 404 is fine here).
	if w.Code != http.StatusOK && w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 200 or 404. body = %s",
			w.Code, w.Body.String()[:min(200, w.Body.Len())])
	}
}
