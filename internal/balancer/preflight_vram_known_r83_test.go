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
// окно не указывал: отказ ПО VRAM недопустим (fail-open). R664 шаг B добавляет
// второй исход — «заявки нет и данные не влезают» → 413 с числами, но это отказ
// по ОТСУТСТВИЮ ЗАЯВКИ, а не по VRAM: подмени мы причину, оператор искал бы
// свободную память там, где нужно заявить окно.
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
	cfg := NCtxReloadConfig{AutoReloadMaxNCtx: 131072}

	// (1) Промпт ВЛЕЗАЕТ в загруженное окно: обслуживаем как есть, отказа нет —
	// это и есть fail-open по VRAM (живой дефект был именно здесь).
	fits := &RequestMeta{EstimatedPromptTokens: 20000, RequestedNPredict: 8000}
	if res := DecidePreflight(fits, state, cfg); res.Decision != PreflightNoOp {
		t.Fatalf("промпт влезает в окно 32768 — ожидался NoOp, получено %v: %s",
			res.Decision, res.RejectBody)
	}

	// (2) Промпт НЕ влезает и заявки нет: 413 шага B, и причина в нём — «заяви
	// окно», а не «мало VRAM» (иначе совет оператору был бы ложным).
	big := &RequestMeta{EstimatedPromptTokens: 40000, RequestedNPredict: 8000}
	res := DecidePreflight(big, state, cfg)
	if res.Decision != PreflightReject {
		t.Fatalf("промпт 40000 не влезает в окно 32768 без заявки — ожидался 413, получено %v: %s",
			res.Decision, res.RejectBody)
	}
	out := assertWindowExhaustedR664(t, []byte(res.RejectBody), 40000)
	if got := toIntR447(out["available_context_tokens"]); got != 131072 {
		t.Fatalf("available_context_tokens = %d, want 131072: потолок известен, VRAM-ограничение "+
			"не должно подменять его нулём", got)
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
	// Промпт влезает в загруженное окно 32768 — решения «отказать» быть не может.
	meta := &RequestMeta{EstimatedPromptTokens: 20000, RequestedNPredict: 8000}
	cfg := NCtxReloadConfig{AutoReloadMaxNCtx: 131072}

	res := DecidePreflight(meta, state, cfg)
	if res.Decision == PreflightReject {
		t.Fatalf("Decision = PreflightReject при неизвестной VRAM: "+
			"fail-open сломан (body=%s)", res.RejectBody)
	}
}

// TestR83_DecidePreflight_VRAMKnownAndPositive_StillReloads — VRAM известна и
// потолок больше нуля: законный рост под ЗАЯВКУ клиента идёт прежним путём,
// отказ шага B его не перехватывает.
func TestR83_DecidePreflight_VRAMKnownAndPositive_StillReloads(t *testing.T) {
	state := &NCtxBackendState{
		BackendID:       "cppworker",
		CurrentNCtx:     8192,
		MaxVRAMNCtx:     40000,
		VRAMKnown:       true,
		ModelMaxContext: 32768,
		GGUFMaxContext:  262144,
	}
	// Заявка клиента 28672: больше загруженных 8192 и не выше потолка 32768.
	meta := &RequestMeta{EstimatedPromptTokens: 20000, RequestedNPredict: 8000,
		RequestedNCtxOverride: 28672}
	cfg := NCtxReloadConfig{AutoReloadMaxNCtx: 131072, AutoReloadVRAMSafetyFactor: 0.9}

	res := DecidePreflight(meta, state, cfg)
	if res.Decision == PreflightReject {
		t.Fatalf("Decision = PreflightReject при max_vram_n_ctx=40000 и заявке 28672: "+
			"отказ перехватил рабочий случай (body=%s)", res.RejectBody)
	}
	if res.Decision != PreflightReload || res.TargetNCtx != 28672 {
		t.Fatalf("Decision = %v (target=%d), want PreflightReload на заявленные 28672",
			res.Decision, res.TargetNCtx)
	}
}
