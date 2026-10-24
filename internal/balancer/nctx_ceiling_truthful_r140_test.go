package balancer

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"ollama-loadbalancer/pkg/types"
)

// R140 (2026-10-16): отказ «окно больше, чем можно дать» обязан называть
// СВЯЗЫВАЮЩИЙ предел.
//
// Живой случай: клиент просит окно больше потолка авто-перезагрузки, и в ответе
// было только операторское число плюс совет «Raise LB_NCTX_RELOAD_MAX_N_CTX».
// Когда в VRAM уже живут другие модели, этот совет невыполним: поднятие
// настройки не создаст память, и клиент уходит в бесплодный цикл. Физический
// предел (max_vram_n_ctx) воркер считает по фактически свободной VRAM, поэтому
// он и есть честный ответ в этом случае.

// TestNCtxRefusalNamesBindingLimitR140 — физический предел НИЖЕ операторского:
// называем физический и НЕ советуем поднимать настройку.
func TestNCtxRefusalNamesBindingLimitR140(t *testing.T) {
	config := createTestConfig()
	proxy := newProxyWithCleanup(t, config)

	cfg := proxy.nctxReload.Config()
	cfg.AutoReloadMaxNCtx = 131072 // операторский потолок
	proxy.nctxReload.SetConfig(cfg)

	// Воркер сообщил, что в свободную VRAM влезает только 8192: часть памяти
	// занята другими загруженными моделями.
	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.llamaMetrics["backend-1"] = &types.LlamaCppMetrics{MaxVRAMNCtx: 8192, VramKnown: true}
	proxy.metricsMgr.mu.Unlock()

	body := []byte(`{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"ок"}],"options":{"num_ctx":262144}}`)
	_, needsProxy, msg, status := proxy.preflightNCtxReloadIfNeeded(
		context.Background(), "backend-1", "gemma-4-E4B-it-Q4_K_M", body, "/api/chat")

	assert.Equal(t, http.StatusRequestEntityTooLarge, status, "ждём честный 413, получили %d: %s", status, msg)
	assert.False(t, needsProxy, "отказ не должен проксировать запрос к воркеру")
	assert.Contains(t, msg, "8192", "отказ обязан назвать физический предел (свободная VRAM с текущими моделями)")
	assert.NotContains(t, msg, "Raise LB_NCTX_RELOAD_MAX_N_CTX",
		"поднимать операторский лимит бесполезно, когда связывает VRAM")
}

// TestNCtxRefusalWithoutPhysicalLimitR140 — физический предел НЕИЗВЕСТЕН
// (воркер ещё не опрошен, max_vram_n_ctx=0): не выдумываем число и говорим
// только про операторский лимит, как раньше.
func TestNCtxRefusalWithoutPhysicalLimitR140(t *testing.T) {
	config := createTestConfig()
	proxy := newProxyWithCleanup(t, config)

	cfg := proxy.nctxReload.Config()
	cfg.AutoReloadMaxNCtx = 131072
	proxy.nctxReload.SetConfig(cfg)

	body := []byte(`{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"ок"}],"options":{"num_ctx":262144}}`)
	_, _, msg, status := proxy.preflightNCtxReloadIfNeeded(
		context.Background(), "backend-1", "gemma-4-E4B-it-Q4_K_M", body, "/api/chat")

	assert.Equal(t, http.StatusRequestEntityTooLarge, status, "отказ остаётся 413: %s", msg)
	assert.Contains(t, msg, "auto_reload_max_n_ctx=131072", "называем операторский лимит")
	assert.NotContains(t, msg, "max_vram_n_ctx=", "неизвестный физический предел не выдумываем")
}
