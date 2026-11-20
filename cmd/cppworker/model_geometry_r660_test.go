//go:build llama_stub

// model_geometry_r660_test.go — R660 (2026-11-20): /api/models отдаёт геометрию KV.
//
// ЗАЧЕМ. R659 решает «поднимется ли модель с НУЖНЫМ ОКНОМ на свободном узле» по
// формуле KV (2 × nLayers × nKvHeads × headDimK × байт-на-элемент, memfit) и по
// свободной VRAM узла. Геометрию берут из /api/models той копии модели, что уже
// загружена. Пока воркер её не отдавал, балансер честно писал «проверить нечем»
// (verified=false) и поднимал модель на свободном узле БЕЗ проверки окна.
//
// ЖИВОЙ ФАКТ, из которого это выросло (локальный стенд, 2026-11-20): у загруженной
// Qwen3-4B в ответе были n_layers/n_embd, но не было n_kv_heads/head_dim_k — при
// том что max_vram_n_ctx=19847 воркер посчитал именно по ним.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestR660_ListModelsExposesKVGeometry — значения, а не только ключи.
//
// Проверяются ИМЕНА полей, которые разбирает балансерный поллер
// (llamacpp_metrics_poller.go: n_kv_heads, head_dim_k, head_dim_v). Переименование
// любого из них молча вернёт R659 в «проверить нечем».
func TestR660_ListModelsExposesKVGeometry(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	const model = "r660-geometry-model"
	if err := backend.InjectLoadedModelForTest(model); err != nil {
		t.Fatalf("инъекция модели: %v", err)
	}
	if err := backend.SetModelGeometryForTest(model, 36, 8, 128, 2560); err != nil {
		t.Fatalf("инъекция геометрии: %v", err)
	}

	w := httptest.NewRecorder()
	handleListModels(w, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Models []struct {
			NLayers   int `json:"n_layers"`
			NKvHeads  int `json:"n_kv_heads"`
			HeadDimK  int `json:"head_dim_k"`
			HeadDimV  int `json:"head_dim_v"`
			NEmbd     int `json:"n_embd"`
			ContextSS int `json:"context_size"`
		} `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if len(resp.Models) != 1 {
		t.Fatalf("моделей в ответе: %d, ожидалась 1", len(resp.Models))
	}
	m := resp.Models[0]
	if m.NLayers != 36 {
		t.Errorf("n_layers = %d, ожидалось 36", m.NLayers)
	}
	if m.NKvHeads != 8 {
		t.Errorf("n_kv_heads = %d, ожидалось 8 — без него R659 не может проверить окно", m.NKvHeads)
	}
	if m.HeadDimK != 128 {
		t.Errorf("head_dim_k = %d, ожидалось 128 — без него R659 не может проверить окно", m.HeadDimK)
	}
	if m.NEmbd != 2560 {
		t.Errorf("n_embd = %d, ожидалось 2560 (регресс существующего поля)", m.NEmbd)
	}
}
