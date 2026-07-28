// handlers_llama_cpp_overrides_test.go — тесты для GET/DELETE override endpoint'ов.
//
// Использует mock-реализацию OverridesStore (без файловой системы), чтобы
// тесты были быстрыми и не зависели от tmpdir. Реальный runtimeoverrides.Store
// покрыт тестами в internal/runtimeoverrides.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// mockOverridesStore — простая in-memory реализация для тестов.
type mockOverridesStore struct {
	config *types.LlamaCppConfig
	exists bool
}

func (m *mockOverridesStore) IsEnabled() bool { return true }
func (m *mockOverridesStore) LoadLlamaCpp() (*types.LlamaCppConfig, bool, error) {
	if !m.exists {
		return nil, false, nil
	}
	return m.config, true, nil
}
func (m *mockOverridesStore) SaveLlamaCpp(ll *types.LlamaCppConfig) error {
	m.config = ll
	m.exists = true
	return nil
}
func (m *mockOverridesStore) ClearLlamaCpp() error {
	m.config = nil
	m.exists = false
	return nil
}
func (m *mockOverridesStore) HasLlamaCppOverride() bool { return m.exists }
func (m *mockOverridesStore) ApplyLlamaCppToConfig(t *types.LoadBalancerConfig) error {
	if m.exists && m.config != nil {
		t.LlamaCpp = *m.config
	}
	return nil
}

func testServerWithOverrides(t *testing.T, store OverridesStore, base *types.LlamaCppConfig) *Server {
	t.Helper()
	cfg := &types.LoadBalancerConfig{}
	configDefaults(cfg)
	if base != nil {
		cfg.LlamaCpp = *base
	}
	s := &Server{config: cfg, overridesStore: store}
	if base != nil {
		s.baseLlamaCpp = base
	}
	return s
}

func TestHandleGetLlamaCppOverride_NotExists(t *testing.T) {
	store := &mockOverridesStore{exists: false}
	s := testServerWithOverrides(t, store, &types.LlamaCppConfig{ContextLength: 2048})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/llama-cpp/overrides", nil)
	w := httptest.NewRecorder()
	s.handleGetLlamaCppOverride(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["exists"] != false {
		t.Errorf("expected exists=false, got %v", resp["exists"])
	}
	if resp["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", resp["enabled"])
	}
	if _, hasConfig := resp["config"]; hasConfig {
		t.Errorf("expected no 'config' field when !exists, got %v", resp["config"])
	}
}

func TestHandleGetLlamaCppOverride_Exists(t *testing.T) {
	override := &types.LlamaCppConfig{ContextLength: 131072, KVCacheType: "q8_0"}
	store := &mockOverridesStore{config: override, exists: true}
	s := testServerWithOverrides(t, store, &types.LlamaCppConfig{ContextLength: 2048})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/llama-cpp/overrides", nil)
	w := httptest.NewRecorder()
	s.handleGetLlamaCppOverride(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["exists"] != true {
		t.Errorf("expected exists=true")
	}
	cfgResp, ok := resp["config"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected config in response, got %v", resp)
	}
	if cfgResp["contextLength"].(float64) != 131072 {
		t.Errorf("contextLength: got %v, want 131072", cfgResp["contextLength"])
	}
	if cfgResp["kvCacheType"] != "q8_0" {
		t.Errorf("kvCacheType: got %v, want q8_0", cfgResp["kvCacheType"])
	}
}

func TestHandleGetLlamaCppOverride_MethodNotAllowed(t *testing.T) {
	store := &mockOverridesStore{exists: false}
	s := testServerWithOverrides(t, store, &types.LlamaCppConfig{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/llama-cpp/overrides", nil)
	w := httptest.NewRecorder()
	s.handleGetLlamaCppOverride(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST, got %d", w.Code)
	}
}

func TestHandleGetLlamaCppOverride_StoreDisabled(t *testing.T) {
	s := &Server{config: &types.LoadBalancerConfig{}} // overridesStore = nil
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/llama-cpp/overrides", nil)
	w := httptest.NewRecorder()
	s.handleGetLlamaCppOverride(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// failingStore — store, который возвращает ошибку при Load (для теста 500).
type failingStore struct{}

func (f *failingStore) IsEnabled() bool { return true }
func (f *failingStore) LoadLlamaCpp() (*types.LlamaCppConfig, bool, error) {
	return nil, false, errors.New("disk i/o error")
}
func (f *failingStore) SaveLlamaCpp(ll *types.LlamaCppConfig) error { return nil }
func (f *failingStore) ClearLlamaCpp() error                          { return nil }
func (f *failingStore) HasLlamaCppOverride() bool                    { return false }
func (f *failingStore) ApplyLlamaCppToConfig(*types.LoadBalancerConfig) error {
	return nil
}

func TestHandleGetLlamaCppOverride_LoadError(t *testing.T) {
	s := testServerWithOverrides(t, &failingStore{}, &types.LlamaCppConfig{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cluster/llama-cpp/overrides", nil)
	w := httptest.NewRecorder()
	s.handleGetLlamaCppOverride(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestHandleDeleteLlamaCppOverride_Success(t *testing.T) {
	store := &mockOverridesStore{exists: true, config: &types.LlamaCppConfig{ContextLength: 131072}}
	base := &types.LlamaCppConfig{ContextLength: 2048}
	s := testServerWithOverrides(t, store, base)
	// Имитируем что override уже применён к in-memory config.
	s.config.LlamaCpp = types.LlamaCppConfig{ContextLength: 131072}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/cluster/llama-cpp/overrides", nil)
	w := httptest.NewRecorder()
	s.handleDeleteLlamaCppOverride(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp["status"] != "reset" {
		t.Errorf("expected status=reset, got %v", resp["status"])
	}
	if resp["existed_before"] != true {
		t.Errorf("expected existed_before=true, got %v", resp["existed_before"])
	}
	// Store очищен.
	if store.HasLlamaCppOverride() {
		t.Error("store should be cleared after DELETE")
	}
	// In-memory state восстановлен к base.
	if s.config.LlamaCpp.ContextLength != 2048 {
		t.Errorf("in-memory should be restored to base: got %d, want 2048", s.config.LlamaCpp.ContextLength)
	}
}

func TestHandleDeleteLlamaCppOverride_NoOverride(t *testing.T) {
	store := &mockOverridesStore{exists: false}
	base := &types.LlamaCppConfig{ContextLength: 2048}
	s := testServerWithOverrides(t, store, base)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/cluster/llama-cpp/overrides", nil)
	w := httptest.NewRecorder()
	s.handleDeleteLlamaCppOverride(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// existed_before = false (не было override).
	if resp["existed_before"] != false {
		t.Errorf("expected existed_before=false, got %v", resp["existed_before"])
	}
}

func TestHandleDeleteLlamaCppOverride_MethodNotAllowed(t *testing.T) {
	store := &mockOverridesStore{exists: false}
	s := testServerWithOverrides(t, store, &types.LlamaCppConfig{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/cluster/llama-cpp/overrides", nil)
	w := httptest.NewRecorder()
	s.handleDeleteLlamaCppOverride(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestHandleDeleteLlamaCppOverride_StoreDisabled(t *testing.T) {
	s := &Server{config: &types.LoadBalancerConfig{}}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/cluster/llama-cpp/overrides", nil)
	w := httptest.NewRecorder()
	s.handleDeleteLlamaCppOverride(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}
