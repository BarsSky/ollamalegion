//go:build !llama_stub || llama_stub

// preflight_vram_known_r83_test.go — R83 §9.2 (2026-09-26).
//
// Ноль в max_vram_n_ctx означает ДВА разных случая, и до этой правки preflight
// не различал их:
//   - метрик нет (cppworker не опрошен) → проверку пропускаем (fail-open);
//   - VRAM ИЗВЕСТНА, но веса модели в неё не влезают (exact-fit потолок = 0) →
//     reload не поможет, клиент должен получить причину, а не таймаут.
//
// Живой контекст: Qwen3.8-27B (16.4 GB) на 8 GB GPU. cppworker отдаёт
// vram_known=true и max_vram_n_ctx=0 — это и есть второй случай.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR83_DecidePreflight_VRAMKnownButZero_Rejects — VRAM известна, потолок 0,
// клиент просит БОЛЬШЕ текущего n_ctx → отказ, а не reload.
//
// current_n_ctx=32768 и запрос на 48000 токенов: именно так выглядит живой
// кейс — модель уже работает через partial offload, клиент просит контекст
// больше, а веса в VRAM не помещаются ни при каком gpu_layers.
func TestR83_DecidePreflight_VRAMKnownButZero_Rejects(t *testing.T) {
	state := &NCtxBackendState{
		BackendID:       "cppworker-gpu-bundled-agent",
		CurrentNCtx:     32768,
		MaxVRAMNCtx:     0, // веса не влезают
		VRAMKnown:       true,
		AvailableVRAMMB: 8191,
		ModelMaxContext: 32768,
		GGUFMaxContext:  262144,
	}
	meta := &RequestMeta{EstimatedPromptTokens: 40000, RequestedNPredict: 8000}
	cfg := NCtxReloadConfig{AutoReloadMaxNCtx: 131072}

	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightReject {
		t.Fatalf("Decision = %v, want PreflightReject (VRAM известна, веса не влезают)", res.Decision)
	}
	if res.RejectStatus == 0 {
		t.Error("RejectStatus не выставлен")
	}

	var body map[string]interface{}
	if err := json.Unmarshal([]byte(res.RejectBody), &body); err != nil {
		t.Fatalf("RejectBody не JSON: %v", err)
	}
	if got, _ := body["vram_known"].(bool); !got {
		t.Errorf("vram_known = %v, want true — клиент/оператор должны видеть, "+
			"что VRAM измерена, а не «нет данных»", body["vram_known"])
	}
	reason, _ := body["reason"].(string)
	if !strings.Contains(reason, "weights do not fit") {
		t.Errorf("reason = %q, want про веса, которые не влезают", reason)
	}
	if !strings.Contains(reason, "Reload cannot raise") {
		t.Errorf("reason = %q, want явное «reload не поможет»", reason)
	}
	suggestion, _ := body["suggestion"].(string)
	if !strings.Contains(suggestion, "smaller model") {
		t.Errorf("suggestion = %q, want совет про меньшую модель/квант, "+
			"а не общий «raise contextLengthMax»", suggestion)
	}
}

// TestR83_DecidePreflight_VRAMUnknown_KeepsFailOpen — метрик нет (старый
// cppworker или poller ещё не опросил) → прежнее поведение: не отказываем
// из-за отсутствия данных.
func TestR83_DecidePreflight_VRAMUnknown_KeepsFailOpen(t *testing.T) {
	state := &NCtxBackendState{
		BackendID:       "old-cppworker",
		CurrentNCtx:     32768,
		MaxVRAMNCtx:     0,
		VRAMKnown:       false, // данных нет
		ModelMaxContext: 32768,
		GGUFMaxContext:  262144,
	}
	meta := &RequestMeta{EstimatedPromptTokens: 20000, RequestedNPredict: 8000}
	cfg := NCtxReloadConfig{AutoReloadMaxNCtx: 131072}

	res := DecidePreflight(meta, state, cfg)
	if res.Decision == PreflightReject {
		t.Fatalf("Decision = PreflightReject при неизвестной VRAM: "+
			"fail-open сломан (body=%s)", res.RejectBody)
	}
}

// TestR83_DecidePreflight_VRAMKnownAndPositive_StillReloads — VRAM известна и
// потолок больше нуля → прежняя логика (reload на partial offload), наш новый
// отказ не должен перехватывать этот случай.
func TestR83_DecidePreflight_VRAMKnownAndPositive_StillReloads(t *testing.T) {
	state := &NCtxBackendState{
		BackendID:       "cppworker",
		CurrentNCtx:     8192,
		MaxVRAMNCtx:     40000,
		VRAMKnown:       true,
		ModelMaxContext: 32768,
		GGUFMaxContext:  262144,
	}
	meta := &RequestMeta{EstimatedPromptTokens: 20000, RequestedNPredict: 8000}
	cfg := NCtxReloadConfig{AutoReloadMaxNCtx: 131072, AutoReloadVRAMSafetyFactor: 0.9}

	res := DecidePreflight(meta, state, cfg)
	if res.Decision == PreflightReject {
		t.Fatalf("Decision = PreflightReject при max_vram_n_ctx=40000: "+
			"новый отказ перехватил рабочий случай (body=%s)", res.RejectBody)
	}
}
