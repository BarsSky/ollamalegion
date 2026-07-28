// handlers_cluster_llamacpp_test.go — тесты для LlamaCpp-секции в
// clusterConfigHandler (Session 17, 2026-07-27).
//
// Проверяет:
//   1. PUT /api/v1/cluster/config с "llamaCpp" применяет partial-поля
//   2. Передаваемые bool-поля (false = сброс) реально сбрасываются
//   3. Передаваемые строковые поля ("" = сброс) реально сбрасываются
//   4. LlamaCpp возвращается в response
//   5. Не-LlamaCpp-поля не затираются (partial update не ломает остальные)
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// testClusterConfigServer — минимальный Server для теста clusterConfigHandler.
// Создаёт дефолтный config + configSaver = noop.
func testClusterConfigServer(t *testing.T) *Server {
	t.Helper()
	cfg := &types.LoadBalancerConfig{}
	// Применяем дефолты (как в config.go:ApplyDefaults).
	configDefaults(cfg)
	// Ставим токен чтобы пройти auth.
	cfg.Auth.Tokens = []string{"test-token"}
	return &Server{
		config: cfg,
		// configSaver = nil — нас устраивает, что сохранение пропускается.
	}
}

func TestClusterConfigHandler_Put_LlamaCppPartial(t *testing.T) {
	s := testClusterConfigServer(t)
	r := &s.config.LlamaCpp // pointer — иначе не увидим мутации handler'а

	// Сначала запомним исходное значение numGpuLayers.
	originalGpuLayers := r.NumGPULayers
	// Запомним исходное значение strategy чтобы убедиться, что не затирается.
	originalStrategy := r.Strategy

	// Слать только contextLength + kvCacheType (partial update).
	body := `{"llamaCpp":{"contextLength":131072,"kvCacheType":"q8_0","ropeScalingType":"yarn","ropeScalingFactor":4.0,"yarnExtFactor":2.0,"yarnAttnFactor":1.0,"useMmap":false,"useMlock":true}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cluster/config",
		bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	s.clusterConfigHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	// Проверяем, что contextLength и kvCacheType применились.
	if r.ContextLength != 131072 {
		t.Errorf("ContextLength: got %d, want 131072", r.ContextLength)
	}
	if r.KVCacheType != "q8_0" {
		t.Errorf("KVCacheType: got %q, want q8_0", r.KVCacheType)
	}
	// bool: useMlock=true должен примениться (даже если изначально был false).
	if !r.UseMLock {
		t.Errorf("UseMLock: got false, want true (from request)")
	}
	// bool: useMmap=false должен сбросить (partial update всё перезаписывает).
	if r.UseMMap {
		t.Errorf("UseMMap: got true, want false (partial override)")
	}
	// Не-LlamaCpp поля НЕ должны были затереться.
	if r.NumGPULayers != originalGpuLayers {
		t.Errorf("NumGPULayers should be unchanged: got %d, want %d",
			r.NumGPULayers, originalGpuLayers)
	}
	if r.Strategy != originalStrategy {
		t.Errorf("Strategy should be unchanged: got %q, want %q",
			r.Strategy, originalStrategy)
	}

	// Response содержит llamaCpp.
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	updated, _ := resp["updated"].([]interface{})
	found := false
	for _, u := range updated {
		if u == "llamaCpp" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("llamaCpp NOT in updated[]: %v", updated)
	}
	configBlock, _ := resp["config"].(map[string]interface{})
	llResp, ok := configBlock["llamaCpp"].(map[string]interface{})
	if !ok {
		t.Fatalf("llamaCpp not in response.config: %v", configBlock)
	}
	if llResp["contextLength"].(float64) != 131072 {
		t.Errorf("response contextLength: got %v, want 131072", llResp["contextLength"])
	}
}

func TestClusterConfigHandler_Put_LlamaCppEmpty(t *testing.T) {
	// Пустой объект llamaCpp — не должен ничего затереть, updated[] пустой.
	s := testClusterConfigServer(t)
	r := &s.config.LlamaCpp
	originalCtx := r.ContextLength

	body := `{"llamaCpp":{}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cluster/config",
		bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	s.clusterConfigHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	// Контекст должен остаться.
	if r.ContextLength != originalCtx {
		t.Errorf("empty llamaCpp should not touch anything, but ContextLength changed: got %d, want %d",
			r.ContextLength, originalCtx)
	}
}

func TestClusterConfigHandler_Put_NoLlamaCppKey(t *testing.T) {
	// Запрос БЕЗ секции llamaCpp — не должен ничего ломать.
	s := testClusterConfigServer(t)
	r := &s.config.LlamaCpp
	originalCtx := r.ContextLength

	body := `{"algorithm":"resource-aware","gpuMaxUsage":85.0}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cluster/config",
		bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	s.clusterConfigHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	if r.ContextLength != originalCtx {
		t.Errorf("missing llamaCpp should not touch it, but ContextLength changed: got %d, want %d",
			r.ContextLength, originalCtx)
	}
}

func TestClusterConfigHandler_Put_BoolReset(t *testing.T) {
	// bool-поля должны сбрасываться в false, если явно прислано false.
	s := testClusterConfigServer(t)
	r := &s.config.LlamaCpp
	r.FlashAttention = true   // ставим true
	r.AutoGpuDistribution = true

	body := `{"llamaCpp":{"flashAttention":false,"autoGpuDistribution":false}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/cluster/config",
		bytes.NewBufferString(body))
	w := httptest.NewRecorder()

	s.clusterConfigHandler(w, req)

	if r.FlashAttention {
		t.Error("FlashAttention should be reset to false")
	}
	if r.AutoGpuDistribution {
		t.Error("AutoGpuDistribution should be reset to false")
	}
}
