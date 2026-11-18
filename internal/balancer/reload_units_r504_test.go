//go:build llama_stub

// reload_units_r504_test.go — R504 (2026-11-19), аудит S4, пункт N-2.
//
// ДЕФЕКТ. Воркер сообщает требование НА СЛОТ (`prompt_too_long`: actual_tokens +
// n_predict против окна слота), а `contextSize` в перезагрузке — СУММАРНОЕ окно
// модели: воркер делит его между слотами. Балансер отправлял требование в
// contextSize как есть, поэтому при parallel=2 модель поднималась ровно на окно
// слота: слот получал цель/2 < требования, и следующий запрос снова ловил 413 —
// «reload без выигрыша» по кругу.
//
// Тесты держат пересчёт в ОБЕИХ ветках решателя (prompt_too_long и
// n_ctx_needs_reload) и то, что при одном слоте ничего не меняется.
package balancer

import "testing"

// reloadCoordR504 — координатор с заданным числом слотов в хинтах reload'а.
func reloadCoordR504(t *testing.T, slots, ceiling int) *NCtxReloadCoordinator {
	t.Helper()
	c := NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx:             true,
		AutoReloadMaxNCtx:          ceiling,
		AutoReloadVRAMSafetyFactor: 0.9,
	})
	c.SetReloadHintsProvider(func(backendID, modelName string) ReloadHints {
		return ReloadHints{Parallel: slots}
	})
	return c
}

// TestR504_PromptTooLongRequirementBecomesTotal — N-2: требование на слот
// пересчитывается в суммарное окно перед перезагрузкой.
//
// 12000+1000=13000 на слот, 2 слота → 26000 суммарно → 32768 после округления.
// До правки уходило 16384 суммарно = 8192 на слот < 13000 → повторный 413.
func TestR504_PromptTooLongRequirementBecomesTotal(t *testing.T) {
	c := reloadCoordR504(t, 2, 131072)
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodePromptTooLong,
		ActualTokens: 12000,
		NPredict:     1000,
		CurrentNCtx:  8192,
	}
	plan := c.DecideReloadBackendForModel("b1", "m", bridgeErr, 0,
		ReloadCeiling{Effective: 131072, Env: 131072})
	if plan.Decision != DecisionReload {
		t.Fatalf("решение = %s, ожидалось reload; причина=%s", plan.Decision.String(), plan.Reason)
	}
	if plan.NewNCtx != 32768 {
		t.Fatalf("цель = %d, ожидалось 32768 (13000 на слот × 2 слота, округлено "+
			"вверх): иначе слот снова получит 8192 при требовании 13000 (аудит N-2)",
			plan.NewNCtx)
	}
}

// TestR504_PromptTooLongCeilingComparesTotals — N-2: потолок СУММАРНЫЙ, поэтому и
// сравнивать с ним надо суммарное требование.
func TestR504_PromptTooLongCeilingComparesTotals(t *testing.T) {
	c := reloadCoordR504(t, 2, 131072)
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodePromptTooLong,
		ActualTokens: 69000,
		NPredict:     1000, // 70000 на слот → 140000 суммарно > 131072
		CurrentNCtx:  8192,
	}
	plan := c.DecideReloadBackendForModel("b1", "m", bridgeErr, 0,
		ReloadCeiling{Effective: 131072, Env: 131072})
	if plan.Decision == DecisionReload {
		t.Fatalf("решение reload при требовании 140000 суммарно и потолке 131072: "+
			"перезагрузка дала бы 65536 на слот при требовании 70000 (причина: %s)",
			plan.Reason)
	}
}

// TestR504_NeedsReloadTargetIsTotal — N-2: вторая ветка (n_ctx_needs_reload)
// тоже отдаёт суммарное окно, и предел VRAM на слот пересчитывается туда же.
func TestR504_NeedsReloadTargetIsTotal(t *testing.T) {
	c := reloadCoordR504(t, 2, 131072)
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodeNCtxNeedsReload,
		RequiredNCtx: 12000, // на слот
		CurrentNCtx:  8192,
		MaxVRAMNCtx:  20000, // на слот (так его считает bridge)
	}
	plan := c.DecideReloadBackendForModel("b1", "m", bridgeErr, 0,
		ReloadCeiling{Effective: 131072, Env: 131072})
	if plan.Decision != DecisionReload {
		t.Fatalf("решение = %s, ожидалось reload; причина=%s", plan.Decision.String(), plan.Reason)
	}
	if plan.NewNCtx != 32768 {
		t.Fatalf("цель = %d, ожидалось 32768 (12000×2=24000, округлено вверх)", plan.NewNCtx)
	}
	if perSeq := plan.NewNCtx / 2; perSeq < 12000 {
		t.Fatalf("на слот придётся %d токенов при требовании 12000 — перезагрузка "+
			"не даёт выигрыша (аудит N-2)", perSeq)
	}
}

// TestR504_SingleSlotUnchanged — один слот: пересчёт не меняет историческое
// поведение (регресс-защита для однocлотовых стендов).
func TestR504_SingleSlotUnchanged(t *testing.T) {
	c := reloadCoordR504(t, 1, 131072)
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodePromptTooLong,
		ActualTokens: 12000,
		NPredict:     1000,
		CurrentNCtx:  8192,
	}
	plan := c.DecideReloadBackendForModel("b1", "m", bridgeErr, 0,
		ReloadCeiling{Effective: 131072, Env: 131072})
	if plan.Decision != DecisionReload || plan.NewNCtx != 16384 {
		t.Fatalf("один слот: решение=%s цель=%d, ожидалось reload/16384",
			plan.Decision.String(), plan.NewNCtx)
	}
}
