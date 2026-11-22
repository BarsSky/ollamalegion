// window_estimate_cap_r466_test.go — R466 (2026-11-06): выведенное из оценки
// окно не может служить причиной отказа «физически не поместится».
//
// ЖИВОЙ ДЕФЕКТ (второй стенд: A10 ×2, 50 ГБ, Qwen3.8-27B, Cline). Клиент
// показывал занятый контекст 63.6k, балансер отвечал «требуется выделить 151743
// на контекстное окно» и окно НЕ поднимал. 151743 — это не требование клиента,
// а наша оценка промпта (эвристика по символам), завышающая в 2–3 раза; клиент
// num_ctx не присылал вовсе.
//
// Правило закреплено тестами: догадка урезается до потолка и НИКОГДА не даёт
// отказ «по физике», требование клиента сверх потолка — по-прежнему честный 413.
//
// R664 ШАГ B (2026-11-20): роста без заявки клиента больше нет, поэтому исход
// «урезали до потолка и грузим» стал «урезали до потолка и отказали с числами»
// (код context_window_exhausted_no_growth_request). Суть R466 сохранена: отказ
// называет ПОТОЛОК, а не нашу оценку, — и с заявкой до потолка рост по-прежнему
// происходит.
package balancer

import (
	"net/http"
	"testing"
)

// r466State — состояние как на живом стенде: окно слота 32768, потолок модели
// 131072 (заявленное/GGUF), слотов 1.
func r466State(slots int) *NCtxBackendState {
	perSeq := 32768
	return &NCtxBackendState{
		BackendID:            "cppworker-gpu-bundled-agent",
		CurrentNCtx:          perSeq * slots,
		CurrentContextPerSeq: perSeq,
		CurrentSlots:         slots,
		ModelMaxContext:      131072,
		PhysicalMaxContext:   131072,
		GGUFMaxContext:       131072,
		AutoReloadMaxNCtx:    131072,
	}
}

// TestR466_AgentDerivedAboveCeilingRefusesWithNumbers — ровно живой случай:
// оценка 140000 + резерв = 140320 при потолке 131072.
//
// R664 ШАГ B меняет исход, но НЕ суть R466: отказом по физике наша догадка
// по-прежнему быть не может (в теле назван потолок 131072, а не «нужно 140320
// физически не помещается»). Меняется то, что роста без заявки нет: окно не
// перезагружается под оценку, клиент получает числа и точное действие.
func TestR466_AgentDerivedAboveCeilingRefusesWithNumbers(t *testing.T) {
	meta := &RequestMeta{ModelName: "Qwen3.8-27B", EstimatedPromptTokens: 140000}

	res := DecidePreflight(meta, r466State(1), DefaultNCtxReloadConfig())
	if res.Decision != PreflightReject || res.RejectStatus != http.StatusRequestEntityTooLarge {
		t.Fatalf("окно выведено из нашей же оценки, заявки нет — ожидался отказ шага B с числами, "+
			"получено %v (status=%d)", res.Decision, res.RejectStatus)
	}
	out := assertWindowExhaustedR664(t, []byte(res.RejectBody), 140000)
	// Потолок на клиента назван честно: 131072, а не «140320 не помещается».
	if got := toIntR447(out["available_context_tokens"]); got != 131072 {
		t.Fatalf("available_context_tokens = %d, want 131072", got)
	}
	if res.AgentDerivedWindow {
		t.Fatal("отказ не выводит окно — флаг AgentDerivedWindow обязан быть снят")
	}

	// Заявка ровно до потолка: перезагрузка идёт до 131072 — прежний смысл R466
	// («расти, а не отказывать по собственной оценке») сохраняется.
	declared := &RequestMeta{ModelName: "m", EstimatedPromptTokens: 140000, RequestedNCtxOverride: 131072}
	res = DecidePreflight(declared, r466State(1), DefaultNCtxReloadConfig())
	if res.Decision != PreflightReload {
		t.Fatalf("заявка до потолка — ожидался рост, получено %v (%s)", res.Decision, res.RejectBody)
	}
	if res.TargetNCtx != 131072 {
		t.Fatalf("TargetNCtx=%d, ожидался потолок 131072 (заявленное клиентом окно)", res.TargetNCtx)
	}
}

