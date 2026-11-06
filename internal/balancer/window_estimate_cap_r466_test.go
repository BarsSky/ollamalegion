// window_estimate_cap_r466_test.go — R466 (2026-11-06): выведенное из оценки
// окно не может служить причиной отказа «физически не поместится».
//
// ЖИВОЙ ДЕФЕКТ (второй стенд: A10 ×2, 50 ГБ, Qwen3.8-27B, Cline). Клиент
// показывал занятый контекст 63.6k, балансер отвечал «требуется выделить 151743
// на контекстное окно» и окно НЕ поднимал. 151743 — это не требование клиента,
// а наша оценка промпта (эвристика по символам), завышающая в 2–3 раза; клиент
// num_ctx не присылал вовсе.
//
// Правило закреплено тестами: догадка урезается до потолка и грузится,
// требование клиента сверх потолка — по-прежнему честный 413.
package balancer

import "testing"

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

// TestR466_AgentDerivedAboveCeilingReloadsInsteadOfRejecting — ровно живой
// случай: оценка 140000 + резерв = 140320 при потолке 131072. Раньше — 413
// «требуется 140320»; теперь — перезагрузка ДО потолка.
func TestR466_AgentDerivedAboveCeilingReloadsInsteadOfRejecting(t *testing.T) {
	meta := &RequestMeta{ModelName: "Qwen3.8-27B", EstimatedPromptTokens: 140000}

	res := DecidePreflight(meta, r466State(1), DefaultNCtxReloadConfig())
	if res.Decision != PreflightReload {
		t.Fatalf("окно выведено из нашей же оценки — отказ недопустим, получено %v (status=%d, body=%s)",
			res.Decision, res.RejectStatus, res.RejectBody)
	}
	if res.TargetNCtx != 131072 {
		t.Fatalf("TargetNCtx=%d, ожидался потолок 131072 (урезанная оценка)", res.TargetNCtx)
	}
	if !res.AgentDerivedWindow {
		t.Fatal("окно выведено из оценки — флаг AgentDerivedWindow обязан быть поднят")
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

// TestR466_CapRespectsSlots — суммарное окно делится между слотами, поэтому
// урезать надо до потолка НА КЛИЕНТА, иначе 413 остался бы.
func TestR466_CapRespectsSlots(t *testing.T) {
	meta := &RequestMeta{ModelName: "m", EstimatedPromptTokens: 140000}

	res := DecidePreflight(meta, r466State(2), DefaultNCtxReloadConfig())
	if res.Decision != PreflightReload {
		t.Fatalf("при 2 слотах ждали рост, получено %v (body=%s)", res.Decision, res.RejectBody)
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
