//go:build llama_stub

// v1_models_context_r219_test.go — R219 (2026-10-21).
//
// Живой дефект R216: `/v1/models` не описывал окно контекста вовсе, а
// единственный заголовок X-Model-Max-Context — потолок модели из GGUF
// (131072/262144), не окно загруженного инстанса. Клиент (deepseek harness,
// OpenWebUI) подбирал историю по потолку, отправлял больше, чем инстанс
// принимает, и получал 413 либо внезапную перезагрузку.
//
// Тест фиксирует ФОРМУ ответа /v1/models: для загруженной модели обязаны быть
// context_length (фактическое окно на слот) и max_context_length (потолок).
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/pkg/types"
)

func r219ProxyWithLoadedModel(t *testing.T) *Proxy {
	t.Helper()
	cfg := &types.LoadBalancerConfig{}
	cfg.Backends = []types.Backend{{
		ID: "b1", Type: types.BackendTypeLlamaCpp, Host: "h", CppWorkerPort: 18092, Status: types.StatusHealthy,
	}}
	p := newProxyWithCleanup(t, cfg)

	p.metricsMgr.mu.Lock()
	lm := p.metricsMgr.llamaMetrics["b1"]
	if lm == nil {
		lm = &types.LlamaCppMetrics{}
		p.metricsMgr.llamaMetrics["b1"] = lm
	}
	lm.LoadedModels = []types.LlamaCppModel{{
		Name: "Qwen3.8-27B-UD-Q4_K_M", State: "loaded",
		ContextLength: 256000, ContextPerSeq: 128000,
		MaxContext: 262144, GGUFMaxContext: 262144,
		Capabilities: &types.ModelCapabilities{MaxContext: 262144},
	}}
	p.metricsMgr.mu.Unlock()
	return p
}

func TestR219_OpenAIModelsCarryContextWindow(t *testing.T) {
	p := r219ProxyWithLoadedModel(t)
	lr := NewLlamaCppRouter(p)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	lr.handleOpenAIModels(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []map[string]interface{} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	var entry map[string]interface{}
	for _, m := range resp.Data {
		if m["id"] == "Qwen3.8-27B-UD-Q4_K_M" {
			entry = m
			break
		}
	}
	require.NotNil(t, entry, "загруженная модель обязана быть в /v1/models")

	// Фактическое окно инстанса — НА СЛОТ (128000), а не потолок (262144).
	assert.Equal(t, float64(128000), entry["context_length"],
		"context_length должен быть окном на слот: иначе клиент подбирает историю по потолку")
	assert.Equal(t, float64(262144), entry["max_context_length"],
		"max_context_length — потолок модели из GGUF")
}

func TestR219_CapabilitiesHeadersCarryLoadedContext(t *testing.T) {
	p := r219ProxyWithLoadedModel(t)
	rec := httptest.NewRecorder()
	p.addModelCapabilitiesHeaders(rec, "Qwen3.8-27B-UD-Q4_K_M")
	// X-Model-Context-Length — окно на слот; X-Model-Max-Context — потолок.
	assert.Equal(t, "128000", rec.Header().Get("X-Model-Context-Length"))
	assert.Equal(t, "262144", rec.Header().Get("X-Model-Max-Context"))
}
