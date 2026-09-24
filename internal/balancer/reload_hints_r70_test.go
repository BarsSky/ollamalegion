// reload_hints_r70_test.go — R70 (2026-09-24): балансер передаёт в адаптивную
// стратегию cppworker известный рабочий тип KV-cache.
//
// Проверяем:
//  1. хинт берётся из профиля модели (в т.ч. по substring-совпадению имени);
//  2. если профиля нет — из фактического kv_cache_type загруженной модели;
//  3. если ничего неизвестно — пустая строка (прежнее поведение);
//  4. queryAdaptiveStrategy действительно добавляет ?kv_cache_type=… в запрос.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestReloadHints_R70_FromProfile — профиль модели задаёт kvCacheType, в том
// числе когда имя профиля короче имени модели ("gemma-4" → "gemma-4-E4B-it-Q4_K_M").
func TestReloadHints_R70_FromProfile(t *testing.T) {
	p := admissionTestProxy(t, 4)
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"gemma-4": {ContextLength: 32768, KVCacheType: "q4_0"},
	}
	hints := p.reloadHintsFor("llama_adm", "gemma-4-E4B-it-Q4_K_M")
	if hints.KVCacheType != "q4_0" {
		t.Fatalf("ожидался q4_0 из профиля (substring match), получено %q", hints.KVCacheType)
	}
}

// TestReloadHints_R70_FromLoadedModel — без профиля берём фактический тип
// загруженной модели (cppworker /api/models → llamaMetrics).
func TestReloadHints_R70_FromLoadedModel(t *testing.T) {
	p := admissionTestProxy(t, 4)
	p.config.LlamaCppModelProfiles = nil
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["llama_adm"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{
			{Name: "gemma-4-E4B-it-Q4_K_M", State: "loaded", ContextLength: 65536, KvCacheType: "q8_0"},
		},
	}
	p.metricsMgr.mu.Unlock()

	hints := p.reloadHintsFor("llama_adm", "gemma-4-E4B-it-Q4_K_M")
	if hints.KVCacheType != "q8_0" {
		t.Fatalf("ожидался q8_0 из загруженной модели, получено %q", hints.KVCacheType)
	}
}

// TestReloadHints_R70_UnknownIsEmpty — ничего неизвестно → пустой хинт
// (стратегия посчитает как раньше).
func TestReloadHints_R70_UnknownIsEmpty(t *testing.T) {
	p := admissionTestProxy(t, 4)
	p.config.LlamaCppModelProfiles = nil
	if hints := p.reloadHintsFor("llama_adm", "unknown-model"); hints.KVCacheType != "" {
		t.Fatalf("ожидался пустой хинт, получено %q", hints.KVCacheType)
	}
}

// TestReloadHintsProvider_R70_WiredInNewProxy — провайдер установлен в NewProxy,
// то есть координатор реально видит профиль (иначе хинт не дойдёт до cppworker).
func TestReloadHintsProvider_R70_WiredInNewProxy(t *testing.T) {
	p := admissionTestProxy(t, 4)
	p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		"gemma": {ContextLength: 8192, KVCacheType: "q4_0"},
	}
	got := p.nctxReload.ReloadHintsFor("llama_adm", "gemma")
	if got.KVCacheType != "q4_0" {
		t.Fatalf("координатор не получил хинт от Proxy: %+v", got)
	}
}

// TestQueryAdaptiveStrategy_R70_PassesKVHint — параметр kv_cache_type реально
// уходит в запрос (иначе cppworker снова посчитает по f16).
func TestQueryAdaptiveStrategy_R70_PassesKVHint(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gpuLayers":42,"nCtx":65536,"kvCacheType":"q4_0","stage":"partial_offload","useMmap":true}`))
	}))
	defer srv.Close()

	st := queryAdaptiveStrategy(srv.URL, "gemma", 65536, "q4_0", srv.Client())
	if st == nil {
		t.Fatal("стратегия не получена")
	}
	if !containsFold(gotQuery, "kv_cache_type=q4_0") {
		t.Errorf("в запросе нет kv_cache_type=q4_0: %q", gotQuery)
	}
	if !containsFold(gotQuery, "n_ctx=65536") {
		t.Errorf("в запросе нет n_ctx=65536: %q", gotQuery)
	}

	// Без хинта параметр не добавляется (прежнее поведение).
	gotQuery = ""
	if st := queryAdaptiveStrategy(srv.URL, "gemma", 65536, "", srv.Client()); st == nil {
		t.Fatal("стратегия без хинта не получена")
	}
	if containsFold(gotQuery, "kv_cache_type") {
		t.Errorf("без хинта параметр kv_cache_type добавляться не должен: %q", gotQuery)
	}
}
