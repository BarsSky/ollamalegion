// llama_cpp_metrics_loading_error_r345_test.go — R345 (2026-10-30).
//
// ЖИВОЙ ДЕФЕКТ (аудит docs/plans/2026-10-30-abort-and-metrics-audit.md, M1).
// Балансер сериализовал причину провала загрузки модели как `loadingError`, а
// WebUI читает `error` (`gguf-load-progress.js:103,107,136,295`). Поэтому причина
// НИКОГДА не показывалась: пользователь видел заглушку «error» и не мог понять,
// что именно не так (не влезает в VRAM, битый файл, нет памяти).
//
// Тест фиксирует имя поля в JSON: `error` (и отсутствие второго имени, чтобы не
// завести два источника правды).
package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR345_LoadingErrorSerializedAsError — имя поля совпадает с тем, что читает WebUI.
func TestR345_LoadingErrorSerializedAsError(t *testing.T) {
	m := LlamaCppModel{
		Name:         "gemma",
		State:        "error",
		LoadingError: "model does not fit in VRAM: needs 6.2 GB, free 5.1 GB",
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, `"error":"model does not fit in VRAM`) {
		t.Errorf("причина провала загрузки не сериализуется как error — WebUI её не увидит: %s", text)
	}
	if strings.Contains(text, `"loadingError"`) {
		t.Errorf("осталось второе имя loadingError: два источника правды разойдутся: %s", text)
	}
}

// TestR345_EmptyLoadingErrorOmitted — без ошибки поле не появляется вовсе.
func TestR345_EmptyLoadingErrorOmitted(t *testing.T) {
	raw, err := json.Marshal(LlamaCppModel{Name: "m", State: "loaded"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"error"`) {
		t.Errorf("пустая причина сериализуется как null/пусто — клиент покажет ложную ошибку: %s", string(raw))
	}
}