// TestR466_ClientRequestedAboveCeilingStillRejects — требование клиента сверх
// физики остаётся отказом: это его число, а не наша догадка.
func TestR466_ClientRequestedAboveCeilingStillRejects(t *testing.T) {
	meta := &RequestMeta{ModelName: "m", EstimatedPromptTokens: 1000, RequestedNCtxOverride: 262144}

	res := DecidePreflight(meta, r466State(1), DefaultNCtxReloadConfig())
	if res.Decision != PreflightReject {
		t.Fatalf("клиент просит 262144 при потолке 131072 — ожидался отказ, получено %v", res.Decision)
	}
	if res.RejectStatus != 413 {
		t.Fatalf("статус отказа = %d, want 413", res.RejectStatus)
	}
}

// TestR466_CapRespectsSlots — суммарное окно делится между слотами, поэтому и
// урезание идёт до потолка НА КЛИЕНТА (65536 при 2 слотах). Раньше это
// проверялось целью роста; после R664 шаг B — полем available_context_tokens того
// же отказа: заявка до этой доли обязана быть выполнимой.
func TestR466_CapRespectsSlots(t *testing.T) {
	meta := &RequestMeta{ModelName: "m", EstimatedPromptTokens: 140000}

	res := DecidePreflight(meta, r466State(2), DefaultNCtxReloadConfig())
	if res.Decision != PreflightReject {
		t.Fatalf("при 2 слотах и без заявки ждали отказ шага B, получено %v (body=%s)",
			res.Decision, res.RejectBody)
	}
	out := assertWindowExhaustedR664(t, []byte(res.RejectBody), 140000)
	if got := toIntR447(out["available_context_tokens"]); got != 65536 {
		t.Fatalf("available_context_tokens = %d, ожидалось 131072/2 = 65536", got)
	}

	// Заявка ровно на долю клиента — рост до неё же.
	declared := &RequestMeta{ModelName: "m", EstimatedPromptTokens: 140000, RequestedNCtxOverride: 65536}
	res = DecidePreflight(declared, r466State(2), DefaultNCtxReloadConfig())
	if res.Decision != PreflightReload {
		t.Fatalf("заявка на долю 65536 при 2 слотах — ждали рост, получено %v (%s)",
			res.Decision, res.RejectBody)
	}
	if res.TargetNCtx != 65536 {
		t.Fatalf("TargetNCtx=%d, ожидалось 131072/2 = 65536", res.TargetNCtx)
	}
}

// TestR466_CapNeverShrinksWindow — если потолок на клиента ниже загруженного
// окна (уменьшать нельзя), решения о перезагрузке нет: обслуживаем как есть.
func TestR466_CapNeverShrinksWindow(t *testing.T) {
	state := r466State(1)
	state.PhysicalMaxContext = 16384 // «потолок» ниже загруженных 32768
	meta := &RequestMeta{ModelName: "m", EstimatedPromptTokens: 40000}

	if res := DecidePreflight(meta, state, DefaultNCtxReloadConfig()); res.Decision == PreflightReload && res.TargetNCtx < 32768 {
		t.Fatalf("рост не может быть уменьшением: TargetNCtx=%d при загруженных 32768", res.TargetNCtx)
	}
}

// TestR466_HelperArithmetic — арифметика помощника без состояния.
func TestR466_HelperArithmetic(t *testing.T) {
	if got := derivedWindowCapR466(131072, 1); got != 131072 {
		t.Fatalf("слот 1: %d", got)
	}
	if got := derivedWindowCapR466(131072, 2); got != 65536 {
		t.Fatalf("слота 2: %d", got)
	}
	if got := derivedWindowCapR466(0, 4); got != 0 {
		t.Fatalf("неизвестный потолок: %d", got)
	}
	// Неделимый потолок: 100000/3 = 33333, выравнивание вниз → 33280, чтобы
	// perClientToTotal(33280,3) = 99840 не превысил потолок.
	if got := derivedWindowCapR466(100000, 3); got != 33280 {
		t.Fatalf("неделимый потолок: %d, want 33280", got)
	}
	if got := perClientToTotal(derivedWindowCapR466(100000, 3), 3); got > 100000 {
		t.Fatalf("суммарное окно %d больше потолка 100000 — 413 вернулся бы", got)
	}
	if v, cut := capDerivedWindowR466(1000, 131072, 1); cut || v != 1000 {
		t.Fatalf("урезать нечего: v=%d cut=%t", v, cut)
	}
	if v, cut := capDerivedWindowR466(200000, 131072, 1); !cut || v != 131072 {
		t.Fatalf("урезание: v=%d cut=%t", v, cut)
	}
}
