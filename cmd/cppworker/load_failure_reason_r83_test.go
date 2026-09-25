//go:build llama_stub

// load_failure_reason_r83_test.go — R83 (2026-09-25).
//
// Проверяет машиночитаемую причину провала загрузки: без неё уведомление
// оператору («почему модель не загрузилась») построить нельзя — был только
// свободный текст llama.cpp.
//
// Живые тексты в таблице взяты из логов cppworker на A10.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestR83_ClassifyLoadFailureReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		text string
		want string
	}{
		{
			name: "живой кейс: файла нет",
			err:  errors.New("gguf_init_from_file: failed to open GGUF file models/qwen3.8:latest.gguf (No such file or directory)"),
			want: LoadFailureModelNotFound,
		},
		{
			name: "границы конфига: ram-fallback-max-n-ctx",
			err:  errors.New("requested n_ctx=131072 exceeds ram-fallback-max-n-ctx=32768"),
			want: LoadFailureConfigOutOfBounds,
		},
		{
			name: "границы конфига: отказ R83",
			err:  errors.New("requested n_ctx=65536 exceeds the physically feasible maximum 32768"),
			want: LoadFailureConfigOutOfBounds,
		},
		{
			name: "нехватка памяти по тексту",
			err:  errors.New("CUDA error: out of memory"),
			want: LoadFailureInsufficientResources,
		},
		{
			name: "нехватка памяти по типу",
			err: &InsufficientResourcesError{
				Model: "Qwen3.8-27B", RequestedNCtx: 65536, MaxViableNCtx: 32768,
			},
			want: LoadFailureInsufficientResources,
		},
		{
			name: "файл есть, но не принят llama.cpp",
			err:  errors.New("llama_model_load: error loading model: unknown architecture"),
			want: LoadFailureGGUFIncompatible,
		},
		{
			name: "бэкенд не готов",
			err:  errors.New("model manager not initialized"),
			want: LoadFailureBackendNotReady,
		},
		{
			name: "таймаут",
			err:  errors.New("context deadline exceeded"),
			want: LoadFailureTimeout,
		},
		{
			name: "битый алиас",
			err:  errors.New("alias source file is missing: /app/models/gone.gguf"),
			want: LoadFailureAliasBroken,
		},
		{
			name: "нераспознанное",
			err:  errors.New("something completely different"),
			want: LoadFailureUnknown,
		},
		{
			name: "nil-ошибка с текстом",
			err:  nil,
			text: "failed to open GGUF file x.gguf (No such file or directory)",
			want: LoadFailureModelNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyLoadFailureReason(tc.err, tc.text); got != tc.want {
				t.Errorf("classifyLoadFailureReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestR83_ClassificationOrder_ConfigBeatsModelNotFound — порядок проверок важен:
// llama.cpp в обоих случаях пишет «failed to load model», поэтому «exceeds ...»
// обязан распознаваться как границы конфига, а не как «файла нет».
func TestR83_ClassificationOrder_ConfigBeatsModelNotFound(t *testing.T) {
	err := errors.New("llama_model_load: failed to load model from models/x.gguf: requested n_ctx=131072 exceeds ram-fallback-max-n-ctx=32768")
	if got := classifyLoadFailureReason(err, err.Error()); got != LoadFailureConfigOutOfBounds {
		t.Errorf("получено %q, want %q (границы конфига важнее общего текста llama.cpp)",
			got, LoadFailureConfigOutOfBounds)
	}
}

func TestR83_LoadFailureSeverity(t *testing.T) {
	for reason, want := range map[string]string{
		LoadFailureConfigOutOfBounds:     "error",
		LoadFailureInsufficientResources: "error",
		LoadFailureModelNotFound:         "error",
		LoadFailureTimeout:               "warning",
		LoadFailureBackendNotReady:       "warning",
	} {
		if got := loadFailureSeverity(reason); got != want {
			t.Errorf("loadFailureSeverity(%q) = %q, want %q", reason, got, want)
		}
	}
}

// TestR83_Registry_RecordDetailedAutoClassifies — вызывающий может не знать код:
// reason="" означает «классифицируй по тексту».
func TestR83_Registry_RecordDetailedAutoClassifies(t *testing.T) {
	reg := newLoadFailureRegistry(time.Minute)
	reg.recordDetailed("Qwen3.8", "",
		errors.New("failed to open GGUF file /app/models/qwen3.8:latest.gguf (No such file or directory)"),
		map[string]interface{}{"attempted_path": "/app/models/qwen3.8:latest.gguf"})

	entry, ok := reg.get("Qwen3.8")
	if !ok {
		t.Fatal("запись о провале не найдена")
	}
	if entry.Reason != LoadFailureModelNotFound {
		t.Errorf("Reason = %q, want %q", entry.Reason, LoadFailureModelNotFound)
	}
	if entry.Diagnostics["attempted_path"] == nil {
		t.Error("диагностика потеряна — оператору нечего показать")
	}
}

func TestR83_Registry_LatestIsNewest(t *testing.T) {
	reg := newLoadFailureRegistry(time.Minute)
	base := time.Now()
	reg.now = func() time.Time { return base }
	reg.record("old-model", errors.New("no such file"))

	reg.now = func() time.Time { return base.Add(time.Second) }
	reg.recordDetailed("new-model", LoadFailureConfigOutOfBounds,
		errors.New("requested n_ctx=131072 exceeds ram-fallback-max-n-ctx=32768"), nil)

	latest, ok := reg.latest()
	if !ok {
		t.Fatal("latest() ничего не вернул")
	}
	if latest.Model != "new-model" {
		t.Errorf("latest().Model = %q, want new-model (самая свежая запись)", latest.Model)
	}
	if latest.Reason != LoadFailureConfigOutOfBounds {
		t.Errorf("latest().Reason = %q", latest.Reason)
	}
}

// TestR83_ModelsExposesLoadFailure — ключевое звено связки: agent забирает причину
// из /api/models (который и так опрашивает), поэтому новых запросов не появляется.
func TestR83_ModelsExposesLoadFailure(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	const model = "Qwen3.8-27B-UD-Q4_K_M"
	loadFailures.clear(model)
	loadFailures.recordDetailed(model, "",
		fmt.Errorf("failed to open GGUF file models/qwen3.8:latest.gguf (No such file or directory)"),
		map[string]interface{}{"attempted_path": "models/qwen3.8:latest.gguf"})
	defer loadFailures.clear(model)

	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	rec := httptest.NewRecorder()
	handleListModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}

	raw, ok := resp["load_failure"]
	if !ok {
		t.Fatalf("в /api/models нет поля load_failure — agent не сможет показать причину")
	}
	lf, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("load_failure не объект: %T", raw)
	}
	if got, _ := lf["reason"].(string); got != LoadFailureModelNotFound {
		t.Errorf("load_failure.reason = %q, want %q", got, LoadFailureModelNotFound)
	}
	if got, _ := lf["model"].(string); got != model {
		t.Errorf("load_failure.model = %q, want %q", got, model)
	}
	if got, _ := lf["severity"].(string); got != "error" {
		t.Errorf("load_failure.severity = %q, want error", got)
	}
	if got, _ := lf["error"].(string); got == "" {
		t.Error("load_failure.error пуст — сырой текст потерян")
	}
	if _, ok := lf["diagnostics"].(map[string]interface{}); !ok {
		t.Errorf("load_failure.diagnostics отсутствует или не объект: %v", lf["diagnostics"])
	}
	if got, _ := lf["at"].(string); got == "" {
		t.Error("load_failure.at пуст")
	}
}

// TestR83_ModelsOmitsLoadFailureWhenClean — без провалов поля быть не должно,
// иначе UI покажет уведомление на пустом месте.
func TestR83_ModelsOmitsLoadFailureWhenClean(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	// Реестр глобальный — чистим возможные остатки от других тестов.
	for name := range loadFailures.list() {
		loadFailures.clear(name)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	rec := httptest.NewRecorder()
	handleListModels(rec, req)

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := resp["load_failure"]; ok {
		t.Error("load_failure присутствует при отсутствии провалов")
	}
}
