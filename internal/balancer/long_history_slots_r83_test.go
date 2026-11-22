//go:build llama_stub

// long_history_slots_r83_test.go — R83-fix (2026-09-30).
//
// ЖИВОЕ ВОСПРОИЗВЕДЕНИЕ ЖАЛОБЫ CLINE (стенд, gemma-4-E4B, parallel=2):
// запрос с историей ~101k токенов получал 413 за 0 секунд с текстом
//
//	reason: "VRAM is known (4791 MB available) but this model's weights do not fit:
//	         max_vram_n_ctx=0 at any gpu_layers ... Reload cannot raise this ceiling."
//
// Две ошибки в этом решении:
//  1. `max_vram_n_ctx == 0` означает лишь «веса не влезают в VRAM ЦЕЛИКОМ», а не
//     «обслужить нельзя»: модель уже работает через partial offload (KV в RAM),
//     и перезагрузка с бОльшим окном задачу решает. Отказывать нельзя, пока
//     требование укладывается в физический потолок (GGUF/operator cap).
//  2. Потолок сравнивался с СУММАРНЫМ окном, хотя клиенту доступен один слот:
//     при slots=2 «на клиента» вдвое меньше, и отказ обязан это называть
//     (иначе Cline бесконечно «сжимает контекст», не понимая предела).
package balancer

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func stateForLongHistory(currentTotal, perSeq, slots int) *NCtxBackendState {
	return &NCtxBackendState{
		BackendID:            "cppworker-1",
		CurrentNCtx:          currentTotal,
		CurrentContextPerSeq: perSeq,
		CurrentSlots:         slots,
		MaxVRAMNCtx:          0, // веса не влезают в VRAM целиком (partial offload)
		VRAMKnown:            true,
		AvailableVRAMMB:      4791,
		ModelMaxContext:      8192, // profile hint (не потолок)
		PhysicalMaxContext:   131072,
		GGUFMaxContext:       131072,
		AutoReloadMaxNCtx:    131072,
	}
}

// TestR83_LongHistory_GrowsInsteadOfFalseVRAMReject — требование, которое
// помещается в физический потолок, но не в текущее окно: должен быть reload,
// а не отказ «VRAM exhausted».
func TestR83_LongHistory_GrowsInsteadOfFalseVRAMReject(t *testing.T) {
	meta := &RequestMeta{EstimatedPromptTokens: 20000, RequestedNPredict: 512}
	state := stateForLongHistory(65536, 32768, 2) // на клиента 32768, нужно ~22.5k… берём ниже

	res := DecidePreflight(meta, state, DefaultNCtxReloadConfig())

	// required = 20000 + 512 + 1 + 2000 = 22513 ≤ perSeq(32768) → NoOp (уже помещается).
	if res.Decision != PreflightNoOp {
		t.Fatalf("запрос помещается в окно слота (22513 ≤ 32768), ожидался NoOp, получено %v: %s",
			res.Decision, res.RejectBody)
	}
	if res.Decision == PreflightReject {
		t.Error("ложный отказ: промпт помещается в окно слота")
	}
}

// TestR83Policy_ClientWindowBeyondSlot_GrowsWithSlotsMultiplier — R83-политика
// (2026-10-01): окно задаёт КЛИЕНТ. Если он просит 40000 на клиента, а загружено
// 32768 на слоте → перезагрузка под клиента, таргет суммарный = окно × слоты.
func TestR83Policy_ClientWindowBeyondSlot_GrowsWithSlotsMultiplier(t *testing.T) {
	meta := &RequestMeta{
		RequestedNCtxOverride: 40000, // клиент явно просит своё окно
		EstimatedPromptTokens: 40000,
		RequestedNPredict:     512,
	}
	state := stateForLongHistory(65536, 32768, 2) // потолок на клиента = 131072/2 = 65536

	res := DecidePreflight(meta, state, DefaultNCtxReloadConfig())

	if res.Decision != PreflightReload {
		t.Fatalf("ожидался reload (клиент просит 40000 на клиента, загружено 32768), "+
			"получено %v: %s", res.Decision, res.RejectBody)
	}
	// TargetNCtx — окно НА КЛИЕНТА; суммарный считается через слоты.
	if res.TargetNCtx != 40000 {
		t.Errorf("TargetNCtx = %d, want 40000 (окно клиента, не суммарное)", res.TargetNCtx)
	}
	total := reloadTargetForState(res.TargetNCtx, state)
	if total < 80000 {
		t.Errorf("суммарный таргет = %d, want >= 80000 (окно клиента × 2 слота)", total)
	}
}

