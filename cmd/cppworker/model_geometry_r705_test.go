//go:build llama_stub

// model_geometry_r705_test.go — R705 (2026-11-24): /api/models и /api/show
// публикуют РЕАЛЬНУЮ ширину головы K/V и её источник.
//
// ПРЕДМЕТ. R660 провёл в /api/models геометрию KV, но значение head_dim_k
// приходило из C-моста, а мост считал n_embd/n_heads. Для моделей стенда это
// занижало ширину и весь KV/токен (Qwen3-4B: 80 против 128 — на 60 %;
// gemma-4-E4B: 320 против 512; Inkling-2-Mini: 96 против 128). Теперь воркер
// отдаёт число из llama_hparams (<arch>.attention.key_length) и рядом —
// head_dim_source (gguf = факт, computed = прежняя формула), чтобы балансер и
// панель видели происхождение.
//
// Тесты проверяют ПРОВОД и значения, а не наличие ключей: на 63a17828 поле
// head_dim_source в ответе отсутствует, а эти проверки это ловят.
package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/cppbackend"
)

// listModelsEntryR705 — одна запись из GET /api/models для указанной модели.
func listModelsEntryR705(t *testing.T) map[string]interface{} {
	t.Helper()
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	const model = "r705-geometry-model"
	if err := backend.InjectLoadedModelForTest(model); err != nil {
		t.Fatalf("инъекция модели: %v", err)
	}
	// Геометрия Qwen3-4B: 36 слоёв, 8 KV-голов; ширина головы — реальная (128).
	if err := backend.SetModelGeometryForTest(model, 36, 8, 128, 2560); err != nil {
		t.Fatalf("инъекция геометрии: %v", err)
	}
	if err := backend.SetModelHeadDimForTest(model, 128, 128, bridge.HeadDimSourceGGUF); err != nil {
		t.Fatalf("инъекция источника ширины: %v", err)
	}

	w := httptest.NewRecorder()
	handleListModels(w, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Models []map[string]interface{} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("моделей в ответе: %d, ожидалась 1", len(resp.Models))
	}
	// Возвращаем копию: defer восстановит backend, но карта уже прочитана.
	return resp.Models[0]
}

// TestR705_ListModelsPublishesRealHeadDim — число 128 (а не формула 80) и источник.
func TestR705_ListModelsPublishesRealHeadDim(t *testing.T) {
	entry := listModelsEntryR705(t)

	num := func(key string) int {
		if v, ok := entry[key].(float64); ok {
			return int(v)
		}
		t.Fatalf("нет числового %q в записи /api/models: %v", key, entry)
		return 0
	}
	if got := num("head_dim_k"); got != 128 {
		t.Errorf("head_dim_k = %d, ожидалось 128 — это число R659 использует для KV", got)
	}
	if got := num("head_dim_v"); got != 128 {
		t.Errorf("head_dim_v = %d, ожидалось 128", got)
	}
	// НЕ-ВАКУУМНОСТЬ: публикуемое значение обязано ОТЛИЧАТЬСЯ от прежней формулы.
	if got := num("head_dim_k"); got == 2560/32 {
		t.Errorf("head_dim_k = %d — это прежняя формула n_embd/n_heads (80), а не ширина из модели", got)
	}
	if got := entry["head_dim_source"]; got != bridge.HeadDimSourceGGUF {
		t.Errorf("head_dim_source = %v, ожидалось %q — без него занижение снова станет невидимым",
			got, bridge.HeadDimSourceGGUF)
	}
	if got := num("n_kv_heads"); got != 8 {
		t.Errorf("n_kv_heads = %d, ожидалось 8 (регресс поля R660)", got)
	}
}

