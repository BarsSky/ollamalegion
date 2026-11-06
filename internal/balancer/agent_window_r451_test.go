// agent_window_r451_test.go — R451 (2026-11-06): окно не растят из-за
// max_tokens клиента.
//
// ЖИВОЙ ДЕФЕКТ (Cline на 9B, второй ход). Первый ход прошёл на окне 32768.
// Второй (продолжение) пришёл с `max_tokens=15393`, и preflight посчитал
// «нужное окно» = оценка промпта 24677 + бюджет клиента 15393 + 64 = 40134 при
// загруженных 32768 → cppworker получил load с `contextSize=40134`, увидел
// другие параметры и сделал unload+load: 243 с посреди хода агента.
//
// Промпт при этом в окно влезал (24677 из 32768, свободно ~8k под ответ), а
// бюджет клиента — «сколько я готов принять», а не «сколько модели нужно места»:
// воркер урезает n_predict по свободному месту сам.
package balancer

import "testing"

// r451State — состояние бэкенда, где окно слота 32768 и потолки большие:
// ровно конфигурация живого случая.
func r451State() *NCtxBackendState {
	return &NCtxBackendState{
		BackendID:            "cppworker-gpu-bundled-agent",
		CurrentNCtx:          32768,
		CurrentContextPerSeq: 32768,
		CurrentSlots:         1,
		ModelMaxContext:      262144,
		PhysicalMaxContext:   262144,
		GGUFMaxContext:       262144,
		AutoReloadMaxNCtx:    131072,
	}
}

// TestR451_ClientMaxTokensDoesNotGrowWindow — ровно живой случай.
func TestR451_ClientMaxTokensDoesNotGrowWindow(t *testing.T) {
	meta := &RequestMeta{
		ModelName:             "Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M",
		EstimatedPromptTokens: 24677, // калиброванная оценка того хода
		RequestedNPredict:     15393, // max_tokens, который прислал Cline
	}

	res := DecidePreflight(meta, r451State(), DefaultNCtxReloadConfig())
	if res.Decision != PreflightNoOp {
		t.Fatalf("промпт 24677 влезает в окно 32768 — перезагрузки быть не должно, "+
			"получено %v (target=%d): это живой дефект R451", res.Decision, res.TargetNCtx)
	}
}

// TestR451_OldFormulaWouldHaveReloaded — контроль: со старым правилом (промпт +
// весь бюджет клиента) этот же вход даёт 40134 и перезагрузку. Тест фиксирует,
// что мы лечим именно эту арифметику, а не что-то ещё.
func TestR451_OldFormulaWouldHaveReloaded(t *testing.T) {
	meta := &RequestMeta{
		ModelName:             "m",
		EstimatedPromptTokens: 24677,
		RequestedNPredict:     15393,
	}
	old := meta.EstimatedPromptTokens + agentOutputBudget(meta) + agentWindowSlackTokens
	if old != 40134 {
		t.Fatalf("старая формула даёт %d, а в живом дефекте было 40134 — числа разошлись", old)
	}
	if old <= r451State().effectivePerSeqNCtx() {
		t.Fatal("старая формула не превышает загруженное окно — тест не про этот дефект")
	}

	t.Setenv("LB_AGENT_WINDOW_BUDGET", "client")
	res := DecidePreflight(meta, r451State(), DefaultNCtxReloadConfig())
	if res.Decision == PreflightNoOp {
		t.Fatal("в режиме client перезагрузка обязана остаться (обратная совместимость)")
	}
	if res.TargetNCtx < old {
		t.Fatalf("в режиме client target=%d, ожидалось >= %d", res.TargetNCtx, old)
	}
}

