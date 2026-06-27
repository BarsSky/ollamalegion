package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleLoadWithParams_MethodNotAllowed — GET вместо POST → 405.
func TestHandleLoadWithParams_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/models/load-with-params", nil)
	w := httptest.NewRecorder()

	handleLoadWithParams(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 Method Not Allowed, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestHandleLoadWithParams_InvalidJSON — POST с некорректным JSON → 400.
func TestHandleLoadWithParams_InvalidJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/models/load-with-params",
		strings.NewReader("{invalid json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handleLoadWithParams(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid JSON") {
		t.Errorf("expected error message about invalid JSON, got: %s", w.Body.String())
	}
}

// TestHandleLoadWithParams_MissingName — POST без name → 400.
func TestHandleLoadWithParams_MissingName(t *testing.T) {
	body := strings.NewReader(`{"contextSize": 32768}`)
	req := httptest.NewRequest(http.MethodPost, "/api/models/load-with-params", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handleLoadWithParams(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "name is required") {
		t.Errorf("expected error message about name, got: %s", w.Body.String())
	}
}

// TestHandleLoadWithParams_RequestStructure — проверяет, что loadWithParamsRequest
// корректно парсит ВСЕ поля (базовые + расширенные) и не падает на пустых
// опциональных значениях (omitempty semantics).
func TestHandleLoadWithParams_RequestStructure(t *testing.T) {
	body := `{
		"name": "gemma-4-E4B-it-Q4_K_M",
		"path": "/models/gemma-4-E4B-it-Q4_K_M.gguf",
		"gpuLayers": 30,
		"contextSize": 32768,
		"batchSize": 512,
		"tensorSplit": [0.5, 0.5],
		"flashAttn": 1,
		"numa": true,
		"useMmap": false,
		"nThreads": 16,
		"parallel": 2,
		"kvCacheType": 1,
		"splitMode": 0,
		"overrideTensor": "blk\\..*\\.ffn_.*_exps=CPU"
	}`

	var req loadWithParamsRequest
	if err := json.NewDecoder(bytes.NewReader([]byte(body))).Decode(&req); err != nil {
		t.Fatalf("failed to decode request: %v", err)
	}

	// Базовые поля
	if req.Name != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("Name: got %q, want gemma-4-E4B-it-Q4_K_M", req.Name)
	}
	if req.Path != "/models/gemma-4-E4B-it-Q4_K_M.gguf" {
		t.Errorf("Path: got %q", req.Path)
	}
	if req.GPULayers == nil || *req.GPULayers != 30 {
		t.Errorf("GPULayers: got %v, want 30", req.GPULayers)
	}
	if req.ContextSize == nil || *req.ContextSize != 32768 {
		t.Errorf("ContextSize: got %v, want 32768", req.ContextSize)
	}
	if req.BatchSize == nil || *req.BatchSize != 512 {
		t.Errorf("BatchSize: got %v, want 512", req.BatchSize)
	}
	if len(req.TensorSplit) != 2 || req.TensorSplit[0] != 0.5 {
		t.Errorf("TensorSplit: got %v", req.TensorSplit)
	}
	if req.FlashAttnType == nil || *req.FlashAttnType != 1 {
		t.Errorf("FlashAttnType: got %v, want 1", req.FlashAttnType)
	}
	if req.NUMA == nil || !*req.NUMA {
		t.Errorf("NUMA: got %v, want true", req.NUMA)
	}
	if req.UseMmap == nil || *req.UseMmap {
		t.Errorf("UseMmap: got %v, want false", req.UseMmap)
	}

	// Расширенные поля (Session 4 — P-1)
	if req.NThreads == nil || *req.NThreads != 16 {
		t.Errorf("NThreads: got %v, want 16", req.NThreads)
	}
	if req.Parallel == nil || *req.Parallel != 2 {
		t.Errorf("Parallel: got %v, want 2", req.Parallel)
	}
	if req.KVCacheType == nil || *req.KVCacheType != 1 {
		t.Errorf("KVCacheType: got %v, want 1 (Q8_0)", req.KVCacheType)
	}
	if req.SplitMode == nil || *req.SplitMode != 0 {
		t.Errorf("SplitMode: got %v, want 0 (layer)", req.SplitMode)
	}
	if req.OverrideTensor == nil || *req.OverrideTensor != "blk\\..*\\.ffn_.*_exps=CPU" {
		t.Errorf("OverrideTensor: got %v", req.OverrideTensor)
	}
}

// TestHandleLoadWithParams_MinimalRequest_OnlyValidation — проверяет валидацию
// (HTTP-уровень: name != "", method == POST) для минимального запроса.
// Не вызывает реальный backend (требует инициализированную global `backend`),
// а проверяет только парсинг JSON и базовую валидацию полей через Decode.
// Endpoint handler ожидает name != "" — это уже покрыто в RequestStructure.
func TestHandleLoadWithParams_MinimalRequest_OnlyValidation(t *testing.T) {
	// Парсинг минимального JSON
	body := `{"name": "test-model"}`
	var req loadWithParamsRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("failed to decode minimal request: %v", err)
	}
	if req.Name != "test-model" {
		t.Errorf("Name: got %q", req.Name)
	}
	// Все остальные поля должны быть nil (omitempty)
	if req.GPULayers != nil {
		t.Errorf("GPULayers should be nil, got %v", *req.GPULayers)
	}
	if req.ContextSize != nil {
		t.Errorf("ContextSize should be nil, got %v", *req.ContextSize)
	}
	if req.NThreads != nil {
		t.Errorf("NThreads should be nil, got %v", *req.NThreads)
	}
	if req.KVCacheType != nil {
		t.Errorf("KVCacheType should be nil, got %v", *req.KVCacheType)
	}
}

