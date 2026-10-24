//go:build llama_stub

// abort_load_r215_test.go — R215 (2026-10-21).
//
// До R215 отменить идущую загрузку было НЕВОЗМОЖНО: единственный вызывающий
// bridge.RequestLoadAbort передавал context.Background(), флаг отмены не
// выставлялся никогда, а `POST /api/models/unload?force=true` во время загрузки
// означал «дождаться конца и выгрузить». На крупной модели веса успевали лечь на
// GPU мастера и узлов раскладки, и VRAM держалась всё время чтения файла.
//
// Теперь force-выгрузка грузящейся модели ставит флаг, progress-callback в
// C-мосте возвращает false, llama.cpp останавливает чтение и освобождает
// частично загруженную модель. Тест фиксирует вход в это поведение:
// отменять можно ТОЛЬКО грузящуюся модель.
package cppbackend

import (
	"testing"

	"ollama-loadbalancer/c/bridge"
)

func r215BackendWithModels() *Backend {
	return &Backend{
		models:  map[string]*modelInstance{},
		loading: map[string]chan struct{}{},
	}
}

func TestR215_AbortLoad_OnlyLoadingModels(t *testing.T) {
	b := r215BackendWithModels()

	// Неизвестная модель: отменять нечего.
	if b.AbortLoad("missing") {
		t.Error("AbortLoad(unknown) = true, want false")
	}

	// Загруженная модель: отмена загрузки не имеет смысла (её надо выгружать).
	b.models["loaded"] = &modelInstance{
		info:   ModelInfo{Name: "loaded", State: StateLoaded},
		handle: &bridge.ModelHandle{},
	}
	if b.AbortLoad("loaded") {
		t.Error("AbortLoad(loaded) = true, want false: загруженную модель выгружают, а не отменяют загрузку")
	}

	// Грузящаяся модель: флаг выставляется.
	b.models["loading"] = &modelInstance{
		info:   ModelInfo{Name: "loading", State: StateLoading},
		handle: &bridge.ModelHandle{},
	}
	if !b.AbortLoad("loading") {
		t.Error("AbortLoad(loading) = false, want true: force-выгрузка обязана отменять загрузку")
	}
}

func TestR215_AbortLoad_NilBackendSafe(t *testing.T) {
	var b *Backend
	if b.AbortLoad("m") {
		t.Error("nil backend: AbortLoad = true, want false")
	}
}
