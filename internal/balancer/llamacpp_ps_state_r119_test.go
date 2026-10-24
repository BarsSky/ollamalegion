//go:build llama_stub

// R119: /api/ps не должен показывать loading-модель как running.
//
// Живой дефект (2026-10-15, стенд): cppworker сообщал state="loading",
// context_size=0, а клиент (OpenWebUI, `ollama ps`, ollama-python) видел модель
// в списке запущенных — с size и expires_at в будущем, то есть «всё готово».
// Ollama в /api/ps перечисляет только модели, ЗАНЯВШИЕ память и обслуживающие
// запросы; loading-модель ещё не обслуживает никого.
//
// Тест падает до правки: handlePS отдавал обе записи.
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR119_HandlePS_ExcludesLoadingModel — в /api/ps только loaded-модель.
func TestR119_HandlePS_ExcludesLoadingModel(t *testing.T) {
	proxy, _ := buildProxyWithStub(t, nil, []types.LlamaCppModel{
		{
			Name:          "m-loaded",
			Path:          "/app/models/m-loaded.gguf",
			Size:          4_000_000_000,
			State:         "loaded",
			ContextLength: 16384,
			LoadedAt:      time.Now().UTC().Format(time.RFC3339),
		},
		{
			// Именно так это выглядит живьём в момент загрузки: state=loading,
			// context_size=0, size уже известен из файла.
			Name:             "m-loading",
			Path:             "/app/models/m-loading.gguf",
			Size:             16_000_000_000,
			State:            "loading",
			ContextLength:    0,
			LoadingSizeBytes: 16_000_000_000,
		},
	})

	resp := callHandlePS(t, proxy)
	if len(resp.Models) != 1 {
		names := make([]string, 0, len(resp.Models))
		for _, m := range resp.Models {
			names = append(names, m.Name)
		}
		t.Fatalf("handlePS: моделей = %d %v, ожидалась только m-loaded "+
			"(loading-модель не обслуживает запросы, показывать её как running нельзя)", len(resp.Models), names)
	}
	if resp.Models[0].Name != "m-loaded" {
		t.Errorf("handlePS: вернулась %q, ожидалась m-loaded", resp.Models[0].Name)
	}
	if !resp.Models[0].ExpiresAt.After(time.Now()) {
		t.Errorf("expires_at = %s — в прошлом для живой модели",
			resp.Models[0].ExpiresAt.Format(time.RFC3339))
	}
}

// Контроль: пустой state (старые сборки cppworker) не выкидывает модель из
// /api/ps — иначе клиент потеряет реально загруженную модель.
func TestR119_HandlePS_EmptyStateStaysVisible(t *testing.T) {
	proxy, _ := buildProxyWithStub(t, nil, []types.LlamaCppModel{
		{Name: "m-nostate", Path: "/app/models/m-nostate.gguf", Size: 1_000_000_000},
	})

	resp := callHandlePS(t, proxy)
	if len(resp.Models) != 1 {
		t.Fatalf("handlePS: моделей = %d, ожидалась 1 (пустой state = loaded)", len(resp.Models))
	}
}
