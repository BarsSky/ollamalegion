//go:build llama_stub

// nctx_vram_known_r519_test.go — R519 (2026-11-19): «0» у max_vram_n_ctx — это
// ДВА разных факта, и отказ обязан звучать по-разному.
//
// ЖИВОЙ ФАКТ (стенд, загружена 4B на RTX 3070): `/api/models` воркера отдаёт
// `max_vram_n_ctx: 0, vram_known: true` — VRAM измерена, свободной не хватает
// даже на минимальное окно сверх загруженного. Балансер склеивал это с
// «неизвестно» (vram_known=false) и в отказе советовал «Raise the declared window
// … LB_NCTX_RELOAD_MAX_N_CTX»: совет, который ничего не создаст — места в VRAM
// нет, и клиент уходил бы в цикл поднятия настройки (та же болезнь, что лечил
// R140, но с другой стороны: R140 знал про случай physical>0, этот — про 0).
package balancer

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// r519Proxy — стенд: операторский потолок 131072, VRAM известна.
func r519Proxy(t *testing.T, vramKnown bool, maxVRAMNCtx int) *Proxy {
	t.Helper()
	config := createTestConfig()
	proxy := newProxyWithCleanup(t, config)
	cfg := proxy.nctxReload.Config()
	cfg.AutoReloadMaxNCtx = 131072
	proxy.nctxReload.SetConfig(cfg)
	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.llamaMetrics["backend-1"] = &types.LlamaCppMetrics{
		MaxVRAMNCtx: maxVRAMNCtx,
		VramKnown:   vramKnown,
		LoadedModels: []types.LlamaCppModel{{
			Name:          "Qwen3-4B-Instruct-2507-Q4_K_M",
			State:         "loaded",
			ContextLength: 32768,
			ContextPerSeq: 32768,
			MaxSlots:      1,
			Parallel:      1,
		}},
	}
	proxy.metricsMgr.mu.Unlock()
	return proxy
}

func r519Body() []byte {
	return []byte(`{"model":"Qwen3-4B-Instruct-2507-Q4_K_M","messages":[{"role":"user","content":"ок"}],"options":{"num_ctx":262144}}`)
}

// TestR519_VRAMExhaustedNamesVRAMNotEnv — «VRAM есть, места нет»: не советуем
// поднимать настройку, называем настоящую причину.
func TestR519_VRAMExhaustedNamesVRAMNotEnv(t *testing.T) {
	proxy := r519Proxy(t, true, 0)
	_, needsProxy, msg, status := proxy.preflightNCtxReloadIfNeeded(
		context.Background(), "backend-1", "Qwen3-4B-Instruct-2507-Q4_K_M", r519Body(), "/api/chat")

	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("статус = %d, want 413 (текст: %s)", status, msg)
	}
	if needsProxy {
		t.Fatalf("отказ не должен проксировать запрос к воркеру")
	}
	if !strings.Contains(msg, "free VRAM is not enough") {
		t.Fatalf("отказ обязан сказать, что свободной VRAM не хватает; текст: %s", msg)
	}
	if strings.Contains(msg, "Raise the declared window") || strings.Contains(msg, "LB_NCTX_RELOAD_MAX_N_CTX") {
		t.Fatalf("совет поднять лимит бесполезен, когда связывает VRAM: %s", msg)
	}
}

// TestR519_UnknownVRAMKeepsOldAdvice — измерений нет: честно «неизвестно»,
// прежний совет остаётся (это поведение R140, менять его нельзя).
func TestR519_UnknownVRAMKeepsOldAdvice(t *testing.T) {
	proxy := r519Proxy(t, false, 0)
	_, _, msg, status := proxy.preflightNCtxReloadIfNeeded(
		context.Background(), "backend-1", "Qwen3-4B-Instruct-2507-Q4_K_M", r519Body(), "/api/chat")

	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("статус = %d, want 413 (текст: %s)", status, msg)
	}
	if !strings.Contains(msg, "auto_reload_max_n_ctx=131072") {
		t.Fatalf("при неизвестной VRAM называем операторский лимит; текст: %s", msg)
	}
	if strings.Contains(msg, "free VRAM is not enough") {
		t.Fatalf("неизвестная VRAM не должна выдаваться за «места нет»: %s", msg)
	}
}

// TestR520_EmptyBackendIsUnknownNotNoRoom — на пустом узле «0» = «не считали».
//
// Живой факт: только что развёрнутый стенд (0 моделей, ~7 ГБ свободной VRAM)
// отдаёт max_vram_n_ctx=0 при vram_known=true. Первая версия R519 объявляла это
// «свободной VRAM не хватает» — неправда, вводящая оператора в заблуждение.
func TestR520_EmptyBackendIsUnknownNotNoRoom(t *testing.T) {
	proxy := r519Proxy(t, true, 0)
	// Убираем загруженные модели: exact-fit воркер для них не считал.
	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.llamaMetrics["backend-1"].LoadedModels = nil
	proxy.metricsMgr.mu.Unlock()

	_, _, msg, status := proxy.preflightNCtxReloadIfNeeded(
		context.Background(), "backend-1", "Qwen3-4B-Instruct-2507-Q4_K_M", r519Body(), "/api/chat")
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("статус = %d, want 413 (текст: %s)", status, msg)
	}
	if strings.Contains(msg, "free VRAM is not enough") {
		t.Fatalf("на пустом узле нельзя утверждать «места нет»: %s", msg)
	}
	if !strings.Contains(msg, "auto_reload_max_n_ctx") {
		t.Fatalf("ожидался прежний текст про операторский лимит; получено: %s", msg)
	}
}

// TestR519_PositivePhysicalLimitKeepsPhysicalWording — VRAM известна и предел
// положителен: остаётся формулировка R140 (этот путь не тронут).
func TestR519_PositivePhysicalLimitKeepsPhysicalWording(t *testing.T) {
	proxy := r519Proxy(t, true, 8192)
	_, _, msg, status := proxy.preflightNCtxReloadIfNeeded(
		context.Background(), "backend-1", "Qwen3-4B-Instruct-2507-Q4_K_M", r519Body(), "/api/chat")

	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("статус = %d, want 413 (текст: %s)", status, msg)
	}
	if !strings.Contains(msg, "max_vram_n_ctx=8192") {
		t.Fatalf("отказ обязан назвать физический предел 8192; текст: %s", msg)
	}
}
