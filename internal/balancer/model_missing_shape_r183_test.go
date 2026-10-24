//go:build llama_stub

// model_missing_shape_r183_test.go — R183 (2026-10-19).
//
// НАЙДЕНО матрицей ошибок R180: на OpenAI-поверхности `/v1/*` ошибка «модели нет»
// уходила в ollama-форме — `{"error": "<строка>"}`. OpenAI-клиенты (deepseek
// harness, hermes, openai-python/js) читают `error.message`, поэтому текст причины
// терялся. Статус и текст были верны — неверна была ФОРМА.
//
// Здесь фиксируется контракт: /v1/* -> конверт OpenAI, /api/* -> нативная форма
// Ollama (её читают Cline/OpenWebUI), и оба несут код/модель/доступные модели.
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// decodeModelMissingK183 — разобрать тело 404 и вернуть (status, объект).
func decodeModelMissingK183(t *testing.T, path string) (int, map[string]interface{}) {
	t.Helper()
	rec := httptest.NewRecorder()
	writeModelMissingResponseFor(rec, path, "no-such-model", "не найден в ./models",
		[]string{"gemma-4-E4B-it-Q4_K_M", "Qwen3.8-27B-UD-Q4_K_M"})
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON (%s): %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

// TestR183_OpenAISurface_HasErrorEnvelope — на /v1/* ошибка в конверте OpenAI.
func TestR183_OpenAISurface_HasErrorEnvelope(t *testing.T) {
	code, body := decodeModelMissingK183(t, "/v1/chat/completions")
	if code != http.StatusNotFound {
		t.Fatalf("код %d, want 404", code)
	}
	env, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error должен быть ОБЪЕКТОМ OpenAI (%T): %v — клиент не прочитает error.message",
			body["error"], body["error"])
	}
	msg, _ := env["message"].(string)
	if msg == "" {
		t.Error("error.message пуст — клиент не увидит причину")
	}
	if env["code"] != "model_not_found" {
		t.Errorf("error.code = %v, want model_not_found", env["code"])
	}
	if env["type"] != "invalid_request_error" {
		t.Errorf("error.type = %v, want invalid_request_error", env["type"])
	}
	if env["param"] != "model" {
		t.Errorf("error.param = %v, want model", env["param"])
	}
	// Дополнительные поля не теряются: по ним UI показывает список моделей.
	if body["code"] != "model_not_found" {
		t.Errorf("верхний code = %v, want model_not_found", body["code"])
	}
	if _, ok := body["availableModels"]; !ok {
		t.Error("потерян availableModels — UI не покажет, что доступно")
	}
}

// TestR183_OllamaSurface_KeepsStringError — на /api/* форма прежняя: error строкой.
func TestR183_OllamaSurface_KeepsStringError(t *testing.T) {
	code, body := decodeModelMissingK183(t, "/api/chat")
	if code != http.StatusNotFound {
		t.Fatalf("код %d, want 404", code)
	}
	msg, ok := body["error"].(string)
	if !ok {
		t.Fatalf("на нативной поверхности error должен быть СТРОКОЙ (%T) — Cline/OpenWebUI "+
			"читают именно её", body["error"])
	}
	if msg == "" {
		t.Error("error пуст")
	}
}

// TestR183_IsOpenAISurface — какие пути считаются OpenAI-поверхностью.
func TestR183_IsOpenAISurface(t *testing.T) {
	cases := map[string]bool{
		"/v1/chat/completions": true,
		"/v1/completions":      true,
		"/v1/embeddings":       true,
		"/v1/models":           true,
		"/api/chat":            false,
		"/api/generate":        false,
		"/ollama/api/chat":     false,
		"":                     false,
	}
	for path, want := range cases {
		if got := isOpenAISurface(path); got != want {
			t.Errorf("isOpenAISurface(%q) = %v, want %v", path, got, want)
		}
	}
}