// TestR451_PromptBeyondWindowStillGrows — промпт не влезает: рост обязателен.
func TestR451_PromptBeyondWindowStillGrows(t *testing.T) {
	meta := &RequestMeta{ModelName: "m", EstimatedPromptTokens: 40000, RequestedNPredict: 8192}

	res := DecidePreflight(meta, r451State(), DefaultNCtxReloadConfig())
	if res.Decision != PreflightReload {
		t.Fatalf("промпт 40000 не влезает в окно 32768 — ожидался PreflightReload, получено %v", res.Decision)
	}
	if res.TargetNCtx < 40000 {
		t.Fatalf("TargetNCtx=%d меньше промпта 40000 — рост не решит проблему", res.TargetNCtx)
	}
}

// TestR451_NoRoomForOutputGrows — промпт съел окно, под ответ не остаётся
// осмысленного места: рост нужен (иначе воркер урежет вывод до горстки токенов).
func TestR451_NoRoomForOutputGrows(t *testing.T) {
	meta := &RequestMeta{ModelName: "m", EstimatedPromptTokens: 32600}

	res := DecidePreflight(meta, r451State(), DefaultNCtxReloadConfig())
	if res.Decision != PreflightReload {
		t.Fatalf("под ответ остаётся 168 токенов (меньше минимума 256) — ожидался рост, получено %v", res.Decision)
	}
}

// TestR451_ClientExplicitWindowStillWins — явно запрошенное окно не отменяется.
func TestR451_ClientExplicitWindowStillWins(t *testing.T) {
	meta := &RequestMeta{ModelName: "m", EstimatedPromptTokens: 100, RequestedNCtxOverride: 65536}

	res := DecidePreflight(meta, r451State(), DefaultNCtxReloadConfig())
	if res.Decision != PreflightReload {
		t.Fatalf("клиент попросил 65536 при загруженных 32768 — ожидался рост, получено %v", res.Decision)
	}
	if res.TargetNCtx < 65536 {
		t.Fatalf("TargetNCtx=%d меньше запрошенного 65536", res.TargetNCtx)
	}
}

// TestR451_BudgetModeSwitch — режим по умолчанию minimal, старое поведение —
// одной переменной.
func TestR451_BudgetModeSwitch(t *testing.T) {
	if got := agentWindowBudgetMode(); got != "minimal" {
		t.Fatalf("режим по умолчанию = %q, want minimal", got)
	}
	meta := &RequestMeta{EstimatedPromptTokens: 1000, RequestedNPredict: 8000}
	if got := agentWindowReserveTokens(meta); got != agentOutputReserveMinTokens {
		t.Fatalf("резерв в режиме minimal = %d, want %d", got, agentOutputReserveMinTokens)
	}

	t.Setenv("LB_AGENT_WINDOW_BUDGET", "client")
	if got := agentWindowBudgetMode(); got != "client" {
		t.Fatalf("LB_AGENT_WINDOW_BUDGET=client не включает режим: %q", got)
	}
	if got := agentWindowReserveTokens(meta); got != 8000 {
		t.Fatalf("резерв в режиме client = %d, want 8000 (бюджет клиента)", got)
	}

	// Мусор в переменной не ломает поведение: неизвестное значение = default.
	t.Setenv("LB_AGENT_WINDOW_BUDGET", "абракадабра")
	if got := agentWindowBudgetMode(); got != "minimal" {
		t.Fatalf("неизвестное значение переменной = %q, want minimal", got)
	}
}

// TestR451_EstimateMatchesLiveTurn — арифметика живого хода: оценка 24677 (это
// сырые ~61700, поделённые на запасной коэффициент 2.5) укладывается в окно.
func TestR451_EstimateMatchesLiveTurn(t *testing.T) {
	raw := 24677 * 25 / 10 // тот же запасной коэффициент, что в promptCalibration
	if raw < 60000 || raw > 63000 {
		t.Fatalf("сырая оценка живого хода = %d, ожидалось ≈61700", raw)
	}
	if need, why := growthNeedR447(24677, 32768, 0); need {
		t.Fatalf("24677 из 32768 — роста быть не должно, причина: %s", why)
	}
}
