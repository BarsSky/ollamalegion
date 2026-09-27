// load_degraded_r83_test.go — R83 §9.4 шаг 1б (2026-09-26).
//
// Проверяет звено агента в связке cppworker → agent → webui для НОВОГО поля:
// модель загружена, но без GPU (cpu_only). Отдельное поле и отдельный тип нужны
// потому, что это не провал: смешивание с load_failure дало бы оператору
// уведомление «не хватило памяти» про работающую модель.
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR83_ToBackendLoadDegraded — конвертация и «деградации нет»: nil вместо
// пустого события, иначе балансер опубликует предупреждение на пустом месте.
func TestR83_ToBackendLoadDegraded(t *testing.T) {
	if got := toBackendLoadDegraded(nil); got != nil {
		t.Errorf("nil → %+v, want nil", got)
	}
	if got := toBackendLoadDegraded(&llamaLoadDegraded{}); got != nil {
		t.Errorf("пустая структура → %+v, want nil", got)
	}

	src := &llamaLoadDegraded{
		Model:  "qwen3.8:latest",
		Stage:  "cpu_only",
		Reason: "insufficient_resources",
		Detail: "weights_exceed_free_vram: веса не влезли",
		At:     "2026-09-26T12:00:00Z",
		Diagnostics: map[string]interface{}{
			"gpu_layers":      0,
			"usable_vram_mb":  1024,
			"usable_ram_mb":   20480,
			"requested_n_ctx": 32768,
		},
	}
	got := toBackendLoadDegraded(src)
	if got == nil {
		t.Fatal("полная структура → nil, want объект")
	}
	if got.Model != src.Model || got.Stage != src.Stage || got.Reason != src.Reason ||
		got.Detail != src.Detail || got.At != src.At {
		t.Errorf("поля потеряны: %+v", got)
	}
	if got.Diagnostics["gpu_layers"] != 0 {
		t.Errorf("диагностика не перенесена: %+v", got.Diagnostics)
	}
	// Ключ дедупликации обязан быть непустым: без него балансер не отличит
	// повторный push от новой деградации.
	if got.Key() == "" {
		t.Error("Key() пуст — дедупликация уведомлений о деградации сломана")
	}
	if got.Key() == (&types.LoadFailureInfo{Model: src.Model, Reason: src.Reason}).Key() {
		t.Error("ключ деградации совпал с ключом провала — события будут глушить друг друга")
	}
}

// TestR83_LlamaCollector_LoadDegraded — поле доезжает из /api/models в метрики
// бэкенда тем же опросом, что и load_failure (никаких новых запросов).
func TestR83_LlamaCollector_LoadDegraded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/models":
			_, _ = w.Write([]byte(`{
				"models":[],"count":0,
				"load_degraded":{
					"model":"qwen3.8:latest","stage":"cpu_only",
					"reason":"insufficient_resources","detail":"weights_exceed_free_vram",
					"at":"2026-09-26T12:00:00Z","severity":"warning",
					"diagnostics":{"gpu_layers":0,"usable_ram_mb":20480}
				}}`))
		case "/api/gpu":
			_, _ = w.Write([]byte(`{"gpuCount":0,"devices":[]}`))
		case "/api/info":
			_, _ = w.Write([]byte(`{"uptime":"1m","version":"test"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	lc := NewLlamaCollector(srv.URL)
	metrics, err := lc.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if metrics.LoadDegraded == nil {
		t.Fatal("load_degraded из /api/models не разобран — оператор не узнает режим")
	}
	if metrics.LoadDegraded.Stage != "cpu_only" {
		t.Errorf("Stage = %q, want cpu_only", metrics.LoadDegraded.Stage)
	}
	if metrics.LoadDegraded.Model != "qwen3.8:latest" {
		t.Errorf("Model = %q", metrics.LoadDegraded.Model)
	}

	base := toBackendLoadDegraded(metrics.LoadDegraded)
	if base == nil {
		t.Fatal("деградация не доехала до метрик бэкенда")
	}
	if base.Stage != "cpu_only" || base.Reason != "insufficient_resources" {
		t.Errorf("в метриках Stage/Reason = %q/%q", base.Stage, base.Reason)
	}
}

// TestR83_LlamaCollector_NoDegradedNoField — без деградации поля быть не должно,
// иначе каждый push агента порождал бы предупреждение о работающей модели.
func TestR83_LlamaCollector_NoDegradedNoField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/models":
			_, _ = w.Write([]byte(`{"models":[],"count":0}`))
		case "/api/gpu":
			_, _ = w.Write([]byte(`{"gpuCount":0,"devices":[]}`))
		case "/api/info":
			_, _ = w.Write([]byte(`{"uptime":"1m","version":"test"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	lc := NewLlamaCollector(srv.URL)
	metrics, err := lc.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if metrics.LoadDegraded != nil {
		t.Errorf("LoadDegraded = %+v, want nil", metrics.LoadDegraded)
	}
	if got := toBackendLoadDegraded(metrics.LoadDegraded); got != nil {
		t.Errorf("в метрики попала пустая деградация: %+v", got)
	}
}