// TestR83Policy_ClientWindowBeyondPerClientCeiling_RejectedWithClearReason —
// единственный случай, когда о «маленьком окне» сообщаем: запрошенное окно
// физически не помещается. Текст обязан быть понятным человеку.
func TestR83Policy_ClientWindowBeyondPerClientCeiling_RejectedWithClearReason(t *testing.T) {
	meta := &RequestMeta{
		RequestedNCtxOverride: 101000, // на клиента; суммарно нужно ~202240 > потолка 131072
		EstimatedPromptTokens: 101000,
		RequestedNPredict:     512,
	}
	state := stateForLongHistory(65536, 32768, 2)

	res := DecidePreflight(meta, state, DefaultNCtxReloadConfig())
	if res.Decision != PreflightReject {
		t.Fatalf("ожидался отказ (101000 на клиента не влезает в потолок 131072 суммарно), "+
			"получено %v", res.Decision)
	}
	if res.RejectStatus != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", res.RejectStatus)
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(res.RejectBody), &body); err != nil {
		t.Fatalf("тело отказа не JSON: %v", err)
	}
	// Главное требование пользователя: ПОНЯТНЫЙ текст, а не «X + Y = error».
	errText, _ := body["error"].(string)
	for _, want := range []string{"не поместится", "параллельных слотах"} {
		if !strings.Contains(errText, want) {
			t.Errorf("в тексте ошибки нет %q: %s", want, errText)
		}
	}
	if got, _ := body["max_per_client_n_ctx"].(float64); got != 65536 {
		t.Errorf("max_per_client_n_ctx = %v, want 65536 (потолок 131072 ÷ 2 слота)", body["max_per_client_n_ctx"])
	}
	whatToDo, _ := body["what_to_do"].(string)
	for _, want := range []string{"65536", "слот"} {
		if !strings.Contains(whatToDo, want) {
			t.Errorf("подсказка не объясняет, что делать (нет %q): %s", want, whatToDo)
		}
	}
}

