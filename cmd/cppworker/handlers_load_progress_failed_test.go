// handlers_load_progress_failed_test.go — R66d (2026-09-22).
//
// GET /api/models/load/progress?model=X должен отдавать state="failed" с
// настоящей причиной, если модель не загружена и не грузится, но загрузка
// недавно провалилась. До фикса здесь был 404 «model not found and not loading»,
// из-за чего балансер (и WebUI) не могли отличить провал от «ещё грузится».

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleLoadProgress_ReportsFailedLoad(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	const modelName = "gemma-4-E4B-it-Q4_K_M"
	loadFailures.clear(modelName)
	loadFailures.record(modelName, errors.New(
		"load model gemma-4-E4B-it-Q4_K_M: failed to load model from models/gemma-4-E4B-it-Q4_K_M.gguf"))
	defer loadFailures.clear(modelName)

	req := httptest.NewRequest(http.MethodGet, "/api/models/load/progress?model="+modelName, nil)
	rr := httptest.NewRecorder()
	handleLoadProgress(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (раньше был 404 и балансер поллил до maxWait)", rr.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, rr.Body.String())
	}
	if got := body["state"]; got != "failed" {
		t.Errorf("state = %v, want failed", got)
	}
	errText, _ := body["error"].(string)
	if !strings.Contains(errText, "gemma-4-E4B-it-Q4_K_M.gguf") {
		t.Errorf("error = %q, ожидался текст с именем файла", errText)
	}
	if body["failedAt"] == nil {
		t.Error("нет поля failedAt — UI не покажет, когда провал случился")
	}
}

func TestHandleLoadProgress_AllIncludesFailed(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	const modelName = "missing-model-for-list"
	loadFailures.clear(modelName)
	loadFailures.record(modelName, errors.New("failed to open GGUF file"))
	defer loadFailures.clear(modelName)

	req := httptest.NewRequest(http.MethodGet, "/api/models/load/progress", nil)
	rr := httptest.NewRecorder()
	handleLoadProgress(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body struct {
		Models []map[string]interface{} `json:"models"`
		Count  int                      `json:"count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}

	var found bool
	for _, m := range body.Models {
		if m["name"] == modelName {
			found = true
			if m["state"] != "failed" {
				t.Errorf("state для %s = %v, want failed", modelName, m["state"])
			}
		}
	}
	if !found {
		t.Errorf("в общем списке прогресса нет провалившейся модели %s (models=%v)", modelName, body.Models)
	}
}

// TestHandleLoadProgress_UnknownModelStill404 — если про модель вообще ничего
// не известно, поведение прежнее (404), чтобы не маскировать опечатки в имени.
func TestHandleLoadProgress_UnknownModelStill404(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	req := httptest.NewRequest(http.MethodGet, "/api/models/load/progress?model=never-heard-of-it", nil)
	rr := httptest.NewRecorder()
	handleLoadProgress(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}
