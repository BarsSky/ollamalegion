//go:build llama_stub

// ollama_show_per_slot_r402_test.go — R402 (2026-11-05).
//
// ЖИВОЙ ДЕФЕКТ. /api/show на загруженной модели отдавал СУММАРНОЕ окно
// (contextSize), хотя слоты его делят: при parallel=2 и total 131072 один
// клиент получает 65536. Клиент (Cline/Roo) читает context_size как «своё»
// окно, набивает историю до мнимого предела, упирается в реальный и показывает
// «увеличьте контекст вручную».
//
// ФИКС. context_size = окно НА СЛОТ; context_size_total и parallel — отдельными
// полями. model_info["<arch>.context_length"] остаётся training context (это
// физический максимум модели, а не окно клиента).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

func showBodyR402(t *testing.T, info *cppbackend.ModelInfo) map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	writeOllamaShowLoadedResponse(rec, info)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	mi, ok := body["model_info"].(map[string]interface{})
	if !ok {
		t.Fatalf("нет model_info: %v", body)
	}
	return mi
}

// TestR402_ShowLoaded_ContextSizeIsPerSlot — клиент видит окно, которое реально
// может использовать: total/parallel.
func TestR402_ShowLoaded_ContextSizeIsPerSlot(t *testing.T) {
	info := &cppbackend.ModelInfo{
		Name: "gemma-4-E4B-it-Q4_K_M", State: cppbackend.StateLoaded,
		Architecture: "gemma4", ContextSize: 131072, Parallel: 2,
		GGUFContextLength: 131072, GPULayers: 42, KVCacheType: "q4_0",
	}
	mi := showBodyR402(t, info)

	num := func(key string) int {
		v, ok := mi[key].(float64)
		if !ok {
			t.Fatalf("нет числового %q в model_info=%v", key, mi)
		}
		return int(v)
	}
	if got := num("context_size"); got != 65536 {
		t.Errorf("context_size = %d, want 65536 (total 131072 / parallel 2)", got)
	}
	if got := num("context_size_total"); got != 131072 {
		t.Errorf("context_size_total = %d, want 131072", got)
	}
	if got := num("parallel"); got != 2 {
		t.Errorf("parallel = %d, want 2", got)
	}
	// training context не подменяется окном клиента: это физический максимум.
	if got := num("gemma4.context_length"); got != 131072 {
		t.Errorf("gemma4.context_length = %d, want 131072 (GGUFContextLength)", got)
	}
}

// TestR402_ShowLoaded_NoSlotsKeepsTotal — при parallel<=1 делить нечего:
// context_size остаётся суммарным (совместимость со старыми сборками).
func TestR402_ShowLoaded_NoSlotsKeepsTotal(t *testing.T) {
	for _, parallel := range []int{0, 1} {
		info := &cppbackend.ModelInfo{
			Name: "m", State: cppbackend.StateLoaded, Architecture: "llama",
			ContextSize: 32768, Parallel: parallel, GGUFContextLength: 32768,
		}
		mi := showBodyR402(t, info)
		if got := int(mi["context_size"].(float64)); got != 32768 {
			t.Errorf("parallel=%d: context_size = %d, want 32768", parallel, got)
		}
		if got := int(mi["context_size_total"].(float64)); got != 32768 {
			t.Errorf("parallel=%d: context_size_total = %d, want 32768", parallel, got)
		}
	}
}

// TestR402_ShowLoaded_ZeroContext — незагруженная/пустая метрика не должна
// делить ноль и отдавать отрицательные числа.
func TestR402_ShowLoaded_ZeroContext(t *testing.T) {
	mi := showBodyR402(t, &cppbackend.ModelInfo{Name: "m", Architecture: "llama", ContextSize: 0, Parallel: 2})
	if got := int(mi["context_size"].(float64)); got != 0 {
		t.Errorf("context_size = %d, want 0", got)
	}
	if got := int(mi["context_size_total"].(float64)); got != 0 {
		t.Errorf("context_size_total = %d, want 0", got)
	}
}

// TestR403_ShowParametersIsNumCtx — R403: поле parameters — это
// Modelfile-параметры ("num_ctx N"), а не размер модели ("3.3B").
//
// Клиенты (Cline/Roo) читают num_ctx именно отсюда; размер модели обязан
// оставаться в details.parameter_size.
func TestR403_ShowParametersIsNumCtx(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOllamaShowLoadedResponse(rec, &cppbackend.ModelInfo{
		Name: "gemma-4-E4B-it-Q4_K_M", State: cppbackend.StateLoaded,
		Architecture: "gemma4", NLayers: 42, NEmbd: 2560,
		ContextSize: 131072, Parallel: 2, GGUFContextLength: 131072,
	})
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if got, _ := body["parameters"].(string); got != "num_ctx 65536" {
		t.Errorf("parameters = %q, want \"num_ctx 65536\" (окно на слот, total 131072 / 2)", got)
	}
	details, _ := body["details"].(map[string]interface{})
	if got, _ := details["parameter_size"].(string); got == "" || got == "num_ctx 65536" {
		t.Errorf("details.parameter_size = %q, want размер модели (не num_ctx)", got)
	}

	// Незагруженная модель: доступен только training context из GGUF.
	rec2 := httptest.NewRecorder()
	writeOllamaShowMetadataResponse(rec2, &cppbackend.GGUFModelMeta{
		Architecture: "gemma4", ContextLength: 131072, NLayers: 42, NEmbd: 2560,
	})
	var body2 map[string]interface{}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("metadata-ответ не JSON: %v", err)
	}
	if got, _ := body2["parameters"].(string); got != "num_ctx 131072" {
		t.Errorf("metadata parameters = %q, want \"num_ctx 131072\" (training context)", got)
	}
}