// TestR705_ListModelsMarksComputedFallback — старый мост: число прежнее (80), но
// источник обязан быть computed, а не gguf.
func TestR705_ListModelsMarksComputedFallback(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	const model = "r705-legacy-model"
	if err := backend.InjectLoadedModelForTest(model); err != nil {
		t.Fatalf("инъекция модели: %v", err)
	}
	if err := backend.SetModelGeometryForTest(model, 36, 8, 80, 2560); err != nil {
		t.Fatalf("инъекция геометрии: %v", err)
	}
	if err := backend.SetModelHeadDimForTest(model, 80, 80, bridge.HeadDimSourceComputed); err != nil {
		t.Fatalf("инъекция источника ширины: %v", err)
	}

	w := httptest.NewRecorder()
	handleListModels(w, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	var resp struct {
		Models []struct {
			HeadDimK      int    `json:"head_dim_k"`
			HeadDimSource string `json:"head_dim_source"`
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("моделей в ответе: %d, ожидалась 1", len(resp.Models))
	}
	if resp.Models[0].HeadDimK != 80 {
		t.Errorf("head_dim_k = %d, ожидалось 80 (fallback-значение старого моста)",
			resp.Models[0].HeadDimK)
	}
	if resp.Models[0].HeadDimSource != bridge.HeadDimSourceComputed {
		t.Errorf("head_dim_source = %q, ожидалось %q — оценка не должна выглядеть фактом",
			resp.Models[0].HeadDimSource, bridge.HeadDimSourceComputed)
	}
}

// TestR705_ShowLoadedPublishesRealHeadDim — /api/show на загруженной модели.
func TestR705_ShowLoadedPublishesRealHeadDim(t *testing.T) {
	info := &cppbackend.ModelInfo{
		Name: "Qwen3-4B-Instruct-2507-Q4_K_M", State: cppbackend.StateLoaded,
		Architecture: "qwen3", NLayers: 36, NHeads: 32, NKvHeads: 8,
		HeadDimK: 128, HeadDimV: 128, HeadDimSource: bridge.HeadDimSourceGGUF,
		NEmbd: 2560, ContextSize: 8192, ContextPerSeq: 8192,
		GGUFContextLength: 262144, GPULayers: 36, KVCacheType: "q4_0",
	}
	rec := httptest.NewRecorder()
	writeOllamaShowLoadedResponse(rec, info)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		ModelInfo map[string]interface{} `json:"model_info"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if got, _ := body.ModelInfo["head_dim_k"].(float64); int(got) != 128 {
		t.Errorf("/api/show head_dim_k = %v, ожидалось 128", body.ModelInfo["head_dim_k"])
	}
	if got := body.ModelInfo["head_dim_source"]; got != bridge.HeadDimSourceGGUF {
		t.Errorf("/api/show head_dim_source = %v, ожидалось %q", got, bridge.HeadDimSourceGGUF)
	}
}

// TestR705_ShowMetadataUsesGGUFHeader — незагруженная модель: ширина есть в
// заголовке, отдавать 0 (как было) значит врать клиенту и панели.
func TestR705_ShowMetadataUsesGGUFHeader(t *testing.T) {
	meta := &cppbackend.GGUFModelMeta{
		Filename: "Qwen3-4B-Instruct-2507-Q4_K_M.gguf", Architecture: "qwen3",
		NLayers: 36, NHeads: 32, NKvHeads: 8, NEmbd: 2560, ContextLength: 262144,
		KeyLength: 128, ValueLength: 128,
	}
	rec := httptest.NewRecorder()
	writeOllamaShowMetadataResponse(rec, meta)
	var body struct {
		ModelInfo map[string]interface{} `json:"model_info"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if got, _ := body.ModelInfo["head_dim_k"].(float64); int(got) != 128 {
		t.Errorf("head_dim_k = %v, ожидалось 128 (attention.key_length из заголовка)",
			body.ModelInfo["head_dim_k"])
	}
	if got, _ := body.ModelInfo["head_dim_v"].(float64); int(got) != 128 {
		t.Errorf("head_dim_v = %v, ожидалось 128", body.ModelInfo["head_dim_v"])
	}
	if got := body.ModelInfo["head_dim_source"]; got != bridge.HeadDimSourceGGUF {
		t.Errorf("head_dim_source = %v, ожидалось %q", got, bridge.HeadDimSourceGGUF)
	}

	// Без key_length: прежняя формула, но источник — computed.
	metaNoKey := &cppbackend.GGUFModelMeta{
		Filename: "legacy.gguf", Architecture: "qwen3",
		NLayers: 36, NHeads: 32, NKvHeads: 8, NEmbd: 2560, ContextLength: 32768,
	}
	rec = httptest.NewRecorder()
	writeOllamaShowMetadataResponse(rec, metaNoKey)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if got, _ := body.ModelInfo["head_dim_k"].(float64); int(got) != 80 {
		t.Errorf("без key_length head_dim_k = %v, ожидалось 80 (формула)", body.ModelInfo["head_dim_k"])
	}
	if got := body.ModelInfo["head_dim_source"]; got != bridge.HeadDimSourceComputed {
		t.Errorf("без key_length head_dim_source = %v, ожидалось %q", got, bridge.HeadDimSourceComputed)
	}
}

// TestR705_SizeVRAMUsesRealHeadDim — size_vram (уезжает в балансер как VRAMUsage)
// обязан считаться по реальной ширине: разница между 128 и 80 — 475 MiB KV при
// 36 слоях, 8 KV-головах, ctx 8192, f16 (ручной расчёт, не вызов той же функции).
func TestR705_SizeVRAMUsesRealHeadDim(t *testing.T) {
	base := cppbackend.ModelInfo{
		Name: "m", State: cppbackend.StateLoaded, NLayers: 36, NHeads: 32,
		NKvHeads: 8, NEmbd: 2560, ContextSize: 8192, KVCacheType: "f16",
		GPULayers: 36,
	}
	real, computed := base, base
	real.HeadDimK, real.HeadDimV = 128, 128
	computed.HeadDimK, computed.HeadDimV = 80, 80

	const sizeBytes = uint64(1) << 30 // 1 GiB весов, вся модель на GPU
	gotReal := estimateVRAMSizeForModel(&real, sizeBytes)
	gotComputed := estimateVRAMSizeForModel(&computed, sizeBytes)
	if gotReal <= gotComputed {
		t.Fatalf("size_vram(128) = %d не больше size_vram(80) = %d — KV всё ещё считается по формуле",
			gotReal, gotComputed)
	}
	const wantDeltaMiB = 475 // (1267 - 792) MiB
	gotDelta := float64(gotReal-gotComputed) / (1024 * 1024)
	if math.Abs(gotDelta-wantDeltaMiB) > 1 {
		t.Errorf("разница size_vram = %.1f MiB, ожидалось ~%d MiB (KV растёт пропорционально ширине)",
			gotDelta, wantDeltaMiB)
	}
}
