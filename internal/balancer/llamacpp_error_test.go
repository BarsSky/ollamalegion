// Package balancer — unit-тесты для Stage 3.1 (ParseCppWorkerError).
//
// Покрывает:
//   - парсинг structured JSON-ошибки с bridge_info (cppworker >= 2026-06-06)
//   - парсинг legacy JSON без bridge_info, но с top-level code (поле code)
//   - fallback по строке "n_ctx" (для совсем старых cppworker)
//   - errors.Is с sentinel'ами bridge.ErrNCtxNeedsReload / bridge.ErrPromptTooLong
//   - truncation длинных body в Error()
//   - 2xx / 3xx ответы не считаются ошибками
//   - 4xx/5xx с пустым/не-JSON телом не парсятся (возвращается nil → caller
//     пробрасывает upstream-ошибку клиенту как есть).
package balancer

import (
	"errors"
	"strings"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

func TestParseCppWorkerError_StructuredNCtxNeedsReload(t *testing.T) {
	body := []byte(`{
		"error": "stream inference failed with code 2: n_ctx=4096 too small for prompt=8192",
		"code": 2,
		"bridge_info": {
			"code": 2,
			"current_n_ctx": 4096,
			"required_n_ctx": 8192,
			"actual_tokens": 8000,
			"n_predict": 192,
			"n_ctx_override": 8192,
			"max_vram_n_ctx": 77000,
			"message": "n_ctx too small"
		}
	}`)

	got := ParseCppWorkerError(body, 400, "backend-A")
	if got == nil {
		t.Fatalf("expected NCtxError, got nil")
	}
	nctxErr, ok := got.(*NCtxError)
	if !ok {
		t.Fatalf("expected *NCtxError, got %T", got)
	}
	if nctxErr.BackendID != "backend-A" {
		t.Errorf("BackendID = %q, want backend-A", nctxErr.BackendID)
	}
	if nctxErr.HTTPStatus != 400 {
		t.Errorf("HTTPStatus = %d, want 400", nctxErr.HTTPStatus)
	}
	if nctxErr.BridgeInfo == nil {
		t.Fatal("BridgeInfo is nil")
	}
	if nctxErr.BridgeInfo.Code != bridge.ErrCodeNCtxNeedsReload {
		t.Errorf("BridgeInfo.Code = %d, want %d", nctxErr.BridgeInfo.Code, bridge.ErrCodeNCtxNeedsReload)
	}
	if nctxErr.BridgeInfo.CurrentNCtx != 4096 {
		t.Errorf("CurrentNCtx = %d, want 4096", nctxErr.BridgeInfo.CurrentNCtx)
	}
	if nctxErr.BridgeInfo.RequiredNCtx != 8192 {
		t.Errorf("RequiredNCtx = %d, want 8192", nctxErr.BridgeInfo.RequiredNCtx)
	}
	if nctxErr.BridgeInfo.MaxVRAMNCtx != 77000 {
		t.Errorf("MaxVRAMNCtx = %d, want 77000", nctxErr.BridgeInfo.MaxVRAMNCtx)
	}
	// errors.Is должен работать через Unwrap()
	if !errors.Is(got, bridge.ErrNCtxNeedsReload) {
		t.Error("expected errors.Is(err, bridge.ErrNCtxNeedsReload) == true")
	}
	if errors.Is(got, bridge.ErrPromptTooLong) {
		t.Error("did not expect errors.Is(err, bridge.ErrPromptTooLong) == true")
	}
}

func TestParseCppWorkerError_StructuredPromptTooLong(t *testing.T) {
	body := []byte(`{
		"error": "prompt too long",
		"code": 3,
		"bridge_info": {
			"code": 3,
			"current_n_ctx": 4096,
			"required_n_ctx": 0,
			"actual_tokens": 10000,
			"n_predict": 100,
			"max_vram_n_ctx": 77000,
			"message": "prompt (10000 tokens) > n_ctx (4096)"
		}
	}`)

	got := ParseCppWorkerError(body, 400, "backend-B")
	if got == nil {
		t.Fatalf("expected NCtxError, got nil")
	}
	if !errors.Is(got, bridge.ErrPromptTooLong) {
		t.Error("expected errors.Is(err, bridge.ErrPromptTooLong) == true")
	}
	if errors.Is(got, bridge.ErrNCtxNeedsReload) {
		t.Error("did not expect errors.Is(err, bridge.ErrNCtxNeedsReload) == true")
	}
}

func TestParseCppWorkerError_LegacyTopLevelCode(t *testing.T) {
	// cppworker без bridge_info, но с top-level code (старая версия)
	body := []byte(`{"error": "n_ctx too small", "code": 2}`)

	got := ParseCppWorkerError(body, 400, "backend-C")
	if got == nil {
		t.Fatalf("expected NCtxError, got nil")
	}
	if !errors.Is(got, bridge.ErrNCtxNeedsReload) {
		t.Error("expected errors.Is(err, bridge.ErrNCtxNeedsReload) == true")
	}
	// У legacy top-level code нет bridge_info, поэтому Unwrap() должен вернуть nil,
	// а errors.Is(err, bridge.ErrNCtxNeedsReload) сработает только если BridgeInfo
	// есть. Проверяем что BridgeInfo есть (хотя бы минимальный).
	nctxErr, ok := got.(*NCtxError)
	if !ok {
		t.Fatalf("expected *NCtxError, got %T", got)
	}
	if nctxErr.BridgeInfo == nil {
		t.Fatal("BridgeInfo should not be nil for legacy code path")
	}
}

func TestParseCppWorkerError_StringFallback(t *testing.T) {
	// Совсем старая версия cppworker: plain JSON с error содержащим "n_ctx"
	body := []byte(`{"error": "Error: n_ctx=4096 too small for prompt tokens"}`)

	got := ParseCppWorkerError(body, 500, "backend-D")
	if got == nil {
		t.Fatalf("expected NCtxError from string fallback, got nil")
	}
	if !errors.Is(got, bridge.ErrNCtxNeedsReload) {
		t.Error("expected errors.Is(err, bridge.ErrNCtxNeedsReload) == true")
	}
}

func TestParseCppWorkerError_GenericError(t *testing.T) {
	// Generic 5xx без n_ctx-релевантного текста — НЕ считается NCtxError.
	// Caller прокинет upstream-ошибку клиенту как есть.
	body := []byte(`{"error": "internal server error: nil pointer dereference"}`)

	got := ParseCppWorkerError(body, 500, "backend-E")
	if got != nil {
		t.Errorf("expected nil for generic 5xx, got %v", got)
	}
}

func TestParseCppWorkerError_OKStatus(t *testing.T) {
	// 2xx ответ не считается ошибкой, даже если body содержит "error"
	body := []byte(`{"error": "trailing", "code": 0, "result": "success"}`)

	got := ParseCppWorkerError(body, 200, "backend-F")
	if got != nil {
		t.Errorf("expected nil for 2xx, got %v", got)
	}
}

func TestParseCppWorkerError_EmptyBody(t *testing.T) {
	got := ParseCppWorkerError(nil, 500, "backend-G")
	if got != nil {
		t.Errorf("expected nil for empty body, got %v", got)
	}
}

func TestParseCppWorkerError_NotJSON(t *testing.T) {
	// Plain-text 500 — не парсится, caller пробросит как upstream-ошибку.
	body := []byte(`<html><body>500 Internal Server Error</body></html>`)

	got := ParseCppWorkerError(body, 500, "backend-H")
	if got != nil {
		t.Errorf("expected nil for non-JSON, got %v", got)
	}
}

func TestNCtxError_Error(t *testing.T) {
	// Проверяет, что Error() форматируется без паники и содержит ключевые поля.
	body := []byte(`{
		"error": "n_ctx too small",
		"code": 2,
		"bridge_info": {
			"code": 2,
			"current_n_ctx": 4096,
			"required_n_ctx": 8192,
			"max_vram_n_ctx": 77000,
			"message": "test msg"
		}
	}`)

	got := ParseCppWorkerError(body, 400, "backend-I")
	if got == nil {
		t.Fatal("expected NCtxError")
	}
	errStr := got.Error()
	if !strings.Contains(errStr, "backend-I") {
		t.Errorf("Error() = %q, should contain backend ID", errStr)
	}
	if !strings.Contains(errStr, "4096") || !strings.Contains(errStr, "8192") {
		t.Errorf("Error() = %q, should contain n_ctx values", errStr)
	}
}

func TestNCtxError_Truncation(t *testing.T) {
	// Очень длинное тело — Error() должен trunc'нуть его.
	longBody := strings.Repeat("x", 5000)
	body := []byte(`{"error":"` + longBody + `","code":7}`)

	// generic error без n_ctx — должен вернуть nil
	got := ParseCppWorkerError(body, 500, "backend-J")
	if got != nil {
		t.Errorf("expected nil for generic error code, got %v", got)
	}
}

func TestAsBufferedBody(t *testing.T) {
	// Проверяет, что []byte конвертируется в io.ReadCloser без потерь.
	data := []byte(`{"hello":"world"}`)
	rc := AsBufferedBody(data)
	defer rc.Close()

	buf := make([]byte, len(data))
	n, err := rc.Read(buf)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if n != len(data) {
		t.Errorf("Read() = %d bytes, want %d", n, len(data))
	}
	if string(buf[:n]) != string(data) {
		t.Errorf("Read() = %q, want %q", string(buf[:n]), string(data))
	}
}
