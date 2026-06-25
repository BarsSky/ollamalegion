// Тесты для каскадного auto-fallback в cppworker (2026-06-25).
//
// Сценарий: при загрузке модели с большим num_ctx (например, n_ctx=65536 для
// gemma-4 на 8GB VRAM) каскад (RAM mmap → partial offload → cpu-only +
// auto_tune n_ctx) исчерпывается, и cppworker возвращает HTTP 413 с JSON,
// который содержит actionable details для клиента.
//
// Корневая причина: при n_ctx=65536 для gemma-4 4B:
//   - KV-cache в VRAM = 2560 * 42 * 4 * 65536 / 1024 / 1024 = 26 GB → не влезает
//   - Даже cpu-only (gpu_layers=0) с mmap весов в RAM может не влезть,
//     если mmap overhead + KV-cache > 25 GB доступной RAM.

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestInsufficientResourcesError_Error — проверяет форматирование текста ошибки.
func TestInsufficientResourcesError_Error(t *testing.T) {
	err := &InsufficientResourcesError{
		Model:              "gemma-4-E4B-it-Q4_K_M",
		RequestedNCtx:      65536,
		MaxViableNCtx:      32768,
		AvailableVRAMMB:    8192,
		AvailableRAMMB:     25600,
		ModelSizeBytes:     4987000000,
		KVCacheRequiredMB:  2700,
		GPULayersAttempted: 0,
	}
	msg := err.Error()
	want := []string{
		"gemma-4-E4B-it-Q4_K_M",
		"requested n_ctx=65536",
		"8192 MB VRAM",
		"25600 MB RAM",
		"Max viable n_ctx: 32768",
		"Reduce num_ctx",
	}
	for _, w := range want {
		if !contains(msg, w) {
			t.Errorf("Error() message missing %q: %s", w, msg)
		}
	}
}

// TestWriteInsufficientResourcesResponse_HasAllFieldsForClient —
// проверяет, что JSON содержит top-level code=6 и bridge_info с details,
// чтобы клиент (Cline/OpenWebUI) мог показать actionable сообщение.
func TestWriteInsufficientResourcesResponse_HasAllFieldsForClient(t *testing.T) {
	err := &InsufficientResourcesError{
		Model:              "gemma-4-E4B-it-Q4_K_M",
		RequestedNCtx:      65536,
		MaxViableNCtx:      32768,
		AvailableVRAMMB:    8192,
		AvailableRAMMB:     25600,
		ModelSizeBytes:     4987000000,
		KVCacheRequiredMB:  2700,
		GPULayersAttempted: 0,
	}
	rec := httptest.NewRecorder()
	writeInsufficientResourcesResponse(rec, err)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status code=%d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid JSON: %v body=%s", err, rec.Body.String())
	}

	// Top-level error code=6 (ErrCodeInsufficientResources) — клиент и балансер
	// должны видеть именно этот код.
	if got["error"] != "insufficient_resources" {
		t.Errorf("top-level error=%v, want %q", got["error"], "insufficient_resources")
	}
	if code, ok := got["code"].(float64); !ok || int(code) != 6 {
		t.Errorf("top-level code=%v, want 6", got["code"])
	}

	// bridge_info должен содержать все детали для клиента.
	bridgeInfo, ok := got["bridge_info"].(map[string]interface{})
	if !ok {
		t.Fatalf("bridge_info missing or wrong type: %T", got["bridge_info"])
	}
	wantInts := map[string]int{
		"requested_n_ctx":      65536,
		"max_viable_n_ctx":     32768,
		"available_vram_mb":    8192,
		"available_ram_mb":     25600,
		// model_size_mb: 4987000000 / 1024 / 1024 = 4755 (integer division).
		"model_size_mb":        4755,
		"kv_cache_required_mb": 2700,
		"gpu_layers_attempted": 0,
		"code":                 6,
	}
	for k, want := range wantInts {
		got, ok := bridgeInfo[k].(float64)
		if !ok {
			t.Errorf("bridge_info[%q] missing or not number: %v", k, bridgeInfo[k])
			continue
		}
		if int(got) != want {
			t.Errorf("bridge_info[%q]=%d, want %d", k, int(got), want)
		}
	}

	// Suggestion должен содержать max_viable_n_ctx как actionable подсказку.
	suggestion, ok := bridgeInfo["suggestion"].(string)
	if !ok || !contains(suggestion, "32768") {
		t.Errorf("bridge_info[suggestion]=%q, want to contain max viable n_ctx", suggestion)
	}
}

// TestHandleInferenceError_DispatchesInsufficientResources —
// handleInferenceError должен вызвать writeInsufficientResourcesResponse
// для *InsufficientResourcesError и вернуть true (handler должен return).
func TestHandleInferenceError_DispatchesInsufficientResources(t *testing.T) {
	irErr := &InsufficientResourcesError{
		Model:              "test-model",
		RequestedNCtx:      65536,
		MaxViableNCtx:      32768,
		AvailableVRAMMB:    8192,
		AvailableRAMMB:     25600,
		ModelSizeBytes:     5000000000,
		KVCacheRequiredMB:  2700,
		GPULayersAttempted: 0,
	}
	rec := httptest.NewRecorder()
	handled := handleInferenceError(rec, irErr)
	if !handled {
		t.Error("handleInferenceError should return true for *InsufficientResourcesError")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status code=%d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	// Проверяем, что top-level code = 6.
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	if code, ok := got["code"].(float64); !ok || int(code) != 6 {
		t.Errorf("top-level code=%v, want 6", got["code"])
	}
}

// TestHandleInferenceError_DispatchesAutoTuneError —
// handleInferenceError должен также транслировать *AutoTuneError в
// InsufficientResourcesResponse (это синоним недостатка ресурсов).
func TestHandleInferenceError_DispatchesAutoTuneError(t *testing.T) {
	atErr := &AutoTuneError{
		Model:          "test-model",
		RequestedNCtx:  65536,
		MaxViableNCtx:  32768,
		ModelSizeBytes: 5000000000,
		AvailableRAMMB: 25600,
	}
	rec := httptest.NewRecorder()
	handled := handleInferenceError(rec, atErr)
	if !handled {
		t.Error("handleInferenceError should return true for *AutoTuneError")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status code=%d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	if code, ok := got["code"].(float64); !ok || int(code) != 6 {
		t.Errorf("top-level code=%v, want 6 (ErrCodeInsufficientResources)", got["code"])
	}
}

// TestHandleInferenceError_GenericErrorReturnsFalse —
// для generic ошибок (не наши типы) handleInferenceError возвращает false,
// чтобы caller сам решил что делать.
func TestHandleInferenceError_GenericErrorReturnsFalse(t *testing.T) {
	rec := httptest.NewRecorder()
	handled := handleInferenceError(rec, errors.New("generic error"))
	if handled {
		t.Error("handleInferenceError should return false for generic error")
	}
	// rec.Code == 0 (по умолчанию), что подтверждает, что handler ничего не записал.
	if rec.Code != http.StatusOK {
		t.Errorf("status code=%d, want 0 (default)", rec.Code)
	}
}

// TestHandleInferenceError_NilErrorReturnsFalse — для nil ошибки возвращаем false.
func TestHandleInferenceError_NilErrorReturnsFalse(t *testing.T) {
	rec := httptest.NewRecorder()
	handled := handleInferenceError(rec, nil)
	if handled {
		t.Error("handleInferenceError(nil) should return false")
	}
}

// contains — локальный helper для substring check (избегаем strings.Contains
// shadowing в тестах).
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}