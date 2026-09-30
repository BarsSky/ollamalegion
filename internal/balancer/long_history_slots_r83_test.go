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
		ModelMaxContext:      8192,  // profile hint (не потолок)
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

// TestR83_LongHistory_BeyondSlot_GrowsWithSlotsMultiplier — требование больше
// окна слота, но укладывается в потолок НА КЛИЕНТА → reload с суммарным
// таргетом «на клиента × слоты».
func TestR83_LongHistory_BeyondSlot_GrowsWithSlotsMultiplier(t *testing.T) {
	meta := &RequestMeta{EstimatedPromptTokens: 40000, RequestedNPredict: 512}
	state := stateForLongHistory(65536, 32768, 2) // потолок на клиента = 131072/2 = 65536

	res := DecidePreflight(meta, state, DefaultNCtxReloadConfig())

	if res.Decision != PreflightReload {
		t.Fatalf("ожидался reload (нужно ~42513 на клиента при потолке 65536), получено %v: %s",
			res.Decision, res.RejectBody)
	}
	// Таргет пересчитывается в суммарный: PAD256(42513)=42752 × 2 слота = 85504.
	total := reloadTargetForState(res.TargetNCtx, state)
	if total < 85504 {
		t.Errorf("суммарный таргет = %d, want >= 85504 (окно клиента × 2 слота)", total)
	}
}

// TestR83_LongHistory_BeyondPerClientCeiling_RejectedWithClearReason — если
// требование выше потолка НА КЛИЕНТА, отказ обязан назвать этот потолок и
// способы его поднять (parallel=1 / окно / история), а не «VRAM exhausted».
func TestR83_LongHistory_BeyondPerClientCeiling_RejectedWithClearReason(t *testing.T) {
	meta := &RequestMeta{EstimatedPromptTokens: 101000, RequestedNPredict: 512}
	state := stateForLongHistory(65536, 32768, 2)

	res := DecidePreflight(meta, state, DefaultNCtxReloadConfig())
	if res.Decision != PreflightReject {
		t.Fatalf("ожидался отказ (101k на клиента при потолке 65536), получено %v", res.Decision)
	}
	if res.RejectStatus != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", res.RejectStatus)
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(res.RejectBody), &body); err != nil {
		t.Fatalf("тело отказа не JSON: %v", err)
	}
	reason, _ := body["reason"].(string)
	if !strings.Contains(reason, "per-client context ceiling") {
		t.Errorf("в причине нет потолка НА КЛИЕНТА: %s", reason)
	}
	suggestion, _ := body["suggestion"].(string)
	for _, want := range []string{"parallel=1", "слотах"} {
		if !strings.Contains(suggestion, want) {
			t.Errorf("подсказка не объясняет, что делать (нет %q): %s", want, suggestion)
		}
	}
	// Главное: ложного «Reload cannot raise this ceiling» быть не должно — оно
	// уводило оператора и Cline в сторону памяти вместо окна на клиента.
	if strings.Contains(reason, "VRAM is known") {
		t.Errorf("остался старый текст про VRAM: %s", reason)
	}
}

// TestR83_SingleSlot_BehaviourUnchanged — при одном слоте сравнение идёт с
// суммарным потолком, как раньше.
func TestR83_SingleSlot_BehaviourUnchanged(t *testing.T) {
	meta := &RequestMeta{EstimatedPromptTokens: 40000, RequestedNPredict: 512}
	state := stateForLongHistory(65536, 65536, 1)

	res := DecidePreflight(meta, state, DefaultNCtxReloadConfig())
	if res.Decision != PreflightNoOp {
		t.Fatalf("при одном слоте 42513 ≤ 65536 → NoOp, получено %v: %s", res.Decision, res.RejectBody)
	}
}
