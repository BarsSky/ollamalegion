//go:build llama_stub

// lazyload_nofit_refusal_r83_test.go — R83 §9.3 (2026-09-26).
//
// «Не влезает даже в CPU-only» (Source="fallback_no_fit") раньше был только
// записью в лог: opts всё равно возвращались, и загрузка шла — 16.4 GB модель
// уезжала в RAM целиком, вытесняя остальное, а клиент видел OOM/таймаут вместо
// причины. Теперь случай превращается в *InsufficientResourcesError, которую
// handleInferenceError отдаёт как HTTP 413 + structured JSON (code=6).
package main

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestR83_FallbackNoFit_BecomesInsufficientResources — helper обязан вернуть
// типизированный отказ с числами.
func TestR83_FallbackNoFit_BecomesInsufficientResources(t *testing.T) {
	r := LazyLoadRationale{
		Source:             "fallback_no_fit",
		RequestedNCtx:      32768,
		MaxViableNCtx:      0,
		ModelSize:          16464440224, // Qwen3.8-27B
		AvailableRAMBytes:  8 * 1024 * 1024 * 1024,
		AvailableVRAMBytes: 6 * 1024 * 1024 * 1024,
		EstimatedKVCacheMB: 1088,
	}

	err := insufficientResourcesFromFallbackNoFit("qwen3.8:latest", r)
	if err == nil {
		t.Fatal("отказ не сформирован — загрузка 16 GB в RAM продолжилась бы")
	}
	var irErr *InsufficientResourcesError
	if !errors.As(err, &irErr) {
		t.Fatalf("тип ошибки %T, want *InsufficientResourcesError (её понимает handleInferenceError)", err)
	}
	if irErr.Model != "qwen3.8:latest" {
		t.Errorf("Model = %q", irErr.Model)
	}
	if irErr.ModelSizeBytes != r.ModelSize {
		t.Errorf("ModelSizeBytes = %d, want %d", irErr.ModelSizeBytes, r.ModelSize)
	}
	if irErr.AvailableRAMMB != 8192 {
		t.Errorf("AvailableRAMMB = %d, want 8192", irErr.AvailableRAMMB)
	}
	if irErr.KVCacheRequiredMB != 1088 {
		t.Errorf("KVCacheRequiredMB = %d, want 1088", irErr.KVCacheRequiredMB)
	}

	// Сообщение должно быть actionable: имя модели, размер, доступная память.
	// Размер печатается в MB целочисленно (15701 MB для 16 464 440 224 байт).
	msg := err.Error()
	for _, want := range []string{"qwen3.8:latest", "15701 MB", "8192 MB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("в сообщении нет %q: %s", want, msg)
		}
	}
}

// TestR83_FallbackNoFit_OtherSourcesUnaffected — остальные источники решения
// (exact_fit / reduced_nctx / partial_offload / fallback_no_meta) отказа не дают.
func TestR83_FallbackNoFit_OtherSourcesUnaffected(t *testing.T) {
	for _, src := range []string{"exact_fit", "reduced_nctx", "partial_offload", "fallback_no_meta", ""} {
		r := LazyLoadRationale{Source: src, RequestedNCtx: 32768}
		if err := insufficientResourcesFromFallbackNoFit("m", r); err != nil {
			t.Errorf("source=%q: получен отказ %v, want nil", src, err)
		}
	}
}

// TestR83_FallbackNoFit_HTTPShape — отказ должен давать 413 и JSON с
// code=insufficient_resources: эту форму читает и балансер, и клиент.
func TestR83_FallbackNoFit_HTTPShape(t *testing.T) {
	r := LazyLoadRationale{
		Source:             "fallback_no_fit",
		RequestedNCtx:      32768,
		ModelSize:          16464440224,
		AvailableRAMBytes:  4 * 1024 * 1024 * 1024,
		AvailableVRAMBytes: 1 * 1024 * 1024 * 1024,
	}
	irErr, ok := insufficientResourcesFromFallbackNoFit("m", r).(*InsufficientResourcesError)
	if !ok {
		t.Fatal("ожидался *InsufficientResourcesError")
	}
	rec := httptest.NewRecorder()
	writeInsufficientResourcesResponse(rec, irErr)
	if rec.Code != 413 {
		t.Errorf("HTTP = %d, want 413", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error":"insufficient_resources"`) {
		t.Errorf("body без insufficient_resources: %s", rec.Body.String())
	}
}
