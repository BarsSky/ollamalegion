//go:build llama_stub

// Round 51.6 (2026-08-20): tests for handleUnloadModel safety check.
// R51.6 changed behavior: refuse unload if model has active_queries > 0.
// Before R51.6, unload could free C-bridge handle while generate was in
// flight, causing use-after-free (crash) or EOF mid-stream (silent data loss).
//
// These tests verify:
//  1. Unload with no active queries → 200
//  2. Unload with active queries (no force) → 409 with details
//  3. Unload with ?force=true → 200 + active generations cancelled
//  4. Missing name → 400
//  5. Unloaded model → 404
//  6. ListModels response includes active_queries per model

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

const unloadTestModel = "gemma-4-E4B-it-Q4_K_M"

func setupUnloadTestBackend(t *testing.T) {
	t.Helper()
	cfg := cppbackend.Config{
		ModelsDir:       t.TempDir(),
		DefaultCtxSize:  4096,
		DefaultBatchSize: 512,
	}
	backend = cppbackend.NewBackend(cfg)
}

// TestHandleUnloadModel_NoActiveQueries_200 — happy path: unload with no in-flight.
func TestHandleUnloadModel_NoActiveQueries_200(t *testing.T) {
	setupUnloadTestBackend(t)
	injectLoadedModel(t, unloadTestModel, nil)

	if n := backend.InFlight().Get(unloadTestModel); n != 0 {
		t.Fatalf("expected 0 inflight, got %d", n)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/models/unload?name="+unloadTestModel, nil)
	handleUnloadModel(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "unloaded" {
		t.Errorf("expected status=unloaded, got %q (body=%s)", resp["status"], w.Body.String())
	}
}

// TestHandleUnloadModel_WithActiveQueries_409 — refuses unload with active inflight.
func TestHandleUnloadModel_WithActiveQueries_409(t *testing.T) {
	setupUnloadTestBackend(t)
	injectLoadedModel(t, unloadTestModel, nil)

	// Simulate active generation for this model
	cancel := context.CancelFunc(func() {})
	ok := backend.ActiveGenerations().Add("req-1", "user-42", unloadTestModel, "backend-test", cancel)
	if !ok {
		t.Fatal("failed to register active generation")
	}
	backend.InFlight().Inc(unloadTestModel)

	// Verify model still loaded
	if _, err := backend.GetModel(unloadTestModel); err != nil {
		t.Fatal("test setup: model should be loaded")
	}

	// Make unload request — no force
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/models/unload?name="+unloadTestModel, nil)
	handleUnloadModel(w, r)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict, got %d body=%s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if errStr, _ := resp["error"].(string); !strings.Contains(errStr, "busy") {
		t.Errorf("expected error to mention 'busy', got %q", errStr)
	}
	if n, _ := resp["active_queries"].(float64); n != 1 {
		t.Errorf("expected active_queries=1, got %v", resp["active_queries"])
	}
	if uids, _ := resp["user_ids"].([]interface{}); len(uids) != 1 || uids[0] != "user-42" {
		t.Errorf("expected user_ids=[user-42], got %v", resp["user_ids"])
	}
	// Model should NOT be unloaded
	if _, err := backend.GetModel(unloadTestModel); err != nil {
		t.Error("model was unloaded despite 409 (should remain loaded)")
	}
}

// TestHandleUnloadModel_ForceTrue_CancelsActive — ?force=true overrides, cancels active gens.
func TestHandleUnloadModel_ForceTrue_CancelsActive(t *testing.T) {
	setupUnloadTestBackend(t)
	injectLoadedModel(t, unloadTestModel, nil)

	cancelled := false
	cancel := context.CancelFunc(func() { cancelled = true })
	backend.ActiveGenerations().Add("req-2", "user-7", unloadTestModel, "backend-test", cancel)
	backend.InFlight().Inc(unloadTestModel)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/models/unload?name="+unloadTestModel+"&force=true", nil)
	handleUnloadModel(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with force=true, got %d body=%s", w.Code, w.Body.String())
	}
	if !cancelled {
		t.Error("force=true should have cancelled the active generation")
	}
	if _, err := backend.GetModel(unloadTestModel); err == nil {
		t.Error("model should be unloaded after force=true")
	}
}

// TestHandleUnloadModel_MissingName_400 — ?name= required.
func TestHandleUnloadModel_MissingName_400(t *testing.T) {
	setupUnloadTestBackend(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/models/unload", nil)
	handleUnloadModel(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandleUnloadModel_NotLoaded_404 — unload of non-existent model.
func TestHandleUnloadModel_NotLoaded_404(t *testing.T) {
	setupUnloadTestBackend(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/models/unload?name=does-not-exist", nil)
	handleUnloadModel(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandleListModels_IncludesActiveQueries — per-model active_queries в /api/models.
func TestHandleListModels_IncludesActiveQueries(t *testing.T) {
	setupUnloadTestBackend(t)
	injectLoadedModel(t, unloadTestModel, nil)

	// Register 2 active generations
	backend.ActiveGenerations().Add("req-a", "user-1", unloadTestModel, "backend-test", func() {})
	backend.ActiveGenerations().Add("req-b", "user-2", unloadTestModel, "backend-test", func() {})
	backend.InFlight().Inc(unloadTestModel)
	backend.InFlight().Inc(unloadTestModel)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/models", nil)
	handleListModels(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Models []map[string]interface{} `json:"models"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	if len(resp.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(resp.Models))
	}
	got, ok := resp.Models[0]["active_queries"].(float64)
	if !ok {
		t.Fatalf("active_queries field missing or not number: %v", resp.Models[0]["active_queries"])
	}
	if got != 2 {
		t.Errorf("expected active_queries=2, got %v", got)
	}
}

// injectLoadedModel — fake a loaded model для unload тестов. Помечает model как
// loaded в backend.models map без реальной C-bridge загрузки.
func injectLoadedModel(t *testing.T, name string, _ interface{}) {
	t.Helper()
	if err := backend.InjectLoadedModelForTest(name); err != nil {
		t.Fatalf("injectLoadedModel: %v", err)
	}
}
