//go:build llama_stub

// ollama_show_loaded_context_length_r119_test.go — пункт H3 аудита.
//
// ДЕФЕКТ: /api/show на ЗАГРУЖЕННОЙ модели (Path 1, writeOllamaShowLoadedResponse)
// отдавал только runtime-ключ context_size (n_ctx, урезанный под VRAM), но НЕ
// отдавал "<arch>.context_length" — training context из GGUF-заголовка
// (info.GGUFContextLength). Незагруженная ветка (Path 2,
// writeOllamaShowMetadataResponse) уже отдаёт этот ключ с R65d, поэтому клиенты
// (Cline/Roo/OpenWebUI), читающие model_info["<arch>.context_length"], видели
// заголовок только до первой загрузки модели: после загрузки окно выглядело
// неопределённым и история урезалась по context_size (n_ctx), а не по реальному
// контексту модели.
//
// ФИКС: если info.GGUFContextLength > 0 и info.Architecture != "", Path 1
// добавляет model_info["<arch>.context_length"] = info.GGUFContextLength.
// Ключ context_size остаётся как есть — это runtime n_ctx.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// loadedGemma4R119 — загруженная gemma-4 с УЖЕ УРЕЗАННЫМ runtime n_ctx
// (ContextSize=8192) и настоящим контекстом из GGUF-заголовка
// (GGUFContextLength=32768). Именно этот разрыв и должен быть виден клиенту.
func loadedGemma4R119() *cppbackend.ModelInfo {
	return &cppbackend.ModelInfo{
		Name:              "gemma-4-E4B-it-Q4_K_M",
		State:             cppbackend.StateLoaded,
		Architecture:      "gemma4",
		NLayers:           42,
		NHeads:            8,
		NKvHeads:          2,
		NEmbd:             2560,
		NVocab:            262144,
		ContextSize:       8192,
		GGUFContextLength: 32768,
		GPULayers:         19,
		KVCacheType:       "q4_0",
	}
}

// TestR119_OllamaShowLoaded_ContextLengthKey — на загруженной модели в
// model_info обязаны быть ОБА ключа: context_size (runtime n_ctx) и
// gemma4.context_length (training context из GGUF).
func TestR119_OllamaShowLoaded_ContextLengthKey(t *testing.T) {
	rec := httptest.NewRecorder()

	writeOllamaShowLoadedResponse(rec, loadedGemma4R119())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v (body=%q)", err, rec.Body.String())
	}
	modelInfo, ok := body["model_info"].(map[string]interface{})
	if !ok {
		t.Fatalf("нет объекта model_info (body=%v)", body)
	}

	// context_size — runtime n_ctx, не должен подменяться training context'ом.
	gotCtxSize, ok := modelInfo["context_size"].(float64)
	if !ok {
		t.Fatalf("нет числового context_size в model_info=%v", modelInfo)
	}
	if int(gotCtxSize) != 8192 {
		t.Errorf("context_size = %v, want 8192 (runtime n_ctx не должен меняться)", gotCtxSize)
	}

	// <arch>.context_length — training context из GGUF-заголовка (H3).
	raw, ok := modelInfo["gemma4.context_length"]
	if !ok {
		t.Fatalf("H3: нет ключа gemma4.context_length в model_info=%v "+
			"(клиент как у настоящего Ollama не увидит контекст загруженной модели)", modelInfo)
	}
	gotCtxLen, ok := raw.(float64)
	if !ok {
		t.Fatalf("gemma4.context_length = %#v, want число", raw)
	}
	if int(gotCtxLen) != 32768 {
		t.Errorf("gemma4.context_length = %v, want 32768 (info.GGUFContextLength)", gotCtxLen)
	}
}

// TestR119_OllamaShowLoaded_NoContextLengthWithoutGGUF — если заголовок не
// прочитан (GGUFContextLength=0) или архитектура неизвестна, ключ
// "<arch>.context_length" не появляется: не выдумываем контекст и не пишем
// ключ ".context_length" с пустым префиксом.
func TestR119_OllamaShowLoaded_NoContextLengthWithoutGGUF(t *testing.T) {
	cases := []struct {
		name string
		info *cppbackend.ModelInfo
	}{
		{
			name: "нет GGUF context_length",
			info: &cppbackend.ModelInfo{
				Name: "gemma-4-E4B-it-Q4_K_M", Architecture: "gemma4",
				ContextSize: 8192, GGUFContextLength: 0,
			},
		},
		{
			name: "нет архитектуры",
			info: &cppbackend.ModelInfo{
				Name: "unknown-model", Architecture: "",
				ContextSize: 4096, GGUFContextLength: 32768,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeOllamaShowLoadedResponse(rec, tc.info)

			var body map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("ответ не JSON: %v", err)
			}
			modelInfo, _ := body["model_info"].(map[string]interface{})
			if _, ok := modelInfo[tc.info.Architecture+".context_length"]; ok {
				t.Errorf("ключ %q не должен появляться при GGUFContextLength=%d, arch=%q (model_info=%v)",
					tc.info.Architecture+".context_length", tc.info.GGUFContextLength, tc.info.Architecture, modelInfo)
			}
			if _, ok := modelInfo["context_size"]; !ok {
				t.Errorf("context_size обязан остаться в ответе (model_info=%v)", modelInfo)
			}
		})
	}
}
