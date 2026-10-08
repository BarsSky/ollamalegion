// utils_hint_r88_test.go — R88 (2026-10-08).
//
// ТРЕБОВАНИЕ ОПЕРАТОРА (дословно): «отсутствие таймаутов и явное объяснение
// ошибок». Таймауты сняты по доктрине (internal/sdbackend/timeout_policy.go),
// а это — вторая половина: КАЖДАЯ ошибка воркера обязана нести не только код,
// но и «что делать». До R88 клиент видел `generation_timeout` и шёл читать
// документацию; теперь объяснение приходит в теле ответа полем hint.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHintForCode_MainFailureModesExplained — подсказка есть у всех кодов,
// которые клиент реально получает в ответ на генерацию/загрузку.
func TestHintForCode_MainFailureModesExplained(t *testing.T) {
	codes := []string{
		"generation_timeout", "queue_full", "engine_queue_full",
		"model_not_found", "model_not_loaded", "model_loading",
		"cannot_cancel_generating", "sd_server_startup_failed",
		"sd_server_incompatible", "job_not_found", "job_gone",
		"generation_failed", "engine_job_failed",
		"invalid_generation_params", "invalid_size", "payload_too_large",
		"image_required", "init_images_required", "invalid_image", "invalid_mask",
		"prompt_required", "invalid_json", "invalid_content_type",
		"invalid_multipart", "method_not_allowed", "not_implemented",
	}
	for _, code := range codes {
		h := hintForCode(code)
		if h == "" {
			t.Errorf("для кода %q нет подсказки «что делать»", code)
			continue
		}
		if len(h) < 20 {
			t.Errorf("подсказка для %q слишком куцая (%q) — она должна объяснять действие", code, h)
		}
	}
}

// TestHintForCode_UnknownCodeGetsNoInventedHint — для неизвестного кода
// подсказку НЕ выдумываем: иначе hint перестаёт означать конкретное действие.
func TestHintForCode_UnknownCodeGetsNoInventedHint(t *testing.T) {
	if got := hintForCode("какой-то-новый-код-из-будущего"); got != "" {
		t.Errorf("для неизвестного кода подсказка выдумана: %q", got)
	}
}

// TestHintForTimeoutNamesTheCapVariable — при обрыве по капу оператор должен
// сразу видеть, КАКУЮ переменную снять (иначе «почему оборвалось» снова загадка).
func TestHintForTimeoutNamesTheCapVariable(t *testing.T) {
	h := hintForCode("generation_timeout")
	for _, want := range []string{"SDWORKER_GENERATION_TIMEOUT_SEC", "profile.timeoutSec", "терминального состояния"} {
		if !strings.Contains(h, want) {
			t.Errorf("подсказка про generation_timeout не содержит %q: %s", want, h)
		}
	}
}

// TestWriteJSONError_CarriesHint — нативный контракт (/api/image/*, /sdapi/*).
func TestWriteJSONError_CarriesHint(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSONError(rec, http.StatusGatewayTimeout, "generation_timeout", "generation timed out")

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	if got, _ := body["code"].(string); got != "generation_timeout" {
		t.Errorf("code = %q, want generation_timeout", got)
	}
	if got, _ := body["error"].(string); got != "generation timed out" {
		t.Errorf("error = %q", got)
	}
	if got, _ := body["hint"].(string); got == "" {
		t.Error("нет hint — клиент получит код без объяснения")
	}
}

// TestWriteOpenAIError_HintInsideEnvelopeAndAtTop — OpenAI-клиенты читают
// error.message; hint кладём И внутрь конверта, И на верхний уровень, чтобы его
// нашли и SDK-совместимые клиенты, и наш WebUI.
func TestWriteOpenAIError_HintInsideEnvelopeAndAtTop(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOpenAIError(rec, http.StatusConflict, "model_not_loaded", "no image model is loaded")

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	if got, _ := body["hint"].(string); got == "" {
		t.Error("нет hint на верхнем уровне")
	}
	inner, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("нет конверта error: %s", rec.Body.String())
	}
	if got, _ := inner["code"].(string); got != "model_not_loaded" {
		t.Errorf("error.code = %q", got)
	}
	if got, _ := inner["hint"].(string); got == "" {
		t.Error("нет hint внутри конверта error")
	}
	// Классификация типа ошибки по статусу не должна пострадать от правки:
	// 409 = конфликт состояния, а не «invalid_request_error».
	if got, _ := inner["type"].(string); got != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error", got)
	}
}

// TestWriteOpenAIError_ServerErrorTypePreserved — 5xx остаётся server_error.
func TestWriteOpenAIError_ServerErrorTypePreserved(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOpenAIError(rec, http.StatusInternalServerError, "generation_failed", "boom")

	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	inner, _ := body["error"].(map[string]any)
	if got, _ := inner["type"].(string); got != "server_error" {
		t.Errorf("error.type = %q, want server_error", got)
	}
}