// TestHandleLoadWithParams_BackwardCompatibility — все поля из
// loadModelRequest должны парситься в loadWithParamsRequest без изменений
// (обратная совместимость).
func TestHandleLoadWithParams_BackwardCompatibility(t *testing.T) {
	// Старый формат с базовыми полями
	body := `{
		"name": "test-model",
		"contextSize": 32768,
		"gpuLayers": 30,
		"batchSize": 512,
		"flashAttn": 1,
		"numa": true,
		"useMmap": true
	}`

	var req loadWithParamsRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("failed to decode backward-compatible request: %v", err)
	}

	if req.Name != "test-model" {
		t.Errorf("Name: got %q", req.Name)
	}
	if req.ContextSize == nil || *req.ContextSize != 32768 {
		t.Errorf("ContextSize: got %v", req.ContextSize)
	}
	// Расширенные поля остаются nil (omitempty → не сериализуются, не парсятся).
	if req.NThreads != nil {
		t.Errorf("NThreads should be nil for old format, got %v", req.NThreads)
	}
	if req.OverrideTensor != nil {
		t.Errorf("OverrideTensor should be nil for old format, got %v", req.OverrideTensor)
	}
}

// TestHandleLoadWithParams_KVCacheTypeValidValues — проверяет, что структура
// принимает все валидные значения kvCacheType (0=F16, 1=Q8_0, 2=Q4_0).
func TestHandleLoadWithParams_KVCacheTypeValidValues(t *testing.T) {
	for _, kvType := range []int{0, 1, 2} {
		body := `{"name":"test","kvCacheType":` + intToStr(kvType) + `}`
		var req loadWithParamsRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Errorf("kvCacheType=%d: failed to decode: %v", kvType, err)
			continue
		}
		if req.KVCacheType == nil || *req.KVCacheType != kvType {
			t.Errorf("kvCacheType=%d: got %v", kvType, req.KVCacheType)
		}
	}
}

// TestHandleLoadWithParams_SplitModeValidValues — проверяет splitMode (0=layer, 1=row).
func TestHandleLoadWithParams_SplitModeValidValues(t *testing.T) {
	for _, sm := range []int{0, 1} {
		body := `{"name":"test","splitMode":` + intToStr(sm) + `}`
		var req loadWithParamsRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Errorf("splitMode=%d: failed to decode: %v", sm, err)
			continue
		}
		if req.SplitMode == nil || *req.SplitMode != sm {
			t.Errorf("splitMode=%d: got %v", sm, req.SplitMode)
		}
	}
}

// TestHandleLoadWithParams_NegativeValuesRejectedByHandler — handler
// принимает отрицательные значения (валидация в checkVRAMForModel/cppworker).
// Тест проверяет только парсинг, не runtime-валидацию.
func TestHandleLoadWithParams_NegativeValuesParse(t *testing.T) {
	body := `{"name":"test","gpuLayers":-1,"kvCacheType":-1,"splitMode":-1,"nThreads":-1,"parallel":-1}`
	var req loadWithParamsRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("failed to decode request with negative values: %v", err)
	}

	if req.GPULayers == nil || *req.GPULayers != -1 {
		t.Errorf("GPULayers: got %v", req.GPULayers)
	}
	if req.KVCacheType == nil || *req.KVCacheType != -1 {
		t.Errorf("KVCacheType: got %v", req.KVCacheType)
	}
	if req.SplitMode == nil || *req.SplitMode != -1 {
		t.Errorf("SplitMode: got %v", req.SplitMode)
	}
	if req.NThreads == nil || *req.NThreads != -1 {
		t.Errorf("NThreads: got %v", req.NThreads)
	}
	if req.Parallel == nil || *req.Parallel != -1 {
		t.Errorf("Parallel: got %v", req.Parallel)
	}
}

// TestHandleLoadWithParams_EmptyOverrideTensor — пустой overrideTensor
// парсится в указатель на "" (json.Unmarshal создаёт pointer для любого
// значения поля, в т.ч. пустой строки). Handler трактует *OverrideTensor == ""
// как "не задан" (см. handleLoadWithParams).
func TestHandleLoadWithParams_EmptyOverrideTensor(t *testing.T) {
	body := `{"name":"test","overrideTensor":""}`
	var req loadWithParamsRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if req.OverrideTensor == nil {
		t.Fatalf("OverrideTensor pointer should be set (json.Unmarshal создаёт pointer для любого присутствующего поля)")
	}
	if *req.OverrideTensor != "" {
		t.Errorf("OverrideTensor should be empty string, got %q", *req.OverrideTensor)
	}
}

// TestHandleLoadWithParams_EmptyTensorSplit — пустой tensorSplit парсится в nil slice.
func TestHandleLoadWithParams_EmptyTensorSplit(t *testing.T) {
	body := `{"name":"test","tensorSplit":[]}`
	var req loadWithParamsRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if len(req.TensorSplit) != 0 {
		t.Errorf("TensorSplit should be empty slice, got %v", req.TensorSplit)
	}
}

// intToStr — локальный хелпер для конкатенации int в JSON.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}