// TestR83Policy_SilentClient_ContractAfterR466 — КОНТРАКТ МЕНЯЛСЯ ДВАЖДЫ, тест
// фиксирует последний.
//
// БЫЛО (R83, 2026-10-01): клиент не указал num_ctx → NoOp, никаких перезагрузок
// «по оценке промпта».
//
// СТАЛО в R253: промпт по оценке не влезает в окно слота → окно растёт по НАШЕЙ
// оценке; R466 урезал выведенное окно до потолка на клиента, чтобы отказ не
// строился на завышенной эвристике.
//
// СТАЛО в R664 ШАГ B (2026-11-20): рост — ТОЛЬКО под заявку клиента (R83/R68).
// Оценка промпта больше не основание перезагружать модель: это была перезагрузка
// под число, которого клиент не просил (эвристика по символам завышает в 2–3
// раза — живой случай «нужно 151743 при занятых 63.6k»). Без заявки ответ —
// 413 context_window_exhausted_no_growth_request, и в нём ТРИ числа: сколько в
// промпте, сколько загружено, сколько минимум заявить, плюс потолок на клиента
// (если он ниже нужного, заявка невыполнима — и это видно из того же тела).
//
// Прежнее поведение R83 возвращается одной переменной LB_NCTX_GROW_FOR_AGENTS=0 —
// второй кейс ниже.
func TestR83Policy_SilentClient_ContractAfterR466(t *testing.T) {
	meta := &RequestMeta{
		// Клиент молчит про окно, но промпт по оценке огромный.
		EstimatedPromptTokens: 100000,
		RequestedNPredict:     8192,
		HasTools:              true,
	}
	state := stateForLongHistory(65536, 32768, 2) // на клиента 32768, потолок 65536

	res := DecidePreflight(meta, state, DefaultNCtxReloadConfig())
	if res.Decision != PreflightReject || res.RejectStatus != http.StatusRequestEntityTooLarge {
		t.Fatalf("окно клиент не заявил — ожидался отказ 413 с числами, получено %v (status=%d): %s",
			res.Decision, res.RejectStatus, res.RejectBody)
	}
	out := assertWindowExhaustedR664(t, []byte(res.RejectBody), 100000)
	// Потолок на клиента меньше нужного минимума: заявкой ЭТОТ запрос не спасти, и
	// тело обязано называть оба числа, чтобы оператор это увидел.
	if got := toIntR447(out["available_context_tokens"]); got != 65536 {
		t.Fatalf("available_context_tokens = %d, want 65536 (131072 ÷ 2 слота)", got)
	}
	if got := toIntR447(out["required_window_tokens"]); got <= 65536 {
		t.Fatalf("required_window_tokens = %d обязано превышать потолок на клиента 65536 — "+
			"запрос этим окном действительно не обслужить", got)
	}

	// Явно запрошенное клиентом окно сверх потолка — по-прежнему отказ: это его
	// требование, а не догадка балансера.
	explicit := &RequestMeta{EstimatedPromptTokens: 100, RequestedNCtxOverride: 131072}
	if rej := DecidePreflight(explicit, state, DefaultNCtxReloadConfig()); rej.Decision != PreflightReject {
		t.Fatalf("клиент просит 131072 при потолке 65536 на клиента — ожидался 413, получено %v", rej.Decision)
	}

	// Поведение R83 возвращается одной переменной (обратная совместимость).
	t.Setenv("LB_NCTX_GROW_FOR_AGENTS", "0")
	resLegacy := DecidePreflight(meta, state, DefaultNCtxReloadConfig())
	if resLegacy.Decision != PreflightNoOp {
		t.Fatalf("LB_NCTX_GROW_FOR_AGENTS=0 должен возвращать прежнее NoOp, получено %v: %s",
			resLegacy.Decision, resLegacy.RejectBody)
	}
}

// TestR83Policy_ModelNotLoaded_LoadsWithClientWindow — модель ещё не загружена:
// решение о загрузке принимает cppworker (ensureModelLoadedWithNCtx), а балансер
// пропускает запрос как есть.
func TestR83Policy_ModelNotLoaded_LoadsWithClientWindow(t *testing.T) {
	meta := &RequestMeta{RequestedNCtxOverride: 32768, EstimatedPromptTokens: 100}
	state := &NCtxBackendState{
		BackendID:          "cppworker-1",
		CurrentNCtx:        0, // не загружена
		PhysicalMaxContext: 131072,
	}

	res := DecidePreflight(meta, state, DefaultNCtxReloadConfig())
	if res.Decision != PreflightNoOp {
		t.Fatalf("модель не загружена — ожидался NoOp (cppworker загрузит её окном клиента), "+
			"получено %v", res.Decision)
	}
}

// TestR83Policy_LoadedWindowBigger_NoReloadNoNotice — загружено БОЛЬШЕ, чем
// просит клиент: не перезагружаем и не уведомляем (требование пользователя).
func TestR83Policy_LoadedWindowBigger_NoReloadNoNotice(t *testing.T) {
	meta := &RequestMeta{RequestedNCtxOverride: 8192, EstimatedPromptTokens: 100}
	state := stateForLongHistory(65536, 32768, 2)

	res := DecidePreflight(meta, state, DefaultNCtxReloadConfig())
	if res.Decision != PreflightNoOp {
		t.Fatalf("загружено 32768 на клиента при запросе 8192 — ожидался NoOp, получено %v", res.Decision)
	}
}

