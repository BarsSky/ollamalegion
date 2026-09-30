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

// TestR83Policy_ClientWindowBeyondCeiling_Rejects — R83-политика (2026-10-01):
// отказ выдаётся только когда запрошенное КЛИЕНТОМ окно не влезает в потолок.
//
// Раньше здесь отказывали из-за max_vram_n_ctx=0 (веса не влезают целиком),
// даже не спрашивая клиента. Теперь молчащий клиент никогда не получает отказ
// «на всякий случай»: решение принимается по его окну.
func TestR83Policy_ClientWindowBeyondCeiling_Rejects(t *testing.T) {
	state := &NCtxBackendState{
		BackendID:          "cppworker-gpu-bundled-agent",
		CurrentNCtx:        32768,
		MaxVRAMNCtx:        0, // веса не влезают целиком (partial offload)
		VRAMKnown:          true,
		AvailableVRAMMB:    8191,
		ModelMaxContext:    131072,
		PhysicalMaxContext: 131072,
		GGUFMaxContext:     131072,
	}
	meta := &RequestMeta{
		RequestedNCtxOverride: 262144, // клиент просит больше, чем модель держит
		EstimatedPromptTokens: 40000,
		RequestedNPredict:     8000,
	}
	cfg := NCtxReloadConfig{AutoReloadMaxNCtx: 131072}

	res := DecidePreflight(meta, state, cfg)
	if res.Decision != PreflightReject {
		t.Fatalf("Decision = %v, want PreflightReject (262144 > потолка 131072)", res.Decision)
	}
	if res.RejectStatus == 0 {
		t.Error("RejectStatus не выставлен")
	}

	var body map[string]interface{}
	if err := json.Unmarshal([]byte(res.RejectBody), &body); err != nil {
		t.Fatalf("RejectBody не JSON: %v", err)
	}
	// Требование пользователя: понятный текст, а не «параметр + параметр = ошибка».
	errText, _ := body["error"].(string)
	if !strings.Contains(errText, "не поместится") {
		t.Errorf("error = %q, ожидалось объяснение «не поместится»", errText)
	}
	if got, _ := body["backend_ceiling_n_ctx"].(float64); got != 131072 {
		t.Errorf("backend_ceiling_n_ctx = %v, ожидалось 131072", body["backend_ceiling_n_ctx"])
	}
	if wtd, _ := body["what_to_do"].(string); !strings.Contains(wtd, "Уменьшите окно") {
		t.Errorf("what_to_do = %q, ожидался совет, что делать", wtd)
	}
}

// TestR83Policy_VRAMKnownZero_SilentClientServed — та же VRAM-картина, но клиент
// окно не указывал → NoOp (обслуживаем как есть), без ложного отказа.
func TestR83Policy_VRAMKnownZero_SilentClientServed(t *testing.T) {
	state := &NCtxBackendState{
		BackendID:       "cppworker-gpu-bundled-agent",
		CurrentNCtx:     32768,
		MaxVRAMNCtx:     0,
		VRAMKnown:       true,
		AvailableVRAMMB: 8191,
		ModelMaxContext: 131072,
		GGUFMaxContext:  131072,
	}
	meta := &RequestMeta{EstimatedPromptTokens: 40000, RequestedNPredict: 8000}

	res := DecidePreflight(meta, state, NCtxReloadConfig{AutoReloadMaxNCtx: 131072})
	if res.Decision != PreflightNoOp {
		t.Fatalf("Decision = %v, want PreflightNoOp (клиент не указал окно): %s",
			res.Decision, res.RejectBody)
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
