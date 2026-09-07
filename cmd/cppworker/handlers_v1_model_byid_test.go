//go:build llama_stub

// handlers_v1_model_byid_test.go — R60.8 (2026-09-07) tests for
// OpenAI-compatible GET /v1/models/{model_id} endpoint.
//
// Endpoint: GET /v1/models/{model_id} → OpenAI-format JSON model object.
// Used by OpenWebUI + OpenAI python clients to display model card.
// Pre-R60.8: 404 page not found (endpoint not implemented).
//
// Tests use newTestBackend() to avoid nil-deref on package-level `backend`
// global. We verify HTTP semantics (200/404/400) and JSON body shape.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleV1ModelByID_EmptyID — /v1/models/ (empty ID after slash) → 400.
// Doesn't need backend (fails before lookup).
func TestHandleV1ModelByID_EmptyID(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/models/", nil)
	w := httptest.NewRecorder()
	handleV1ModelByID(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty ID status = %d, want 400. body = %s",
			w.Code, w.Body.String()[:min(200, w.Body.Len())])
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v. body = %s", err, w.Body.String())
	}
	if resp["error"] == nil {
		t.Errorf("response missing 'error' field: %v", resp)
	}
}

// TestHandleV1ModelByID_NestedPath — /v1/models/foo/bar → 400 (single segment only).
// Doesn't need backend (fails before lookup).
func TestHandleV1ModelByID_NestedPath(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/models/foo/bar", nil)
	w := httptest.NewRecorder()
	handleV1ModelByID(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("nested path status = %d, want 400. body = %s",
			w.Code, w.Body.String()[:min(200, w.Body.Len())])
	}
}

// TestHandleV1ModelByID_NotFound — non-existent model returns 404 with
// JSON error body. Uses newTestBackend() so the singleton `backend` is
// non-nil during the lookup.
func TestHandleV1ModelByID_NotFound(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	r := httptest.NewRequest(http.MethodGet, "/v1/models/nonexistent-r60-8-test-model", nil)
	w := httptest.NewRecorder()
	handleV1ModelByID(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("not-found status = %d, want 404. body = %s",
			w.Code, w.Body.String()[:min(200, w.Body.Len())])
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v. body = %s", err, w.Body.String())
	}
	if resp["error"] == nil {
		t.Errorf("response missing 'error' field: %v", resp)
	}
}

// TestHandleV1ModelByID_RouteOrdering — Go ServeMux exact match for
// /v1/models (list) takes precedence over subtree match /v1/models/.
// R60.8: registering both in router.go must NOT cause /v1/models to
// be served by handleV1ModelByID.
func TestHandleV1ModelByID_RouteOrdering(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", handleV1Models)
	mux.HandleFunc("/v1/models/", handleV1ModelByID)

	// /v1/models → handleV1Models (list) returns 200 with {"data":[]}
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("/v1/models status = %d, want 200", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("/v1/models body not JSON: %v", err)
	}
	if _, hasData := resp["data"]; !hasData {
		t.Errorf("/v1/models response missing 'data' field: %v", resp)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