// TestR83Policy_ReasoningMismatch_ClearConflict — если клиент требует
// размышление, а модель загружена без него, перезагрузки НЕТ: отказ 409 с
// перечислением отличий и понятным объяснением.
func TestR83Policy_ReasoningMismatch_ClearConflict(t *testing.T) {
	loadedOff := false
	state := stateForLongHistory(65536, 32768, 2)
	state.CurrentReasoningEnabled = &loadedOff
	wantOn := true
	meta := &RequestMeta{
		ModelName:             "gemma-4-E4B-it-Q4_K_M",
		RequestedNCtxOverride: 32768,
		RequestedThink:        &wantOn,
	}

	res := DecidePreflight(meta, state, DefaultNCtxReloadConfig())
	if res.Decision != PreflightReject {
		t.Fatalf("ожидался отказ при расхождении reasoning, получено %v: %s", res.Decision, res.RejectBody)
	}
	if res.RejectStatus != http.StatusConflict {
		t.Errorf("status = %d, want 409 (это конфликт параметров, а не размер окна)", res.RejectStatus)
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(res.RejectBody), &body); err != nil {
		t.Fatalf("тело отказа не JSON: %v", err)
	}
	errText, _ := body["error"].(string)
	if !strings.Contains(errText, "reasoning") || !strings.Contains(errText, "выключено") {
		t.Errorf("текст не объясняет расхождение: %s", errText)
	}
	diffs, _ := body["param_differences"].([]interface{})
	if len(diffs) != 1 {
		t.Fatalf("param_differences = %v, ожидался один элемент", body["param_differences"])
	}
	first, _ := diffs[0].(map[string]interface{})
	if first["parameter"] != "reasoning" || first["loaded"] != "выключено" || first["requested"] != "включено" {
		t.Errorf("param_differences[0] = %v", first)
	}
	if wtd, _ := body["what_to_do"].(string); !strings.Contains(wtd, "think=true") {
		t.Errorf("подсказка не говорит, что делать: %s", wtd)
	}
}

// TestR83Policy_ReasoningMismatch_MatchingDoesNotBlock — совпадающий параметр
// ничего не ломает: запрос идёт дальше.
func TestR83Policy_ReasoningMismatch_MatchingDoesNotBlock(t *testing.T) {
	loadedOn := true
	state := stateForLongHistory(65536, 32768, 2)
	state.CurrentReasoningEnabled = &loadedOn
	wantOn := true
	meta := &RequestMeta{RequestedNCtxOverride: 8192, RequestedThink: &wantOn}

	if res := DecidePreflight(meta, state, DefaultNCtxReloadConfig()); res.Decision != PreflightNoOp {
		t.Fatalf("reasoning совпадает — ожидался NoOp, получено %v: %s", res.Decision, res.RejectBody)
	}
}

// TestR83Policy_ReasoningUnknown_DoesNotBlock — cppworker не сообщил режим
// (модель не загружена / старая версия) — расхождение не выдумываем.
func TestR83Policy_ReasoningUnknown_DoesNotBlock(t *testing.T) {
	state := stateForLongHistory(65536, 32768, 2) // CurrentReasoningEnabled == nil
	wantOn := true
	meta := &RequestMeta{RequestedNCtxOverride: 8192, RequestedThink: &wantOn}

	if res := DecidePreflight(meta, state, DefaultNCtxReloadConfig()); res.Decision != PreflightNoOp {
		t.Fatalf("режим reasoning неизвестен — ожидался NoOp, получено %v: %s", res.Decision, res.RejectBody)
	}
}

// TestR83_SingleSlot_BehaviourUnchanged — при одном слоте сравнение идёт с
// суммарным потолком, как раньше (клиент молчит про окно → NoOp).
func TestR83_SingleSlot_BehaviourUnchanged(t *testing.T) {
	meta := &RequestMeta{EstimatedPromptTokens: 40000, RequestedNPredict: 512}
	state := stateForLongHistory(65536, 65536, 1)

	res := DecidePreflight(meta, state, DefaultNCtxReloadConfig())
	if res.Decision != PreflightNoOp {
		t.Fatalf("клиент не указал окно → NoOp, получено %v: %s", res.Decision, res.RejectBody)
	}
}
