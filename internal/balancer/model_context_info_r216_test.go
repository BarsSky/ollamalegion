//go:build llama_stub

// model_context_info_r216_test.go — R216 (2026-10-21).
//
// ЖИВОЙ ВОПРОС ОПЕРАТОРА. «deepseek harness получил неправильный размер
// доступного контекста: продолжает показывать жёстко заданное в конфиге, а
// реально использовал уже много больше; скорее всего проблема в передаче
// контекста от клиента до модели».
//
// Разбор: /v1/models вообще не описывал окно контекста, а единственный заголовок
// X-Model-Max-Context — это ПОТОЛОК модели из GGUF (например 131072), не окно
// загруженного инстанса (n_ctx, может быть 8192). Клиент подбирал историю по
// потолку, отправлял больше, чем инстанс принимает, и получал 413/prompt-too-long
// или внезапную перезагрузку — снаружи «контекст не учитывается».
//
// Тесты фиксируют: modelContextInfo отдаёт ФАКТ (на слот), а не потолок;
// не загруженная модель не выдумывает окно; имя с .gguf нормализуется.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func r216ProxyWithModel(t *testing.T, m types.LlamaCppModel) *Proxy {
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
	lm.LoadedModels = []types.LlamaCppModel{m}
	p.metricsMgr.mu.Unlock()
	return p
}

func TestR216_ModelContextInfo_ReportsLoadedWindowNotCeiling(t *testing.T) {
	p := r216ProxyWithModel(t, types.LlamaCppModel{
		Name:          "gemma-4-E4B-it-Q4_K_M",
		State:         "loaded",
		ContextLength: 8192,
		ContextPerSeq: 8192,
		MaxContext:    131072,
	})
	loaded, maxCtx := p.modelContextInfo("gemma-4-E4B-it-Q4_K_M")
	if loaded != 8192 {
		t.Errorf("loaded = %d, want 8192 (фактическое окно инстанса, не потолок)", loaded)
	}
	if maxCtx != 131072 {
		t.Errorf("maxCtx = %d, want 131072 (потолок модели из GGUF)", maxCtx)
	}
}

// Окно НА СЛОТ важнее суммарного: клиент упирает собственным prompt именно его.
func TestR216_ModelContextInfo_PrefersPerSlotWindow(t *testing.T) {
	p := r216ProxyWithModel(t, types.LlamaCppModel{
		Name:          "m",
		State:         "loaded",
		ContextLength: 16384,
		ContextPerSeq: 8192,
		MaxContext:    32768,
	})
	loaded, _ := p.modelContextInfo("m")
	if loaded != 8192 {
		t.Errorf("loaded = %d, want 8192 (окно одного слота)", loaded)
	}
}

// Имя с .gguf и другим регистром — та же модель (клиенты присылают по-разному).
func TestR216_ModelContextInfo_NormalizesName(t *testing.T) {
	p := r216ProxyWithModel(t, types.LlamaCppModel{
		Name: "Gemma-4-E4B-it-Q4_K_M", State: "loaded", ContextLength: 8192, MaxContext: 131072,
	})
	if loaded, _ := p.modelContextInfo("gemma-4-E4B-it-Q4_K_M.gguf"); loaded != 8192 {
		t.Errorf("loaded = %d, want 8192 при имени с .gguf", loaded)
	}
}

// Неизвестная/незагруженная модель: окна нет, ничего не выдумываем.
func TestR216_ModelContextInfo_UnknownModelIsZero(t *testing.T) {
	p := r216ProxyWithModel(t, types.LlamaCppModel{Name: "other", State: "loaded", ContextLength: 4096})
	loaded, maxCtx := p.modelContextInfo("missing-model")
	if loaded != 0 || maxCtx != 0 {
		t.Errorf("loaded/maxCtx = %d/%d, want 0/0 для неизвестной модели", loaded, maxCtx)
	}
}

func TestR216_ModelContextInfo_NilProxySafe(t *testing.T) {
	var p *Proxy
	if loaded, maxCtx := p.modelContextInfo("m"); loaded != 0 || maxCtx != 0 {
		t.Errorf("nil proxy: %d/%d, want 0/0", loaded, maxCtx)
	}
}
