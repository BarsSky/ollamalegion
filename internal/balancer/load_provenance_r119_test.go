//go:build llama_stub

// load_provenance_r119_test.go — R119 (2026-10-15), пункт 4 плана.
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «непонятно, к какой модели применяются настройки и
// применяются ли вообще». Причина: у одного параметра четыре источника (явный
// запрос клиента, per-model профиль, «настройки по умолчанию», env cppworker), а
// в UI/API показывались только ЗНАЧЕНИЯ — какой слой их поставил, видно не было.
//
// Тесты фиксируют, что происхождение ИЗМЕРЯЕТСЯ по ходу применения слоёв
// (сравнением снимков) и потому не может разойтись с тем, что реально уехало в
// cppworker:
//
//	request         — значение пришло в запросе и слои его не трогали;
//	profile         — поставил per-model профиль;
//	default_profile — поставили «настройки по умолчанию»;
//	backend         — никто не задавал, решит cppworker (env/config).
package balancer

import (
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func provIntPtr(v int) *int    { return &v }
func provBoolPtr(v bool) *bool { return &v }

// TestR119_Provenance_FourSources — все четыре источника в одном запросе.
func TestR119_Provenance_FourSources(t *testing.T) {
	req := ModelOpRequest{
		ModelName:   "m",
		ContextSize: provIntPtr(8192), // пришло от клиента/WebUI-формы
	}
	b := newLoadProvenanceBuilder(&req)

	// Слой 1: профиль модели (ставит parallel, contextPerSeq, kvCacheType).
	prof := types.LlamaCppModelProfile{
		Parallel:      2,
		ContextPerSeq: 4096,
		KVCacheType:   "q4_0",
	}
	b.observe(LoadSourceProfile, func() { applyProfileLoadParams(&req, prof) })

	// Слой 2: «настройки по умолчанию» (ставят batchSize и useMmap).
	dp := types.LlamaCppModelProfile{
		BatchSize: 512,
		UseMmap:   provBoolPtr(false),
	}
	b.observe(LoadSourceDefaultProfile, func() { applyProfileLoadParams(&req, dp) })

	prov := b.finalize()

	want := map[string]string{
		"contextLength":   LoadSourceRequest,        // было в запросе, слои не трогали
		"parallel":        LoadSourceProfile,        // поставил профиль модели
		"contextPerSeq":   LoadSourceProfile,        // поставил профиль модели
		"kvCacheType":     LoadSourceProfile,        // поставил профиль модели
		"batchSize":       LoadSourceDefaultProfile, // поставили настройки по умолчанию
		"useMmap":         LoadSourceDefaultProfile, // поставили настройки по умолчанию
		"gpuLayers":       LoadSourceBackend,        // никто не задал → cppworker
		"flashAttn":       LoadSourceBackend,
		"enableReasoning": LoadSourceBackend,
	}
	for key, wantSource := range want {
		got, ok := prov[key]
		if !ok {
			t.Errorf("параметр %q отсутствует в provenance", key)
			continue
		}
		if got.Source != wantSource {
			t.Errorf("%s: source = %q, want %q", key, got.Source, wantSource)
		}
	}

	// Значения обязаны совпадать с тем, что реально уехало бы в cppworker.
	if v, _ := prov["contextLength"].Value.(int); v != 8192 {
		t.Errorf("contextLength.value = %v, want 8192", prov["contextLength"].Value)
	}
	if v, _ := prov["parallel"].Value.(int); v != 2 {
		t.Errorf("parallel.value = %v, want 2", prov["parallel"].Value)
	}
	if v, _ := prov["useMmap"].Value.(bool); v != false {
		t.Errorf("useMmap.value = %v, want false", prov["useMmap"].Value)
	}
	if prov["gpuLayers"].Value != nil {
		t.Errorf("gpuLayers.value = %v, want nil (cppworker решит сам)", prov["gpuLayers"].Value)
	}
}

// TestR119_Provenance_RequestBeatsProfile — значение из запроса слои не
// перезаписывают, и source остаётся request (это и есть приоритет оператора-клиента).
func TestR119_Provenance_RequestBeatsProfile(t *testing.T) {
	req := ModelOpRequest{ModelName: "m", Parallel: provIntPtr(3)}
	b := newLoadProvenanceBuilder(&req)
	b.observe(LoadSourceProfile, func() {
		applyProfileLoadParams(&req, types.LlamaCppModelProfile{Parallel: 2})
	})
	prov := b.finalize()
	if prov["parallel"].Source != LoadSourceRequest {
		t.Errorf("parallel.source = %q, want request (значение клиента важнее профиля)", prov["parallel"].Source)
	}
	if v, _ := prov["parallel"].Value.(int); v != 3 {
		t.Errorf("parallel.value = %v, want 3 (значение запроса не перезаписано профилем)", prov["parallel"].Value)
	}
}

// TestR119_Provenance_NoProfileNoDefault — без слоёв всё либо request, либо backend.
func TestR119_Provenance_NoProfileNoDefault(t *testing.T) {
	req := ModelOpRequest{ModelName: "m", BatchSize: provIntPtr(256)}
	b := newLoadProvenanceBuilder(&req)
	prov := b.finalize()
	if prov["batchSize"].Source != LoadSourceRequest {
		t.Errorf("batchSize.source = %q, want request", prov["batchSize"].Source)
	}
	if prov["parallel"].Source != LoadSourceBackend {
		t.Errorf("parallel.source = %q, want backend", prov["parallel"].Source)
	}
}

// TestR119_Provenance_StoredAndExposedAsJSON — хранилище и форма для API/UI.
func TestR119_Provenance_StoredAndExposedAsJSON(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	req := ModelOpRequest{ModelName: "gemma", ContextSize: provIntPtr(16384)}
	b := newLoadProvenanceBuilder(&req)
	b.observe(LoadSourceProfile, func() {
		applyProfileLoadParams(&req, types.LlamaCppModelProfile{Parallel: 2, KVCacheType: "q4_0"})
	})
	p.RecordLoadProvenance("bk-1", "gemma", b.finalize())

	js := p.LoadProvenanceJSON("bk-1", "gemma")
	if len(js) == 0 {
		t.Fatal("provenance не отдан API-слою")
	}
	entry, ok := js["contextLength"].(map[string]interface{})
	if !ok {
		t.Fatalf("contextLength не объект {value,source}: %v", js["contextLength"])
	}
	if entry["source"] != LoadSourceRequest || entry["value"] != 16384 {
		t.Errorf("contextLength = %v, want {value:16384, source:request}", entry)
	}
	if got := p.LoadProvenanceJSON("bk-2", "gemma"); got != nil {
		t.Errorf("чужой бэкенд вернул %v, want nil", got)
	}
	if got := p.LoadProvenanceJSON("bk-1", "other"); got != nil {
		t.Errorf("чужая модель вернула %v, want nil", got)
	}
}

// TestR119_Provenance_RecordIsSafeOnNilProxy — защита от паники на пустом прокси.
func TestR119_Provenance_RecordIsSafeOnNilProxy(t *testing.T) {
	var p *Proxy
	p.RecordLoadProvenance("bk", "m", loadProvenance{"parallel": loadParamOrigin{Value: 2, Source: LoadSourceProfile}})
	if got := p.LoadProvenance("bk", "m"); got != nil {
		t.Errorf("nil-прокси вернул %v", got)
	}
}

// TestR119_Provenance_WiredIntoLoadPath — защита от отката: загрузка обязана
// считать provenance и сохранять его.
func TestR119_Provenance_WiredIntoLoadPath(t *testing.T) {
	raw, err := os.ReadFile("model_management.go")
	if err != nil {
		t.Fatalf("read model_management.go: %v", err)
	}
	src := string(raw)
	for _, needle := range []string{
		"newLoadProvenanceBuilder(&req)",
		"provenance.observe(LoadSourceProfile,",
		"provenance.observe(LoadSourceDefaultProfile,",
		"provenance.finalize()",
	} {
		if !strings.Contains(src, needle) {
			t.Errorf("в executeLlamaCppLoad нет %q — происхождение параметров снова неизвестно", needle)
		}
	}
}
