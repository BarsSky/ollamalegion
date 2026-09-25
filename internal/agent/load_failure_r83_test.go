// load_failure_r83_test.go — R83 (2026-09-25).
//
// Проверяет звено агента в связке cppworker → agent → webui: причина провала
// загрузки берётся из /api/models (который агент и так опрашивает) и уезжает в
// метриках бэкенда. Никаких дополнительных HTTP-запросов при этом не появляется —
// именно этого требовало ограничение «не нагружать балансер и не плодить опросы».
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR83_ToBackendLoadFailure — конвертация и, главное, поведение на «провала нет»:
// nil означает отсутствие уведомления, а не пустое событие.
func TestR83_ToBackendLoadFailure(t *testing.T) {
	if got := toBackendLoadFailure(nil); got != nil {
		t.Errorf("nil → %+v, want nil (иначе балансер опубликует ложное уведомление)", got)
	}
	if got := toBackendLoadFailure(&llamaLoadFailure{}); got != nil {
		t.Errorf("пустая структура → %+v, want nil", got)
	}

	src := &llamaLoadFailure{
		Model:    "qwen3.8:latest",
		Reason:   "config_out_of_bounds",
		Error:    "requested n_ctx=131072 exceeds ram-fallback-max-n-ctx=32768",
		At:       "2026-09-25T09:46:14Z",
		Severity: "error",
		Diagnostics: map[string]interface{}{
			"requested_n_ctx":      131072,
			"feasible_max_context": 32768,
		},
	}
	got := toBackendLoadFailure(src)
	if got == nil {
		t.Fatal("полная структура → nil, want объект")
	}
	if got.Model != src.Model || got.Reason != src.Reason || got.Error != src.Error ||
		got.At != src.At || got.Severity != src.Severity {
		t.Errorf("поля потеряны: %+v", got)
	}
	if got.Diagnostics["requested_n_ctx"] != 131072 {
		t.Errorf("диагностика не перенесена: %+v", got.Diagnostics)
	}
	// Ключ дедупликации должен быть непустым — иначе балансер не отличит
	// повторный push от нового провала.
	if got.Key() == "" {
		t.Error("Key() пуст — дедупликация уведомлений сломана")
	}
}

// TestR83_LlamaCollector_PicksUpLoadFailure — причина приходит из ТОГО ЖЕ
// /api/models, который агент опрашивает для списка моделей.
func TestR83_LlamaCollector_PicksUpLoadFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/models":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []interface{}{},
				"count":  0,
				"load_failure": map[string]interface{}{
					"model":    "qwen3.8:latest",
					"reason":   "model_not_found",
					"error":    "gguf_init_from_file: failed to open GGUF file models/qwen3.8:latest.gguf (No such file or directory)",
					"at":       "2026-09-25T09:46:14Z",
					"severity": "error",
					"diagnostics": map[string]interface{}{
						"attempted_path":   "models/qwen3.8:latest.gguf",
						"available_models": []interface{}{"Qwen3.8-27B-UD-Q4_K_M"},
					},
				},
			})
		case "/api/gpu":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"gpuCount": 0, "devices": []interface{}{}})
		case "/api/info":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"uptime": "1m", "version": "test"})
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
	if metrics.LoadFailure == nil {
		t.Fatal("load_failure из /api/models не разобран")
	}
	if metrics.LoadFailure.Reason != "model_not_found" {
		t.Errorf("Reason = %q, want model_not_found", metrics.LoadFailure.Reason)
	}
	if metrics.LoadFailure.Model != "qwen3.8:latest" {
		t.Errorf("Model = %q", metrics.LoadFailure.Model)
	}

	// И дальше — в метрики бэкенда, которые агент пушит балансеру.
	base := toBackendLoadFailure(metrics.LoadFailure)
	if base == nil {
		t.Fatal("причина не доехала до метрик бэкенда")
	}
	if base.Reason != "model_not_found" {
		t.Errorf("в метриках Reason = %q", base.Reason)
	}
	if base.Severity != string(types.SeverityError) {
		t.Errorf("Severity = %q, want error", base.Severity)
	}
}

// TestR83_LlamaCollector_NoFailureNoField — когда провалов нет, поля быть не
// должно (иначе балансер опубликует уведомление на пустом месте).
func TestR83_LlamaCollector_NoFailureNoField(t *testing.T) {
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
	if metrics.LoadFailure != nil {
		t.Errorf("LoadFailure = %+v, want nil", metrics.LoadFailure)
	}
	if got := toBackendLoadFailure(metrics.LoadFailure); got != nil {
		t.Errorf("toBackendLoadFailure(nil) = %+v, want nil", got)
	}
}
